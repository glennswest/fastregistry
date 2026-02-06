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

// ListCloneHistory returns all clone history entries.
func (s *Store) ListCloneHistory() ([]CloneHistoryEntry, error) {
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
