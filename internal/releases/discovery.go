package releases

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Discovery queries upstream Quay.io for available OpenShift releases
type Discovery struct {
	upstream   string // e.g. "quay.io"
	repository string // e.g. "openshift-release-dev/ocp-release"
	client     *http.Client
	logFunc    func(string, ...interface{})
}

type quayTagsResponse struct {
	Tags          []quayTag `json:"tags"`
	Page          int       `json:"page"`
	HasAdditional bool      `json:"has_additional"`
}

type quayTag struct {
	Name           string `json:"name"`
	ManifestDigest string `json:"manifest_digest"`
	Size           int    `json:"size"`
}

var versionTagRe = regexp.MustCompile(`^(\d+\.\d+\.\d+)-(.+)$`)

// NewDiscovery creates a new upstream discovery client
func NewDiscovery(upstream, repository string) *Discovery {
	return &Discovery{
		upstream:   upstream,
		repository: repository,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// fetchTagsPage fetches a single page of tags from the Quay API
func (d *Discovery) fetchTagsPage(ctx context.Context, url string) (*quayTagsResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying upstream tags: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	var result quayTagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

// fetchAllTags fetches all pages of tags matching the given base URL
func (d *Discovery) fetchAllTags(ctx context.Context, baseURL string) ([]quayTag, error) {
	const maxPages = 200
	var allTags []quayTag

	logf := d.logFunc
	if logf == nil {
		logf = log.Printf
	}

	for page := 1; page <= maxPages; page++ {
		url := fmt.Sprintf("%s&page=%d&onlyActiveTags=true", baseURL, page)
		result, err := d.fetchTagsPage(ctx, url)
		if err != nil {
			if len(allTags) > 0 {
				logf("Discovery: stopping at page %d after error (got %d tags so far): %v", page, len(allTags), err)
				break
			}
			return nil, err
		}
		allTags = append(allTags, result.Tags...)
		if !result.HasAdditional {
			break
		}
		if page%10 == 0 {
			logf("Discovery: fetched %d pages (%d tags so far)...", page, len(allTags))
		}
	}
	return allTags, nil
}

// ListUpstreamVersions returns all available upstream versions for the given architectures
func (d *Discovery) ListUpstreamVersions(ctx context.Context, archs []string) ([]UpstreamRelease, error) {
	baseURL := fmt.Sprintf("https://%s/api/v1/repository/%s/tag/?limit=100", d.upstream, d.repository)

	tags, err := d.fetchAllTags(ctx, baseURL)
	if err != nil {
		return nil, err
	}

	archSet := make(map[string]bool)
	for _, a := range archs {
		archSet[a] = true
	}

	var releases []UpstreamRelease
	for _, tag := range tags {
		m := versionTagRe.FindStringSubmatch(tag.Name)
		if m == nil {
			continue
		}
		version, arch := m[1], m[2]
		if !archSet[arch] {
			continue
		}
		releases = append(releases, UpstreamRelease{
			Version:      version,
			Architecture: arch,
			Tag:          tag.Name,
			Digest:       tag.ManifestDigest,
		})
	}

	sort.Slice(releases, func(i, j int) bool {
		return compareSemver(releases[i].Version, releases[j].Version) > 0
	})

	return releases, nil
}

// ListUpstreamForMajorMinor returns versions matching a major.minor prefix
func (d *Discovery) ListUpstreamForMajorMinor(ctx context.Context, majorMinor string, archs []string) ([]UpstreamRelease, error) {
	baseURL := fmt.Sprintf("https://%s/api/v1/repository/%s/tag/?filter_tag_name=like:%s&limit=100",
		d.upstream, d.repository, majorMinor)

	tags, err := d.fetchAllTags(ctx, baseURL)
	if err != nil {
		return nil, err
	}

	archSet := make(map[string]bool)
	for _, a := range archs {
		archSet[a] = true
	}

	var releases []UpstreamRelease
	prefix := majorMinor + "."
	for _, tag := range tags {
		m := versionTagRe.FindStringSubmatch(tag.Name)
		if m == nil {
			continue
		}
		version, arch := m[1], m[2]
		if !strings.HasPrefix(version, prefix) {
			continue
		}
		if !archSet[arch] {
			continue
		}
		releases = append(releases, UpstreamRelease{
			Version:      version,
			Architecture: arch,
			Tag:          tag.Name,
			Digest:       tag.ManifestDigest,
		})
	}

	sort.Slice(releases, func(i, j int) bool {
		return compareSemver(releases[i].Version, releases[j].Version) > 0
	})

	return releases, nil
}

// UpstreamRelease represents a discovered upstream release
type UpstreamRelease struct {
	Version      string `json:"version"`
	Architecture string `json:"architecture"`
	Tag          string `json:"tag"`
	Digest       string `json:"digest"`
}

// compareSemver compares two semver strings, returns positive if a > b
func compareSemver(a, b string) int {
	ap := parseSemver(a)
	bp := parseSemver(b)
	for i := 0; i < 3; i++ {
		if ap[i] != bp[i] {
			return ap[i] - bp[i]
		}
	}
	return 0
}

func parseSemver(v string) [3]int {
	var parts [3]int
	for i, s := range strings.SplitN(v, ".", 3) {
		if i >= 3 {
			break
		}
		parts[i], _ = strconv.Atoi(s)
	}
	return parts
}
