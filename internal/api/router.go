package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gwest/fastregistry/internal/api/ui"
	v2 "github.com/gwest/fastregistry/internal/api/v2"
	"github.com/gwest/fastregistry/internal/certs"
	"github.com/gwest/fastregistry/internal/mirror"
	"github.com/gwest/fastregistry/internal/releases"
	"github.com/gwest/fastregistry/internal/storage"
	"github.com/gwest/fastregistry/internal/sync"
)

// Router is the main HTTP router for FastRegistry
type Router struct {
	v2Handler      *v2.Handler
	uiHandler      *ui.Handler
	auth           Authenticator
	adminPrefix    string
	proxy          *mirror.Proxy
	scheduler      *sync.Scheduler
	releaseManager *releases.Manager
	certManager    *certs.Manager
	filesHandler   http.Handler
}

// Authenticator defines the authentication interface
type Authenticator interface {
	Authenticate(r *http.Request) (string, bool)
	Required() bool
}

// NewRouter creates a new router
func NewRouter(blobs *storage.BlobStore, metadata *storage.MetadataStore, uploads *storage.UploadManager, auth Authenticator) *Router {
	return &Router{
		v2Handler:   v2.NewHandler(blobs, metadata, uploads),
		auth:        auth,
		adminPrefix: "/admin",
	}
}

// SetProxy sets the mirror proxy
func (r *Router) SetProxy(proxy *mirror.Proxy) {
	r.proxy = proxy
	r.v2Handler.SetProxy(proxy)
}

// SetScheduler sets the sync scheduler
func (r *Router) SetScheduler(scheduler *sync.Scheduler) {
	r.scheduler = scheduler
}

// SetUI sets the web UI handler
func (r *Router) SetUI(handler *ui.Handler) {
	r.uiHandler = handler
}

// SetFilesHandler sets the static file handler for release artifacts
func (r *Router) SetFilesHandler(handler http.Handler) {
	r.filesHandler = handler
}

// ServeHTTP implements http.Handler
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := time.Now()

	// Wrap response writer to capture status
	wrapped := &responseWriter{ResponseWriter: w, status: http.StatusOK}

	// Add common headers
	wrapped.Header().Set("Docker-Distribution-API-Version", "registry/2.0")

	// Handle CORS preflight
	if req.Method == http.MethodOptions {
		r.handleCORS(wrapped)
		return
	}

	// Authentication
	if r.auth != nil && r.auth.Required() {
		user, ok := r.auth.Authenticate(req)
		if !ok {
			wrapped.Header().Set("WWW-Authenticate", `Basic realm="FastRegistry"`)
			http.Error(wrapped, "Unauthorized", http.StatusUnauthorized)
			r.logRequest(req, wrapped.status, time.Since(start))
			return
		}
		// Store user in request context if needed
		_ = user
	}

	// Route to appropriate handler
	path := req.URL.Path

	switch {
	case path == "/":
		http.Redirect(wrapped, req, "/ui/", http.StatusFound)

	case strings.HasPrefix(path, "/ui"):
		if r.uiHandler != nil {
			r.uiHandler.ServeHTTP(wrapped, req)
		} else {
			http.NotFound(wrapped, req)
		}

	case strings.HasPrefix(path, "/v2"):
		r.v2Handler.ServeHTTP(wrapped, req)

	case strings.HasPrefix(path, "/files/"):
		if r.filesHandler != nil {
			r.filesHandler.ServeHTTP(wrapped, req)
		} else {
			http.NotFound(wrapped, req)
		}

	case strings.HasPrefix(path, r.adminPrefix):
		r.handleAdmin(wrapped, req)

	case path == "/health" || path == "/healthz":
		r.handleHealth(wrapped, req)

	case path == "/metrics":
		r.handleMetrics(wrapped, req)

	default:
		http.NotFound(wrapped, req)
	}

	r.logRequest(req, wrapped.status, time.Since(start))
}

func (r *Router) handleCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Docker-Content-Digest")
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
}

func (r *Router) handleHealth(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"healthy"}`))
}

func (r *Router) handleMetrics(w http.ResponseWriter, req *http.Request) {
	// TODO: Prometheus metrics
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("# FastRegistry metrics\n"))
}

func (r *Router) handleAdmin(w http.ResponseWriter, req *http.Request) {
	path := strings.TrimPrefix(req.URL.Path, r.adminPrefix)
	w.Header().Set("Content-Type", "application/json")

	switch {
	case path == "/gc":
		// Trigger garbage collection
		w.Write([]byte(`{"message":"GC triggered"}`))

	case path == "/status":
		w.Write([]byte(`{"status":"running","version":"0.1.0"}`))

	case path == "/sync/jobs":
		// List sync jobs
		if r.scheduler == nil {
			w.Write([]byte(`{"jobs":[]}`))
			return
		}
		jobs := r.scheduler.ListJobs()
		json.NewEncoder(w).Encode(map[string]interface{}{"jobs": jobs})

	case strings.HasPrefix(path, "/sync/trigger/"):
		// Trigger a sync job
		if r.scheduler == nil {
			http.Error(w, `{"error":"sync not configured"}`, http.StatusNotFound)
			return
		}
		jobName := strings.TrimPrefix(path, "/sync/trigger/")
		if err := r.scheduler.TriggerSync(jobName); err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"message":"sync triggered","job":"` + jobName + `"}`))

	case strings.HasPrefix(path, "/sync/status/"):
		// Get sync job status
		if r.scheduler == nil {
			http.Error(w, `{"error":"sync not configured"}`, http.StatusNotFound)
			return
		}
		jobName := strings.TrimPrefix(path, "/sync/status/")
		status, err := r.scheduler.GetJobStatus(jobName)
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(status)

	case strings.HasPrefix(path, "/releases"):
		r.handleReleases(w, req)

	case strings.HasPrefix(path, "/certs"):
		r.handleCerts(w, req)

	default:
		http.NotFound(w, req)
	}
}

func (r *Router) logRequest(req *http.Request, status int, duration time.Duration) {
	log.Printf("%s %s %d %v", req.Method, req.URL.Path, status, duration)
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
