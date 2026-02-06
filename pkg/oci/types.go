package oci

import (
	"github.com/gwest/fastregistry/pkg/digest"
)

// Media types for OCI artifacts
const (
	MediaTypeManifest         = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeManifestList     = "application/vnd.oci.image.index.v1+json"
	MediaTypeImageConfig      = "application/vnd.oci.image.config.v1+json"
	MediaTypeImageLayer       = "application/vnd.oci.image.layer.v1.tar+gzip"
	MediaTypeImageLayerZstd   = "application/vnd.oci.image.layer.v1.tar+zstd"
	MediaTypeImageLayerNonDist = "application/vnd.oci.image.layer.nondistributable.v1.tar+gzip"

	// Docker media types for compatibility
	MediaTypeDockerManifest      = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerManifestList  = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeDockerImageConfig   = "application/vnd.docker.container.image.v1+json"
	MediaTypeDockerImageLayer    = "application/vnd.docker.image.rootfs.diff.tar.gzip"
)

// Descriptor describes a content-addressable blob
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      digest.Digest     `json:"digest"`
	Size        int64             `json:"size"`
	URLs        []string          `json:"urls,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *Platform         `json:"platform,omitempty"`
}

// Platform describes the platform for an image
type Platform struct {
	Architecture string   `json:"architecture"`
	OS           string   `json:"os"`
	OSVersion    string   `json:"os.version,omitempty"`
	OSFeatures   []string `json:"os.features,omitempty"`
	Variant      string   `json:"variant,omitempty"`
}

// Manifest represents an OCI image manifest
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
	Subject       *Descriptor       `json:"subject,omitempty"` // OCI 1.1
}

// Index represents an OCI image index (multi-arch manifest)
type Index struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	Manifests     []Descriptor      `json:"manifests"`
	Annotations   map[string]string `json:"annotations,omitempty"`
	Subject       *Descriptor       `json:"subject,omitempty"` // OCI 1.1
}

// ImageConfig represents the OCI image configuration
type ImageConfig struct {
	Created      string            `json:"created,omitempty"`
	Author       string            `json:"author,omitempty"`
	Architecture string            `json:"architecture"`
	OS           string            `json:"os"`
	Config       *ContainerConfig  `json:"config,omitempty"`
	RootFS       RootFS            `json:"rootfs"`
	History      []History         `json:"history,omitempty"`
}

// ContainerConfig holds container runtime configuration
type ContainerConfig struct {
	User         string            `json:"User,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	Env          []string          `json:"Env,omitempty"`
	Entrypoint   []string          `json:"Entrypoint,omitempty"`
	Cmd          []string          `json:"Cmd,omitempty"`
	Volumes      map[string]struct{} `json:"Volumes,omitempty"`
	WorkingDir   string            `json:"WorkingDir,omitempty"`
	Labels       map[string]string `json:"Labels,omitempty"`
	StopSignal   string            `json:"StopSignal,omitempty"`
}

// RootFS describes the root filesystem
type RootFS struct {
	Type    string          `json:"type"`
	DiffIDs []digest.Digest `json:"diff_ids"`
}

// History represents a single layer history entry
type History struct {
	Created    string `json:"created,omitempty"`
	CreatedBy  string `json:"created_by,omitempty"`
	Author     string `json:"author,omitempty"`
	Comment    string `json:"comment,omitempty"`
	EmptyLayer bool   `json:"empty_layer,omitempty"`
}

// ErrorResponse represents an OCI registry error
type ErrorResponse struct {
	Errors []Error `json:"errors"`
}

// Error represents a single error
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

// Standard OCI error codes
const (
	ErrorCodeBlobUnknown         = "BLOB_UNKNOWN"
	ErrorCodeBlobUploadInvalid   = "BLOB_UPLOAD_INVALID"
	ErrorCodeBlobUploadUnknown   = "BLOB_UPLOAD_UNKNOWN"
	ErrorCodeDigestInvalid       = "DIGEST_INVALID"
	ErrorCodeManifestBlobUnknown = "MANIFEST_BLOB_UNKNOWN"
	ErrorCodeManifestInvalid     = "MANIFEST_INVALID"
	ErrorCodeManifestUnknown     = "MANIFEST_UNKNOWN"
	ErrorCodeNameInvalid         = "NAME_INVALID"
	ErrorCodeNameUnknown         = "NAME_UNKNOWN"
	ErrorCodeSizeInvalid         = "SIZE_INVALID"
	ErrorCodeUnauthorized        = "UNAUTHORIZED"
	ErrorCodeDenied              = "DENIED"
	ErrorCodeUnsupported         = "UNSUPPORTED"
)

// TagList represents the response from GET /v2/<name>/tags/list
type TagList struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// Catalog represents the response from GET /v2/_catalog
type Catalog struct {
	Repositories []string `json:"repositories"`
}
