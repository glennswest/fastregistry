package signing

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gwest/fastregistry/internal/storage"
	"github.com/gwest/fastregistry/pkg/digest"
	"github.com/gwest/fastregistry/pkg/oci"
)

// Cosign media types
const (
	// Signature artifact types
	MediaTypeCosignSignature = "application/vnd.dev.cosign.simplesigning.v1+json"
	MediaTypeCosignPublicKey = "application/vnd.dev.cosign.cosign.pub+pem"

	// SBOM artifact types
	MediaTypeSPDX        = "application/spdx+json"
	MediaTypeCycloneDX   = "application/vnd.cyclonedx+json"
	MediaTypeSyftJSON    = "application/vnd.syft+json"

	// Attestation types
	MediaTypeIntotoAttestation = "application/vnd.dsse.envelope.v1+json"
	MediaTypeIntotoPayload     = "application/vnd.in-toto+json"

	// OCI 1.1 artifact type
	ArtifactTypeCosignSignature = "application/vnd.dev.cosign.artifact.sig.v1+json"
	ArtifactTypeSBOM            = "application/vnd.dev.cosign.artifact.sbom.v1+json"
	ArtifactTypeAttestation     = "application/vnd.dev.cosign.artifact.attest.v1+json"
)

// SignatureStore handles storage and retrieval of image signatures
type SignatureStore struct {
	blobs    *storage.BlobStore
	metadata *storage.MetadataStore
}

// NewSignatureStore creates a new signature store
func NewSignatureStore(blobs *storage.BlobStore, metadata *storage.MetadataStore) *SignatureStore {
	return &SignatureStore{
		blobs:    blobs,
		metadata: metadata,
	}
}

// GetSignatureTag returns the tag used to store signatures for a digest
// Format: sha256-<hex>.sig
func GetSignatureTag(dgst digest.Digest) string {
	hex := dgst.Hex()
	return fmt.Sprintf("sha256-%s.sig", hex)
}

// GetSBOMTag returns the tag used to store SBOMs for a digest
// Format: sha256-<hex>.sbom
func GetSBOMTag(dgst digest.Digest) string {
	hex := dgst.Hex()
	return fmt.Sprintf("sha256-%s.sbom", hex)
}

// GetAttestationTag returns the tag used to store attestations for a digest
// Format: sha256-<hex>.att
func GetAttestationTag(dgst digest.Digest) string {
	hex := dgst.Hex()
	return fmt.Sprintf("sha256-%s.att", hex)
}

// IsSignatureTag returns true if the tag is a signature tag
func IsSignatureTag(tag string) bool {
	return strings.HasSuffix(tag, ".sig")
}

// IsSBOMTag returns true if the tag is an SBOM tag
func IsSBOMTag(tag string) bool {
	return strings.HasSuffix(tag, ".sbom")
}

// IsAttestationTag returns true if the tag is an attestation tag
func IsAttestationTag(tag string) bool {
	return strings.HasSuffix(tag, ".att")
}

// IsArtifactTag returns true if the tag is any type of artifact tag
func IsArtifactTag(tag string) bool {
	return IsSignatureTag(tag) || IsSBOMTag(tag) || IsAttestationTag(tag)
}

// GetSignatures retrieves all signatures for an image
func (s *SignatureStore) GetSignatures(repo string, imageDgst digest.Digest) ([]Signature, error) {
	sigTag := GetSignatureTag(imageDgst)

	meta, err := s.metadata.GetManifest(repo, sigTag)
	if err == storage.ErrManifestNotFound {
		return nil, nil // No signatures
	}
	if err != nil {
		return nil, err
	}

	// Get the signature manifest
	reader, _, err := s.blobs.Get(meta.Digest)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	var manifest oci.Manifest
	if err := json.NewDecoder(reader).Decode(&manifest); err != nil {
		return nil, err
	}

	var signatures []Signature
	for _, layer := range manifest.Layers {
		if layer.MediaType == MediaTypeCosignSignature {
			sig, err := s.loadSignature(layer.Digest)
			if err != nil {
				continue
			}
			signatures = append(signatures, *sig)
		}
	}

	return signatures, nil
}

// Signature represents a cosign signature
type Signature struct {
	Digest      digest.Digest     `json:"digest"`
	Payload     []byte            `json:"payload"`
	Base64Sig   string            `json:"base64_signature"`
	Certificate string            `json:"certificate,omitempty"`
	Chain       []string          `json:"chain,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

func (s *SignatureStore) loadSignature(dgst digest.Digest) (*Signature, error) {
	reader, _, err := s.blobs.Get(dgst)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	var sig Signature
	sig.Digest = dgst
	if err := json.NewDecoder(reader).Decode(&sig); err != nil {
		return nil, err
	}

	return &sig, nil
}

// GetSBOM retrieves the SBOM for an image
func (s *SignatureStore) GetSBOM(repo string, imageDgst digest.Digest) (*SBOM, error) {
	sbomTag := GetSBOMTag(imageDgst)

	meta, err := s.metadata.GetManifest(repo, sbomTag)
	if err == storage.ErrManifestNotFound {
		return nil, nil // No SBOM
	}
	if err != nil {
		return nil, err
	}

	// Get the SBOM manifest
	reader, _, err := s.blobs.Get(meta.Digest)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	var manifest oci.Manifest
	if err := json.NewDecoder(reader).Decode(&manifest); err != nil {
		return nil, err
	}

	// Find the SBOM layer
	for _, layer := range manifest.Layers {
		if isSBOMMediaType(layer.MediaType) {
			sbomReader, size, err := s.blobs.Get(layer.Digest)
			if err != nil {
				return nil, err
			}
			defer sbomReader.Close()

			return &SBOM{
				Digest:    layer.Digest,
				MediaType: layer.MediaType,
				Size:      size,
			}, nil
		}
	}

	return nil, nil
}

// SBOM represents a Software Bill of Materials
type SBOM struct {
	Digest    digest.Digest `json:"digest"`
	MediaType string        `json:"mediaType"`
	Size      int64         `json:"size"`
}

func isSBOMMediaType(mt string) bool {
	return mt == MediaTypeSPDX || mt == MediaTypeCycloneDX || mt == MediaTypeSyftJSON
}

// ReferrersHandler handles the OCI 1.1 referrers API
// GET /v2/<name>/referrers/<digest>
type ReferrersHandler struct {
	metadata *storage.MetadataStore
	blobs    *storage.BlobStore
}

// NewReferrersHandler creates a new referrers handler
func NewReferrersHandler(metadata *storage.MetadataStore, blobs *storage.BlobStore) *ReferrersHandler {
	return &ReferrersHandler{
		metadata: metadata,
		blobs:    blobs,
	}
}

// ListReferrers returns all artifacts that reference the given digest
func (h *ReferrersHandler) ListReferrers(repo string, dgst digest.Digest, artifactType string) (*oci.Index, error) {
	// List all tags and find those that reference this digest
	tags, err := h.metadata.ListTags(repo)
	if err != nil {
		return nil, err
	}

	var descriptors []oci.Descriptor

	for _, tag := range tags {
		if !IsArtifactTag(tag) {
			continue
		}

		meta, err := h.metadata.GetManifest(repo, tag)
		if err != nil {
			continue
		}

		// Check if this manifest references our target
		if meta.Subject == string(dgst) {
			// Filter by artifact type if specified
			if artifactType != "" && !matchesArtifactType(tag, artifactType) {
				continue
			}

			descriptors = append(descriptors, oci.Descriptor{
				MediaType:   meta.MediaType,
				Digest:      meta.Digest,
				Size:        meta.Size,
				Annotations: meta.Annotations,
			})
		}
	}

	return &oci.Index{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests:     descriptors,
	}, nil
}

func matchesArtifactType(tag, artifactType string) bool {
	switch artifactType {
	case ArtifactTypeCosignSignature:
		return IsSignatureTag(tag)
	case ArtifactTypeSBOM:
		return IsSBOMTag(tag)
	case ArtifactTypeAttestation:
		return IsAttestationTag(tag)
	default:
		return true
	}
}
