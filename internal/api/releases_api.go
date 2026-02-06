package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gwest/fastregistry/internal/certs"
	"github.com/gwest/fastregistry/internal/releases"
)

// handleReleases routes /admin/releases/* requests
func (r *Router) handleReleases(w http.ResponseWriter, req *http.Request) {
	if r.releaseManager == nil {
		http.Error(w, `{"error":"releases not enabled"}`, http.StatusNotFound)
		return
	}

	path := strings.TrimPrefix(req.URL.Path, r.adminPrefix+"/releases")
	w.Header().Set("Content-Type", "application/json")

	switch {
	case path == "" || path == "/":
		r.handleListReleases(w, req)

	case path == "/discover":
		r.handleDiscover(w, req)

	case path == "/clone":
		r.handleClone(w, req)

	case path == "/extract":
		r.handleExtract(w, req)

	case strings.HasSuffix(path, "/extract"):
		version := strings.TrimPrefix(path, "/")
		version = strings.TrimSuffix(version, "/extract")
		r.handleExtractVersion(w, req, version)

	case strings.HasSuffix(path, "/reset"):
		version := strings.TrimPrefix(path, "/")
		version = strings.TrimSuffix(version, "/reset")
		r.handleResetState(w, req, version)

	case strings.HasSuffix(path, "/status"):
		version := strings.TrimPrefix(path, "/")
		version = strings.TrimSuffix(version, "/status")
		r.handleReleaseStatus(w, req, version)

	case strings.HasSuffix(path, "/artifacts"):
		version := strings.TrimPrefix(path, "/")
		version = strings.TrimSuffix(version, "/artifacts")
		r.handleReleaseArtifacts(w, req, version)

	case strings.HasSuffix(path, "/copy"):
		version := strings.TrimPrefix(path, "/")
		version = strings.TrimSuffix(version, "/copy")
		r.handleCopyArtifact(w, req, version)

	case strings.HasSuffix(path, "/iso"):
		version := strings.TrimPrefix(path, "/")
		version = strings.TrimSuffix(version, "/iso")
		r.handleGenerateISO(w, req, version)

	default:
		http.NotFound(w, req)
	}
}

func (r *Router) handleListReleases(w http.ResponseWriter, req *http.Request) {
	stateFilter := req.URL.Query().Get("state")

	var rels []releases.Release
	var err error

	if stateFilter != "" {
		rels, err = r.releaseManager.ListByState(releases.ReleaseState(stateFilter))
	} else {
		rels, err = r.releaseManager.ListReleases()
	}
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	if rels == nil {
		rels = []releases.Release{}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"releases": rels})
}

func (r *Router) handleDiscover(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Run discovery in background so the HTTP request doesn't time out
	r.releaseManager.DiscoverAsync()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "discovery started in background",
	})
}

func (r *Router) handleClone(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if body.Version == "" {
		http.Error(w, `{"error":"version is required"}`, http.StatusBadRequest)
		return
	}

	if err := r.releaseManager.CloneRelease(req.Context(), body.Version); err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{
		"message": "clone started",
		"version": body.Version,
	})
}

func (r *Router) handleExtract(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Version  string   `json:"version"`
		Versions []string `json:"versions"`
		All      bool     `json:"all"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	var versions []string

	switch {
	case body.All:
		// Find all cloned/ready releases that need (re-)extraction
		candidates, err := r.releaseManager.ListExtractable()
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
			return
		}
		versions = candidates

	case len(body.Versions) > 0:
		versions = body.Versions

	case body.Version != "":
		versions = []string{body.Version}

	default:
		http.Error(w, `{"error":"provide version, versions, or all:true"}`, http.StatusBadRequest)
		return
	}

	var started []string
	var errors []string
	for _, v := range versions {
		if err := r.releaseManager.ReExtract(v); err != nil {
			errors = append(errors, v+": "+err.Error())
		} else {
			started = append(started, v)
		}
	}

	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "extraction started",
		"started": started,
		"errors":  errors,
	})
}

func (r *Router) handleResetState(w http.ResponseWriter, req *http.Request, version string) {
	if req.Method != http.MethodPost && req.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	if err := r.releaseManager.ResetState(version); err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{
		"message": "state reset to cloned",
		"version": version,
	})
}

func (r *Router) handleExtractVersion(w http.ResponseWriter, req *http.Request, version string) {
	if req.Method != http.MethodPost && req.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	if err := r.releaseManager.ReExtract(version); err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "extraction started",
		"started": []string{version},
	})
}

func (r *Router) handleReleaseStatus(w http.ResponseWriter, req *http.Request, version string) {
	rel, err := r.releaseManager.GetRelease(version)
	if err != nil {
		http.Error(w, `{"error":"release not found"}`, http.StatusNotFound)
		return
	}

	result := map[string]interface{}{
		"release": rel,
	}

	if progress := r.releaseManager.GetProgress(version); progress != nil {
		result["progress"] = progress
	}

	json.NewEncoder(w).Encode(result)
}

func (r *Router) handleReleaseArtifacts(w http.ResponseWriter, req *http.Request, version string) {
	rel, err := r.releaseManager.GetRelease(version)
	if err != nil {
		http.Error(w, `{"error":"release not found"}`, http.StatusNotFound)
		return
	}

	type artifactInfo struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		Size        int64  `json:"size"`
		SHA256      string `json:"sha256,omitempty"`
		DownloadURL string `json:"download_url"`
	}

	var artifacts []artifactInfo
	for _, a := range rel.Artifacts {
		artifacts = append(artifacts, artifactInfo{
			Name:        a.Name,
			Type:        a.Type,
			Size:        a.Size,
			SHA256:      a.SHA256,
			DownloadURL: "/files/releases/" + version + "/" + a.Name,
		})
	}

	if artifacts == nil {
		artifacts = []artifactInfo{}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"artifacts": artifacts})
}

func (r *Router) handleCopyArtifact(w http.ResponseWriter, req *http.Request, version string) {
	if req.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Artifact    string `json:"artifact"`
		Destination string `json:"destination"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if body.Artifact == "" {
		http.Error(w, `{"error":"artifact is required"}`, http.StatusBadRequest)
		return
	}
	if body.Destination == "" {
		http.Error(w, `{"error":"destination is required"}`, http.StatusBadRequest)
		return
	}

	if err := r.releaseManager.CopyArtifact(version, body.Artifact, body.Destination); err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{
		"message":     "copy complete",
		"artifact":    body.Artifact,
		"destination": body.Destination,
	})
}

func (r *Router) handleGenerateISO(w http.ResponseWriter, req *http.Request, version string) {
	if req.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Ignition      string `json:"ignition"`       // Raw ignition JSON (option 1)
		InstallConfig string `json:"install_config"` // install-config.yaml content (option 2)
		AgentConfig   string `json:"agent_config"`   // agent-config.yaml content (option 2)
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	var ignition []byte
	var err error

	if body.Ignition != "" {
		// Option 1: Raw ignition provided
		ignition = []byte(body.Ignition)
	} else if body.InstallConfig != "" && body.AgentConfig != "" {
		// Option 2: Generate ignition from configs
		releaseImage := r.releaseManager.GetReleaseImage(version)
		ignition, err = releases.GenerateAgentIgnition(
			[]byte(body.InstallConfig),
			[]byte(body.AgentConfig),
			releaseImage,
		)
		if err != nil {
			http.Error(w, `{"error":"generating ignition: `+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
	} else {
		http.Error(w, `{"error":"provide either 'ignition' or both 'install_config' and 'agent_config'"}`, http.StatusBadRequest)
		return
	}

	id, isoURL, err := r.releaseManager.GenerateISO(version, ignition)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	// Build full URL for sanboot
	scheme := "http"
	if req.TLS != nil {
		scheme = "https"
	}
	host := req.Host
	fullURL := scheme + "://" + host + isoURL

	json.NewEncoder(w).Encode(map[string]string{
		"id":       id,
		"iso_url":  isoURL,
		"full_url": fullURL,
	})
}

// handleCerts routes /admin/certs/* requests
func (r *Router) handleCerts(w http.ResponseWriter, req *http.Request) {
	if r.certManager == nil {
		http.Error(w, `{"error":"certs not configured"}`, http.StatusNotFound)
		return
	}

	path := strings.TrimPrefix(req.URL.Path, r.adminPrefix+"/certs")

	switch {
	case path == "/ca" || path == "/ca/":
		r.handleCACert(w, req)
	default:
		http.NotFound(w, req)
	}
}

func (r *Router) handleCACert(w http.ResponseWriter, req *http.Request) {
	if !r.certManager.HasCA() {
		http.Error(w, `{"error":"no CA certificate available"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", "attachment; filename=ca.crt")
	w.Write(r.certManager.CACertPEM())
}

// These methods are added to Router (declared in router.go)
// The fields are set via SetReleaseManager and SetCertManager

// SetReleaseManager sets the release manager on the router
func (r *Router) SetReleaseManager(mgr *releases.Manager) {
	r.releaseManager = mgr
}

// SetCertManager sets the certificate manager on the router
func (r *Router) SetCertManager(mgr *certs.Manager) {
	r.certManager = mgr
}
