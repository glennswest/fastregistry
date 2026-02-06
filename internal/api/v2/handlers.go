package v2

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gwest/fastregistry/internal/mirror"
	"github.com/gwest/fastregistry/internal/storage"
	"github.com/gwest/fastregistry/pkg/digest"
	"github.com/gwest/fastregistry/pkg/oci"
)

// Handler implements the OCI Distribution API v2
type Handler struct {
	blobs    *storage.BlobStore
	metadata *storage.MetadataStore
	uploads  *storage.UploadManager
	cache    *storage.LRUCache
	proxy    *mirror.Proxy
}

// NewHandler creates a new v2 API handler
func NewHandler(blobs *storage.BlobStore, metadata *storage.MetadataStore, uploads *storage.UploadManager) *Handler {
	return &Handler{
		blobs:    blobs,
		metadata: metadata,
		uploads:  uploads,
		cache:    storage.NewLRUCache(10000), // Cache 10k manifests
	}
}

// SetProxy sets the mirror proxy for pull-through caching
func (h *Handler) SetProxy(proxy *mirror.Proxy) {
	h.proxy = proxy
}

// Regex patterns for routing
var (
	repoNamePattern     = `[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*`
	tagPattern          = `[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}`
	digestPattern       = `sha256:[a-f0-9]{64}`
	referencePattern    = fmt.Sprintf(`(%s|%s)`, tagPattern, digestPattern)

	manifestPathRe      = regexp.MustCompile(fmt.Sprintf(`^/v2/(%s)/manifests/(%s)$`, repoNamePattern, referencePattern))
	blobPathRe          = regexp.MustCompile(fmt.Sprintf(`^/v2/(%s)/blobs/(%s)$`, repoNamePattern, digestPattern))
	uploadStartRe       = regexp.MustCompile(fmt.Sprintf(`^/v2/(%s)/blobs/uploads/?$`, repoNamePattern))
	uploadPathRe        = regexp.MustCompile(fmt.Sprintf(`^/v2/(%s)/blobs/uploads/([a-f0-9-]+)$`, repoNamePattern))
	tagsListRe          = regexp.MustCompile(fmt.Sprintf(`^/v2/(%s)/tags/list$`, repoNamePattern))
)

// ServeHTTP routes requests to appropriate handlers
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// API version check
	if path == "/v2/" || path == "/v2" {
		h.handleBase(w, r)
		return
	}

	// Catalog
	if path == "/v2/_catalog" {
		h.handleCatalog(w, r)
		return
	}

	// Manifests
	if matches := manifestPathRe.FindStringSubmatch(path); matches != nil {
		repo, ref := matches[1], matches[2]
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			h.handleGetManifest(w, r, repo, ref)
		case http.MethodPut:
			h.handlePutManifest(w, r, repo, ref)
		case http.MethodDelete:
			h.handleDeleteManifest(w, r, repo, ref)
		default:
			h.errorResponse(w, http.StatusMethodNotAllowed, oci.ErrorCodeUnsupported, "method not allowed")
		}
		return
	}

	// Blobs
	if matches := blobPathRe.FindStringSubmatch(path); matches != nil {
		repo, dgst := matches[1], matches[2]
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			h.handleGetBlob(w, r, repo, dgst)
		case http.MethodDelete:
			h.handleDeleteBlob(w, r, repo, dgst)
		default:
			h.errorResponse(w, http.StatusMethodNotAllowed, oci.ErrorCodeUnsupported, "method not allowed")
		}
		return
	}

	// Upload start
	if matches := uploadStartRe.FindStringSubmatch(path); matches != nil {
		repo := matches[1]
		if r.Method == http.MethodPost {
			h.handleStartUpload(w, r, repo)
			return
		}
		h.errorResponse(w, http.StatusMethodNotAllowed, oci.ErrorCodeUnsupported, "method not allowed")
		return
	}

	// Upload progress
	if matches := uploadPathRe.FindStringSubmatch(path); matches != nil {
		repo, uploadID := matches[1], matches[2]
		switch r.Method {
		case http.MethodPatch:
			h.handleUploadChunk(w, r, repo, uploadID)
		case http.MethodPut:
			h.handleFinishUpload(w, r, repo, uploadID)
		case http.MethodGet:
			h.handleUploadStatus(w, r, repo, uploadID)
		case http.MethodDelete:
			h.handleCancelUpload(w, r, repo, uploadID)
		default:
			h.errorResponse(w, http.StatusMethodNotAllowed, oci.ErrorCodeUnsupported, "method not allowed")
		}
		return
	}

	// Tags list
	if matches := tagsListRe.FindStringSubmatch(path); matches != nil {
		repo := matches[1]
		if r.Method == http.MethodGet {
			h.handleListTags(w, r, repo)
			return
		}
		h.errorResponse(w, http.StatusMethodNotAllowed, oci.ErrorCodeUnsupported, "method not allowed")
		return
	}

	h.errorResponse(w, http.StatusNotFound, oci.ErrorCodeNameUnknown, "not found")
}

// handleBase handles GET /v2/
func (h *Handler) handleBase(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	w.WriteHeader(http.StatusOK)
}

// handleCatalog handles GET /v2/_catalog
func (h *Handler) handleCatalog(w http.ResponseWriter, r *http.Request) {
	repos, err := h.metadata.ListRepositories()
	if err != nil {
		h.errorResponse(w, http.StatusInternalServerError, oci.ErrorCodeUnauthorized, err.Error())
		return
	}

	catalog := oci.Catalog{Repositories: repos}
	h.jsonResponse(w, http.StatusOK, catalog)
}

// handleGetManifest handles GET/HEAD /v2/<name>/manifests/<reference>
func (h *Handler) handleGetManifest(w http.ResponseWriter, r *http.Request, repo, ref string) {
	// Check cache first
	cacheKey := repo + ":" + ref
	if cached, ok := h.cache.Get(cacheKey); ok {
		meta := cached.(*storage.ManifestMeta)
		h.serveManifest(w, r, repo, meta)
		return
	}

	meta, err := h.metadata.GetManifest(repo, ref)
	if err == storage.ErrManifestNotFound {
		// Try pull-through proxy if configured
		if h.proxy != nil {
			if mirrorName, ok := h.proxy.IsMirrored(repo); ok {
				proxyMeta, body, err := h.proxy.GetManifest(r.Context(), mirrorName, repo, ref)
				if err == nil {
					// Serve directly from fetched body
					w.Header().Set("Content-Type", proxyMeta.MediaType)
					w.Header().Set("Content-Length", strconv.FormatInt(int64(len(body)), 10))
					w.Header().Set("Docker-Content-Digest", string(proxyMeta.Digest))
					w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
					if r.Method != http.MethodHead {
						w.Write(body)
					}
					return
				}
			}
		}
		h.errorResponse(w, http.StatusNotFound, oci.ErrorCodeManifestUnknown, "manifest not found")
		return
	}
	if err != nil {
		h.errorResponse(w, http.StatusInternalServerError, oci.ErrorCodeManifestInvalid, err.Error())
		return
	}

	// Cache it
	h.cache.Set(cacheKey, meta)

	h.serveManifest(w, r, repo, meta)
}

func (h *Handler) serveManifest(w http.ResponseWriter, r *http.Request, repo string, meta *storage.ManifestMeta) {
	// Get the manifest blob
	reader, size, err := h.blobs.Get(meta.Digest)
	if err != nil {
		h.errorResponse(w, http.StatusInternalServerError, oci.ErrorCodeManifestBlobUnknown, err.Error())
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", meta.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Docker-Content-Digest", string(meta.Digest))
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")

	if r.Method == http.MethodHead {
		return
	}

	io.Copy(w, reader)
}

// handlePutManifest handles PUT /v2/<name>/manifests/<reference>
func (h *Handler) handlePutManifest(w http.ResponseWriter, r *http.Request, repo, ref string) {
	// Read the manifest
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024)) // 10MB limit
	if err != nil {
		h.errorResponse(w, http.StatusBadRequest, oci.ErrorCodeManifestInvalid, "failed to read body")
		return
	}

	// Compute digest
	dgst := digest.FromBytes(body)

	// Parse to validate and extract layer info
	var manifest struct {
		MediaType string           `json:"mediaType"`
		Config    oci.Descriptor   `json:"config"`
		Layers    []oci.Descriptor `json:"layers"`
		Manifests []oci.Descriptor `json:"manifests"` // For index
		Subject   *oci.Descriptor  `json:"subject"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		h.errorResponse(w, http.StatusBadRequest, oci.ErrorCodeManifestInvalid, "invalid manifest JSON")
		return
	}

	// Determine media type
	mediaType := manifest.MediaType
	if mediaType == "" {
		mediaType = r.Header.Get("Content-Type")
	}

	// Verify all referenced blobs exist
	if manifest.Config.Digest != "" {
		if !h.blobs.Exists(manifest.Config.Digest) {
			h.errorResponse(w, http.StatusBadRequest, oci.ErrorCodeManifestBlobUnknown,
				fmt.Sprintf("config blob unknown: %s", manifest.Config.Digest))
			return
		}
	}

	for _, layer := range manifest.Layers {
		if !h.blobs.Exists(layer.Digest) {
			h.errorResponse(w, http.StatusBadRequest, oci.ErrorCodeManifestBlobUnknown,
				fmt.Sprintf("layer blob unknown: %s", layer.Digest))
			return
		}
	}

	// Store manifest blob
	if err := h.blobs.Put(dgst, strings.NewReader(string(body)), int64(len(body))); err != nil {
		h.errorResponse(w, http.StatusInternalServerError, oci.ErrorCodeManifestInvalid, err.Error())
		return
	}

	// Store metadata
	var layers []string
	for _, l := range manifest.Layers {
		layers = append(layers, string(l.Digest))
	}

	meta := &storage.ManifestMeta{
		Digest:    dgst,
		MediaType: mediaType,
		Size:      int64(len(body)),
		Layers:    layers,
	}

	if manifest.Subject != nil {
		meta.Subject = string(manifest.Subject.Digest)
	}

	if err := h.metadata.PutManifest(repo, ref, meta); err != nil {
		h.errorResponse(w, http.StatusInternalServerError, oci.ErrorCodeManifestInvalid, err.Error())
		return
	}

	// Link blobs to repo for GC
	h.metadata.LinkBlobToRepo(dgst, repo)
	if manifest.Config.Digest != "" {
		h.metadata.LinkBlobToRepo(manifest.Config.Digest, repo)
	}
	for _, layer := range manifest.Layers {
		h.metadata.LinkBlobToRepo(layer.Digest, repo)
	}

	// Invalidate cache
	h.cache.Delete(repo + ":" + ref)
	h.cache.Delete(repo + ":" + string(dgst))

	w.Header().Set("Docker-Content-Digest", string(dgst))
	w.Header().Set("Location", fmt.Sprintf("/v2/%s/manifests/%s", repo, dgst))
	w.WriteHeader(http.StatusCreated)
}

// handleDeleteManifest handles DELETE /v2/<name>/manifests/<reference>
func (h *Handler) handleDeleteManifest(w http.ResponseWriter, r *http.Request, repo, ref string) {
	if err := h.metadata.DeleteManifest(repo, ref); err == storage.ErrManifestNotFound {
		h.errorResponse(w, http.StatusNotFound, oci.ErrorCodeManifestUnknown, "manifest not found")
		return
	} else if err != nil {
		h.errorResponse(w, http.StatusInternalServerError, oci.ErrorCodeManifestInvalid, err.Error())
		return
	}

	// Invalidate cache
	h.cache.Delete(repo + ":" + ref)

	w.WriteHeader(http.StatusAccepted)
}

// handleGetBlob handles GET/HEAD /v2/<name>/blobs/<digest>
func (h *Handler) handleGetBlob(w http.ResponseWriter, r *http.Request, repo, dgstStr string) {
	dgst, err := digest.Parse(dgstStr)
	if err != nil {
		h.errorResponse(w, http.StatusBadRequest, oci.ErrorCodeDigestInvalid, err.Error())
		return
	}

	// Check for range request
	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" {
		h.handleRangeRequest(w, r, dgst, rangeHeader)
		return
	}

	reader, size, err := h.blobs.Get(dgst)
	if err == storage.ErrBlobNotFound {
		// Try pull-through proxy if configured
		if h.proxy != nil {
			if mirrorName, ok := h.proxy.IsMirrored(repo); ok {
				proxyReader, proxySize, err := h.proxy.GetBlob(r.Context(), mirrorName, repo, dgst)
				if err == nil {
					defer proxyReader.Close()
					w.Header().Set("Content-Type", "application/octet-stream")
					w.Header().Set("Content-Length", strconv.FormatInt(proxySize, 10))
					w.Header().Set("Docker-Content-Digest", string(dgst))
					w.Header().Set("Accept-Ranges", "bytes")
					if r.Method != http.MethodHead {
						io.Copy(w, proxyReader)
					}
					return
				}
			}
		}
		h.errorResponse(w, http.StatusNotFound, oci.ErrorCodeBlobUnknown, "blob not found")
		return
	}
	if err != nil {
		h.errorResponse(w, http.StatusInternalServerError, oci.ErrorCodeBlobUnknown, err.Error())
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Docker-Content-Digest", string(dgst))
	w.Header().Set("Accept-Ranges", "bytes")

	if r.Method == http.MethodHead {
		return
	}

	io.Copy(w, reader)
}

func (h *Handler) handleRangeRequest(w http.ResponseWriter, r *http.Request, dgst digest.Digest, rangeHeader string) {
	// Parse range header: bytes=start-end
	if !strings.HasPrefix(rangeHeader, "bytes=") {
		h.errorResponse(w, http.StatusBadRequest, oci.ErrorCodeUnsupported, "invalid range header")
		return
	}

	size, err := h.blobs.Size(dgst)
	if err != nil {
		h.errorResponse(w, http.StatusNotFound, oci.ErrorCodeBlobUnknown, "blob not found")
		return
	}

	rangePart := strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.Split(rangePart, "-")
	if len(parts) != 2 {
		h.errorResponse(w, http.StatusBadRequest, oci.ErrorCodeUnsupported, "invalid range")
		return
	}

	var start, end int64

	if parts[0] == "" {
		// Suffix range: -500 means last 500 bytes
		suffixLen, _ := strconv.ParseInt(parts[1], 10, 64)
		start = size - suffixLen
		end = size - 1
	} else if parts[1] == "" {
		// Open-ended: 500- means from 500 to end
		start, _ = strconv.ParseInt(parts[0], 10, 64)
		end = size - 1
	} else {
		start, _ = strconv.ParseInt(parts[0], 10, 64)
		end, _ = strconv.ParseInt(parts[1], 10, 64)
	}

	if start < 0 || end >= size || start > end {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}

	reader, err := h.blobs.GetRange(dgst, start, end)
	if err != nil {
		h.errorResponse(w, http.StatusInternalServerError, oci.ErrorCodeBlobUnknown, err.Error())
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	w.Header().Set("Docker-Content-Digest", string(dgst))
	w.WriteHeader(http.StatusPartialContent)

	if r.Method == http.MethodHead {
		return
	}

	io.Copy(w, reader)
}

// handleDeleteBlob handles DELETE /v2/<name>/blobs/<digest>
func (h *Handler) handleDeleteBlob(w http.ResponseWriter, r *http.Request, repo, dgstStr string) {
	dgst, err := digest.Parse(dgstStr)
	if err != nil {
		h.errorResponse(w, http.StatusBadRequest, oci.ErrorCodeDigestInvalid, err.Error())
		return
	}

	// Unlink from repo
	h.metadata.UnlinkBlobFromRepo(dgst, repo)

	// Check if any repos still reference this blob
	repos, _ := h.metadata.GetBlobRepos(dgst)
	if len(repos) == 0 {
		// No references, safe to delete
		if err := h.blobs.Delete(dgst); err != nil && err != storage.ErrBlobNotFound {
			h.errorResponse(w, http.StatusInternalServerError, oci.ErrorCodeBlobUnknown, err.Error())
			return
		}
	}

	w.WriteHeader(http.StatusAccepted)
}

// handleStartUpload handles POST /v2/<name>/blobs/uploads/
func (h *Handler) handleStartUpload(w http.ResponseWriter, r *http.Request, repo string) {
	// Check for cross-repo mount
	if mount := r.URL.Query().Get("mount"); mount != "" {
		from := r.URL.Query().Get("from")
		if h.handleMountBlob(w, r, repo, mount, from) {
			return
		}
	}

	// Check for single-request upload (digest query param)
	if dgstStr := r.URL.Query().Get("digest"); dgstStr != "" {
		dgst, err := digest.Parse(dgstStr)
		if err != nil {
			h.errorResponse(w, http.StatusBadRequest, oci.ErrorCodeDigestInvalid, err.Error())
			return
		}

		contentLength, _ := strconv.ParseInt(r.Header.Get("Content-Length"), 10, 64)
		if err := h.blobs.Put(dgst, r.Body, contentLength); err != nil {
			h.errorResponse(w, http.StatusBadRequest, oci.ErrorCodeDigestInvalid, err.Error())
			return
		}

		h.metadata.LinkBlobToRepo(dgst, repo)

		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", repo, dgst))
		w.Header().Set("Docker-Content-Digest", string(dgst))
		w.WriteHeader(http.StatusCreated)
		return
	}

	// Start chunked upload
	upload, err := h.uploads.Start(repo)
	if err != nil {
		h.errorResponse(w, http.StatusInternalServerError, oci.ErrorCodeBlobUploadInvalid, err.Error())
		return
	}

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", repo, upload.ID))
	w.Header().Set("Docker-Upload-UUID", upload.ID)
	w.Header().Set("Range", "0-0")
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) handleMountBlob(w http.ResponseWriter, r *http.Request, repo, dgstStr, from string) bool {
	dgst, err := digest.Parse(dgstStr)
	if err != nil {
		return false
	}

	// Check if blob exists and is accessible from source repo
	if from != "" && !h.metadata.HasBlob(from, dgst) {
		return false
	}

	if !h.blobs.Exists(dgst) {
		return false
	}

	// Link to new repo
	h.metadata.LinkBlobToRepo(dgst, repo)

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", repo, dgst))
	w.Header().Set("Docker-Content-Digest", string(dgst))
	w.WriteHeader(http.StatusCreated)
	return true
}

// handleUploadChunk handles PATCH /v2/<name>/blobs/uploads/<uuid>
func (h *Handler) handleUploadChunk(w http.ResponseWriter, r *http.Request, repo, uploadID string) {
	upload, err := h.uploads.Get(uploadID)
	if err == storage.ErrUploadNotFound {
		h.errorResponse(w, http.StatusNotFound, oci.ErrorCodeBlobUploadUnknown, "upload not found")
		return
	}
	if err != nil {
		h.errorResponse(w, http.StatusInternalServerError, oci.ErrorCodeBlobUploadInvalid, err.Error())
		return
	}

	// Check Content-Range header for offset
	var written int64
	contentRange := r.Header.Get("Content-Range")
	if contentRange != "" {
		// Parse Content-Range: bytes start-end/total
		var start int64
		fmt.Sscanf(contentRange, "bytes %d-", &start)
		written, err = upload.WriteAt(r.Body, start)
	} else {
		written, err = upload.Write(r.Body)
	}

	if err != nil {
		h.errorResponse(w, http.StatusInternalServerError, oci.ErrorCodeBlobUploadInvalid, err.Error())
		return
	}

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", repo, uploadID))
	w.Header().Set("Docker-Upload-UUID", uploadID)
	w.Header().Set("Range", fmt.Sprintf("0-%d", upload.Size-1))
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusAccepted)
	_ = written
}

// handleFinishUpload handles PUT /v2/<name>/blobs/uploads/<uuid>
func (h *Handler) handleFinishUpload(w http.ResponseWriter, r *http.Request, repo, uploadID string) {
	dgstStr := r.URL.Query().Get("digest")
	if dgstStr == "" {
		h.errorResponse(w, http.StatusBadRequest, oci.ErrorCodeDigestInvalid, "digest required")
		return
	}

	dgst, err := digest.Parse(dgstStr)
	if err != nil {
		h.errorResponse(w, http.StatusBadRequest, oci.ErrorCodeDigestInvalid, err.Error())
		return
	}

	// Handle any final data in the body
	upload, err := h.uploads.Get(uploadID)
	if err == storage.ErrUploadNotFound {
		h.errorResponse(w, http.StatusNotFound, oci.ErrorCodeBlobUploadUnknown, "upload not found")
		return
	}
	if err != nil {
		h.errorResponse(w, http.StatusInternalServerError, oci.ErrorCodeBlobUploadInvalid, err.Error())
		return
	}

	// Write any remaining data
	if r.ContentLength > 0 {
		upload.Write(r.Body)
	}

	// Finalize upload
	if err := h.uploads.Finish(uploadID, dgst, h.blobs); err != nil {
		h.errorResponse(w, http.StatusBadRequest, oci.ErrorCodeDigestInvalid, err.Error())
		return
	}

	// Link to repo
	h.metadata.LinkBlobToRepo(dgst, repo)

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", repo, dgst))
	w.Header().Set("Docker-Content-Digest", string(dgst))
	w.WriteHeader(http.StatusCreated)
}

// handleUploadStatus handles GET /v2/<name>/blobs/uploads/<uuid>
func (h *Handler) handleUploadStatus(w http.ResponseWriter, r *http.Request, repo, uploadID string) {
	upload, err := h.uploads.Get(uploadID)
	if err == storage.ErrUploadNotFound {
		h.errorResponse(w, http.StatusNotFound, oci.ErrorCodeBlobUploadUnknown, "upload not found")
		return
	}
	if err != nil {
		h.errorResponse(w, http.StatusInternalServerError, oci.ErrorCodeBlobUploadInvalid, err.Error())
		return
	}

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", repo, uploadID))
	w.Header().Set("Docker-Upload-UUID", uploadID)
	w.Header().Set("Range", fmt.Sprintf("0-%d", upload.Size-1))
	w.WriteHeader(http.StatusNoContent)
}

// handleCancelUpload handles DELETE /v2/<name>/blobs/uploads/<uuid>
func (h *Handler) handleCancelUpload(w http.ResponseWriter, r *http.Request, repo, uploadID string) {
	if err := h.uploads.Cancel(uploadID); err == storage.ErrUploadNotFound {
		h.errorResponse(w, http.StatusNotFound, oci.ErrorCodeBlobUploadUnknown, "upload not found")
		return
	} else if err != nil {
		h.errorResponse(w, http.StatusInternalServerError, oci.ErrorCodeBlobUploadInvalid, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleListTags handles GET /v2/<name>/tags/list
func (h *Handler) handleListTags(w http.ResponseWriter, r *http.Request, repo string) {
	tags, err := h.metadata.ListTags(repo)
	if err != nil {
		h.errorResponse(w, http.StatusInternalServerError, oci.ErrorCodeNameUnknown, err.Error())
		return
	}

	// Handle pagination
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	last := r.URL.Query().Get("last")

	if last != "" {
		// Find starting point
		for i, t := range tags {
			if t == last {
				tags = tags[i+1:]
				break
			}
		}
	}

	if n > 0 && len(tags) > n {
		tags = tags[:n]
		// Set Link header for next page
		if len(tags) > 0 {
			w.Header().Set("Link", fmt.Sprintf(`</v2/%s/tags/list?n=%d&last=%s>; rel="next"`, repo, n, tags[len(tags)-1]))
		}
	}

	tagList := oci.TagList{
		Name: repo,
		Tags: tags,
	}
	h.jsonResponse(w, http.StatusOK, tagList)
}

// Helper methods

func (h *Handler) errorResponse(w http.ResponseWriter, status int, code string, message string) {
	resp := oci.ErrorResponse{
		Errors: []oci.Error{{
			Code:    code,
			Message: message,
		}},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

func (h *Handler) jsonResponse(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
