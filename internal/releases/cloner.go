package releases

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gwest/fastregistry/internal/storage"
	"github.com/gwest/fastregistry/pkg/digest"
)

// Cloner pulls release images from Quay.io into local storage
type Cloner struct {
	upstream   string // e.g. "quay.io"
	repository string // e.g. "openshift-release-dev/ocp-release"
	localRepo  string // e.g. "openshift/release"
	pullSecret string // path to pull-secret.json
	blobs      *storage.BlobStore
	metadata   *storage.MetadataStore
	client     *http.Client

	// Concurrency tunables — defaults are calibrated for fast home internet.
	// LayerConcurrency caps parallel blob fetches inside a single component.
	// ComponentConcurrency caps parallel components mirrored at once.
	// Total HTTP fan-out ≈ LayerConcurrency × ComponentConcurrency.
	LayerConcurrency     int
	ComponentConcurrency int

	// logFunc receives high-level lifecycle messages (mirror progress, etc.)
	// so they appear in the UI's Log tab. Set by Manager via NewManager.
	logFunc func(string, ...interface{})

	mu       sync.Mutex
	active   map[string]*CloneProgress
	tokens   map[string]tokenEntry // cached bearer tokens
	authConf *dockerConfig
}

// logf forwards to logFunc if set, otherwise to the standard logger.
func (c *Cloner) logf(format string, args ...interface{}) {
	if c.logFunc != nil {
		c.logFunc(format, args...)
		return
	}
	log.Printf(format, args...)
}

type tokenEntry struct {
	token   string
	expires time.Time
}

type dockerConfig struct {
	Auths map[string]struct {
		Auth string `json:"auth"`
	} `json:"auths"`
}

// NewCloner creates a new release image cloner
func NewCloner(upstream, repository, localRepo, pullSecret string, blobs *storage.BlobStore, metadata *storage.MetadataStore) *Cloner {
	return &Cloner{
		upstream:             upstream,
		repository:           repository,
		localRepo:            localRepo,
		pullSecret:           pullSecret,
		blobs:                blobs,
		metadata:             metadata,
		LayerConcurrency:     10,
		ComponentConcurrency: 4,
		client: &http.Client{
			Timeout: 30 * time.Minute,
			Transport: &http.Transport{
				// Bumped to support the higher fan-out from MirrorComponents
				// (multiple components × multiple layers each).
				MaxIdleConns:        200,
				MaxIdleConnsPerHost: 50,
				MaxConnsPerHost:     50,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		active: make(map[string]*CloneProgress),
		tokens: make(map[string]tokenEntry),
	}
}

// Clone pulls a release image from upstream into local storage
func (c *Cloner) Clone(ctx context.Context, version, arch string) (*CloneProgress, error) {
	tag := version + "-" + arch

	progress := &CloneProgress{
		Version: version,
		Phase:   "pulling_manifest",
	}
	c.mu.Lock()
	c.active[version] = progress
	c.mu.Unlock()

	// NOTE: progress is intentionally NOT deleted on return — it stays in
	// c.active so subsequent phases (mirror, extract) can keep updating it
	// and the status endpoint can read it. The Manager calls FinishProgress()
	// once the whole pipeline is done (success or failure).

	// Load auth config
	if err := c.loadAuth(); err != nil {
		return progress, fmt.Errorf("loading pull secret: %w", err)
	}

	// Fetch manifest
	manifestBytes, manifestDigest, mediaType, err := c.fetchManifest(ctx, tag)
	if err != nil {
		return progress, fmt.Errorf("fetching manifest: %w", err)
	}

	// Parse manifest for layers
	var manifest struct {
		Config struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"config"`
		Layers []struct {
			Digest    string `json:"digest"`
			Size      int64  `json:"size"`
			MediaType string `json:"mediaType"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return progress, fmt.Errorf("parsing manifest: %w", err)
	}

	// Calculate totals
	totalBlobs := len(manifest.Layers)
	if manifest.Config.Digest != "" {
		totalBlobs++
	}
	var totalBytes int64
	for _, l := range manifest.Layers {
		totalBytes += l.Size
	}
	totalBytes += manifest.Config.Size

	progress.Phase = "pulling_blobs"
	progress.TotalBlobs = totalBlobs
	progress.TotalBytes = totalBytes

	var syncedBlobs atomic.Int32
	var syncedBytes atomic.Int64

	// Sync config blob
	if manifest.Config.Digest != "" {
		dgst, err := digest.Parse(manifest.Config.Digest)
		if err != nil {
			return progress, fmt.Errorf("parsing config digest: %w", err)
		}
		if err := c.syncBlob(ctx, dgst, manifest.Config.Size, &syncedBytes); err != nil {
			return progress, fmt.Errorf("syncing config: %w", err)
		}
		syncedBlobs.Add(1)
		progress.SyncedBlobs = int(syncedBlobs.Load())
		progress.SyncedBytes = syncedBytes.Load()
		progress.PercentDone = float64(progress.SyncedBytes) / float64(progress.TotalBytes) * 100
	}

	// Sync layers with concurrency
	conc := c.LayerConcurrency
	if conc <= 0 {
		conc = 10
	}
	sem := make(chan struct{}, conc)
	errCh := make(chan error, len(manifest.Layers))

	for _, layer := range manifest.Layers {
		sem <- struct{}{}
		go func(dgstStr string, size int64) {
			defer func() { <-sem }()

			dgst, err := digest.Parse(dgstStr)
			if err != nil {
				errCh <- fmt.Errorf("parsing digest %s: %w", dgstStr, err)
				return
			}

			if err := c.syncBlob(ctx, dgst, size, &syncedBytes); err != nil {
				errCh <- fmt.Errorf("syncing blob %s: %w", dgstStr[:12], err)
				return
			}

			n := syncedBlobs.Add(1)
			c.mu.Lock()
			progress.SyncedBlobs = int(n)
			progress.SyncedBytes = syncedBytes.Load()
			if progress.TotalBytes > 0 {
				progress.PercentDone = float64(progress.SyncedBytes) / float64(progress.TotalBytes) * 100
			}
			c.mu.Unlock()
		}(layer.Digest, layer.Size)
	}

	// Wait for all
	for i := 0; i < cap(sem); i++ {
		sem <- struct{}{}
	}

	select {
	case err := <-errCh:
		return progress, err
	default:
	}

	// Store manifest
	if err := c.blobs.Put(manifestDigest, strings.NewReader(string(manifestBytes)), int64(len(manifestBytes))); err != nil {
		return progress, fmt.Errorf("storing manifest: %w", err)
	}

	// Store manifest metadata
	meta := &storage.ManifestMeta{
		Digest:    manifestDigest,
		MediaType: mediaType,
		Size:      int64(len(manifestBytes)),
		CreatedAt: time.Now(),
	}
	for _, l := range manifest.Layers {
		meta.Layers = append(meta.Layers, l.Digest)
	}

	if err := c.metadata.PutManifest(c.localRepo, tag, meta); err != nil {
		return progress, fmt.Errorf("storing manifest metadata: %w", err)
	}

	// Link blobs to repo
	c.metadata.LinkBlobToRepo(manifestDigest, c.localRepo)
	if manifest.Config.Digest != "" {
		cfgDgst, _ := digest.Parse(manifest.Config.Digest)
		c.metadata.LinkBlobToRepo(cfgDgst, c.localRepo)
	}
	for _, l := range manifest.Layers {
		lDgst, _ := digest.Parse(l.Digest)
		c.metadata.LinkBlobToRepo(lDgst, c.localRepo)
	}

	progress.PercentDone = 100
	log.Printf("Cloned release %s (%d blobs, %d bytes)", tag, totalBlobs, totalBytes)
	return progress, nil
}

// FinishProgress removes the progress entry once the entire clone+mirror
// pipeline has completed (success or failure). The Manager calls this in a
// defer so the in-memory progress map doesn't leak across runs.
func (c *Cloner) FinishProgress(version string) {
	c.mu.Lock()
	delete(c.active, version)
	c.mu.Unlock()
}

// GetProgress returns clone progress for a version, or nil if not active.
// Accepts either bare version ("4.20.4") or tag ("4.20.4-x86_64") since the
// API consistently uses tag while the manager stores under bare version.
func (c *Cloner) GetProgress(version string) *CloneProgress {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p, ok := c.active[version]; ok {
		cp := *p
		return &cp
	}
	// Fall back: strip arch suffix if caller passed a tag like "4.20.4-x86_64"
	if i := strings.IndexByte(version, '-'); i > 0 {
		if p, ok := c.active[version[:i]]; ok {
			cp := *p
			return &cp
		}
	}
	// Or the reverse: caller passed bare version, entry stored under tag
	for k, p := range c.active {
		if strings.HasPrefix(k, version+"-") || k == version {
			cp := *p
			return &cp
		}
	}
	return nil
}

// GetAllProgress returns a copy of all active clone progress entries.
func (c *Cloner) GetAllProgress() []CloneProgress {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]CloneProgress, 0, len(c.active))
	for _, p := range c.active {
		cp := *p
		result = append(result, cp)
	}
	return result
}

func (c *Cloner) loadAuth() error {
	if c.authConf != nil {
		return nil
	}
	if c.pullSecret == "" {
		return nil
	}

	data, err := os.ReadFile(c.pullSecret)
	if err != nil {
		return err
	}

	var conf dockerConfig
	if err := json.Unmarshal(data, &conf); err != nil {
		return err
	}
	c.authConf = &conf
	return nil
}

func (c *Cloner) getToken(ctx context.Context, scope string) (string, error) {
	c.mu.Lock()
	if entry, ok := c.tokens[scope]; ok && time.Now().Before(entry.expires) {
		c.mu.Unlock()
		return entry.token, nil
	}
	c.mu.Unlock()

	// Challenge the registry to get auth URL
	challengeURL := fmt.Sprintf("https://%s/v2/", c.upstream)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, challengeURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		return "", nil // No auth needed
	}

	wwwAuth := resp.Header.Get("WWW-Authenticate")
	realm, service := parseWWWAuthenticate(wwwAuth)
	if realm == "" {
		return "", fmt.Errorf("no realm in WWW-Authenticate header")
	}

	// Request bearer token
	tokenURL := fmt.Sprintf("%s?service=%s&scope=%s", realm, service, scope)
	tokenReq, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", err
	}

	// Add basic auth from pull secret
	if c.authConf != nil {
		for host, auth := range c.authConf.Auths {
			if strings.Contains(host, c.upstream) {
				tokenReq.Header.Set("Authorization", "Basic "+auth.Auth)
				break
			}
		}
	}

	tokenResp, err := c.client.Do(tokenReq)
	if err != nil {
		return "", err
	}
	defer tokenResp.Body.Close()

	if tokenResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(tokenResp.Body)
		return "", fmt.Errorf("token request failed (%d): %s", tokenResp.StatusCode, string(body))
	}

	var tokenResult struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tokenResult); err != nil {
		return "", err
	}

	// Cache the token
	expiry := 300 // default 5 min
	if tokenResult.ExpiresIn > 0 {
		expiry = tokenResult.ExpiresIn
	}
	c.mu.Lock()
	c.tokens[scope] = tokenEntry{
		token:   tokenResult.Token,
		expires: time.Now().Add(time.Duration(expiry-30) * time.Second),
	}
	c.mu.Unlock()

	return tokenResult.Token, nil
}

func (c *Cloner) fetchManifest(ctx context.Context, ref string) ([]byte, digest.Digest, string, error) {
	scope := fmt.Sprintf("repository:%s:pull", c.repository)
	token, err := c.getToken(ctx, scope)
	if err != nil {
		return nil, "", "", fmt.Errorf("getting token: %w", err)
	}

	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", c.upstream, c.repository, ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", "", err
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
	}, ", "))

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", "", fmt.Errorf("upstream returned %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", "", err
	}

	dgst := digest.FromBytes(body)
	mediaType := resp.Header.Get("Content-Type")

	return body, dgst, mediaType, nil
}

func (c *Cloner) syncBlob(ctx context.Context, dgst digest.Digest, size int64, syncedBytes *atomic.Int64) error {
	if c.blobs.Exists(dgst) {
		syncedBytes.Add(size)
		return nil
	}

	scope := fmt.Sprintf("repository:%s:pull", c.repository)
	token, err := c.getToken(ctx, scope)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", c.upstream, c.repository, dgst)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream returned %d for blob %s", resp.StatusCode, dgst)
	}

	reader := &countingReader{reader: resp.Body, counter: syncedBytes}
	return c.blobs.Put(dgst, reader, size)
}

type countingReader struct {
	reader  io.Reader
	counter *atomic.Int64
}

func (r *countingReader) Read(p []byte) (n int, err error) {
	n, err = r.reader.Read(p)
	if n > 0 {
		r.counter.Add(int64(n))
	}
	return
}

// PullComponentImage pulls a component image by full reference (e.g.,
// "quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:abc123") and stores
// its blobs in local storage. Returns the manifest bytes.
func (c *Cloner) PullComponentImage(ctx context.Context, imageRef string) ([]byte, error) {
	if err := c.loadAuth(); err != nil {
		return nil, fmt.Errorf("loading pull secret: %w", err)
	}

	registry, repo, ref := parseImageRef(imageRef)
	if repo == "" || ref == "" {
		return nil, fmt.Errorf("invalid image reference: %s", imageRef)
	}
	if registry == "" {
		registry = c.upstream
	}

	log.Printf("Pulling component image %s/%s", repo, truncateStr(ref, 24))

	// Get auth token for the component repo
	scope := fmt.Sprintf("repository:%s:pull", repo)
	token, err := c.getToken(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("getting token for %s: %w", repo, err)
	}

	// Fetch manifest
	manifestURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", registry, repo, ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
	}, ", "))

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("upstream returned %d: %s", resp.StatusCode, truncateStr(string(body), 200))
	}

	manifestBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Store the manifest in the local blob store keyed by its own digest.
	// Component images are referenced from image-references by `repo@sha256:<d>`,
	// so future lookups (extract, on-demand pull) can read it locally without network.
	mDigest := digest.FromBytes(manifestBytes)
	if !c.blobs.Exists(mDigest) {
		if err := c.blobs.Put(mDigest, bytes.NewReader(manifestBytes), int64(len(manifestBytes))); err != nil {
			return nil, fmt.Errorf("storing component manifest blob: %w", err)
		}
	}
	// Link to a synthetic repo so it shows up in catalog and is reachable
	// at /v2/openshift/release-components/manifests/<sha256:digest>.
	c.metadata.LinkBlobToRepo(mDigest, c.localRepo+"-components")

	// Parse manifest for blobs to sync
	var manifest struct {
		Config struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("parsing component manifest: %w", err)
	}

	// Sync config blob
	if manifest.Config.Digest != "" {
		dgst, err := digest.Parse(manifest.Config.Digest)
		if err == nil {
			if err := c.syncBlobFromRepo(ctx, registry, repo, dgst, manifest.Config.Size); err != nil {
				return nil, fmt.Errorf("syncing config: %w", err)
			}
		}
	}

	// Sync layers concurrently
	conc := c.LayerConcurrency
	if conc <= 0 {
		conc = 10
	}
	c.logf("Syncing %d layers for component (parallel=%d)", len(manifest.Layers), conc)
	sem := make(chan struct{}, conc)
	errCh := make(chan error, len(manifest.Layers))

	for _, layer := range manifest.Layers {
		sem <- struct{}{}
		go func(dgstStr string, size int64) {
			defer func() { <-sem }()
			dgst, err := digest.Parse(dgstStr)
			if err != nil {
				errCh <- err
				return
			}
			if err := c.syncBlobFromRepo(ctx, registry, repo, dgst, size); err != nil {
				errCh <- fmt.Errorf("syncing blob %s: %w", dgstStr[:min(12, len(dgstStr))], err)
				return
			}
			c.metadata.LinkBlobToRepo(dgst, c.localRepo+"-components")
		}(layer.Digest, layer.Size)
	}

	for i := 0; i < cap(sem); i++ {
		sem <- struct{}{}
	}

	select {
	case err := <-errCh:
		return nil, err
	default:
	}

	if manifest.Config.Digest != "" {
		if cfgDgst, err := digest.Parse(manifest.Config.Digest); err == nil {
			c.metadata.LinkBlobToRepo(cfgDgst, c.localRepo+"-components")
		}
	}

	log.Printf("Component image pulled successfully (%d layers)", len(manifest.Layers))
	return manifestBytes, nil
}

// MirrorComponents pulls every component image referenced by a release
// (manifest + config + all layers) into the local blob store. After it
// returns nil, the release is completely self-contained locally and
// extraction can run with no network.
//
// Components are mirrored in parallel up to ComponentConcurrency, and each
// component's layers are pulled in parallel up to LayerConcurrency, so
// effective fan-out is the product of the two. Progress is updated atomically
// as each component completes.
func (c *Cloner) MirrorComponents(ctx context.Context, version string, refs map[string]string, progress *CloneProgress) error {
	if len(refs) == 0 {
		return nil
	}
	total := len(refs)
	workers := c.ComponentConcurrency
	if workers <= 0 {
		workers = 4
	}
	c.logf("Mirroring %d component images for release %s (parallel=%d)", total, version, workers)

	if progress != nil {
		c.mu.Lock()
		progress.Phase = "mirroring_components"
		progress.TotalComponents = total
		progress.MirroredComponents = 0
		progress.ComponentPercent = 0
		progress.PercentDone = 0
		c.mu.Unlock()
	}

	type job struct{ component, ref string }
	jobs := make(chan job, total)
	for component, ref := range refs {
		jobs <- job{component, ref}
	}
	close(jobs)

	var mirrored atomic.Int32
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)

	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				if progress != nil {
					c.mu.Lock()
					progress.CurrentComponent = j.component
					c.mu.Unlock()
				}
				idx := mirrored.Load() + 1
				c.logf("[mirror %d/%d] component %q: %s", idx, total, j.component, truncateStr(j.ref, 60))
				if _, err := c.PullComponentImage(ctx, j.ref); err != nil {
					select {
					case errCh <- fmt.Errorf("mirroring %s: %w", j.component, err):
					default:
					}
					return
				}
				n := mirrored.Add(1)
				if progress != nil {
					c.mu.Lock()
					progress.MirroredComponents = int(n)
					pct := float64(n) / float64(total) * 100
					progress.ComponentPercent = pct
					progress.PercentDone = pct
					c.mu.Unlock()
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		return err
	}
	c.logf("Mirrored %d component images successfully", mirrored.Load())
	return nil
}

// LookupComponentManifest reads a component manifest from the local blob store
// using the digest embedded in an image reference (e.g. "quay.io/...@sha256:abc").
// Returns the raw manifest bytes; callers parse as needed.
func (c *Cloner) LookupComponentManifest(imageRef string) ([]byte, error) {
	_, _, ref := parseImageRef(imageRef)
	if ref == "" {
		return nil, fmt.Errorf("image reference has no digest: %s", imageRef)
	}
	dgst, err := digest.Parse(ref)
	if err != nil {
		return nil, fmt.Errorf("parsing digest %q: %w", ref, err)
	}
	r, _, err := c.blobs.Get(dgst)
	if err != nil {
		return nil, fmt.Errorf("reading manifest blob %s: %w", dgst, err)
	}
	defer r.Close()
	return io.ReadAll(r)
}

// syncBlobFromRepo syncs a blob from a specific repo on the given registry.
func (c *Cloner) syncBlobFromRepo(ctx context.Context, registry, repo string, dgst digest.Digest, size int64) error {
	if c.blobs.Exists(dgst) {
		return nil
	}

	scope := fmt.Sprintf("repository:%s:pull", repo)
	token, err := c.getToken(ctx, scope)
	if err != nil {
		return err
	}

	blobURL := fmt.Sprintf("https://%s/v2/%s/blobs/%s", registry, repo, dgst)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, blobURL, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream returned %d for blob %s", resp.StatusCode, dgst)
	}

	return c.blobs.Put(dgst, resp.Body, size)
}

// parseImageRef splits "quay.io/org/repo@sha256:abc" into registry, repo, reference.
func parseImageRef(s string) (registry, repo, ref string) {
	atIdx := strings.Index(s, "@")
	if atIdx >= 0 {
		ref = s[atIdx+1:]
		s = s[:atIdx]
	} else {
		colonIdx := strings.LastIndex(s, ":")
		slashIdx := strings.LastIndex(s, "/")
		if colonIdx > slashIdx && colonIdx > 0 {
			ref = s[colonIdx+1:]
			s = s[:colonIdx]
		}
	}
	slashIdx := strings.Index(s, "/")
	if slashIdx < 0 {
		return "", s, ref
	}
	first := s[:slashIdx]
	if strings.Contains(first, ".") || strings.Contains(first, ":") {
		return first, s[slashIdx+1:], ref
	}
	return "", s, ref
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// parseWWWAuthenticate parses realm and service from a WWW-Authenticate header
func parseWWWAuthenticate(header string) (realm, service string) {
	header = strings.TrimPrefix(header, "Bearer ")
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "realm=") {
			realm = strings.Trim(strings.TrimPrefix(part, "realm="), `"`)
		} else if strings.HasPrefix(part, "service=") {
			service = strings.Trim(strings.TrimPrefix(part, "service="), `"`)
		}
	}
	return
}
