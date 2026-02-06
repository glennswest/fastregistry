package storage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/gwest/fastregistry/pkg/digest"
)

// BlobStore handles content-addressable blob storage with memory-mapped I/O
type BlobStore struct {
	root string
	mu   sync.RWMutex

	// Track open mmap'd files for cleanup
	mmaps map[string][]byte
	mmapMu sync.Mutex
}

// NewBlobStore creates a new blob store at the given root path
func NewBlobStore(root string) (*BlobStore, error) {
	blobDir := filepath.Join(root, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		return nil, fmt.Errorf("creating blob directory: %w", err)
	}

	// Create subdirectories for first two hex chars (256 dirs)
	for i := 0; i < 256; i++ {
		subdir := filepath.Join(blobDir, fmt.Sprintf("%02x", i))
		if err := os.MkdirAll(subdir, 0755); err != nil {
			return nil, fmt.Errorf("creating blob subdir: %w", err)
		}
	}

	return &BlobStore{
		root:  root,
		mmaps: make(map[string][]byte),
	}, nil
}

// blobPath returns the filesystem path for a digest
func (bs *BlobStore) blobPath(d digest.Digest) string {
	hex := d.Hex()
	// Store as blobs/sha256/ab/abcdef123...
	return filepath.Join(bs.root, "blobs", d.Algorithm(), hex[:2], hex)
}

// Exists checks if a blob exists
func (bs *BlobStore) Exists(d digest.Digest) bool {
	bs.mu.RLock()
	defer bs.mu.RUnlock()

	_, err := os.Stat(bs.blobPath(d))
	return err == nil
}

// Size returns the size of a blob
func (bs *BlobStore) Size(d digest.Digest) (int64, error) {
	bs.mu.RLock()
	defer bs.mu.RUnlock()

	info, err := os.Stat(bs.blobPath(d))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, ErrBlobNotFound
		}
		return 0, err
	}
	return info.Size(), nil
}

// Get returns a reader for the blob content
// For small blobs, returns mmap'd data; for large blobs, returns file handle
func (bs *BlobStore) Get(d digest.Digest) (io.ReadCloser, int64, error) {
	bs.mu.RLock()
	defer bs.mu.RUnlock()

	path := bs.blobPath(d)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, ErrBlobNotFound
		}
		return nil, 0, err
	}

	size := info.Size()

	// For blobs > 10MB, use regular file I/O to avoid mmap overhead
	if size > 10*1024*1024 {
		f, err := os.Open(path)
		if err != nil {
			return nil, 0, err
		}
		return f, size, nil
	}

	// For smaller blobs, use mmap for zero-copy reads
	data, err := bs.mmap(path, size)
	if err != nil {
		// Fall back to regular file I/O
		f, err := os.Open(path)
		if err != nil {
			return nil, 0, err
		}
		return f, size, nil
	}

	return &mmapReader{data: data, bs: bs, path: path}, size, nil
}

// GetRange returns a reader for a byte range of the blob
func (bs *BlobStore) GetRange(d digest.Digest, start, end int64) (io.ReadCloser, error) {
	bs.mu.RLock()
	defer bs.mu.RUnlock()

	path := bs.blobPath(d)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrBlobNotFound
		}
		return nil, err
	}

	if _, err := f.Seek(start, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}

	return &limitedReadCloser{
		Reader: io.LimitReader(f, end-start+1),
		Closer: f,
	}, nil
}

// Put stores a blob, verifying the digest matches
func (bs *BlobStore) Put(d digest.Digest, r io.Reader, expectedSize int64) error {
	bs.mu.Lock()
	defer bs.mu.Unlock()

	path := bs.blobPath(d)

	// Check if already exists (deduplication)
	if _, err := os.Stat(path); err == nil {
		// Blob exists, verify size matches
		info, _ := os.Stat(path)
		if info.Size() == expectedSize {
			// Drain the reader
			io.Copy(io.Discard, r)
			return nil
		}
	}

	// Create temp file in same directory for atomic rename
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpPath) // Clean up on error
	}()

	// Pre-allocate space if size is known
	if expectedSize > 0 {
		if err := tmp.Truncate(expectedSize); err != nil {
			// Ignore - some filesystems don't support this
		}
	}

	// Copy and compute digest simultaneously
	verifier, err := digest.NewVerifier(d)
	if err != nil {
		return err
	}

	written, err := io.Copy(io.MultiWriter(tmp, verifier), r)
	if err != nil {
		return fmt.Errorf("writing blob: %w", err)
	}

	// Verify digest
	if !verifier.Verified() {
		return fmt.Errorf("digest verification failed for %s", d)
	}

	// Verify size if expected
	if expectedSize > 0 && written != expectedSize {
		return fmt.Errorf("size mismatch: expected %d, got %d", expectedSize, written)
	}

	// Sync to disk
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("syncing blob: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming blob: %w", err)
	}

	return nil
}

// Delete removes a blob
func (bs *BlobStore) Delete(d digest.Digest) error {
	bs.mu.Lock()
	defer bs.mu.Unlock()

	path := bs.blobPath(d)
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return ErrBlobNotFound
		}
		return err
	}
	return nil
}

// mmap maps a file into memory
func (bs *BlobStore) mmap(path string, size int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	data, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, err
	}

	bs.mmapMu.Lock()
	bs.mmaps[path] = data
	bs.mmapMu.Unlock()

	return data, nil
}

// munmap unmaps a previously mapped file
func (bs *BlobStore) munmap(path string) {
	bs.mmapMu.Lock()
	defer bs.mmapMu.Unlock()

	if data, ok := bs.mmaps[path]; ok {
		syscall.Munmap(data)
		delete(bs.mmaps, path)
	}
}

// Close releases all resources
func (bs *BlobStore) Close() error {
	bs.mmapMu.Lock()
	defer bs.mmapMu.Unlock()

	for path, data := range bs.mmaps {
		syscall.Munmap(data)
		delete(bs.mmaps, path)
	}
	return nil
}

// mmapReader wraps mmap'd data as an io.ReadCloser
type mmapReader struct {
	data   []byte
	offset int
	bs     *BlobStore
	path   string
}

func (r *mmapReader) Read(p []byte) (n int, err error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	n = copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}

func (r *mmapReader) Close() error {
	// Don't unmap immediately - let the cache handle it
	return nil
}

// limitedReadCloser wraps a LimitReader with a Closer
type limitedReadCloser struct {
	io.Reader
	io.Closer
}

// Errors
var (
	ErrBlobNotFound = fmt.Errorf("blob not found")
)
