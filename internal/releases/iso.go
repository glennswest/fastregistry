package releases

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

const (
	sectorSize = 2048
	pvdSector  = 16
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

// embedIgnition embeds an ignition config into a CoreOS ISO by writing a
// gzipped CPIO archive into the /images/ignition.img region of the ISO.
func embedIgnition(isoPath string, ignition []byte) error {
	f, err := os.OpenFile(isoPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("opening ISO: %w", err)
	}
	defer f.Close()

	offset, size, err := findIgnitionImg(f)
	if err != nil {
		return fmt.Errorf("finding ignition.img in ISO: %w", err)
	}

	initrd, err := buildInitrd(ignition)
	if err != nil {
		return fmt.Errorf("building initrd: %w", err)
	}

	if int64(len(initrd)) > size {
		return fmt.Errorf("initrd (%d bytes) exceeds ignition.img slot (%d bytes)", len(initrd), size)
	}

	// Write initrd at the extent offset, zero-pad to original size
	buf := make([]byte, size)
	copy(buf, initrd)

	if _, err := f.WriteAt(buf, offset); err != nil {
		return fmt.Errorf("writing initrd to ISO: %w", err)
	}

	return f.Sync()
}

// findIgnitionImg locates /images/ignition.img in an ISO 9660 filesystem
// and returns its byte offset and size.
func findIgnitionImg(f *os.File) (offset int64, size int64, err error) {
	// Read Primary Volume Descriptor at sector 16
	pvd := make([]byte, sectorSize)
	if _, err := f.ReadAt(pvd, pvdSector*sectorSize); err != nil {
		return 0, 0, fmt.Errorf("reading PVD: %w", err)
	}

	// Verify PVD signature
	if pvd[0] != 1 || string(pvd[1:6]) != "CD001" {
		return 0, 0, fmt.Errorf("invalid PVD: bad type or signature")
	}

	// Root directory record starts at offset 156, length 34 bytes
	rootRecord := pvd[156:190]
	rootExtent := int64(binary.LittleEndian.Uint32(rootRecord[2:6]))
	rootSize := int64(binary.LittleEndian.Uint32(rootRecord[10:14]))

	// Read root directory and find IMAGES directory
	imagesExtent, imagesSize, err := findDirEntry(f, rootExtent, rootSize, "IMAGES")
	if err != nil {
		return 0, 0, fmt.Errorf("finding IMAGES directory: %w", err)
	}

	// Read IMAGES directory and find IGNITION.IMG
	ignExtent, ignSize, err := findDirEntry(f, imagesExtent, imagesSize, "IGNITION.IMG")
	if err != nil {
		return 0, 0, fmt.Errorf("finding IGNITION.IMG: %w", err)
	}

	return ignExtent * sectorSize, ignSize, nil
}

// findDirEntry searches a directory at the given extent for an entry with the
// specified name (ISO 9660 uses uppercase). Returns the entry's extent and size.
func findDirEntry(f *os.File, dirExtent, dirSize int64, name string) (int64, int64, error) {
	dirData := make([]byte, dirSize)
	if _, err := f.ReadAt(dirData, dirExtent*sectorSize); err != nil {
		return 0, 0, fmt.Errorf("reading directory: %w", err)
	}

	pos := 0
	for pos < len(dirData) {
		recLen := int(dirData[pos])
		if recLen == 0 {
			// Skip to next sector boundary
			nextSector := ((pos / sectorSize) + 1) * sectorSize
			if nextSector >= len(dirData) {
				break
			}
			pos = nextSector
			continue
		}

		if pos+33 >= len(dirData) {
			break
		}

		nameLen := int(dirData[pos+32])
		if pos+33+nameLen > len(dirData) {
			break
		}

		entryName := string(dirData[pos+33 : pos+33+nameLen])
		// ISO 9660 appends ";1" version suffix to file names
		entryName = strings.TrimSuffix(entryName, ";1")

		if strings.EqualFold(entryName, name) {
			extent := int64(binary.LittleEndian.Uint32(dirData[pos+2 : pos+6]))
			size := int64(binary.LittleEndian.Uint32(dirData[pos+10 : pos+14]))
			return extent, size, nil
		}

		pos += recLen
	}

	return 0, 0, fmt.Errorf("entry %q not found", name)
}

// buildInitrd creates a gzip-compressed CPIO newc archive containing config.ign.
func buildInitrd(ignition []byte) ([]byte, error) {
	var cpioData bytes.Buffer
	writeCPIOEntry(&cpioData, "config.ign", ignition)
	writeCPIOEntry(&cpioData, "TRAILER!!!", nil)

	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	if _, err := gz.Write(cpioData.Bytes()); err != nil {
		return nil, fmt.Errorf("gzip write: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("gzip close: %w", err)
	}

	return out.Bytes(), nil
}

// writeCPIOEntry writes a single CPIO newc format entry.
func writeCPIOEntry(w io.Writer, name string, data []byte) {
	nameWithNull := name + "\x00"
	headerSize := 110 + len(nameWithNull)
	namePad := (4 - (headerSize % 4)) % 4
	dataPad := (4 - (len(data) % 4)) % 4

	// CPIO newc header: magic + 13 fields of 8 hex chars each
	hdr := fmt.Sprintf("070701"+
		"%08X"+ // inode
		"%08X"+ // mode
		"%08X"+ // uid
		"%08X"+ // gid
		"%08X"+ // nlink
		"%08X"+ // mtime
		"%08X"+ // filesize
		"%08X"+ // devmajor
		"%08X"+ // devminor
		"%08X"+ // rdevmajor
		"%08X"+ // rdevminor
		"%08X"+ // namesize
		"%08X", // check
		0,                  // inode
		0o100644,           // mode (regular file)
		0,                  // uid
		0,                  // gid
		1,                  // nlink
		0,                  // mtime
		len(data),          // filesize
		0,                  // devmajor
		0,                  // devminor
		0,                  // rdevmajor
		0,                  // rdevminor
		len(nameWithNull),  // namesize (includes null terminator)
		0,                  // check
	)

	w.Write([]byte(hdr))
	w.Write([]byte(nameWithNull))
	w.Write(make([]byte, namePad))
	if len(data) > 0 {
		w.Write(data)
		if dataPad > 0 {
			w.Write(make([]byte, dataPad))
		}
	}
}
