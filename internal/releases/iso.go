package releases

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/google/uuid"
)


// ISOGenerator creates agent ISOs with embedded ignition configs.
type ISOGenerator struct {
	artifactPath string
	installsPath string
}

// NewISOGenerator creates a new ISO generator.
func NewISOGenerator(artifactPath string) *ISOGenerator {
	installsPath := filepath.Join(filepath.Dir(artifactPath), "installs")
	os.MkdirAll(installsPath, 0755)
	return &ISOGenerator{
		artifactPath: artifactPath,
		installsPath: installsPath,
	}
}

// GenerateAgentISO creates an agent ISO with embedded ignition.
// Returns the UUID and path to the generated ISO.
func (g *ISOGenerator) GenerateAgentISO(version string, ignition []byte) (string, string, error) {
	// Find base ISO
	baseISO := filepath.Join(g.artifactPath, version, "coreos.iso")
	if _, err := os.Stat(baseISO); err != nil {
		return "", "", fmt.Errorf("base ISO not found: %s", baseISO)
	}

	// Generate UUID for this install
	id := uuid.New().String()
	outDir := filepath.Join(g.installsPath, id)
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return "", "", fmt.Errorf("creating output dir: %w", err)
	}

	outISO := filepath.Join(outDir, "agent.iso")

	// Copy base ISO to output
	if err := copyFile(baseISO, outISO); err != nil {
		return "", "", fmt.Errorf("copying base ISO: %w", err)
	}

	// Embed ignition into the copy
	if err := embedIgnition(outISO, ignition); err != nil {
		os.RemoveAll(outDir)
		return "", "", fmt.Errorf("embedding ignition: %w", err)
	}

	return id, outISO, nil
}

// GetInstallPath returns the filesystem path for an install UUID.
func (g *ISOGenerator) GetInstallPath(id string) string {
	return filepath.Join(g.installsPath, id)
}

// InstallsPath returns the base path for installs.
func (g *ISOGenerator) InstallsPath() string {
	return g.installsPath
}

// copyFile copies src to dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// embedIgnition embeds ignition config into a CoreOS ISO using coreos-installer.
func embedIgnition(isoPath string, ignition []byte) error {
	// Write ignition to a temp file
	ignFile, err := os.CreateTemp("", "ignition-*.ign")
	if err != nil {
		return fmt.Errorf("creating temp ignition file: %w", err)
	}
	defer os.Remove(ignFile.Name())

	if _, err := ignFile.Write(ignition); err != nil {
		ignFile.Close()
		return fmt.Errorf("writing ignition: %w", err)
	}
	ignFile.Close()

	// Use coreos-installer to embed ignition
	cmd := exec.Command("coreos-installer", "iso", "ignition", "embed",
		"-i", ignFile.Name(),
		"-f", // force overwrite existing ignition
		isoPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("coreos-installer failed: %v: %s", err, string(output))
	}

	return nil
}
