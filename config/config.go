package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the complete FastRegistry configuration
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Storage  StorageConfig  `yaml:"storage"`
	Mesh     MeshConfig     `yaml:"mesh"`
	Mirrors  []Mirror       `yaml:"mirrors"`
	Sync     SyncConfig     `yaml:"sync"`
	Auth     AuthConfig     `yaml:"auth"`
	Releases ReleasesConfig `yaml:"releases"`
	Certs    CertsConfig    `yaml:"certs"`
}

// ServerConfig holds HTTP server settings
type ServerConfig struct {
	Addr string    `yaml:"addr"`
	TLS  TLSConfig `yaml:"tls"`
}

// TLSConfig holds TLS certificate paths
type TLSConfig struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

// StorageConfig holds storage engine settings
type StorageConfig struct {
	Path        string   `yaml:"path"`
	CacheSizeMB int      `yaml:"cache_size_mb"`
	GC          GCConfig `yaml:"gc"`
}

// GCConfig holds garbage collection settings
type GCConfig struct {
	Enabled    bool   `yaml:"enabled"`
	Schedule   string `yaml:"schedule"`
	KeepRecent int    `yaml:"keep_recent"`
}

// MeshConfig holds mesh networking settings
type MeshConfig struct {
	Enabled           bool     `yaml:"enabled"`
	Bind              string   `yaml:"bind"`
	Peers             []string `yaml:"peers"`
	ReplicationFactor int      `yaml:"replication_factor"`
}

// Mirror represents an upstream registry to mirror/cache
type Mirror struct {
	Name     string        `yaml:"name"`
	Upstream string        `yaml:"upstream"`
	CacheTTL time.Duration `yaml:"cache_ttl"`
}

// SyncConfig holds registry sync settings
type SyncConfig struct {
	Sources []SyncSource `yaml:"sources"`
}

// SyncSource represents a source registry to sync from
type SyncSource struct {
	Name           string   `yaml:"name"`
	Type           string   `yaml:"type"` // "quay", "registry"
	URL            string   `yaml:"url"`
	Token          string   `yaml:"token"`
	RobotAccount   string   `yaml:"robot_account"`
	RobotToken     string   `yaml:"robot_token"`
	Organizations  []string `yaml:"organizations"`
	Repositories   []string `yaml:"repositories"`
	IncludeTags    string   `yaml:"include_tags"`
	ExcludeTags    string   `yaml:"exclude_tags"`
	SyncSignatures bool     `yaml:"sync_signatures"`
	SyncSBOMs      bool     `yaml:"sync_sboms"`
	Schedule       string   `yaml:"schedule"`
	Mode           string   `yaml:"mode"` // "continuous", "once"
	Concurrency    int      `yaml:"concurrency"`
}

// AuthConfig holds authentication settings
type AuthConfig struct {
	Type         string `yaml:"type"` // "htpasswd", "none"
	HtpasswdFile string `yaml:"htpasswd_file"`
}

// ReleasesConfig holds OpenShift release management settings
type ReleasesConfig struct {
	Enabled          bool     `yaml:"enabled"`
	Upstream         string   `yaml:"upstream"`
	Repository       string   `yaml:"repository"`
	LocalRepo        string   `yaml:"local_repo"`
	Architectures    []string `yaml:"architectures"`
	ArtifactPath     string   `yaml:"artifact_path"`
	PullSecret       string   `yaml:"pull_secret"`
	AutoDiscover     bool     `yaml:"auto_discover"`
	DiscoverSchedule string   `yaml:"discover_schedule"`
}

// CertsConfig holds certificate management settings
type CertsConfig struct {
	AutoGenerate bool   `yaml:"auto_generate"`
	CACert       string `yaml:"ca_cert"`
	CAKey        string `yaml:"ca_key"`
	OutputDir    string `yaml:"output_dir"`
}

// DefaultConfig returns a configuration with sensible defaults
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Addr: ":5000",
		},
		Storage: StorageConfig{
			Path:        "/var/lib/fastregistry",
			CacheSizeMB: 1024,
			GC: GCConfig{
				Enabled:    true,
				Schedule:   "0 2 * * *",
				KeepRecent: 10,
			},
		},
		Mesh: MeshConfig{
			Enabled:           false,
			Bind:              ":7946",
			ReplicationFactor: 2,
		},
		Auth: AuthConfig{
			Type: "none",
		},
		Releases: ReleasesConfig{
			Enabled:          false,
			Upstream:         "quay.io",
			Repository:       "openshift-release-dev/ocp-release",
			LocalRepo:        "openshift/release",
			Architectures:    []string{"x86_64", "aarch64"},
			AutoDiscover:     false,
			DiscoverSchedule: "0 */6 * * *",
		},
		Certs: CertsConfig{
			AutoGenerate: true,
		},
	}
}

// Load reads configuration from a YAML file
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	// Expand environment variables
	data = []byte(os.ExpandEnv(string(data)))

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return cfg, nil
}

// Validate checks the configuration for errors
func (c *Config) Validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("server.addr is required")
	}

	if c.Storage.Path == "" {
		return fmt.Errorf("storage.path is required")
	}

	// Set defaults for sync sources
	for i := range c.Sync.Sources {
		if c.Sync.Sources[i].Concurrency == 0 {
			c.Sync.Sources[i].Concurrency = 5
		}
		if c.Sync.Sources[i].Mode == "" {
			c.Sync.Sources[i].Mode = "once"
		}
	}

	// Set path defaults for releases
	if c.Releases.ArtifactPath == "" {
		c.Releases.ArtifactPath = filepath.Join(c.Storage.Path, "releases")
	}

	// Set path defaults for certs
	if c.Certs.OutputDir == "" {
		c.Certs.OutputDir = filepath.Join(c.Storage.Path, "certs")
	}

	return nil
}
