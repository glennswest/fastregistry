package storage

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/gwest/fastregistry/pkg/digest"
)

// MetadataStore handles repository metadata using BadgerDB
type MetadataStore struct {
	db *badger.DB
}

// Key prefixes for different data types
const (
	prefixManifest   = "m:"  // m:<repo>:<reference> -> ManifestMeta
	prefixTag        = "t:"  // t:<repo>:<tag> -> digest
	prefixRepo       = "r:"  // r:<repo> -> RepoMeta
	prefixBlobRepo   = "br:" // br:<digest>:<repo> -> exists (for GC)
	prefixRepoBlob   = "rb:" // rb:<repo>:<digest> -> exists
)

// ManifestMeta holds metadata about a manifest
type ManifestMeta struct {
	Digest      digest.Digest `json:"digest"`
	MediaType   string        `json:"mediaType"`
	Size        int64         `json:"size"`
	CreatedAt   time.Time     `json:"createdAt"`
	Layers      []string      `json:"layers,omitempty"`      // Layer digests
	Subject     string        `json:"subject,omitempty"`     // OCI 1.1 subject
	Annotations map[string]string `json:"annotations,omitempty"`
}

// RepoMeta holds metadata about a repository
type RepoMeta struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// NewMetadataStore creates a new metadata store
func NewMetadataStore(root string) (*MetadataStore, error) {
	dbPath := filepath.Join(root, "metadata")

	opts := badger.DefaultOptions(dbPath)
	opts.Logger = nil // Disable badger logging
	opts.SyncWrites = true // Ensure durability
	opts.CompactL0OnClose = true

	db, err := badger.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("opening badger db: %w", err)
	}

	ms := &MetadataStore{db: db}

	// Start background GC
	go ms.runGC()

	return ms, nil
}

// Close closes the database
func (ms *MetadataStore) Close() error {
	return ms.db.Close()
}

// runGC periodically runs badger's garbage collection
func (ms *MetadataStore) runGC() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		for {
			err := ms.db.RunValueLogGC(0.5)
			if err != nil {
				break
			}
		}
	}
}

// PutManifest stores a manifest
func (ms *MetadataStore) PutManifest(repo string, reference string, meta *ManifestMeta) error {
	return ms.db.Update(func(txn *badger.Txn) error {
		// Store manifest metadata
		key := prefixManifest + repo + ":" + string(meta.Digest)
		data, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		if err := txn.Set([]byte(key), data); err != nil {
			return err
		}

		// If reference is a tag (not a digest), store tag -> digest mapping
		if !strings.HasPrefix(reference, "sha256:") {
			tagKey := prefixTag + repo + ":" + reference
			if err := txn.Set([]byte(tagKey), []byte(meta.Digest)); err != nil {
				return err
			}
		}

		// Update repo metadata
		repoKey := prefixRepo + repo
		repoMeta := &RepoMeta{
			Name:      repo,
			UpdatedAt: time.Now(),
		}

		// Check if repo exists
		item, err := txn.Get([]byte(repoKey))
		if err == badger.ErrKeyNotFound {
			repoMeta.CreatedAt = time.Now()
		} else if err == nil {
			var existing RepoMeta
			item.Value(func(val []byte) error {
				return json.Unmarshal(val, &existing)
			})
			repoMeta.CreatedAt = existing.CreatedAt
		}

		repoData, _ := json.Marshal(repoMeta)
		return txn.Set([]byte(repoKey), repoData)
	})
}

// GetManifest retrieves manifest metadata by reference (tag or digest)
func (ms *MetadataStore) GetManifest(repo, reference string) (*ManifestMeta, error) {
	var meta ManifestMeta

	err := ms.db.View(func(txn *badger.Txn) error {
		var d digest.Digest

		// If reference is a tag, resolve to digest first
		if !strings.HasPrefix(reference, "sha256:") {
			tagKey := prefixTag + repo + ":" + reference
			item, err := txn.Get([]byte(tagKey))
			if err == badger.ErrKeyNotFound {
				return ErrManifestNotFound
			} else if err != nil {
				return err
			}
			err = item.Value(func(val []byte) error {
				d = digest.Digest(val)
				return nil
			})
			if err != nil {
				return err
			}
		} else {
			d = digest.Digest(reference)
		}

		// Get manifest metadata
		key := prefixManifest + repo + ":" + string(d)
		item, err := txn.Get([]byte(key))
		if err == badger.ErrKeyNotFound {
			return ErrManifestNotFound
		} else if err != nil {
			return err
		}

		return item.Value(func(val []byte) error {
			return json.Unmarshal(val, &meta)
		})
	})

	if err != nil {
		return nil, err
	}
	return &meta, nil
}

// DeleteManifest removes a manifest
func (ms *MetadataStore) DeleteManifest(repo, reference string) error {
	return ms.db.Update(func(txn *badger.Txn) error {
		var d digest.Digest

		// If reference is a tag, resolve and delete tag
		if !strings.HasPrefix(reference, "sha256:") {
			tagKey := prefixTag + repo + ":" + reference
			item, err := txn.Get([]byte(tagKey))
			if err == badger.ErrKeyNotFound {
				return ErrManifestNotFound
			} else if err != nil {
				return err
			}
			item.Value(func(val []byte) error {
				d = digest.Digest(val)
				return nil
			})
			// Delete the tag
			if err := txn.Delete([]byte(tagKey)); err != nil {
				return err
			}
		} else {
			d = digest.Digest(reference)
		}

		// Delete manifest metadata
		key := prefixManifest + repo + ":" + string(d)
		return txn.Delete([]byte(key))
	})
}

// ListTags returns all tags for a repository
func (ms *MetadataStore) ListTags(repo string) ([]string, error) {
	var tags []string
	prefix := []byte(prefixTag + repo + ":")

	err := ms.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			key := it.Item().Key()
			tag := string(key[len(prefix):])
			tags = append(tags, tag)
		}
		return nil
	})

	return tags, err
}

// ListRepositories returns all repository names
func (ms *MetadataStore) ListRepositories() ([]string, error) {
	var repos []string
	prefix := []byte(prefixRepo)

	err := ms.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			key := it.Item().Key()
			repo := string(key[len(prefix):])
			repos = append(repos, repo)
		}
		return nil
	})

	return repos, err
}

// GetRepoMeta retrieves metadata for a repository
func (ms *MetadataStore) GetRepoMeta(repo string) (*RepoMeta, error) {
	var meta RepoMeta
	err := ms.db.View(func(txn *badger.Txn) error {
		key := prefixRepo + repo
		item, err := txn.Get([]byte(key))
		if err == badger.ErrKeyNotFound {
			return fmt.Errorf("repository not found: %s", repo)
		}
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			return json.Unmarshal(val, &meta)
		})
	})
	if err != nil {
		return nil, err
	}
	return &meta, nil
}

// LinkBlobToRepo records that a blob is used by a repository
func (ms *MetadataStore) LinkBlobToRepo(d digest.Digest, repo string) error {
	return ms.db.Update(func(txn *badger.Txn) error {
		// Store both directions for efficient lookups
		brKey := prefixBlobRepo + string(d) + ":" + repo
		rbKey := prefixRepoBlob + repo + ":" + string(d)

		if err := txn.Set([]byte(brKey), nil); err != nil {
			return err
		}
		return txn.Set([]byte(rbKey), nil)
	})
}

// UnlinkBlobFromRepo removes the link between a blob and repository
func (ms *MetadataStore) UnlinkBlobFromRepo(d digest.Digest, repo string) error {
	return ms.db.Update(func(txn *badger.Txn) error {
		brKey := prefixBlobRepo + string(d) + ":" + repo
		rbKey := prefixRepoBlob + repo + ":" + string(d)

		txn.Delete([]byte(brKey))
		txn.Delete([]byte(rbKey))
		return nil
	})
}

// GetBlobRepos returns all repositories that reference a blob
func (ms *MetadataStore) GetBlobRepos(d digest.Digest) ([]string, error) {
	var repos []string
	prefix := []byte(prefixBlobRepo + string(d) + ":")

	err := ms.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			key := it.Item().Key()
			repo := string(key[len(prefix):])
			repos = append(repos, repo)
		}
		return nil
	})

	return repos, err
}

// GetRepoBlobs returns all blob digests referenced by a repository
func (ms *MetadataStore) GetRepoBlobs(repo string) ([]digest.Digest, error) {
	var digests []digest.Digest
	prefix := []byte(prefixRepoBlob + repo + ":")

	err := ms.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			key := it.Item().Key()
			d := digest.Digest(key[len(prefix):])
			digests = append(digests, d)
		}
		return nil
	})

	return digests, err
}

// HasBlob checks if a blob exists for a repository
func (ms *MetadataStore) HasBlob(repo string, d digest.Digest) bool {
	key := prefixRepoBlob + repo + ":" + string(d)
	err := ms.db.View(func(txn *badger.Txn) error {
		_, err := txn.Get([]byte(key))
		return err
	})
	return err == nil
}

// PutRaw stores arbitrary bytes under a key
func (ms *MetadataStore) PutRaw(key string, data []byte) error {
	return ms.db.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte(key), data)
	})
}

// GetRaw retrieves arbitrary bytes by key
func (ms *MetadataStore) GetRaw(key string) ([]byte, error) {
	var val []byte
	err := ms.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(key))
		if err != nil {
			return err
		}
		return item.Value(func(v []byte) error {
			val = append([]byte{}, v...)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return val, nil
}

// ScanPrefix returns all key-value pairs with the given prefix
func (ms *MetadataStore) ScanPrefix(prefix string) (map[string][]byte, error) {
	result := make(map[string][]byte)
	pfx := []byte(prefix)

	err := ms.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 100
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(pfx); it.ValidForPrefix(pfx); it.Next() {
			item := it.Item()
			key := string(item.Key())
			err := item.Value(func(v []byte) error {
				result[key] = append([]byte{}, v...)
				return nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	})

	return result, err
}

// ScanPrefixOrdered returns values in key-sorted order for the given prefix.
// If limit > 0, at most limit values are returned.
func (ms *MetadataStore) ScanPrefixOrdered(prefix string, limit int) ([][]byte, error) {
	var result [][]byte
	pfx := []byte(prefix)

	err := ms.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 100
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(pfx); it.ValidForPrefix(pfx); it.Next() {
			item := it.Item()
			err := item.Value(func(v []byte) error {
				result = append(result, append([]byte{}, v...))
				return nil
			})
			if err != nil {
				return err
			}
			if limit > 0 && len(result) >= limit {
				break
			}
		}
		return nil
	})

	return result, err
}

// Errors
var (
	ErrManifestNotFound = fmt.Errorf("manifest not found")
)
