package releases

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gwest/fastregistry/config"
	"github.com/gwest/fastregistry/internal/events"
	"github.com/gwest/fastregistry/internal/storage"
)

const releaseKeyPrefix = "rel:"

// Manager orchestrates release discovery, cloning, and extraction
type Manager struct {
	cfg        config.ReleasesConfig
	discovery  *Discovery
	cloner     *Cloner
	extractor  *Extractor
	isoGen     *ISOGenerator
	metadata   *storage.MetadataStore
	eventStore *events.Store

	mu       sync.RWMutex
	ctx      context.Context
	cancel   context.CancelFunc
	stopOnce sync.Once

	logLines            []string
	logMu               sync.RWMutex
	initialDiscoveredAt time.Time
}

// NewManager creates a new release manager
func NewManager(cfg config.ReleasesConfig, blobs *storage.BlobStore, metadata *storage.MetadataStore) *Manager {
	ctx, cancel := context.WithCancel(context.Background())

	cloner := NewCloner(cfg.Upstream, cfg.Repository, cfg.LocalRepo, cfg.PullSecret, blobs, metadata)

	m := &Manager{
		cfg:       cfg,
		discovery: NewDiscovery(cfg.Upstream, cfg.Repository),
		cloner:    cloner,
		extractor: NewExtractor(blobs, metadata, cloner, cfg.ArtifactPath, cfg.LocalRepo),
		isoGen:    NewISOGenerator(cfg.ArtifactPath),
		metadata:  metadata,
		ctx:       ctx,
		cancel:    cancel,
	}
	m.discovery.logFunc = m.logf
	return m
}

// SetEventStore sets the event store for recording events.
func (m *Manager) SetEventStore(es *events.Store) {
	m.eventStore = es
}

// GetAllProgress returns all active clone progress entries.
func (m *Manager) GetAllProgress() []CloneProgress {
	return m.cloner.GetAllProgress()
}

// Start begins background discovery if configured
func (m *Manager) Start() {
	if !m.cfg.AutoDiscover {
		return
	}

	go m.discoveryLoop()
}

// Stop halts background operations
func (m *Manager) Stop() {
	m.stopOnce.Do(func() {
		m.cancel()
	})
}

// logf writes a timestamped line to the in-memory log buffer and to the standard logger.
func (m *Manager) logf(format string, args ...interface{}) {
	line := time.Now().Format("15:04:05") + " " + fmt.Sprintf(format, args...)
	log.Printf(format, args...)

	m.logMu.Lock()
	m.logLines = append(m.logLines, line)
	if len(m.logLines) > 500 {
		m.logLines = m.logLines[len(m.logLines)-500:]
	}
	m.logMu.Unlock()
}

// GetDiscoveryLog returns a copy of the in-memory discovery log.
func (m *Manager) GetDiscoveryLog() []string {
	m.logMu.RLock()
	defer m.logMu.RUnlock()
	out := make([]string, len(m.logLines))
	copy(out, m.logLines)
	return out
}

func (m *Manager) discoveryLoop() {
	// Run immediately on start
	if err := m.DiscoverUpstream(m.ctx); err != nil {
		m.logf("Initial release discovery failed: %v", err)
	}

	// Parse schedule - simple interval based on cron-like schedule
	// For now, default to 6 hours
	interval := 6 * time.Hour

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			if err := m.DiscoverUpstream(m.ctx); err != nil {
				m.logf("Release discovery failed: %v", err)
			}
		}
	}
}

// DiscoverAsync starts discovery in background using the manager's long-lived context.
// Returns immediately so HTTP handlers don't time out.
func (m *Manager) DiscoverAsync() {
	go func() {
		if err := m.DiscoverUpstream(m.ctx); err != nil {
			m.logf("Background discovery failed: %v", err)
		}
	}()
}

// DiscoverUpstream queries upstream for available releases and persists new ones
func (m *Manager) DiscoverUpstream(ctx context.Context) error {
	m.logf("Discovery starting: querying upstream %s/%s", m.cfg.Upstream, m.cfg.Repository)

	upstream, err := m.discovery.ListUpstreamVersions(ctx, m.cfg.Architectures)
	if err != nil {
		return fmt.Errorf("listing upstream versions: %w", err)
	}

	m.logf("Discovery: received %d tags from upstream, processing...", len(upstream))

	newCount := 0
	for _, ur := range upstream {
		key := releaseKeyPrefix + ur.Tag

		// Check if we already know about this version+arch
		if _, err := m.metadata.GetRaw(key); err == nil {
			continue
		}

		rel := &Release{
			Version:        ur.Version,
			Architecture:   ur.Architecture,
			Tag:            ur.Tag,
			State:          StateAvailable,
			UpstreamDigest: ur.Digest,
			DiscoveredAt:   time.Now(),
		}

		data, err := json.Marshal(rel)
		if err != nil {
			continue
		}

		if err := m.metadata.PutRaw(key, data); err != nil {
			m.logf("Failed to persist release %s: %v", ur.Tag, err)
		} else {
			newCount++
		}
	}

	m.logf("Discovery complete: found %d upstream releases (%d new)", len(upstream), newCount)

	if m.eventStore != nil {
		m.eventStore.RecordEvent(events.EventDiscoveryRun, events.SeverityInfo, "",
			fmt.Sprintf("Found %d upstream releases (%d new)", len(upstream), newCount),
			map[string]string{
				"total": fmt.Sprintf("%d", len(upstream)),
				"new":   fmt.Sprintf("%d", newCount),
			})
	}

	if m.initialDiscoveredAt.IsZero() {
		m.initialDiscoveredAt = time.Now()
		m.logf("Initial discovery baseline set — News tab will show future discoveries only")
	}

	return nil
}

// CloneRelease starts an async clone + extract operation.
// If the release is not yet registered (e.g. called directly via API),
// it auto-discovers and registers it from upstream first.
func (m *Manager) CloneRelease(ctx context.Context, version string) error {
	rel, err := m.getRelease(version)
	if err != nil {
		// Not registered yet — try to auto-register from upstream
		rel, err = m.autoRegister(ctx, version)
		if err != nil {
			return fmt.Errorf("release not found and auto-register failed: %w", err)
		}
	}

	if rel.State == StateCloning || rel.State == StateExtracting {
		return fmt.Errorf("release %s is already being processed", version)
	}

	// Update state to cloning
	rel.State = StateCloning
	rel.Error = ""
	if err := m.saveRelease(rel); err != nil {
		return err
	}

	// Use canonical version (not tag) so Clone/Extract construct tags correctly
	ver := rel.Version
	arch := rel.Architecture
	if arch == "" {
		arch = m.cfg.Architectures[0]
	}

	// Run async
	go func() {
		cloneStart := time.Now()

		if m.eventStore != nil {
			m.eventStore.RecordEvent(events.EventReleaseCloneStart, events.SeverityInfo, ver,
				fmt.Sprintf("Cloning release %s (%s)", ver, arch), nil)
		}

		// Clone
		progress, err := m.cloner.Clone(m.ctx, ver, arch)
		if err != nil {
			log.Printf("Clone failed for %s: %v", ver, err)
			rel.State = StateFailed
			rel.Error = err.Error()
			m.saveRelease(rel)
			if m.eventStore != nil {
				m.eventStore.RecordEvent(events.EventReleaseFailed, events.SeverityError, ver,
					fmt.Sprintf("Clone failed: %v", err), nil)
				m.eventStore.SaveCloneHistory(events.CloneHistoryEntry{
					Version: ver, Phase: "clone", Error: err.Error(),
					StartedAt: cloneStart, CompletedAt: time.Now(),
					Duration: time.Since(cloneStart).Round(time.Second).String(),
					Success:  false,
				})
			}
			return
		}

		rel.State = StateExtracting
		rel.ClonedAt = time.Now()
		m.saveRelease(rel)

		if m.eventStore != nil {
			m.eventStore.RecordEvent(events.EventReleaseCloned, events.SeveritySuccess, ver,
				fmt.Sprintf("Cloned %d blobs (%d bytes)", progress.TotalBlobs, progress.TotalBytes), nil)
		}

		// Extract
		artifacts, err := m.extractor.Extract(m.ctx, ver, arch)
		if err != nil {
			log.Printf("Extraction failed for %s: %v", ver, err)
			rel.State = StateCloned
			rel.Error = "extraction: " + err.Error()
			m.saveRelease(rel)
			if m.eventStore != nil {
				m.eventStore.RecordEvent(events.EventReleaseFailed, events.SeverityError, ver,
					fmt.Sprintf("Extraction failed: %v", err), nil)
				m.eventStore.SaveCloneHistory(events.CloneHistoryEntry{
					Version: ver, Phase: "extract", Error: err.Error(),
					StartedAt: cloneStart, CompletedAt: time.Now(),
					Duration:   time.Since(cloneStart).Round(time.Second).String(),
					TotalBlobs: progress.TotalBlobs, TotalBytes: progress.TotalBytes,
					Success: false,
				})
			}
			return
		}

		rel.State = StateReady
		rel.Artifacts = artifacts
		rel.Error = ""
		m.saveRelease(rel)

		log.Printf("Release %s is ready with %d artifacts", ver, len(artifacts))

		if m.eventStore != nil {
			m.eventStore.RecordEvent(events.EventReleaseReady, events.SeveritySuccess, ver,
				fmt.Sprintf("Release ready with %d artifacts", len(artifacts)), nil)
			m.eventStore.SaveCloneHistory(events.CloneHistoryEntry{
				Version: ver, Phase: "complete",
				StartedAt: cloneStart, CompletedAt: time.Now(),
				Duration:   time.Since(cloneStart).Round(time.Second).String(),
				TotalBlobs: progress.TotalBlobs, TotalBytes: progress.TotalBytes,
				Success: true,
			})
		}
	}()

	return nil
}

// ResetState resets a stuck release back to cloned state.
func (m *Manager) ResetState(version string) error {
	rel, err := m.getRelease(version)
	if err != nil {
		return fmt.Errorf("release not found: %w", err)
	}

	if rel.State == StateCloning || rel.State == StateExtracting {
		rel.State = StateCloned
		rel.Error = "reset by admin"
		return m.saveRelease(rel)
	}
	return nil
}

// ReExtract triggers extraction for a release that has already been cloned.
func (m *Manager) ReExtract(version string) error {
	rel, err := m.getRelease(version)
	if err != nil {
		return fmt.Errorf("release not found: %w", err)
	}

	if rel.State == StateCloning || rel.State == StateExtracting {
		return fmt.Errorf("release %s is currently being processed", version)
	}

	ver := rel.Version
	arch := rel.Architecture
	if arch == "" && len(m.cfg.Architectures) > 0 {
		arch = m.cfg.Architectures[0]
	}

	rel.State = StateExtracting
	rel.Error = ""
	m.saveRelease(rel)

	go func() {
		artifacts, err := m.extractor.Extract(m.ctx, ver, arch)
		if err != nil {
			log.Printf("Re-extraction failed for %s: %v", ver, err)
			rel.State = StateCloned
			rel.Error = "extraction: " + err.Error()
			m.saveRelease(rel)
			if m.eventStore != nil {
				m.eventStore.RecordEvent(events.EventReleaseFailed, events.SeverityError, ver,
					fmt.Sprintf("Re-extraction failed: %v", err), nil)
			}
			return
		}

		rel.State = StateReady
		rel.Artifacts = artifacts
		rel.Error = ""
		m.saveRelease(rel)
		log.Printf("Re-extraction complete for %s: %d artifacts", ver, len(artifacts))

		if m.eventStore != nil {
			m.eventStore.RecordEvent(events.EventReleaseReady, events.SeveritySuccess, ver,
				fmt.Sprintf("Re-extraction complete with %d artifacts", len(artifacts)), nil)
		}
	}()

	return nil
}

// autoRegister discovers a single version from upstream and registers it.
// Accepts version ("4.18.10"), tag ("4.18.10-x86_64"), or partial ("4.18").
func (m *Manager) autoRegister(ctx context.Context, version string) (*Release, error) {
	// Parse version and arch from input
	ver := version
	arch := ""
	if matches := versionTagRe.FindStringSubmatch(version); matches != nil {
		ver = matches[1]
		arch = matches[2]
	}
	if arch == "" && len(m.cfg.Architectures) > 0 {
		arch = m.cfg.Architectures[0]
	}

	tag := ver + "-" + arch

	// Quick check: look up this specific tag from upstream
	baseURL := fmt.Sprintf("https://%s/api/v1/repository/%s/tag/?filter_tag_name=like:%s&limit=10",
		m.cfg.Upstream, m.cfg.Repository, ver)

	tags, err := m.discovery.fetchAllTags(ctx, baseURL)
	if err != nil {
		return nil, fmt.Errorf("querying upstream for %s: %w", ver, err)
	}

	var found *quayTag
	for i, t := range tags {
		if t.Name == tag {
			found = &tags[i]
			break
		}
	}
	if found == nil {
		return nil, fmt.Errorf("tag %s not found upstream", tag)
	}

	rel := &Release{
		Version:        ver,
		Architecture:   arch,
		Tag:            tag,
		State:          StateAvailable,
		UpstreamDigest: found.ManifestDigest,
		DiscoveredAt:   time.Now(),
	}

	if err := m.saveRelease(rel); err != nil {
		return nil, fmt.Errorf("saving release: %w", err)
	}

	m.logf("Auto-registered release %s from upstream", tag)
	return rel, nil
}

// ListExtractable returns version tags for releases that have been cloned
// but have no artifacts (or are in a failed state from a prior extraction).
func (m *Manager) ListExtractable() ([]string, error) {
	all, err := m.ListReleases()
	if err != nil {
		return nil, err
	}
	var versions []string
	for _, rel := range all {
		switch rel.State {
		case StateCloned, StateFailed, StateReady:
			// cloned but no artifacts, failed extraction, or ready but user wants re-extract
			if rel.State == StateReady && len(rel.Artifacts) > 0 {
				// already has artifacts — include anyway so user can re-extract
			}
			tag := rel.Tag
			if tag == "" {
				tag = rel.Version
			}
			versions = append(versions, tag)
		}
	}
	return versions, nil
}

// ListReleases returns all known releases
func (m *Manager) ListReleases() ([]Release, error) {
	data, err := m.metadata.ScanPrefix(releaseKeyPrefix)
	if err != nil {
		return nil, err
	}

	var releases []Release
	for _, v := range data {
		var rel Release
		if err := json.Unmarshal(v, &rel); err != nil {
			continue
		}
		releases = append(releases, rel)
	}

	return releases, nil
}

// ListByState returns releases filtered by state
func (m *Manager) ListByState(state ReleaseState) ([]Release, error) {
	all, err := m.ListReleases()
	if err != nil {
		return nil, err
	}

	var filtered []Release
	for _, rel := range all {
		if rel.State == state {
			filtered = append(filtered, rel)
		}
	}
	return filtered, nil
}

// GetRelease returns a single release by version
func (m *Manager) GetRelease(version string) (*Release, error) {
	return m.getRelease(version)
}

// GetProgress returns the current clone progress for a version
func (m *Manager) GetProgress(version string) *CloneProgress {
	return m.cloner.GetProgress(version)
}

// ArtifactPath returns the configured artifact path
func (m *Manager) ArtifactPath() string {
	return m.cfg.ArtifactPath
}

// LocalRepo returns the configured local repository name
func (m *Manager) LocalRepo() string {
	return m.cfg.LocalRepo
}

// GetReleaseImage returns the full release image reference for a version
func (m *Manager) GetReleaseImage(version string) string {
	// version might be "4.21.0" or "4.21.0-x86_64"
	tag := version
	if !strings.Contains(version, "-") {
		// Add default architecture
		if len(m.cfg.Architectures) > 0 {
			tag = version + "-" + m.cfg.Architectures[0]
		}
	}
	// Return local registry reference
	return fmt.Sprintf("fastregistry.gw.lo:5000/%s:%s", m.cfg.LocalRepo, tag)
}

// GenerateISO creates an agent ISO with embedded ignition.
// Returns the install UUID and URL path.
func (m *Manager) GenerateISO(version string, ignition []byte) (string, string, error) {
	rel, err := m.getRelease(version)
	if err != nil {
		return "", "", fmt.Errorf("release not found: %w", err)
	}

	if rel.State != StateReady {
		return "", "", fmt.Errorf("release %s is not ready (state: %s)", version, rel.State)
	}

	// Check that coreos.iso exists
	hasISO := false
	for _, a := range rel.Artifacts {
		if a.Name == "coreos.iso" {
			hasISO = true
			break
		}
	}
	if !hasISO {
		return "", "", fmt.Errorf("release %s does not have coreos.iso artifact", version)
	}

	id, path, err := m.isoGen.GenerateAgentISO(rel.Version, ignition)
	if err != nil {
		return "", "", err
	}

	urlPath := "/files/installs/" + id + "/agent.iso"
	log.Printf("Generated agent ISO for %s: %s (%s)", version, id, path)

	if m.eventStore != nil {
		m.eventStore.RecordEvent(events.EventDownloadArtifact, events.SeverityInfo, rel.Version,
			fmt.Sprintf("Generated agent ISO: %s", id),
			map[string]string{"install_id": id})
	}

	return id, urlPath, nil
}

// InstallsPath returns the base path for generated install ISOs.
func (m *Manager) InstallsPath() string {
	return m.isoGen.InstallsPath()
}

// extractMajorMinor returns the "X.Y" portion of a semver string
func extractMajorMinor(version string) string {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return version
}

// ListGrouped returns releases grouped by major.minor, sorted descending
func (m *Manager) ListGrouped() ([]MajorMinorGroup, error) {
	all, err := m.ListReleases()
	if err != nil {
		return nil, err
	}

	// Sort all releases by version descending
	sort.Slice(all, func(i, j int) bool {
		return compareSemver(all[i].Version, all[j].Version) > 0
	})

	// Group by major.minor
	groupMap := make(map[string]*MajorMinorGroup)
	var groupOrder []string

	for _, rel := range all {
		mm := extractMajorMinor(rel.Version)
		g, ok := groupMap[mm]
		if !ok {
			g = &MajorMinorGroup{MajorMinor: mm}
			groupMap[mm] = g
			groupOrder = append(groupOrder, mm)
		}
		g.Releases = append(g.Releases, rel)
		g.TotalCount++
		switch rel.State {
		case StateReady:
			g.ReadyCount++
		case StateAvailable:
			g.AvailableCount++
		case StateCloning, StateExtracting:
			g.CloningCount++
		}
	}

	// Build result in order (already sorted since all was sorted descending)
	var groups []MajorMinorGroup
	for _, mm := range groupOrder {
		g := groupMap[mm]
		if len(g.Releases) > 0 {
			g.LatestVersion = g.Releases[0].Version
		}
		groups = append(groups, *g)
	}

	return groups, nil
}

// ListRecent returns the n most recently discovered releases found after the initial discovery.
func (m *Manager) ListRecent(n int) ([]Release, error) {
	if m.initialDiscoveredAt.IsZero() {
		return nil, nil
	}

	all, err := m.ListReleases()
	if err != nil {
		return nil, err
	}

	// Only include releases discovered after the initial run
	var filtered []Release
	for _, rel := range all {
		if rel.DiscoveredAt.After(m.initialDiscoveredAt) {
			filtered = append(filtered, rel)
		}
	}

	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].DiscoveredAt.After(filtered[j].DiscoveredAt)
	})

	if len(filtered) > n {
		filtered = filtered[:n]
	}
	return filtered, nil
}

func (m *Manager) getRelease(version string) (*Release, error) {
	// Try tag-based key first (new format: rel:4.17.0-x86_64)
	// Fall back to version-only key (old format: rel:4.17.0)
	key := releaseKeyPrefix + version
	data, err := m.metadata.GetRaw(key)
	if err != nil {
		// Search all releases for this version
		all, listErr := m.ListReleases()
		if listErr != nil {
			return nil, err
		}
		for _, rel := range all {
			if rel.Version == version || rel.Tag == version {
				return &rel, nil
			}
		}
		return nil, err
	}

	var rel Release
	if err := json.Unmarshal(data, &rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func (m *Manager) saveRelease(rel *Release) error {
	key := releaseKeyPrefix + rel.Tag
	if rel.Tag == "" {
		key = releaseKeyPrefix + rel.Version
	}
	data, err := json.Marshal(rel)
	if err != nil {
		return err
	}
	return m.metadata.PutRaw(key, data)
}

// CopyArtifact copies an artifact to a remote destination using SCP.
// Destination format: user@host:/path/ or just /local/path/
func (m *Manager) CopyArtifact(version, artifact, destination string) error {
	rel, err := m.getRelease(version)
	if err != nil {
		return fmt.Errorf("release not found: %w", err)
	}

	if rel.State != StateReady {
		return fmt.Errorf("release %s is not ready (state: %s)", version, rel.State)
	}

	// Find the artifact
	var found *Artifact
	for i, a := range rel.Artifacts {
		if a.Name == artifact {
			found = &rel.Artifacts[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("artifact %q not found in release %s", artifact, version)
	}

	// Build source path
	srcPath := filepath.Join(m.cfg.ArtifactPath, rel.Version, artifact)

	// Determine if remote (contains @) or local copy
	if strings.Contains(destination, "@") {
		// Remote copy via SCP
		cmd := exec.Command("scp", "-o", "StrictHostKeyChecking=no", srcPath, destination)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("scp failed: %v: %s", err, string(output))
		}
		log.Printf("Copied %s to %s", artifact, destination)
	} else {
		// Local copy
		cmd := exec.Command("cp", srcPath, destination)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("cp failed: %v: %s", err, string(output))
		}
		log.Printf("Copied %s to %s", artifact, destination)
	}

	if m.eventStore != nil {
		m.eventStore.RecordEvent(events.EventDownloadArtifact, events.SeverityInfo, rel.Version,
			fmt.Sprintf("Copied %s to %s", artifact, destination),
			map[string]string{"artifact": artifact, "destination": destination})
	}

	return nil
}
