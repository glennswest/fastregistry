package releases

import "time"

// ReleaseState represents the lifecycle state of a release
type ReleaseState string

const (
	StateAvailable  ReleaseState = "available"
	StateCloning    ReleaseState = "cloning"
	StateCloned     ReleaseState = "cloned"
	StateExtracting ReleaseState = "extracting"
	StateReady      ReleaseState = "ready"
	StateFailed     ReleaseState = "failed"
)

// Release represents an OpenShift release version
type Release struct {
	Version        string       `json:"version"`
	Architecture   string       `json:"architecture"`
	Tag            string       `json:"tag"`
	State          ReleaseState `json:"state"`
	UpstreamDigest string       `json:"upstream_digest,omitempty"`
	LocalDigest    string       `json:"local_digest,omitempty"`
	DiscoveredAt   time.Time    `json:"discovered_at"`
	ClonedAt       time.Time    `json:"cloned_at,omitempty"`
	VerifiedAt     time.Time    `json:"verified_at,omitempty"`
	Size           int64        `json:"size"`
	Error          string       `json:"error,omitempty"`
	Artifacts      []Artifact   `json:"artifacts,omitempty"`

	// Verify summary attached to the release once the pipeline has run a
	// completeness check (release blobs + component manifests + component
	// blobs all present locally). Populated automatically before the state
	// transitions to "ready"; if any blob is missing, the release goes to
	// "failed" instead and Verify carries the diagnostic counts.
	Verify *VerifySummary `json:"verify,omitempty"`
}

// VerifySummary is the trimmed shape of VerifyResult that gets persisted on
// a Release so the UI can show a "verified" badge with details without
// re-walking the metadata store on every page load.
type VerifySummary struct {
	Complete               bool      `json:"complete"`
	At                     time.Time `json:"at"`
	ReleaseBlobsExpected   int       `json:"release_blobs_expected"`
	ReleaseBlobsPresent    int       `json:"release_blobs_present"`
	ComponentsExpected     int       `json:"components_expected"`
	ComponentManifestsOK   int       `json:"component_manifests_ok"`
	ComponentBlobsExpected int       `json:"component_blobs_expected"`
	ComponentBlobsPresent  int       `json:"component_blobs_present"`
}

// Artifact represents an extracted file from a release
type Artifact struct {
	Name   string `json:"name"`
	Type   string `json:"type"` // "binary", "iso", "kernel", "initramfs", "rootfs"
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

// MajorMinorGroup groups releases by their major.minor version
type MajorMinorGroup struct {
	MajorMinor     string
	Releases       []Release // sorted descending by version
	LatestVersion  string
	TotalCount     int
	ReadyCount     int
	AvailableCount int
	CloningCount   int
}

// CloneProgress tracks the progress of an active clone operation.
// Phase progression: "pulling_manifest" → "pulling_blobs" (release image
// itself) → "mirroring_components" (all image-references components) →
// "extracting" (binaries). The mirror phase is the slowest because OCP
// releases reference ~190+ component images.
type CloneProgress struct {
	Version     string  `json:"version"`
	Phase       string  `json:"phase"`
	TotalBlobs  int     `json:"total_blobs"`
	SyncedBlobs int     `json:"synced_blobs"`
	TotalBytes  int64   `json:"total_bytes"`
	SyncedBytes int64   `json:"synced_bytes"`
	PercentDone float64 `json:"percent_done"`

	// Component-mirror tracking (only meaningful during "mirroring_components")
	TotalComponents   int     `json:"total_components"`
	MirroredComponents int    `json:"mirrored_components"`
	ComponentPercent   float64 `json:"component_percent"`
	CurrentComponent   string  `json:"current_component"`
}
