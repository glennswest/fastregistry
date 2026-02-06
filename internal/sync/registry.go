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

// RegistryClient handles synchronization from OCI/Docker registries (including FastRegistry)
type RegistryClient struct {
	config   config.SyncSource
	blobs    *storage.BlobStore
	metadata *storage.MetadataStore
	client   *http.Client
}

// NewRegistryClient creates a new registry sync client
func NewRegistryClient(cfg config.SyncSource, blobs *storage.BlobStore, metadata *storage.MetadataStore) *RegistryClient {
	return &RegistryClient{
		config:   cfg,
		blobs:    blobs,
		metadata: metadata,
		client: &http.Client{
			Timeout: 10 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// ListRepositories returns all repositories from the registry
func (r *RegistryClient) ListRepositories(ctx context.Context) ([]string, error) {
	// If specific repositories are configured, use those
	if len(r.config.Repositories) > 0 {
		return r.config.Repositories, nil
	}

	// Otherwise, list from catalog
	url := fmt.Sprintf("%s/v2/_catalog", strings.TrimSuffix(r.config.URL, "/"))

	var allRepos []string
	for url != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		r.addAuth(req)

		resp, err := r.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("catalog request failed: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("catalog returned %d", resp.StatusCode)
		}

		var result struct {
			Repositories []string `json:"repositories"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decoding catalog: %w", err)
		}
		resp.Body.Close()

		allRepos = append(allRepos, result.Repositories...)

		// Handle pagination via Link header
		url = getNextLink(resp.Header.Get("Link"))
	}

	return allRepos, nil
}

// SyncRepository syncs a single repository
func (r *RegistryClient) SyncRepository(ctx context.Context, repo string, progress *Progress) error {
	log.Printf("Syncing repository: %s", repo)

	// List tags
	tags, err := r.listTags(ctx, repo)
	if err != nil {
		return fmt.Errorf("listing tags: %w", err)
	}

	// Filter tags
	filteredTags := r.filterTags(tags)
	progress.TotalTags = len(filteredTags)

	for _, tag := range filteredTags {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := r.syncTag(ctx, repo, tag, progress); err != nil {
			log.Printf("Warning: failed to sync %s:%s: %v", repo, tag, err)
			progress.FailedTags++
			continue
		}
		progress.SyncedTags++
	}

	return nil
}

func (r *RegistryClient) listTags(ctx context.Context, repo string) ([]string, error) {
	url := fmt.Sprintf("%s/v2/%s/tags/list", strings.TrimSuffix(r.config.URL, "/"), repo)

	var allTags []string
	for url != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		r.addAuth(req)

		resp, err := r.client.Do(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			return nil, nil // Repository has no tags
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("tags list returned %d", resp.StatusCode)
		}

		var result struct {
			Tags []string `json:"tags"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			return nil, err
		}
		resp.Body.Close()

		allTags = append(allTags, result.Tags...)
		url = getNextLink(resp.Header.Get("Link"))
	}

	return allTags, nil
}

func (r *RegistryClient) filterTags(tags []string) []string {
	if r.config.IncludeTags == "" && r.config.ExcludeTags == "" {
		return tags
	}

	var filtered []string
	for _, tag := range tags {
		if r.shouldIncludeTag(tag) {
			filtered = append(filtered, tag)
		}
	}

	return filtered
}

func (r *RegistryClient) shouldIncludeTag(tag string) bool {
	// Check exclude pattern first
	if r.config.ExcludeTags != "" {
		matched, _ := path.Match(r.config.ExcludeTags, tag)
		if matched {
			return false
		}
	}

	// Check include pattern
	if r.config.IncludeTags != "" {
		matched, _ := path.Match(r.config.IncludeTags, tag)
		return matched
	}

	return true
}

func (r *RegistryClient) syncTag(ctx context.Context, repo, tag string, progress *Progress) error {
	// Fetch manifest
	manifest, manifestBytes, err := r.fetchManifest(ctx, repo, tag)
	if err != nil {
		return fmt.Errorf("fetching manifest: %w", err)
	}

	// Check if we already have this manifest
	existing, err := r.metadata.GetManifest(repo, tag)
	if err == nil && existing.Digest == manifest.Digest {
		// Already up to date
		return nil
	}

	// Parse manifest to get blob references
	var parsed struct {
		MediaType string `json:"mediaType"`
		Config    struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"layers"`
		Manifests []struct {
			Digest   string `json:"digest"`
			Size     int64  `json:"size"`
			Platform struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
			} `json:"platform"`
		} `json:"manifests"`
	}

	if err := json.Unmarshal(manifestBytes, &parsed); err != nil {
		return fmt.Errorf("parsing manifest: %w", err)
	}

	// Handle manifest list/index - sync all referenced manifests
	if len(parsed.Manifests) > 0 {
		for _, m := range parsed.Manifests {
			if err := r.syncTag(ctx, repo, m.Digest, progress); err != nil {
				log.Printf("Warning: failed to sync platform manifest %s: %v", m.Digest, err)
			}
		}
	}

	// Sync config blob
	if parsed.Config.Digest != "" {
		dgst, _ := digest.Parse(parsed.Config.Digest)
		if err := r.syncBlob(ctx, repo, dgst, parsed.Config.Size, progress); err != nil {
			return fmt.Errorf("syncing config: %w", err)
		}
	}

	// Sync layer blobs with concurrency
	sem := make(chan struct{}, r.config.Concurrency)
	if r.config.Concurrency == 0 {
		sem = make(chan struct{}, 5) // default
	}
	errCh := make(chan error, len(parsed.Layers))

	for _, layer := range parsed.Layers {
		sem <- struct{}{}
		go func(dgstStr string, size int64) {
			defer func() { <-sem }()
			dgst, _ := digest.Parse(dgstStr)
			if err := r.syncBlob(ctx, repo, dgst, size, progress); err != nil {
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

	// Store manifest blob
	if err := r.blobs.Put(manifest.Digest, strings.NewReader(string(manifestBytes)), int64(len(manifestBytes))); err != nil {
		return fmt.Errorf("storing manifest: %w", err)
	}

	// Store manifest metadata
	if err := r.metadata.PutManifest(repo, tag, manifest); err != nil {
		return fmt.Errorf("storing manifest metadata: %w", err)
	}

	// Link blobs to repo
	r.metadata.LinkBlobToRepo(manifest.Digest, repo)
	if parsed.Config.Digest != "" {
		cfgDgst, _ := digest.Parse(parsed.Config.Digest)
		r.metadata.LinkBlobToRepo(cfgDgst, repo)
	}
	for _, layer := range parsed.Layers {
		layerDgst, _ := digest.Parse(layer.Digest)
		r.metadata.LinkBlobToRepo(layerDgst, repo)
	}

	log.Printf("Synced %s:%s", repo, tag)
	return nil
}

func (r *RegistryClient) fetchManifest(ctx context.Context, repo, ref string) (*storage.ManifestMeta, []byte, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", strings.TrimSuffix(r.config.URL, "/"), repo, ref)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	r.addAuth(req)

	// Accept multiple manifest types
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
	}, ", "))

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("manifest fetch returned %d", resp.StatusCode)
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

func (r *RegistryClient) syncBlob(ctx context.Context, repo string, dgst digest.Digest, size int64, progress *Progress) error {
	// Check if we already have this blob
	if r.blobs.Exists(dgst) {
		return nil
	}

	url := fmt.Sprintf("%s/v2/%s/blobs/%s", strings.TrimSuffix(r.config.URL, "/"), repo, dgst)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	r.addAuth(req)

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("blob fetch returned %d", resp.StatusCode)
	}

	// Track progress
	reader := &progressReader{
		reader:   resp.Body,
		progress: progress,
	}

	return r.blobs.Put(dgst, reader, size)
}

func (r *RegistryClient) addAuth(req *http.Request) {
	// Support basic auth via token field (format: "username:password")
	if r.config.Token != "" {
		if strings.Contains(r.config.Token, ":") {
			// Basic auth
			parts := strings.SplitN(r.config.Token, ":", 2)
			req.SetBasicAuth(parts[0], parts[1])
		} else {
			// Bearer token
			req.Header.Set("Authorization", "Bearer "+r.config.Token)
		}
	}

	// Robot account (Quay-style)
	if r.config.RobotAccount != "" && r.config.RobotToken != "" {
		req.SetBasicAuth(r.config.RobotAccount, r.config.RobotToken)
	}
}

// getNextLink parses the Link header for pagination
func getNextLink(linkHeader string) string {
	if linkHeader == "" {
		return ""
	}

	// Parse: <url>; rel="next"
	for _, part := range strings.Split(linkHeader, ",") {
		part = strings.TrimSpace(part)
		if strings.Contains(part, `rel="next"`) {
			start := strings.Index(part, "<")
			end := strings.Index(part, ">")
			if start >= 0 && end > start {
				return part[start+1 : end]
			}
		}
	}

	return ""
}
