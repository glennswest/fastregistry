package events

import "time"

// Severity classifies the importance of an event.
type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
	SeveritySuccess Severity = "success"
)

// EventType identifies what happened.
type EventType string

const (
	EventReleaseDiscovered EventType = "release.discovered"
	EventReleaseCloneStart EventType = "release.clone_start"
	EventReleaseCloned     EventType = "release.cloned"
	EventReleaseReady      EventType = "release.ready"
	EventReleaseFailed     EventType = "release.failed"
	EventDownloadArtifact  EventType = "download.artifact"
	EventDiscoveryRun      EventType = "discovery.run"
)

// Event is a single recorded event.
type Event struct {
	ID       string            `json:"id"`
	Type     EventType         `json:"type"`
	Severity Severity          `json:"severity"`
	Time     time.Time         `json:"time"`
	Version  string            `json:"version,omitempty"`
	Details  string            `json:"details"`
	Extra    map[string]string `json:"extra,omitempty"`
}

// CloneHistoryEntry records a completed (or failed) clone operation.
type CloneHistoryEntry struct {
	Version     string    `json:"version"`
	Phase       string    `json:"phase"`
	Duration    string    `json:"duration"`
	Error       string    `json:"error,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	TotalBlobs  int       `json:"total_blobs"`
	TotalBytes  int64     `json:"total_bytes"`
	Success     bool      `json:"success"`
}

// DownloadRecord captures a single file download.
type DownloadRecord struct {
	Time      time.Time     `json:"time"`
	Version   string        `json:"version"`
	Artifact  string        `json:"artifact"`
	ClientIP  string        `json:"client_ip"`
	UserAgent string        `json:"user_agent"`
	Size      int64         `json:"size"`
	Duration  time.Duration `json:"duration"`
}

// DownloadCounter holds aggregate download statistics.
type DownloadCounter struct {
	TotalDownloads int64            `json:"total_downloads"`
	TotalBytes     int64            `json:"total_bytes"`
	ByVersion      map[string]int64 `json:"by_version"`
	ByArtifact     map[string]int64 `json:"by_artifact"`
	LastDownload   time.Time        `json:"last_download"`
}
