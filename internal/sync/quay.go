package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gwest/fastregistry/config"
	"github.com/gwest/fastregistry/internal/storage"
	"github.com/gwest/fastregistry/pkg/digest"
)

// QuayClient handles synchronization from Quay registries
type QuayClient struct {
	config   config.SyncSource
	blobs    *storage.BlobStore
	metadata *storage.MetadataStore
	client   *http.Client
}

// QuayRepository represents a repository in Quay's API
type QuayRepository struct {
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Description string `json:"description"`
	IsPublic    bool   `json:"is_public"`
}

// QuayTag represents a tag in Quay's API
type QuayTag struct {
	Name           string `json:"name"`
	ManifestDigest string `json:"manifest_digest"`
	LastModified   string `json:"last_modified"`
	Size           int64  `json:"size"`
}

// NewQuayClient creates a new Quay sync client
func NewQuayClient(cfg config.SyncSource, blobs *storage.BlobStore, metadata *storage.MetadataStore) *QuayClient {
	return &QuayClient{
		config:   cfg,
		blobs:    blobs,
		metadata: metadata,
		client: &http.Client{
			Timeout: 5 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// ListRepositories returns all repositories from configured organizations
func (q *QuayClient) ListRepositories(ctx context.Context) ([]string, error) {
	var repos []string

	for _, org := range q.config.Organizations {
		orgRepos, err := q.listOrgRepositories(ctx, org)
		if err != nil {
			log.Printf("Warning: failed to list repos for org %s: %v", org, err)
			continue
		}
		repos = append(repos, orgRepos...)
	}

	// Add explicitly configured repos
	repos = append(repos, q.config.Repositories...)

	return repos, nil
}

func (q *QuayClient) listOrgRepositories(ctx context.Context, org string) ([]string, error) {
	url := fmt.Sprintf("%s/api/v1/repository?namespace=%s", strings.TrimSuffix(q.config.URL, "/"), org)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	q.addAuth(req)

	resp, err := q.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Quay API returned %d", resp.StatusCode)
	}

	var result struct {
		Repositories []QuayRepository `json:"repositories"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	var repos []string
	for _, repo := range result.Repositories {
		repos = append(repos, path.Join(repo.Namespace, repo.Name))
	}

	return repos, nil
}

// ListTags returns all tags for a repository
func (q *QuayClient) ListTags(ctx context.Context, repo string) ([]QuayTag, error) {
	url := fmt.Sprintf("%s/api/v1/repository/%s/tag/", strings.TrimSuffix(q.config.URL, "/"), repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	q.addAuth(req)

	resp, err := q.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Quay API returned %d", resp.StatusCode)
	}

	var result struct {
		Tags []QuayTag `json:"tags"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	// Filter tags based on include/exclude patterns
	var filtered []QuayTag
	for _, tag := range result.Tags {
		if q.shouldIncludeTag(tag.Name) {
			filtered = append(filtered, tag)
		}
	}

	return filtered, nil
}

func (q *QuayClient) shouldIncludeTag(tag string) bool {
	// Check exclude pattern first
	if q.config.ExcludeTags != "" {
		matched, _ := path.Match(q.config.ExcludeTags, tag)
		if matched {
			return false
		}
	}

	// Check include pattern
	if q.config.IncludeTags != "" {
		matched, _ := path.Match(q.config.IncludeTags, tag)
		return matched
	}

	return true
}

// SyncRepository synchronizes a single repository
func (q *QuayClient) SyncRepository(ctx context.Context, repo string, progress *Progress) error {
	log.Printf("Syncing repository: %s", repo)

	tags, err := q.ListTags(ctx, repo)
	if err != nil {
		return fmt.Errorf("listing tags: %w", err)
	}

	progress.TotalTags = len(tags)

	for _, tag := range tags {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := q.syncTag(ctx, repo, tag, progress); err != nil {
			log.Printf("Warning: failed to sync %s:%s: %v", repo, tag.Name, err)
			progress.FailedTags++
			continue
		}
		progress.SyncedTags++
	}

	return nil
}

func (q *QuayClient) syncTag(ctx context.Context, repo string, tag QuayTag, progress *Progress) error {
	// Check if we already have this manifest
	existing, err := q.metadata.GetManifest(repo, tag.Name)
	if err == nil && string(existing.Digest) == tag.ManifestDigest {
		// Already up to date
		return nil
	}

	// Fetch manifest from Quay (using OCI distribution API)
	manifest, manifestBytes, err := q.fetchManifest(ctx, repo, tag.Name)
	if err != nil {
		return fmt.Errorf("fetching manifest: %w", err)
	}

	// Parse manifest to get layers
	var parsed struct {
		Config struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"layers"`
	}
	json.Unmarshal(manifestBytes, &parsed)

	// Sync config blob
	if parsed.Config.Digest != "" {
		dgst, _ := digest.Parse(parsed.Config.Digest)
		if err := q.syncBlob(ctx, repo, dgst, parsed.Config.Size, progress); err != nil {
			return fmt.Errorf("syncing config: %w", err)
		}
	}

	// Sync layers in parallel
	sem := make(chan struct{}, q.config.Concurrency)
	errCh := make(chan error, len(parsed.Layers))

	for _, layer := range parsed.Layers {
		sem <- struct{}{}
		go func(dgstStr string, size int64) {
			defer func() { <-sem }()
			dgst, _ := digest.Parse(dgstStr)
			if err := q.syncBlob(ctx, repo, dgst, size, progress); err != nil {
				errCh <- fmt.Errorf("syncing layer %s: %w", dgstStr[:12], err)
			}
		}(layer.Digest, layer.Size)
	}

	// Wait for all layers
	for i := 0; i < cap(sem); i++ {
		sem <- struct{}{}
	}

	// Check for errors
	select {
	case err := <-errCh:
		return err
	default:
	}

	// Store manifest
	dgst := digest.FromBytes(manifestBytes)
	if err := q.blobs.Put(dgst, strings.NewReader(string(manifestBytes)), int64(len(manifestBytes))); err != nil {
		return fmt.Errorf("storing manifest: %w", err)
	}

	// Store metadata
	if err := q.metadata.PutManifest(repo, tag.Name, manifest); err != nil {
		return fmt.Errorf("storing manifest metadata: %w", err)
	}

	// Link blobs
	q.metadata.LinkBlobToRepo(dgst, repo)
	if parsed.Config.Digest != "" {
		cfgDgst, _ := digest.Parse(parsed.Config.Digest)
		q.metadata.LinkBlobToRepo(cfgDgst, repo)
	}
	for _, layer := range parsed.Layers {
		layerDgst, _ := digest.Parse(layer.Digest)
		q.metadata.LinkBlobToRepo(layerDgst, repo)
	}

	log.Printf("Synced %s:%s", repo, tag.Name)
	return nil
}

func (q *QuayClient) fetchManifest(ctx context.Context, repo, ref string) (*storage.ManifestMeta, []byte, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", strings.TrimSuffix(q.config.URL, "/"), repo, ref)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	q.addAuth(req)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
	}, ", "))

	resp, err := q.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}

	dgst := digest.FromBytes(body)
	meta := &storage.ManifestMeta{
		Digest:    dgst,
		MediaType: resp.Header.Get("Content-Type"),
		Size:      int64(len(body)),
	}

	return meta, body, nil
}

func (q *QuayClient) syncBlob(ctx context.Context, repo string, dgst digest.Digest, size int64, progress *Progress) error {
	// Check if we already have this blob
	if q.blobs.Exists(dgst) {
		return nil
	}

	url := fmt.Sprintf("%s/v2/%s/blobs/%s", strings.TrimSuffix(q.config.URL, "/"), repo, dgst)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	q.addAuth(req)

	resp, err := q.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	// Track progress
	reader := &progressReader{
		reader:   resp.Body,
		progress: progress,
	}

	return q.blobs.Put(dgst, reader, size)
}

func (q *QuayClient) addAuth(req *http.Request) {
	if q.config.Token != "" {
		req.Header.Set("Authorization", "Bearer "+q.config.Token)
	} else if q.config.RobotToken != "" {
		req.Header.Set("Authorization", "Bearer "+q.config.RobotToken)
	}
}

// Progress tracks sync progress
type Progress struct {
	TotalRepos   int
	SyncedRepos  int
	FailedRepos  int
	TotalTags    int
	SyncedTags   int
	FailedTags   int
	BytesTotal   int64
	BytesSynced  int64
	StartTime    time.Time
	mu           chan struct{}
}

// NewProgress creates a new progress tracker
func NewProgress() *Progress {
	return &Progress{
		StartTime: time.Now(),
		mu:        make(chan struct{}, 1),
	}
}

func (p *Progress) addBytes(n int64) {
	select {
	case p.mu <- struct{}{}:
		p.BytesSynced += n
		<-p.mu
	default:
	}
}

type progressReader struct {
	reader   io.Reader
	progress *Progress
}

func (r *progressReader) Read(p []byte) (n int, err error) {
	n, err = r.reader.Read(p)
	if n > 0 && r.progress != nil {
		r.progress.addBytes(int64(n))
	}
	return
}
