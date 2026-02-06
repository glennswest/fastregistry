package releases

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/gwest/fastregistry/internal/storage"
	"github.com/gwest/fastregistry/pkg/digest"
)

// Extractor extracts install artifacts from cloned release images
type Extractor struct {
	blobs        *storage.BlobStore
	metadata     *storage.MetadataStore
	cloner       *Cloner
	artifactPath string
	localRepo    string
}

// NewExtractor creates a new artifact extractor
func NewExtractor(blobs *storage.BlobStore, metadata *storage.MetadataStore, cloner *Cloner, artifactPath, localRepo string) *Extractor {
	return &Extractor{
		blobs:        blobs,
		metadata:     metadata,
		cloner:       cloner,
		artifactPath: artifactPath,
		localRepo:    localRepo,
	}
}

// imageStream is the format of the image-references file in OpenShift release images.
type imageStream struct {
	Spec struct {
		Tags []imageStreamTag `json:"tags"`
	} `json:"spec"`
}

type imageStreamTag struct {
	Name string `json:"name"`
	From struct {
		Name string `json:"name"` // full image reference, e.g. quay.io/...@sha256:...
	} `json:"from"`
}

// extractRule defines what to extract from a component image.
type extractRule struct {
	matchNames   []string // base filenames to match
	pathContains string   // required substring in full path (e.g. "bin/")
	outName      string   // output filename
	artType      string   // artifact type label
}

// desiredComponents maps component names to extraction rules.
var desiredComponents = map[string][]extractRule{
	"installer": {
		{matchNames: []string{"openshift-install", "openshift-install-linux"}, pathContains: "bin/", outName: "openshift-install", artType: "binary"},
	},
	"cli": {
		{matchNames: []string{"oc"}, pathContains: "bin/", outName: "oc", artType: "binary"},
	},
	"installer-artifacts": {
		// Mac installer binaries - in /usr/share/openshift/mac/ and /mac_arm64/
		{matchNames: []string{"openshift-install"}, pathContains: "/mac/", outName: "openshift-install-mac", artType: "binary"},
		{matchNames: []string{"openshift-install"}, pathContains: "/mac_arm64/", outName: "openshift-install-mac-arm64", artType: "binary"},
	},
	"cli-artifacts": {
		// Mac oc client - in /usr/share/openshift/mac/ and /mac_arm64/
		{matchNames: []string{"oc"}, pathContains: "/mac/", outName: "oc-mac", artType: "binary"},
		{matchNames: []string{"oc"}, pathContains: "/mac_arm64/", outName: "oc-mac-arm64", artType: "binary"},
		// Windows oc
		{matchNames: []string{"oc.exe"}, pathContains: "/windows/", outName: "oc.exe", artType: "binary"},
	},
	"machine-os-images": {
		// CoreOS ISO for agent-based installs
		{matchNames: []string{"coreos-x86_64.iso", "rhcos-live.x86_64.iso"}, pathContains: "", outName: "coreos.iso", artType: "iso"},
	},
}

// Extract extracts install artifacts from a cloned release by reading its
// image-references, pulling the relevant component images, and extracting
// the binaries from their layers.
func (e *Extractor) Extract(ctx context.Context, version, arch string) ([]Artifact, error) {
	tag := version + "-" + arch
	outDir := filepath.Join(e.artifactPath, version)

	if err := os.MkdirAll(outDir, 0755); err != nil {
		return nil, fmt.Errorf("creating output dir: %w", err)
	}

	// Get the release manifest from local store
	meta, err := e.metadata.GetManifest(e.localRepo, tag)
	if err != nil {
		return nil, fmt.Errorf("getting release manifest: %w", err)
	}

	// Read the manifest content
	manifestReader, _, err := e.blobs.Get(meta.Digest)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	manifestBytes, err := io.ReadAll(manifestReader)
	manifestReader.Close()
	if err != nil {
		return nil, fmt.Errorf("reading manifest bytes: %w", err)
	}

	// Parse manifest to get layers
	var manifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest    string `json:"digest"`
			MediaType string `json:"mediaType"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}

	// Collect layer digests
	var layerDigests []string
	for _, l := range manifest.Layers {
		layerDigests = append(layerDigests, l.Digest)
	}

	var artifacts []Artifact

	// Step 1: Find image-references in the release image layers
	refs, err := e.findImageReferences(layerDigests)
	if err != nil {
		log.Printf("Warning: could not find image-references: %v", err)
	}

	// Step 2: Pull and extract from component images
	if refs != nil && e.cloner != nil {
		for component, rules := range desiredComponents {
			imageRef, ok := refs[component]
			if !ok {
				log.Printf("Component %q not found in image-references, skipping", component)
				continue
			}

			log.Printf("Extracting from component %q: %s", component, truncateStr(imageRef, 60))
			found, err := e.extractFromComponent(ctx, imageRef, outDir, rules)
			if err != nil {
				log.Printf("Warning: failed to extract from component %q: %v", component, err)
				continue
			}
			artifacts = append(artifacts, found...)
		}
	}

	// Step 3: Fallback — scan release image layers directly (original behavior)
	if len(artifacts) == 0 {
		log.Printf("No artifacts from components, falling back to direct layer scan for %s", version)
		for _, layerDigest := range layerDigests {
			dgst, err := digest.Parse(layerDigest)
			if err != nil {
				continue
			}
			found, err := e.extractFromLayer(dgst, outDir, version)
			if err != nil {
				log.Printf("Warning: error scanning layer %s: %v", layerDigest[:16], err)
				continue
			}
			artifacts = append(artifacts, found...)
		}
	}

	// Compute SHA256 for all extracted artifacts
	for i := range artifacts {
		if artifacts[i].SHA256 == "" {
			hash, err := hashFile(artifacts[i].Path)
			if err == nil {
				artifacts[i].SHA256 = hash
			}
		}
	}

	log.Printf("Extracted %d artifacts for release %s", len(artifacts), version)
	return artifacts, nil
}

// findImageReferences scans release image layers for the image-references file
// and returns a map of component name -> full image reference.
func (e *Extractor) findImageReferences(layerDigests []string) (map[string]string, error) {
	for _, layerDigest := range layerDigests {
		dgst, err := digest.Parse(layerDigest)
		if err != nil {
			continue
		}
		refs, err := e.scanLayerForImageReferences(dgst)
		if err != nil {
			continue
		}
		if refs != nil {
			return refs, nil
		}
	}
	return nil, fmt.Errorf("image-references not found in any layer")
}

func (e *Extractor) scanLayerForImageReferences(dgst digest.Digest) (map[string]string, error) {
	reader, _, err := e.blobs.Get(dgst)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	gzr, err := gzip.NewReader(reader)
	if err != nil {
		return nil, nil // not gzipped
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil
		}

		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		if filepath.Base(hdr.Name) == "image-references" {
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}

			var is imageStream
			if err := json.Unmarshal(data, &is); err != nil {
				return nil, fmt.Errorf("parsing image-references: %w", err)
			}

			refs := make(map[string]string)
			for _, tag := range is.Spec.Tags {
				refs[tag.Name] = tag.From.Name
			}

			log.Printf("Found image-references with %d components", len(refs))
			return refs, nil
		}
	}
	return nil, nil
}

// extractFromComponent pulls a component image and extracts matching files.
func (e *Extractor) extractFromComponent(ctx context.Context, imageRef, outDir string, rules []extractRule) ([]Artifact, error) {
	manifestBytes, err := e.cloner.PullComponentImage(ctx, imageRef)
	if err != nil {
		return nil, fmt.Errorf("pulling component image: %w", err)
	}

	var manifest struct {
		Layers []struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("parsing component manifest: %w", err)
	}

	var artifacts []Artifact
	for _, layer := range manifest.Layers {
		dgst, err := digest.Parse(layer.Digest)
		if err != nil {
			continue
		}
		found, err := e.extractFromComponentLayer(dgst, outDir, rules)
		if err != nil {
			log.Printf("Warning: error extracting from component layer %s: %v", layer.Digest[:16], err)
			continue
		}
		artifacts = append(artifacts, found...)
	}

	return artifacts, nil
}

func (e *Extractor) extractFromComponentLayer(dgst digest.Digest, outDir string, rules []extractRule) ([]Artifact, error) {
	reader, _, err := e.blobs.Get(dgst)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	gzr, err := gzip.NewReader(reader)
	if err != nil {
		return nil, nil // not gzipped
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	var artifacts []Artifact

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return artifacts, nil // partial extraction is OK
		}

		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		name := filepath.Base(hdr.Name)
		for _, rule := range rules {
			if !matchesRule(name, hdr.Name, rule) {
				continue
			}

			outPath := filepath.Join(outDir, rule.outName)
			if err := extractFile(tr, outPath, hdr.FileInfo().Mode()); err != nil {
				log.Printf("Warning: failed to extract %s: %v", name, err)
				continue
			}

			info, err := os.Stat(outPath)
			if err != nil {
				continue
			}

			artifacts = append(artifacts, Artifact{
				Name: rule.outName,
				Type: rule.artType,
				Path: outPath,
				Size: info.Size(),
			})
			log.Printf("Extracted: %s (%d bytes) from component layer", rule.outName, info.Size())
		}
	}

	return artifacts, nil
}

func matchesRule(name, fullPath string, rule extractRule) bool {
	matched := false
	for _, n := range rule.matchNames {
		if name == n {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	if rule.pathContains != "" && !strings.Contains(fullPath, rule.pathContains) {
		return false
	}
	return true
}

// extractFromLayer scans a release image layer for known artifacts (fallback).
func (e *Extractor) extractFromLayer(dgst digest.Digest, outDir, version string) ([]Artifact, error) {
	reader, _, err := e.blobs.Get(dgst)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	gzr, err := gzip.NewReader(reader)
	if err != nil {
		return nil, nil
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	var artifacts []Artifact

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return artifacts, nil
		}

		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		name := filepath.Base(hdr.Name)
		artifact, ok := matchArtifact(name, hdr.Name)
		if !ok {
			continue
		}

		outPath := filepath.Join(outDir, artifact.Name)
		if err := extractFile(tr, outPath, hdr.FileInfo().Mode()); err != nil {
			log.Printf("Warning: failed to extract %s: %v", name, err)
			continue
		}

		info, err := os.Stat(outPath)
		if err != nil {
			continue
		}

		artifact.Path = outPath
		artifact.Size = info.Size()
		artifacts = append(artifacts, artifact)
		log.Printf("Extracted: %s (%d bytes)", artifact.Name, artifact.Size)
	}

	return artifacts, nil
}

// matchArtifact checks if a file from a tar layer is a known artifact (fallback matcher).
func matchArtifact(name, fullPath string) (Artifact, bool) {
	switch {
	case name == "openshift-install" || name == "openshift-install-linux":
		return Artifact{Name: "openshift-install", Type: "binary"}, true

	case strings.HasSuffix(name, ".iso") && strings.Contains(fullPath, "agent"):
		return Artifact{Name: "agent.x86_64.iso", Type: "iso"}, true

	case strings.HasSuffix(name, ".iso") && strings.Contains(name, "rhcos"):
		return Artifact{Name: "agent.x86_64.iso", Type: "iso"}, true

	case name == "rootfs.img" || strings.Contains(name, "rootfs"):
		return Artifact{Name: "rootfs.img", Type: "rootfs"}, true

	case name == "vmlinuz" || (strings.Contains(name, "vmlinuz") && !strings.HasSuffix(name, ".sig")):
		return Artifact{Name: "vmlinuz", Type: "kernel"}, true

	case name == "initramfs.img" || strings.Contains(name, "initramfs"):
		return Artifact{Name: "initramfs.img", Type: "initramfs"}, true
	}
	return Artifact{}, false
}

func extractFile(r io.Reader, path string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.Copy(f, r); err != nil {
		return err
	}

	return f.Sync()
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}
