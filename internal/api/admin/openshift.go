package admin

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OpenShiftIntegration handles OpenShift-specific functionality
type OpenShiftIntegration struct {
	enabled        bool
	apiServerURL   string
	serviceAccount string
	token          string
	tokenPath      string
	client         *http.Client
	mu             sync.RWMutex
}

// OpenShiftConfig holds OpenShift integration configuration
type OpenShiftConfig struct {
	Enabled        bool
	APIServerURL   string // e.g., https://kubernetes.default.svc
	ServiceAccount string
	TokenPath      string // Path to service account token
}

// NewOpenShiftIntegration creates a new OpenShift integration
func NewOpenShiftIntegration(cfg OpenShiftConfig) *OpenShiftIntegration {
	oi := &OpenShiftIntegration{
		enabled:        cfg.Enabled,
		apiServerURL:   cfg.APIServerURL,
		serviceAccount: cfg.ServiceAccount,
		tokenPath:      cfg.TokenPath,
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true, // In-cluster, trust the API server
				},
			},
		},
	}

	if cfg.TokenPath == "" {
		oi.tokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	}

	if cfg.APIServerURL == "" {
		oi.apiServerURL = "https://kubernetes.default.svc"
	}

	return oi
}

// ImageStream represents an OpenShift ImageStream
type ImageStream struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   ImageStreamMeta   `json:"metadata"`
	Spec       ImageStreamSpec   `json:"spec,omitempty"`
	Status     ImageStreamStatus `json:"status,omitempty"`
}

// ImageStreamMeta holds metadata for an ImageStream
type ImageStreamMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// ImageStreamSpec holds the spec for an ImageStream
type ImageStreamSpec struct {
	DockerImageRepository string          `json:"dockerImageRepository,omitempty"`
	Tags                  []ImageStreamTag `json:"tags,omitempty"`
}

// ImageStreamTag represents a tag in an ImageStream
type ImageStreamTag struct {
	Name string              `json:"name"`
	From *ImageStreamTagFrom `json:"from,omitempty"`
}

// ImageStreamTagFrom specifies the source for a tag
type ImageStreamTagFrom struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// ImageStreamStatus holds the status of an ImageStream
type ImageStreamStatus struct {
	DockerImageRepository string `json:"dockerImageRepository"`
}

// ImageStreamImport represents an import request
type ImageStreamImport struct {
	APIVersion string                  `json:"apiVersion"`
	Kind       string                  `json:"kind"`
	Metadata   ImageStreamMeta         `json:"metadata"`
	Spec       ImageStreamImportSpec   `json:"spec"`
	Status     ImageStreamImportStatus `json:"status,omitempty"`
}

// ImageStreamImportSpec holds import specification
type ImageStreamImportSpec struct {
	Import bool                    `json:"import"`
	Images []ImageStreamImportImage `json:"images,omitempty"`
}

// ImageStreamImportImage represents an image to import
type ImageStreamImportImage struct {
	From ImageStreamTagFrom `json:"from"`
	To   *ImageStreamTagTo  `json:"to,omitempty"`
}

// ImageStreamTagTo specifies the destination for an import
type ImageStreamTagTo struct {
	Name string `json:"name"`
}

// ImageStreamImportStatus holds import status
type ImageStreamImportStatus struct {
	Images []ImageStreamImportImageStatus `json:"images,omitempty"`
}

// ImageStreamImportImageStatus holds status for an imported image
type ImageStreamImportImageStatus struct {
	Status string `json:"status"`
	Image  string `json:"image,omitempty"`
}

// WebhookPayload represents an ImageStream webhook payload
type WebhookPayload struct {
	Type      string     `json:"type"`
	Namespace string     `json:"namespace"`
	Name      string     `json:"name"`
	Tag       string     `json:"tag,omitempty"`
	Digest    string     `json:"digest,omitempty"`
	Image     string     `json:"image,omitempty"`
}

// ValidateToken validates an OpenShift OAuth token
func (oi *OpenShiftIntegration) ValidateToken(token string) (*TokenReview, error) {
	if !oi.enabled {
		return nil, fmt.Errorf("OpenShift integration not enabled")
	}

	saToken, err := oi.getServiceAccountToken()
	if err != nil {
		return nil, err
	}

	// Create TokenReview request
	review := map[string]interface{}{
		"apiVersion": "authentication.k8s.io/v1",
		"kind":       "TokenReview",
		"spec": map[string]interface{}{
			"token": token,
		},
	}

	body, _ := json.Marshal(review)
	req, err := http.NewRequest("POST",
		oi.apiServerURL+"/apis/authentication.k8s.io/v1/tokenreviews",
		strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+saToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := oi.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token review returned %d", resp.StatusCode)
	}

	var result TokenReview
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &result, nil
}

// TokenReview represents a token review response
type TokenReview struct {
	Status TokenReviewStatus `json:"status"`
}

// TokenReviewStatus holds the result of a token review
type TokenReviewStatus struct {
	Authenticated bool   `json:"authenticated"`
	User          User   `json:"user,omitempty"`
	Error         string `json:"error,omitempty"`
}

// User represents an authenticated user
type User struct {
	Username string   `json:"username"`
	UID      string   `json:"uid"`
	Groups   []string `json:"groups"`
}

// CheckAccess verifies if a user can access a repository
func (oi *OpenShiftIntegration) CheckAccess(user, namespace, repo, verb string) (bool, error) {
	if !oi.enabled {
		return true, nil // Allow all if not enabled
	}

	saToken, err := oi.getServiceAccountToken()
	if err != nil {
		return false, err
	}

	// Create SubjectAccessReview
	review := map[string]interface{}{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SubjectAccessReview",
		"spec": map[string]interface{}{
			"user": user,
			"resourceAttributes": map[string]interface{}{
				"namespace": namespace,
				"verb":      verb,
				"group":     "image.openshift.io",
				"resource":  "imagestreams",
				"name":      repo,
			},
		},
	}

	body, _ := json.Marshal(review)
	req, err := http.NewRequest("POST",
		oi.apiServerURL+"/apis/authorization.k8s.io/v1/subjectaccessreviews",
		strings.NewReader(string(body)))
	if err != nil {
		return false, err
	}

	req.Header.Set("Authorization", "Bearer "+saToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := oi.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("access review returned %d", resp.StatusCode)
	}

	var result struct {
		Status struct {
			Allowed bool `json:"allowed"`
		} `json:"status"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, err
	}

	return result.Status.Allowed, nil
}

// NotifyImagePush sends a webhook notification when an image is pushed
func (oi *OpenShiftIntegration) NotifyImagePush(namespace, repo, tag, digest string) error {
	if !oi.enabled {
		return nil
	}

	log.Printf("OpenShift: Image pushed %s/%s:%s (%s)", namespace, repo, tag, digest[:12])

	// In a real implementation, this would:
	// 1. Update the ImageStream with the new tag
	// 2. Trigger any builds that depend on this image
	// 3. Update deployment configs if configured

	return nil
}

// getServiceAccountToken reads the service account token
func (oi *OpenShiftIntegration) getServiceAccountToken() (string, error) {
	oi.mu.RLock()
	if oi.token != "" {
		token := oi.token
		oi.mu.RUnlock()
		return token, nil
	}
	oi.mu.RUnlock()

	// Read from file
	data, err := io.ReadAll(strings.NewReader(oi.tokenPath))
	if err != nil {
		// In development, might not have a token
		return "", nil
	}

	oi.mu.Lock()
	oi.token = string(data)
	oi.mu.Unlock()

	return oi.token, nil
}

// WebhookHandler handles ImageStream webhooks
type WebhookHandler struct {
	callbacks []func(WebhookPayload)
	mu        sync.RWMutex
}

// NewWebhookHandler creates a new webhook handler
func NewWebhookHandler() *WebhookHandler {
	return &WebhookHandler{}
}

// OnPush registers a callback for push events
func (h *WebhookHandler) OnPush(fn func(WebhookPayload)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.callbacks = append(h.callbacks, fn)
}

// ServeHTTP handles webhook requests
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload WebhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	h.mu.RLock()
	callbacks := h.callbacks
	h.mu.RUnlock()

	for _, fn := range callbacks {
		go fn(payload)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}
