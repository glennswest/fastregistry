package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strings"
)

// Digest represents a content-addressable digest (e.g., sha256:abc123...)
type Digest string

// Algorithm returns the algorithm part of the digest (e.g., "sha256")
func (d Digest) Algorithm() string {
	parts := strings.SplitN(string(d), ":", 2)
	if len(parts) != 2 {
		return ""
	}
	return parts[0]
}

// Hex returns the hex-encoded hash part
func (d Digest) Hex() string {
	parts := strings.SplitN(string(d), ":", 2)
	if len(parts) != 2 {
		return ""
	}
	return parts[1]
}

// Validate checks if the digest is well-formed
func (d Digest) Validate() error {
	if d.Algorithm() == "" || d.Hex() == "" {
		return fmt.Errorf("invalid digest format: %s", d)
	}
	if d.Algorithm() != "sha256" {
		return fmt.Errorf("unsupported algorithm: %s", d.Algorithm())
	}
	if len(d.Hex()) != 64 {
		return fmt.Errorf("invalid sha256 length: %d", len(d.Hex()))
	}
	return nil
}

// String returns the string representation
func (d Digest) String() string {
	return string(d)
}

// ShortHex returns the first 12 characters of the hex (for logging)
func (d Digest) ShortHex() string {
	h := d.Hex()
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// FromBytes computes a sha256 digest from bytes
func FromBytes(data []byte) Digest {
	h := sha256.Sum256(data)
	return Digest("sha256:" + hex.EncodeToString(h[:]))
}

// FromReader computes a sha256 digest from a reader
func FromReader(r io.Reader) (Digest, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	return Digest("sha256:" + hex.EncodeToString(h.Sum(nil))), n, nil
}

// Parse parses a digest string
func Parse(s string) (Digest, error) {
	d := Digest(s)
	if err := d.Validate(); err != nil {
		return "", err
	}
	return d, nil
}

// Verifier returns a hash.Hash that can be used to verify content
type Verifier struct {
	expected Digest
	hash     hash.Hash
}

// NewVerifier creates a verifier for the given expected digest
func NewVerifier(expected Digest) (*Verifier, error) {
	if err := expected.Validate(); err != nil {
		return nil, err
	}
	return &Verifier{
		expected: expected,
		hash:     sha256.New(),
	}, nil
}

// Write implements io.Writer
func (v *Verifier) Write(p []byte) (n int, err error) {
	return v.hash.Write(p)
}

// Verified returns true if the written content matches the expected digest
func (v *Verifier) Verified() bool {
	actual := hex.EncodeToString(v.hash.Sum(nil))
	return actual == v.expected.Hex()
}
