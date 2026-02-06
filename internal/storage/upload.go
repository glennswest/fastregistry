package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gwest/fastregistry/pkg/digest"
)

// UploadManager handles blob upload sessions
type UploadManager struct {
	root    string
	uploads map[string]*Upload
	mu      sync.RWMutex
}

// Upload represents an in-progress blob upload
type Upload struct {
	ID        string
	Repo      string
	StartedAt time.Time
	Size      int64
	file      *os.File
	hash      hash.Hash
	mu        sync.Mutex
}

// NewUploadManager creates a new upload manager
func NewUploadManager(root string) (*UploadManager, error) {
	uploadDir := filepath.Join(root, "uploads")
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		return nil, fmt.Errorf("creating upload directory: %w", err)
	}

	um := &UploadManager{
		root:    root,
		uploads: make(map[string]*Upload),
	}

	// Clean up stale uploads on startup
	go um.cleanupStale()

	return um, nil
}

// Start initiates a new upload session
func (um *UploadManager) Start(repo string) (*Upload, error) {
	um.mu.Lock()
	defer um.mu.Unlock()

	id := generateUploadID()
	path := um.uploadPath(id)

	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("creating upload file: %w", err)
	}

	upload := &Upload{
		ID:        id,
		Repo:      repo,
		StartedAt: time.Now(),
		file:      f,
		hash:      sha256.New(),
	}

	um.uploads[id] = upload
	return upload, nil
}

// Get retrieves an existing upload session
func (um *UploadManager) Get(id string) (*Upload, error) {
	um.mu.RLock()
	defer um.mu.RUnlock()

	upload, ok := um.uploads[id]
	if !ok {
		return nil, ErrUploadNotFound
	}
	return upload, nil
}

// uploadPath returns the filesystem path for an upload
func (um *UploadManager) uploadPath(id string) string {
	return filepath.Join(um.root, "uploads", id)
}

// Write appends data to the upload
func (u *Upload) Write(r io.Reader) (int64, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	// Write to both file and hash
	n, err := io.Copy(io.MultiWriter(u.file, u.hash), r)
	u.Size += n
	return n, err
}

// WriteAt writes data at a specific offset (for chunked uploads)
func (u *Upload) WriteAt(r io.Reader, offset int64) (int64, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	// Seek to offset
	if _, err := u.file.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}

	// For chunked uploads with offset, we can't incrementally hash
	// We'll need to rehash on finalize
	n, err := io.Copy(u.file, r)
	if offset+n > u.Size {
		u.Size = offset + n
	}
	return n, err
}

// Finish completes the upload and moves blob to content-addressable storage
func (um *UploadManager) Finish(id string, expectedDigest digest.Digest, bs *BlobStore) error {
	um.mu.Lock()
	upload, ok := um.uploads[id]
	if !ok {
		um.mu.Unlock()
		return ErrUploadNotFound
	}
	delete(um.uploads, id)
	um.mu.Unlock()

	upload.mu.Lock()
	defer upload.mu.Unlock()

	// Sync and close the file
	if err := upload.file.Sync(); err != nil {
		return fmt.Errorf("syncing upload: %w", err)
	}

	uploadPath := um.uploadPath(id)

	// Compute final digest
	if _, err := upload.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seeking to start: %w", err)
	}

	h := sha256.New()
	if _, err := io.Copy(h, upload.file); err != nil {
		return fmt.Errorf("computing digest: %w", err)
	}

	actualDigest := digest.Digest("sha256:" + hex.EncodeToString(h.Sum(nil)))

	if err := upload.file.Close(); err != nil {
		return fmt.Errorf("closing upload: %w", err)
	}

	// Verify digest matches
	if actualDigest != expectedDigest {
		os.Remove(uploadPath)
		return fmt.Errorf("digest mismatch: expected %s, got %s", expectedDigest, actualDigest)
	}

	// Move to blob store
	blobPath := bs.blobPath(expectedDigest)
	if err := os.MkdirAll(filepath.Dir(blobPath), 0755); err != nil {
		os.Remove(uploadPath)
		return fmt.Errorf("creating blob directory: %w", err)
	}

	// Check if blob already exists (deduplication)
	if _, err := os.Stat(blobPath); err == nil {
		os.Remove(uploadPath)
		return nil
	}

	if err := os.Rename(uploadPath, blobPath); err != nil {
		os.Remove(uploadPath)
		return fmt.Errorf("moving blob: %w", err)
	}

	return nil
}

// Cancel aborts an upload and cleans up
func (um *UploadManager) Cancel(id string) error {
	um.mu.Lock()
	upload, ok := um.uploads[id]
	if !ok {
		um.mu.Unlock()
		return ErrUploadNotFound
	}
	delete(um.uploads, id)
	um.mu.Unlock()

	upload.mu.Lock()
	defer upload.mu.Unlock()

	upload.file.Close()
	return os.Remove(um.uploadPath(id))
}

// cleanupStale removes uploads older than 24 hours
func (um *UploadManager) cleanupStale() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		um.mu.Lock()
		now := time.Now()
		for id, upload := range um.uploads {
			if now.Sub(upload.StartedAt) > 24*time.Hour {
				upload.file.Close()
				os.Remove(um.uploadPath(id))
				delete(um.uploads, id)
			}
		}
		um.mu.Unlock()
	}
}

// generateUploadID creates a unique upload ID
func generateUploadID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(cryptoRandReader, b); err != nil {
		// Fallback to time-based ID
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// cryptoRandReader is the crypto/rand reader
var cryptoRandReader io.Reader

func init() {
	cryptoRandReader = cryptoRand{}
}

type cryptoRand struct{}

func (cryptoRand) Read(p []byte) (n int, err error) {
	return io.ReadFull(randReader, p)
}

var randReader io.Reader

func init() {
	f, err := os.Open("/dev/urandom")
	if err != nil {
		// Fallback - shouldn't happen on Unix systems
		randReader = &timeRand{}
		return
	}
	randReader = f
}

type timeRand struct {
	mu sync.Mutex
	n  int64
}

func (r *timeRand) Read(p []byte) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	s := fmt.Sprintf("%d%d", time.Now().UnixNano(), r.n)
	h := sha256.Sum256([]byte(s))
	return copy(p, h[:]), nil
}

// Errors
var (
	ErrUploadNotFound = fmt.Errorf("upload not found")
)
