package gc

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gwest/fastregistry/internal/storage"
	"github.com/gwest/fastregistry/pkg/digest"
)

// Collector performs garbage collection on the registry
type Collector struct {
	blobs      *storage.BlobStore
	metadata   *storage.MetadataStore
	storagePath string
	config     Config
	running    bool
	mu         sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
}

// Config holds garbage collection configuration
type Config struct {
	Enabled    bool
	Schedule   string // Cron-like schedule
	KeepRecent int    // Keep N most recent tags per repo
	DryRun     bool   // Don't actually delete
}

// Stats holds garbage collection statistics
type Stats struct {
	StartTime       time.Time
	EndTime         time.Time
	BlobsScanned    int
	BlobsDeleted    int
	BytesFreed      int64
	ManifestsKept   int
	ManifestsDeleted int
	Errors          []string
}

// NewCollector creates a new garbage collector
func NewCollector(blobs *storage.BlobStore, metadata *storage.MetadataStore, storagePath string, cfg Config) *Collector {
	ctx, cancel := context.WithCancel(context.Background())

	return &Collector{
		blobs:       blobs,
		metadata:    metadata,
		storagePath: storagePath,
		config:      cfg,
		ctx:         ctx,
		cancel:      cancel,
	}
}

// Start starts the scheduled garbage collector
func (c *Collector) Start() {
	if !c.config.Enabled {
		return
	}

	go c.scheduleLoop()
}

// Stop stops the garbage collector
func (c *Collector) Stop() {
	c.cancel()
}

func (c *Collector) scheduleLoop() {
	// Parse schedule (simplified cron: "0 2 * * *" = 2 AM daily)
	hour := 2 // Default 2 AM

	parts := strings.Fields(c.config.Schedule)
	if len(parts) >= 2 {
		fmt.Sscanf(parts[1], "%d", &hour)
	}

	for {
		// Calculate next run time
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
		if next.Before(now) {
			next = next.Add(24 * time.Hour)
		}

		select {
		case <-c.ctx.Done():
			return
		case <-time.After(time.Until(next)):
			stats := c.Run()
			log.Printf("GC completed: freed %d bytes, deleted %d blobs", stats.BytesFreed, stats.BlobsDeleted)
		}
	}
}

// Run performs garbage collection
func (c *Collector) Run() *Stats {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return nil
	}
	c.running = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()

	stats := &Stats{
		StartTime: time.Now(),
	}

	log.Printf("Starting garbage collection (dry_run=%v)", c.config.DryRun)

	// Phase 1: Mark - collect all referenced blobs
	referenced := c.markPhase(stats)

	// Phase 2: Sweep - delete unreferenced blobs
	c.sweepPhase(stats, referenced)

	stats.EndTime = time.Now()

	log.Printf("GC complete in %v: scanned %d blobs, deleted %d, freed %d bytes",
		stats.EndTime.Sub(stats.StartTime),
		stats.BlobsScanned,
		stats.BlobsDeleted,
		stats.BytesFreed,
	)

	return stats
}

// markPhase collects all referenced blob digests
func (c *Collector) markPhase(stats *Stats) map[string]bool {
	referenced := make(map[string]bool)

	// Get all repositories
	repos, err := c.metadata.ListRepositories()
	if err != nil {
		stats.Errors = append(stats.Errors, "listing repos: "+err.Error())
		return referenced
	}

	for _, repo := range repos {
		c.markRepo(repo, stats, referenced)
	}

	return referenced
}

func (c *Collector) markRepo(repo string, stats *Stats, referenced map[string]bool) {
	// Get all tags
	tags, err := c.metadata.ListTags(repo)
	if err != nil {
		stats.Errors = append(stats.Errors, "listing tags for "+repo+": "+err.Error())
		return
	}

	// Sort tags by time (if we need to keep only recent ones)
	type tagInfo struct {
		name   string
		digest string
		time   time.Time
	}

	var tagInfos []tagInfo
	for _, tag := range tags {
		meta, err := c.metadata.GetManifest(repo, tag)
		if err != nil {
			continue
		}

		tagInfos = append(tagInfos, tagInfo{
			name:   tag,
			digest: string(meta.Digest),
			time:   meta.CreatedAt,
		})
	}

	// Sort by time, newest first
	sort.Slice(tagInfos, func(i, j int) bool {
		return tagInfos[i].time.After(tagInfos[j].time)
	})

	// Determine which tags to keep
	keepCount := len(tagInfos)
	if c.config.KeepRecent > 0 && keepCount > c.config.KeepRecent {
		keepCount = c.config.KeepRecent
	}

	// Mark referenced blobs for kept tags
	for i := 0; i < keepCount; i++ {
		c.markManifest(repo, tagInfos[i].digest, stats, referenced)
		stats.ManifestsKept++
	}

	// Optionally delete old tags
	for i := keepCount; i < len(tagInfos); i++ {
		if !c.config.DryRun {
			c.metadata.DeleteManifest(repo, tagInfos[i].name)
		}
		stats.ManifestsDeleted++
	}
}

func (c *Collector) markManifest(repo, dgstStr string, stats *Stats, referenced map[string]bool) {
	// Mark the manifest blob itself
	referenced[dgstStr] = true

	// Get manifest content to find referenced blobs
	dgst, err := digest.Parse(dgstStr)
	if err != nil {
		return
	}

	reader, _, err := c.blobs.Get(dgst)
	if err != nil {
		return
	}
	defer reader.Close()

	var manifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"` // For index
	}

	if err := json.NewDecoder(reader).Decode(&manifest); err != nil {
		return
	}

	// Mark config blob
	if manifest.Config.Digest != "" {
		referenced[manifest.Config.Digest] = true
	}

	// Mark layer blobs
	for _, layer := range manifest.Layers {
		referenced[layer.Digest] = true
	}

	// Mark nested manifests (for multi-arch)
	for _, m := range manifest.Manifests {
		c.markManifest(repo, m.Digest, stats, referenced)
	}
}

// sweepPhase deletes unreferenced blobs
func (c *Collector) sweepPhase(stats *Stats, referenced map[string]bool) {
	blobsDir := filepath.Join(c.storagePath, "blobs", "sha256")

	// Walk the blob directory
	filepath.Walk(blobsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		stats.BlobsScanned++

		// Get digest from path
		// Path format: blobs/sha256/ab/abcdef123...
		filename := info.Name()
		if len(filename) != 64 {
			return nil
		}

		dgstStr := "sha256:" + filename

		// Check if referenced
		if referenced[dgstStr] {
			return nil
		}

		// Delete unreferenced blob
		if !c.config.DryRun {
			if err := os.Remove(path); err != nil {
				stats.Errors = append(stats.Errors, "deleting "+dgstStr+": "+err.Error())
				return nil
			}
		}

		stats.BlobsDeleted++
		stats.BytesFreed += info.Size()

		return nil
	})
}

// IsRunning returns true if GC is currently running
func (c *Collector) IsRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

// Trigger manually triggers garbage collection
func (c *Collector) Trigger() *Stats {
	return c.Run()
}
