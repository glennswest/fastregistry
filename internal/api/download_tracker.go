package api

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gwest/fastregistry/internal/events"
)

// TrackingFileServer wraps an http.Handler (typically http.FileServer) to record downloads.
type TrackingFileServer struct {
	inner      http.Handler
	eventStore *events.Store
}

// NewTrackingFileServer wraps inner with download tracking.
func NewTrackingFileServer(inner http.Handler, eventStore *events.Store) *TrackingFileServer {
	return &TrackingFileServer{inner: inner, eventStore: eventStore}
}

func (t *TrackingFileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || t.eventStore == nil {
		t.inner.ServeHTTP(w, r)
		return
	}

	// Parse version and artifact from path: /files/releases/<version>/<artifact>
	// The path has already been stripped of /files/releases/ by StripPrefix,
	// but we receive the original URL.
	version, artifact := parseReleasePath(r.URL.Path)
	if version == "" || artifact == "" {
		t.inner.ServeHTTP(w, r)
		return
	}

	tw := &trackingResponseWriter{ResponseWriter: w}
	start := time.Now()

	t.inner.ServeHTTP(tw, r)

	if tw.status == 0 || tw.status == http.StatusOK {
		rec := events.DownloadRecord{
			Time:      time.Now(),
			Version:   version,
			Artifact:  artifact,
			ClientIP:  clientIP(r),
			UserAgent: r.UserAgent(),
			Size:      tw.written,
			Duration:  time.Since(start),
		}
		t.eventStore.RecordDownload(rec)
		t.eventStore.RecordEvent(
			events.EventDownloadArtifact,
			events.SeverityInfo,
			version,
			artifact+" downloaded by "+clientIP(r),
			map[string]string{"artifact": artifact, "size": formatBytes(tw.written)},
		)
	}
}

// parseReleasePath extracts version and artifact from a URL path like
// "<anything>/<version>/<artifact>" (last two segments).
func parseReleasePath(path string) (version, artifact string) {
	path = strings.TrimPrefix(path, "/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return "", ""
	}
	return parts[len(parts)-2], parts[len(parts)-1]
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	suffixes := []string{"KB", "MB", "GB", "TB"}
	return fmt.Sprintf("%.1f %s", float64(b)/float64(div), suffixes[exp])
}

type trackingResponseWriter struct {
	http.ResponseWriter
	status  int
	written int64
}

func (w *trackingResponseWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *trackingResponseWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.written += int64(n)
	return n, err
}
