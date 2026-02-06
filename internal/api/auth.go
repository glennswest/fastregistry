package api

import (
	"bufio"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

// HtpasswdAuth implements htpasswd-based authentication
type HtpasswdAuth struct {
	users    map[string]string // username -> hashed password
	mu       sync.RWMutex
	filePath string
	required bool
}

// NewHtpasswdAuth creates a new htpasswd authenticator
func NewHtpasswdAuth(filePath string) (*HtpasswdAuth, error) {
	auth := &HtpasswdAuth{
		users:    make(map[string]string),
		filePath: filePath,
		required: true,
	}

	if filePath == "" {
		auth.required = false
		return auth, nil
	}

	if err := auth.load(); err != nil {
		return nil, err
	}

	return auth, nil
}

// load reads the htpasswd file
func (a *HtpasswdAuth) load() error {
	f, err := os.Open(a.filePath)
	if err != nil {
		return fmt.Errorf("opening htpasswd file: %w", err)
	}
	defer f.Close()

	a.mu.Lock()
	defer a.mu.Unlock()

	a.users = make(map[string]string)
	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		username := parts[0]
		password := parts[1]
		a.users[username] = password
	}

	return scanner.Err()
}

// Reload reloads the htpasswd file
func (a *HtpasswdAuth) Reload() error {
	return a.load()
}

// Required returns true if authentication is required
func (a *HtpasswdAuth) Required() bool {
	return a.required
}

// Authenticate checks the request for valid credentials
func (a *HtpasswdAuth) Authenticate(r *http.Request) (string, bool) {
	if !a.required {
		return "anonymous", true
	}

	// Get Authorization header
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", false
	}

	// Parse Basic auth
	if !strings.HasPrefix(auth, "Basic ") {
		return "", false
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
	if err != nil {
		return "", false
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return "", false
	}

	username := parts[0]
	password := parts[1]

	return username, a.verify(username, password)
}

// verify checks username/password against stored credentials
func (a *HtpasswdAuth) verify(username, password string) bool {
	a.mu.RLock()
	stored, ok := a.users[username]
	a.mu.RUnlock()

	if !ok {
		return false
	}

	return checkPassword(password, stored)
}

// checkPassword verifies a password against various htpasswd hash formats
func checkPassword(password, stored string) bool {
	// Bcrypt (starts with $2a$, $2b$, or $2y$)
	if strings.HasPrefix(stored, "$2") {
		err := bcrypt.CompareHashAndPassword([]byte(stored), []byte(password))
		return err == nil
	}

	// SHA1 ({SHA}base64hash)
	if strings.HasPrefix(stored, "{SHA}") {
		hash := sha1.Sum([]byte(password))
		expected := "{SHA}" + base64.StdEncoding.EncodeToString(hash[:])
		return subtle.ConstantTimeCompare([]byte(expected), []byte(stored)) == 1
	}

	// Plain text (not recommended but supported)
	if !strings.Contains(stored, "$") && !strings.HasPrefix(stored, "{") {
		return subtle.ConstantTimeCompare([]byte(password), []byte(stored)) == 1
	}

	// Apr1/MD5 ($apr1$salt$hash) - not implemented, return false
	if strings.HasPrefix(stored, "$apr1$") {
		// TODO: Implement APR1-MD5 if needed
		return false
	}

	return false
}

// NoAuth is an authenticator that allows all requests
type NoAuth struct{}

// Authenticate always returns true
func (a *NoAuth) Authenticate(r *http.Request) (string, bool) {
	return "anonymous", true
}

// Required returns false
func (a *NoAuth) Required() bool {
	return false
}
