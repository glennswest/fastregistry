package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gwest/fastregistry/config"
	"github.com/gwest/fastregistry/internal/storage"
	"github.com/gwest/fastregistry/pkg/digest"
)

// Proxy handles pull-through caching from upstream registries
type Proxy struct {
	mirrors  map[string]*upstream
	blobs    *storage.BlobStore
	metadata *storage.MetadataStore
	client   *http.Client
	mu       sync.RWMutex
}

type upstream struct {
	name     string
	url      string
	cacheTTL time.Duration
	token    string // Docker Hub token
	tokenExp time.Time
}

// NewProxy creates a new mirror proxy
func NewProxy(cfg []config.Mirror, blobs *storage.BlobStore, metadata *storage.MetadataStore) *Proxy {
	p := &Proxy{
		mirrors:  make(map[string]*upstream),
		blobs:    blobs,
		metadata: metadata,
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}

	for _, m := range cfg {
		p.mirrors[m.Name] = &upstream{
			name:     m.Name,
			url:      strings.TrimSuffix(m.Upstream, "/"),
			cacheTTL: m.CacheTTL,
		}
	}

	return p
}

// IsMirrored returns true if the repo prefix matches a configured mirror
func (p *Proxy) IsMirrored(repo string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	// Check for exact match or prefix match (e.g., "docker.io/library/nginx")
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) > 0 {
		if _, ok := p.mirrors[parts[0]]; ok {
			return parts[0], true
		}
	}

	return "", false
}

// GetManifest fetches a manifest from upstream, caching it locally
func (p *Proxy) GetManifest(ctx context.Context, mirrorName, repo, ref string) (*storage.ManifestMeta, []byte, error) {
	p.mu.RLock()
	up, ok := p.mirrors[mirrorName]
	p.mu.RUnlock()
	if !ok {
		return nil, nil, fmt.Errorf("unknown mirror: %s", mirrorName)
	}

	// Build upstream URL
	// Strip mirror prefix from repo
	upstreamRepo := strings.TrimPrefix(repo, mirrorName+"/")
	if upstreamRepo == repo {
		upstreamRepo = repo
	}

	// Handle Docker Hub's library images
	if mirrorName == "docker.io" && !strings.Contains(upstreamRepo, "/") {
		upstreamRepo = "library/" + upstreamRepo
	}

	url := fmt.Sprintf("%s/v2/%s/manifests/%s", up.url, upstreamRepo, ref)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}

	// Accept multiple manifest types
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
	}, ", "))

	// Add auth for Docker Hub
	if mirrorName == "docker.io" {
		token, err := p.getDockerHubToken(ctx, upstreamRepo)
		if err != nil {
			log.Printf("Warning: failed to get Docker Hub token: %v", err)
		} else {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("fetching manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil, storage.ErrManifestNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("reading manifest: %w", err)
	}

	// Compute digest
	dgst := digest.FromBytes(body)
	contentType := resp.Header.Get("Content-Type")

	// Store manifest blob
	if err := p.blobs.Put(dgst, strings.NewReader(string(body)), int64(len(body))); err != nil {
		log.Printf("Warning: failed to cache manifest blob: %v", err)
	}

	// Store metadata
	meta := &storage.ManifestMeta{
		Digest:    dgst,
		MediaType: contentType,
		Size:      int64(len(body)),
	}

	if err := p.metadata.PutManifest(repo, ref, meta); err != nil {
		log.Printf("Warning: failed to cache manifest metadata: %v", err)
	}

	// Link blob to repo
	p.metadata.LinkBlobToRepo(dgst, repo)

	return meta, body, nil
}

// GetBlob fetches a blob from upstream, caching it locally
func (p *Proxy) GetBlob(ctx context.Context, mirrorName, repo string, dgst digest.Digest) (io.ReadCloser, int64, error) {
	p.mu.RLock()
	up, ok := p.mirrors[mirrorName]
	p.mu.RUnlock()
	if !ok {
		return nil, 0, fmt.Errorf("unknown mirror: %s", mirrorName)
	}

	// Check if already cached
	if p.blobs.Exists(dgst) {
		return p.blobs.Get(dgst)
	}

	// Build upstream URL
	upstreamRepo := strings.TrimPrefix(repo, mirrorName+"/")
	if mirrorName == "docker.io" && !strings.Contains(upstreamRepo, "/") {
		upstreamRepo = "library/" + upstreamRepo
	}

	url := fmt.Sprintf("%s/v2/%s/blobs/%s", up.url, upstreamRepo, dgst)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}

	// Add auth for Docker Hub
	if mirrorName == "docker.io" {
		token, err := p.getDockerHubToken(ctx, upstreamRepo)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("fetching blob: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, 0, storage.ErrBlobNotFound
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	// Stream to local storage while returning to client
	pr, pw := io.Pipe()

	go func() {
		defer resp.Body.Close()
		defer pw.Close()

		// Tee the response to both local storage and the client
		tee := io.TeeReader(resp.Body, pw)

		if err := p.blobs.Put(dgst, tee, resp.ContentLength); err != nil {
			log.Printf("Warning: failed to cache blob: %v", err)
		}

		// Link to repo
		p.metadata.LinkBlobToRepo(dgst, repo)
	}()

	return pr, resp.ContentLength, nil
}

// getDockerHubToken gets an auth token for Docker Hub
func (p *Proxy) getDockerHubToken(ctx context.Context, repo string) (string, error) {
	p.mu.RLock()
	up := p.mirrors["docker.io"]
	if up != nil && up.token != "" && time.Now().Before(up.tokenExp) {
		token := up.token
		p.mu.RUnlock()
		return token, nil
	}
	p.mu.RUnlock()

	// Request new token
	url := fmt.Sprintf("https://auth.docker.io/token?service=registry.docker.io&scope=repository:%s:pull", repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token request returned %d", resp.StatusCode)
	}

	var tokenResp struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expires_in"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", err
	}

	// Cache token
	p.mu.Lock()
	if up := p.mirrors["docker.io"]; up != nil {
		up.token = tokenResp.Token
		up.tokenExp = time.Now().Add(time.Duration(tokenResp.ExpiresIn-60) * time.Second)
	}
	p.mu.Unlock()

	return tokenResp.Token, nil
}
