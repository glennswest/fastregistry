package releases

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// InstallConfig represents the install-config.yaml structure
type InstallConfig struct {
	APIVersion string `yaml:"apiVersion"`
	BaseDomain string `yaml:"baseDomain"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Networking struct {
		NetworkType   string `yaml:"networkType"`
		ClusterNetwork []struct {
			CIDR       string `yaml:"cidr"`
			HostPrefix int    `yaml:"hostPrefix"`
		} `yaml:"clusterNetwork"`
		ServiceNetwork []string `yaml:"serviceNetwork"`
		MachineNetwork []struct {
			CIDR string `yaml:"cidr"`
		} `yaml:"machineNetwork"`
	} `yaml:"networking"`
	ControlPlane struct {
		Name     string `yaml:"name"`
		Replicas int    `yaml:"replicas"`
	} `yaml:"controlPlane"`
	Compute []struct {
		Name     string `yaml:"name"`
		Replicas int    `yaml:"replicas"`
	} `yaml:"compute"`
	Platform struct {
		None *struct{} `yaml:"none,omitempty"`
	} `yaml:"platform"`
	PullSecret       string `yaml:"pullSecret"`
	SSHKey           string `yaml:"sshKey"`
	ImageContentSources []struct {
		Source  string   `yaml:"source"`
		Mirrors []string `yaml:"mirrors"`
	} `yaml:"imageContentSources"`
}

// AgentConfig represents the agent-config.yaml structure
type AgentConfig struct {
	APIVersion string `yaml:"apiVersion"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	RendezvousIP string `yaml:"rendezvousIP"`
	Hosts        []struct {
		Hostname   string `yaml:"hostname"`
		Role       string `yaml:"role"`
		Interfaces []struct {
			Name       string `yaml:"name"`
			MacAddress string `yaml:"macAddress"`
		} `yaml:"interfaces"`
		NetworkConfig struct {
			Interfaces []struct {
				Name  string `yaml:"name"`
				Type  string `yaml:"type"`
				State string `yaml:"state"`
				IPv4  struct {
					Enabled bool `yaml:"enabled"`
					DHCP    bool `yaml:"dhcp"`
					Address []struct {
						IP           string `yaml:"ip"`
						PrefixLength int    `yaml:"prefix-length"`
					} `yaml:"address"`
				} `yaml:"ipv4"`
				IPv6 struct {
					Enabled bool `yaml:"enabled"`
				} `yaml:"ipv6"`
			} `yaml:"interfaces"`
			DNSResolver struct {
				Config struct {
					Server []string `yaml:"server"`
				} `yaml:"config"`
			} `yaml:"dns-resolver"`
			Routes struct {
				Config []struct {
					Destination      string `yaml:"destination"`
					NextHopAddress   string `yaml:"next-hop-address"`
					NextHopInterface string `yaml:"next-hop-interface"`
				} `yaml:"config"`
			} `yaml:"routes"`
		} `yaml:"networkConfig"`
	} `yaml:"hosts"`
}

// Ignition config structures (Ignition spec 3.2)
type IgnitionConfig struct {
	Ignition IgnitionMeta  `json:"ignition"`
	Passwd   *IgnPasswd    `json:"passwd,omitempty"`
	Storage  *IgnStorage   `json:"storage,omitempty"`
	Systemd  *IgnSystemd   `json:"systemd,omitempty"`
}

type IgnitionMeta struct {
	Version string      `json:"version"`
	Config  *IgnCfgRefs `json:"config,omitempty"`
}

type IgnCfgRefs struct {
	Merge []IgnCfgRef `json:"merge,omitempty"`
}

type IgnCfgRef struct {
	Source string `json:"source,omitempty"`
}

type IgnPasswd struct {
	Users []IgnUser `json:"users,omitempty"`
}

type IgnUser struct {
	Name              string   `json:"name"`
	SSHAuthorizedKeys []string `json:"sshAuthorizedKeys,omitempty"`
}

type IgnStorage struct {
	Files []IgnFile `json:"files,omitempty"`
}

type IgnFile struct {
	Path      string       `json:"path"`
	Mode      *int         `json:"mode,omitempty"`
	Overwrite *bool        `json:"overwrite,omitempty"`
	Contents  *IgnContents `json:"contents,omitempty"`
}

type IgnContents struct {
	Source string `json:"source,omitempty"`
}

type IgnSystemd struct {
	Units []IgnUnit `json:"units,omitempty"`
}

type IgnUnit struct {
	Name     string `json:"name"`
	Enabled  *bool  `json:"enabled,omitempty"`
	Contents string `json:"contents,omitempty"`
}

// GenerateAgentIgnition creates an ignition config for agent-based install
func GenerateAgentIgnition(installConfigYAML, agentConfigYAML []byte, releaseImage string) ([]byte, error) {
	var installConfig InstallConfig
	if err := yaml.Unmarshal(installConfigYAML, &installConfig); err != nil {
		return nil, fmt.Errorf("parsing install-config.yaml: %w", err)
	}

	var agentConfig AgentConfig
	if err := yaml.Unmarshal(agentConfigYAML, &agentConfig); err != nil {
		return nil, fmt.Errorf("parsing agent-config.yaml: %w", err)
	}

	// Build ignition config
	enabled := true
	mode := 0644
	overwrite := true

	ign := IgnitionConfig{
		Ignition: IgnitionMeta{
			Version: "3.2.0",
		},
		Passwd: &IgnPasswd{
			Users: []IgnUser{
				{
					Name:              "core",
					SSHAuthorizedKeys: []string{installConfig.SSHKey},
				},
			},
		},
		Storage: &IgnStorage{
			Files: []IgnFile{
				{
					Path:      "/etc/assisted/manifests/pull-secret.json",
					Mode:      &mode,
					Overwrite: &overwrite,
					Contents: &IgnContents{
						Source: dataURL(installConfig.PullSecret),
					},
				},
				{
					Path:      "/etc/assisted/manifests/agent-config.yaml",
					Mode:      &mode,
					Overwrite: &overwrite,
					Contents: &IgnContents{
						Source: dataURL(string(agentConfigYAML)),
					},
				},
				{
					Path:      "/etc/assisted/manifests/install-config.yaml",
					Mode:      &mode,
					Overwrite: &overwrite,
					Contents: &IgnContents{
						Source: dataURL(string(installConfigYAML)),
					},
				},
				{
					Path:      "/etc/assisted/manifests/cluster-image-set.yaml",
					Mode:      &mode,
					Overwrite: &overwrite,
					Contents: &IgnContents{
						Source: dataURL(buildClusterImageSet(releaseImage)),
					},
				},
			},
		},
		Systemd: &IgnSystemd{
			Units: []IgnUnit{
				{
					Name:    "agent.service",
					Enabled: &enabled,
				},
			},
		},
	}

	// Add image content sources if present (for disconnected/mirrored registries)
	if len(installConfig.ImageContentSources) > 0 {
		icsp := buildImageContentSourcePolicy(installConfig)
		mode := 0644
		ign.Storage.Files = append(ign.Storage.Files, IgnFile{
			Path:      "/etc/assisted/manifests/image-content-source-policy.yaml",
			Mode:      &mode,
			Overwrite: &overwrite,
			Contents: &IgnContents{
				Source: dataURL(icsp),
			},
		})
	}

	return json.MarshalIndent(ign, "", "  ")
}

// dataURL encodes content as a data URL for ignition
func dataURL(content string) string {
	return "data:," + urlEncode(content)
}

// urlEncode performs percent-encoding for data URLs
func urlEncode(s string) string {
	var result []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreserved(c) {
			result = append(result, c)
		} else {
			result = append(result, '%')
			result = append(result, hexDigit(c>>4))
			result = append(result, hexDigit(c&0x0f))
		}
	}
	return string(result)
}

func isUnreserved(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
		(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~'
}

func hexDigit(n byte) byte {
	if n < 10 {
		return '0' + n
	}
	return 'A' + n - 10
}

// buildClusterImageSet creates the ClusterImageSet manifest
func buildClusterImageSet(releaseImage string) string {
	return fmt.Sprintf(`apiVersion: hive.openshift.io/v1
kind: ClusterImageSet
metadata:
  name: openshift-release
spec:
  releaseImage: %s
`, releaseImage)
}

// buildImageContentSourcePolicy creates ICSP for mirrored registries
func buildImageContentSourcePolicy(cfg InstallConfig) string {
	result := `apiVersion: operator.openshift.io/v1alpha1
kind: ImageContentSourcePolicy
metadata:
  name: mirror-config
spec:
  repositoryDigestMirrors:
`
	for _, ics := range cfg.ImageContentSources {
		result += fmt.Sprintf("  - source: %s\n    mirrors:\n", ics.Source)
		for _, m := range ics.Mirrors {
			result += fmt.Sprintf("    - %s\n", m)
		}
	}
	return result
}
