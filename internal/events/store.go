package events

import (
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/gwest/fastregistry/internal/storage"
)

// Key prefixes used in the metadata store.
const (
	prefixEvent        = "ev:"  // ev:<reverseTimestamp>
	prefixCloneHistory = "ch:"  // ch:<version>
	prefixDownload     = "dl:"  // dl:<reverseTimestamp>
	prefixDLCounter    = "dlc:" // dlc:global, dlc:v:<version>, dlc:a:<artifact>
)

// Store persists events, clone history, and download records using an existing MetadataStore.
type Store struct {
	metadata *storage.MetadataStore
	mu       sync.Mutex // protects counter read-modify-write
}

// NewStore creates a new event store backed by the given metadata store.
func NewStore(metadata *storage.MetadataStore) *Store {
	return &Store{metadata: metadata}
}

// reverseTimestamp returns a string that sorts newest-first in lexicographic order.
func reverseTimestamp() string {
	return fmt.Sprintf("%019d", math.MaxInt64-time.Now().UnixNano())
}

// --- Events ---

// RecordEvent persists a new event.
func (s *Store) RecordEvent(typ EventType, sev Severity, version, details string, extra map[string]string) {
	ev := Event{
		ID:       reverseTimestamp(),
		Type:     typ,
		Severity: sev,
		Time:     time.Now(),
		Version:  version,
		Details:  details,
		Extra:    extra,
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	s.metadata.PutRaw(prefixEvent+ev.ID, data)
}

// ListEvents returns up to limit events, optionally filtered by type.
func (s *Store) ListEvents(limit int, typeFilter EventType) ([]Event, error) {
	values, err := s.metadata.ScanPrefixOrdered(prefixEvent, 0)
	if err != nil {
		return nil, err
	}

	var events []Event
	for _, v := range values {
		var ev Event
		if json.Unmarshal(v, &ev) != nil {
			continue
		}
		if typeFilter != "" && ev.Type != typeFilter {
			continue
		}
		events = append(events, ev)
		if limit > 0 && len(events) >= limit {
			break
		}
	}
	return events, nil
}

// --- Clone History ---

// SaveCloneHistory persists a clone history entry.
func (s *Store) SaveCloneHistory(entry CloneHistoryEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	key := prefixCloneHistory + entry.Version
	return s.metadata.PutRaw(key, data)
}

// ListCloneHistory returns clone history entries up to limit (0 = all).
func (s *Store) ListCloneHistory(limit int) ([]CloneHistoryEntry, error) {
	raw, err := s.metadata.ScanPrefix(prefixCloneHistory)
	if err != nil {
		return nil, err
	}

	var entries []CloneHistoryEntry
	for _, v := range raw {
		var entry CloneHistoryEntry
		if json.Unmarshal(v, &entry) != nil {
			continue
		}
		entries = append(entries, entry)
		if limit > 0 && len(entries) >= limit {
			break
		}
	}
	return entries, nil
}

// --- Download Records ---

// RecordDownload persists a download record and increments counters.
func (s *Store) RecordDownload(rec DownloadRecord) {
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	key := prefixDownload + reverseTimestamp()
	s.metadata.PutRaw(key, data)

	// Increment counters
	s.mu.Lock()
	defer s.mu.Unlock()

	stats := s.loadCounter()
	stats.TotalDownloads++
	stats.TotalBytes += rec.Size
	stats.LastDownload = rec.Time
	stats.ByVersion[rec.Version]++
	stats.ByArtifact[rec.Artifact]++
	s.saveCounter(stats)
}

// GetDownloadStats returns aggregate download statistics.
func (s *Store) GetDownloadStats() *DownloadCounter {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.loadCounter()
	return &c
}

// ListRecentDownloads returns up to limit recent download records.
func (s *Store) ListRecentDownloads(limit int) ([]DownloadRecord, error) {
	values, err := s.metadata.ScanPrefixOrdered(prefixDownload, limit)
	if err != nil {
		return nil, err
	}

	var records []DownloadRecord
	for _, v := range values {
		var rec DownloadRecord
		if json.Unmarshal(v, &rec) != nil {
			continue
		}
		records = append(records, rec)
	}
	return records, nil
}

// --- Counter helpers ---

func (s *Store) loadCounter() DownloadCounter {
	var c DownloadCounter
	data, err := s.metadata.GetRaw(prefixDLCounter + "global")
	if err == nil {
		json.Unmarshal(data, &c)
	}
	if c.ByVersion == nil {
		c.ByVersion = make(map[string]int64)
	}
	if c.ByArtifact == nil {
		c.ByArtifact = make(map[string]int64)
	}
	return c
}

func (s *Store) saveCounter(c DownloadCounter) {
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	s.metadata.PutRaw(prefixDLCounter+"global", data)
}

// --- Import methods for replication ---

// ImportEvent imports an event from another FastRegistry instance.
// It skips if an event with the same ID already exists.
func (s *Store) ImportEvent(ev Event) error {
	key := prefixEvent + ev.ID
	// Check if already exists
	if _, err := s.metadata.GetRaw(key); err == nil {
		return nil // Already exists
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return s.metadata.PutRaw(key, data)
}

// ImportCloneHistory imports a clone history entry.
// Overwrites existing entry for the same version.
func (s *Store) ImportCloneHistory(entry CloneHistoryEntry) error {
	return s.SaveCloneHistory(entry)
}

// ImportDownloadRecord imports a download record without updating counters.
// Used for replication where counters are synced separately.
func (s *Store) ImportDownloadRecord(rec DownloadRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	// Use timestamp from record to maintain ordering
	key := fmt.Sprintf("%s%019d", prefixDownload, math.MaxInt64-rec.Time.UnixNano())
	return s.metadata.PutRaw(key, data)
}

// ImportDownloadStats imports download statistics, merging with existing.
func (s *Store) ImportDownloadStats(stats *DownloadCounter) {
	if stats == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.loadCounter()

	// Take the larger values (in case of split-brain)
	if stats.TotalDownloads > current.TotalDownloads {
		current.TotalDownloads = stats.TotalDownloads
	}
	if stats.TotalBytes > current.TotalBytes {
		current.TotalBytes = stats.TotalBytes
	}
	if stats.LastDownload.After(current.LastDownload) {
		current.LastDownload = stats.LastDownload
	}

	// Merge per-version/artifact counts (take max)
	for k, v := range stats.ByVersion {
		if v > current.ByVersion[k] {
			current.ByVersion[k] = v
		}
	}
	for k, v := range stats.ByArtifact {
		if v > current.ByArtifact[k] {
			current.ByArtifact[k] = v
		}
	}

	s.saveCounter(current)
}
