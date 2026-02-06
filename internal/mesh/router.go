package mesh

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gwest/fastregistry/internal/storage"
	"github.com/gwest/fastregistry/pkg/digest"
)

// Router handles routing requests to the appropriate node in the mesh
type Router struct {
	cluster *Cluster
	blobs   *storage.BlobStore
	client  *http.Client
	mu      sync.RWMutex
}

// NewRouter creates a new mesh router
func NewRouter(cluster *Cluster, blobs *storage.BlobStore) *Router {
	return &Router{
		cluster: cluster,
		blobs:   blobs,
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// GetBlob retrieves a blob, routing to the appropriate node if needed
func (r *Router) GetBlob(ctx context.Context, dgst digest.Digest) (io.ReadCloser, int64, error) {
	key := string(dgst)

	// Check if we have it locally first
	if r.blobs.Exists(dgst) {
		return r.blobs.Get(dgst)
	}

	// Check if we're the primary node
	if r.cluster.IsLocalPrimary(key) {
		// We're primary but don't have it - it doesn't exist
		return nil, 0, storage.ErrBlobNotFound
	}

	// Route to primary node
	primaryNodeID := r.cluster.GetPrimaryNode(key)
	if primaryNodeID == "" {
		return nil, 0, storage.ErrBlobNotFound
	}

	node := r.cluster.GetNode(primaryNodeID)
	if node == nil || node.RegistryAddr == "" {
		return nil, 0, fmt.Errorf("primary node %s not reachable", primaryNodeID)
	}

	// Fetch from primary node
	return r.fetchBlobFromNode(ctx, node, dgst)
}

// PutBlob stores a blob, routing to the primary node if needed
func (r *Router) PutBlob(ctx context.Context, dgst digest.Digest, reader io.Reader, size int64) error {
	key := string(dgst)

	// If we're the primary, store locally
	if r.cluster.IsLocalPrimary(key) {
		return r.blobs.Put(dgst, reader, size)
	}

	// Route to primary node
	primaryNodeID := r.cluster.GetPrimaryNode(key)
	if primaryNodeID == "" {
		// No cluster, store locally
		return r.blobs.Put(dgst, reader, size)
	}

	node := r.cluster.GetNode(primaryNodeID)
	if node == nil || node.RegistryAddr == "" {
		// Primary not reachable, store locally as fallback
		log.Printf("Primary node %s not reachable, storing locally", primaryNodeID)
		return r.blobs.Put(dgst, reader, size)
	}

	// Forward to primary node
	return r.pushBlobToNode(ctx, node, dgst, reader, size)
}

// HasBlob checks if a blob exists in the cluster
func (r *Router) HasBlob(ctx context.Context, dgst digest.Digest) bool {
	// Check local first
	if r.blobs.Exists(dgst) {
		return true
	}

	key := string(dgst)

	// Check if we're primary
	if r.cluster.IsLocalPrimary(key) {
		return false
	}

	// Check with primary node
	primaryNodeID := r.cluster.GetPrimaryNode(key)
	if primaryNodeID == "" {
		return false
	}

	node := r.cluster.GetNode(primaryNodeID)
	if node == nil || node.RegistryAddr == "" {
		return false
	}

	return r.checkBlobOnNode(ctx, node, dgst)
}

// fetchBlobFromNode retrieves a blob from another node
func (r *Router) fetchBlobFromNode(ctx context.Context, node *NodeMeta, dgst digest.Digest) (io.ReadCloser, int64, error) {
	url := fmt.Sprintf("http://%s/v2/_internal/blobs/%s", node.RegistryAddr, dgst)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, 0, err
	}

	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, 0, storage.ErrBlobNotFound
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("node returned status %d", resp.StatusCode)
	}

	return resp.Body, resp.ContentLength, nil
}

// pushBlobToNode sends a blob to another node
func (r *Router) pushBlobToNode(ctx context.Context, node *NodeMeta, dgst digest.Digest, reader io.Reader, size int64) error {
	url := fmt.Sprintf("http://%s/v2/_internal/blobs/%s", node.RegistryAddr, dgst)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, reader)
	if err != nil {
		return err
	}

	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("node returned status %d", resp.StatusCode)
	}

	return nil
}

// checkBlobOnNode checks if a blob exists on another node
func (r *Router) checkBlobOnNode(ctx context.Context, node *NodeMeta, dgst digest.Digest) bool {
	url := fmt.Sprintf("http://%s/v2/_internal/blobs/%s", node.RegistryAddr, dgst)

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return false
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

// InternalHandler handles internal mesh API requests
type InternalHandler struct {
	blobs *storage.BlobStore
}

// NewInternalHandler creates a handler for internal mesh endpoints
func NewInternalHandler(blobs *storage.BlobStore) *InternalHandler {
	return &InternalHandler{blobs: blobs}
}

// ServeHTTP handles internal blob requests from other nodes
func (h *InternalHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Parse digest from URL: /v2/_internal/blobs/sha256:abc123
	path := r.URL.Path
	if len(path) < 30 {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	dgstStr := path[len("/v2/_internal/blobs/"):]
	dgst, err := digest.Parse(dgstStr)
	if err != nil {
		http.Error(w, "invalid digest", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		h.handleGet(w, r, dgst)
	case http.MethodHead:
		h.handleHead(w, dgst)
	case http.MethodPut:
		h.handlePut(w, r, dgst)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *InternalHandler) handleGet(w http.ResponseWriter, r *http.Request, dgst digest.Digest) {
	reader, size, err := h.blobs.Get(dgst)
	if err == storage.ErrBlobNotFound {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	w.Header().Set("Content-Type", "application/octet-stream")
	io.Copy(w, reader)
}

func (h *InternalHandler) handleHead(w http.ResponseWriter, dgst digest.Digest) {
	size, err := h.blobs.Size(dgst)
	if err == storage.ErrBlobNotFound {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	w.WriteHeader(http.StatusOK)
}

func (h *InternalHandler) handlePut(w http.ResponseWriter, r *http.Request, dgst digest.Digest) {
	if err := h.blobs.Put(dgst, r.Body, r.ContentLength); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
}
