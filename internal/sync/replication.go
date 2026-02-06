package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gwest/fastregistry/config"
	"github.com/gwest/fastregistry/internal/events"
	"github.com/gwest/fastregistry/internal/releases"
	"github.com/gwest/fastregistry/internal/storage"
)

// ReplicationExport contains all FastRegistry metadata for export
type ReplicationExport struct {
	ExportedAt time.Time `json:"exported_at"`
	Version    string    `json:"version"`

	// Release data
	Releases []releases.Release `json:"releases,omitempty"`

	// Event data
	Events          []events.Event             `json:"events,omitempty"`
	CloneHistory    []events.CloneHistoryEntry `json:"clone_history,omitempty"`
	DownloadRecords []events.DownloadRecord    `json:"download_records,omitempty"`
	DownloadStats   *events.DownloadCounter    `json:"download_stats,omitempty"`
}

// ReplicationClient handles full FastRegistry-to-FastRegistry sync
type ReplicationClient struct {
	config     config.SyncSource
	blobs      *storage.BlobStore
	metadata   *storage.MetadataStore
	releaseMgr *releases.Manager
	eventStore *events.Store
	client     *http.Client

	// Embed registry client for OCI data sync
	*RegistryClient
}

// NewReplicationClient creates a new FastRegistry replication client
func NewReplicationClient(
	cfg config.SyncSource,
	blobs *storage.BlobStore,
	metadata *storage.MetadataStore,
	releaseMgr *releases.Manager,
	eventStore *events.Store,
) *ReplicationClient {
	return &ReplicationClient{
		config:     cfg,
		blobs:      blobs,
		metadata:   metadata,
		releaseMgr: releaseMgr,
		eventStore: eventStore,
		client: &http.Client{
			Timeout: 10 * time.Minute,
		},
		RegistryClient: NewRegistryClient(cfg, blobs, metadata),
	}
}

// SyncAll performs full replication: OCI data + FastRegistry metadata
func (r *ReplicationClient) SyncAll(ctx context.Context) error {
	log.Printf("Starting full replication from %s", r.config.URL)

	// 1. Sync OCI registry data (manifests, blobs)
	repos, err := r.ListRepositories(ctx)
	if err != nil {
		log.Printf("Warning: failed to list repositories: %v", err)
	} else {
		progress := NewProgress()
		for _, repo := range repos {
			if err := r.SyncRepository(ctx, repo, progress); err != nil {
				log.Printf("Warning: failed to sync repo %s: %v", repo, err)
			}
		}
		log.Printf("OCI sync complete: %d repos, %d tags, %d bytes",
			progress.SyncedRepos, progress.SyncedTags, progress.BytesSynced)
	}

	// 2. Sync FastRegistry metadata
	if err := r.syncMetadata(ctx); err != nil {
		return fmt.Errorf("metadata sync failed: %w", err)
	}

	log.Printf("Full replication complete")
	return nil
}

// SyncMetadata syncs FastRegistry metadata (releases, events) from upstream
func (r *ReplicationClient) SyncMetadata(ctx context.Context) error {
	return r.syncMetadata(ctx)
}

func (r *ReplicationClient) syncMetadata(ctx context.Context) error {
	// Fetch export from upstream
	url := fmt.Sprintf("%s/admin/sync/export", strings.TrimSuffix(r.config.URL, "/"))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	r.addReplicationAuth(req)

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("export request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("export returned %d: %s", resp.StatusCode, string(body))
	}

	var export ReplicationExport
	if err := json.NewDecoder(resp.Body).Decode(&export); err != nil {
		return fmt.Errorf("decoding export: %w", err)
	}

	log.Printf("Received export from %s (exported at %s)", r.config.URL, export.ExportedAt)

	// Import releases
	if r.releaseMgr != nil && len(export.Releases) > 0 {
		imported := 0
		for _, rel := range export.Releases {
			if err := r.releaseMgr.ImportRelease(rel); err != nil {
				log.Printf("Warning: failed to import release %s: %v", rel.Version, err)
				continue
			}
			imported++
		}
		log.Printf("Imported %d/%d releases", imported, len(export.Releases))
	}

	// Import events
	if r.eventStore != nil {
		if len(export.Events) > 0 {
			imported := 0
			for _, evt := range export.Events {
				if err := r.eventStore.ImportEvent(evt); err != nil {
					continue
				}
				imported++
			}
			log.Printf("Imported %d/%d events", imported, len(export.Events))
		}

		if len(export.CloneHistory) > 0 {
			for _, ch := range export.CloneHistory {
				r.eventStore.ImportCloneHistory(ch)
			}
			log.Printf("Imported %d clone history entries", len(export.CloneHistory))
		}

		if len(export.DownloadRecords) > 0 {
			for _, dr := range export.DownloadRecords {
				r.eventStore.ImportDownloadRecord(dr)
			}
			log.Printf("Imported %d download records", len(export.DownloadRecords))
		}
	}

	return nil
}

func (r *ReplicationClient) addReplicationAuth(req *http.Request) {
	if r.config.Token != "" {
		if strings.Contains(r.config.Token, ":") {
			parts := strings.SplitN(r.config.Token, ":", 2)
			req.SetBasicAuth(parts[0], parts[1])
		} else {
			req.Header.Set("Authorization", "Bearer "+r.config.Token)
		}
	}
}

// Exporter handles the export side of replication
type Exporter struct {
	releaseMgr *releases.Manager
	eventStore *events.Store
	version    string
}

// NewExporter creates a new replication exporter
func NewExporter(releaseMgr *releases.Manager, eventStore *events.Store, version string) *Exporter {
	return &Exporter{
		releaseMgr: releaseMgr,
		eventStore: eventStore,
		version:    version,
	}
}

// Export generates a full metadata export
func (e *Exporter) Export() (*ReplicationExport, error) {
	export := &ReplicationExport{
		ExportedAt: time.Now(),
		Version:    e.version,
	}

	// Export releases
	if e.releaseMgr != nil {
		export.Releases = e.releaseMgr.ListAllReleases()
	}

	// Export events
	if e.eventStore != nil {
		export.Events, _ = e.eventStore.ListEvents(1000, "")
		export.CloneHistory, _ = e.eventStore.ListCloneHistory(100)
		export.DownloadRecords, _ = e.eventStore.ListRecentDownloads(1000)
		export.DownloadStats = e.eventStore.GetDownloadStats()
	}

	return export, nil
}
