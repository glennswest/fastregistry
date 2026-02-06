package ui

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gwest/fastregistry/internal/certs"
	"github.com/gwest/fastregistry/internal/events"
	"github.com/gwest/fastregistry/internal/releases"
	"github.com/gwest/fastregistry/internal/storage"
	"github.com/gwest/fastregistry/internal/sync"
	"github.com/gwest/fastregistry/pkg/digest"
)

// Handler serves the web UI.
type Handler struct {
	metadata   *storage.MetadataStore
	blobs      *storage.BlobStore
	scheduler  *sync.Scheduler
	releaseMgr *releases.Manager
	certMgr    *certs.Manager
	eventStore *events.Store
	pages      map[string]*template.Template
	partials   *template.Template
	staticFS   http.Handler
}

// View types used by templates.

type PageData struct {
	Title      string
	CurrentNav string
	Breadcrumbs []Breadcrumb
	Content    interface{}
}

type Breadcrumb struct {
	Label string
	URL   string
}

type DashboardData struct {
	RepoCount      int
	TagCount       int
	TotalSize      int64
	TotalSizeHuman string
	RecentRepos    []RepoView
}

type RepoView struct {
	Name             string
	Namespace        string
	FullName         string
	TagCount         int
	TotalSize        int64
	TotalSizeHuman   string
	LastUpdated      time.Time
	LastUpdatedHuman string
}

type RepoDetailData struct {
	FullName         string
	RegistryHost     string
	TagCount         int
	TotalSize        int64
	TotalSizeHuman   string
	LastUpdated      time.Time
	LastUpdatedHuman string
	Tags             []TagView
}

type TagView struct {
	Name        string
	Repo        string
	Digest      string
	DigestShort string
	Size        int64
	SizeHuman   string
	Pushed      time.Time
	PushedHuman string
	MediaType   string
}

type ManifestData struct {
	Repo         string
	Reference    string
	Digest       string
	MediaType    string
	Size         int64
	SizeHuman    string
	Created      time.Time
	CreatedHuman string
	Layers       []LayerView
	Annotations  map[string]string
}

type LayerView struct {
	Digest      string
	DigestShort string
	Size        int64
	SizeHuman   string
	SizePercent float64
}

type ReleasesData struct {
	TotalCount     int
	AvailableCount int
	ClonedCount    int
	ReadyCount     int
	FailedCount    int
	HasCA          bool
	Groups         []releases.MajorMinorGroup
	GroupsJSON     template.JS
	RecentReleases []ReleaseView
	ActiveClones   []releases.CloneProgress
	AllActiveClones []releases.CloneProgress
	CloneHistory   []events.CloneHistoryEntry
	RecentEvents   []events.Event
	DownloadStats  *events.DownloadCounter
	RecentDownloads []events.DownloadRecord
	Available      []releases.Release
	DiscoveryLog   []string
}

type ReleaseView struct {
	Version       string
	Architecture  string
	Tag           string
	State         string
	StateBadge    string
	DiscoveredAt  string
	MajorMinor    string
	ArtifactCount int
	Artifacts     []releases.Artifact
	Error         string
	HasError      bool
}

type ReleaseDetailData struct {
	Release     ReleaseView
	HasManifest bool
	Manifest    ManifestData
	Artifacts   []ArtifactView
	TotalSize   string
}

type ArtifactView struct {
	Name        string
	Type        string
	Size        string
	SHA256      string
	DownloadURL string
}

type SyncData struct {
	Jobs []SyncJobView
}

type SyncJobView struct {
	Name         string
	Running      bool
	LastRun      time.Time
	LastRunHuman string
	Progress     *sync.Progress
	Source       *SyncSourceView
}

type SyncSourceView struct {
	Type          string
	URL           string
	Schedule      string
	Mode          string
	Concurrency   int
	Repositories  []string
	Organizations []string
}

// page name → content template name mapping
var pageContentMap = map[string]string{
	"dashboard":       "dashboard-content",
	"repositories":    "repositories-content",
	"repository":      "repository-content",
	"manifest":        "manifest-content",
	"releases":        "releases-content",
	"release-detail":  "release-detail-content",
	"sync":            "sync-content",
}

// NewHandler creates a new UI handler.
func NewHandler(metadata *storage.MetadataStore, blobs *storage.BlobStore, scheduler *sync.Scheduler, releaseMgr *releases.Manager, certMgr *certs.Manager, eventStore *events.Store) *Handler {
	funcMap := template.FuncMap{
		"inc":        func(i int) int { return i + 1 },
		"humanTime":  humanTime,
		"humanBytes": humanBytes,
		"safeJS":     func(s template.JS) template.JS { return s },
		"join":       strings.Join,
		"humanDuration": func(d time.Duration) string {
			if d < time.Second {
				return "< 1s"
			}
			if d < time.Minute {
				return fmt.Sprintf("%ds", int(d.Seconds()))
			}
			return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
		},
		"downloadSpeed": func(size int64, d time.Duration) string {
			if d == 0 {
				return "-"
			}
			bps := float64(size) / d.Seconds()
			return humanBytes(int64(bps)) + "/s"
		},
		"elapsed": func(t time.Time) string {
			if t.IsZero() {
				return "-"
			}
			d := time.Since(t)
			if d < time.Minute {
				return fmt.Sprintf("%ds", int(d.Seconds()))
			}
			if d < time.Hour {
				return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
			}
			return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
		},
		"div": func(a, b int) int {
			if b == 0 {
				return 0
			}
			return a / b
		},
		"mul": func(a, b int) int {
			return a * b
		},
	}

	// Parse shared templates (layout + partials)
	shared := template.Must(
		template.New("").Funcs(funcMap).ParseFS(content, "templates/layout.html", "templates/partials.html"),
	)

	// Build per-page template sets: clone shared, parse page template,
	// then add a "page-content" definition that calls the page's named block.
	pages := make(map[string]*template.Template)
	for pageName, contentName := range pageContentMap {
		clone := template.Must(shared.Clone())
		template.Must(clone.ParseFS(content, "templates/"+pageName+".html"))
		// Wire "page-content" to call the page-specific content block
		template.Must(clone.New("page-content").Parse(`{{template "` + contentName + `" .}}`))
		pages[pageName] = clone
	}

	// Partials-only template for search results
	partials := template.Must(
		template.New("").Funcs(funcMap).ParseFS(content, "templates/partials.html"),
	)

	staticSub, _ := fs.Sub(content, "static")
	staticHandler := http.StripPrefix("/ui/static/", http.FileServer(http.FS(staticSub)))

	return &Handler{
		metadata:   metadata,
		blobs:      blobs,
		scheduler:  scheduler,
		releaseMgr: releaseMgr,
		certMgr:    certMgr,
		eventStore: eventStore,
		pages:      pages,
		partials:   partials,
		staticFS:   staticHandler,
	}
}

// ServeHTTP routes UI requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/ui")
	if path == "" {
		path = "/"
	}

	switch {
	case strings.HasPrefix(path, "/static/"):
		h.staticFS.ServeHTTP(w, r)

	case path == "/" || path == "":
		h.handleDashboard(w, r)

	case path == "/repositories":
		h.handleRepositories(w, r)

	case strings.HasPrefix(path, "/repositories/search"):
		h.handleSearch(w, r)

	case strings.Contains(path, "/manifests/"):
		h.handleManifest(w, r, path)

	case strings.HasPrefix(path, "/repositories/"):
		h.handleRepository(w, r, path)

	case path == "/releases":
		h.handleReleases(w, r)

	case strings.HasPrefix(path, "/releases/"):
		tag := strings.TrimPrefix(path, "/releases/")
		h.handleReleaseDetail(w, r, tag)

	case path == "/sync":
		h.handleSync(w, r)

	case path == "/sync/status":
		h.handleSyncStatus(w, r)

	case strings.HasPrefix(path, "/sync/trigger/"):
		name := strings.TrimPrefix(path, "/sync/trigger/")
		h.handleSyncTrigger(w, r, name)

	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, page string, data PageData) {
	tmpl, ok := h.pages[page]
	if !ok {
		log.Printf("unknown page: %s", page)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	var err error
	if h.isHTMX(r) {
		err = tmpl.ExecuteTemplate(w, "page-content", data)
	} else {
		err = tmpl.ExecuteTemplate(w, "layout", data)
	}
	if err != nil {
		log.Printf("template error (%s): %v", page, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

// --- Handlers ---

func (h *Handler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	repos, _ := h.metadata.ListRepositories()

	var totalTags int
	var totalSize int64
	var repoViews []RepoView

	for _, repoName := range repos {
		rv := h.buildRepoView(repoName)
		totalTags += rv.TagCount
		totalSize += rv.TotalSize
		repoViews = append(repoViews, rv)
	}

	// Sort by last updated descending
	sort.Slice(repoViews, func(i, j int) bool {
		return repoViews[i].LastUpdated.After(repoViews[j].LastUpdated)
	})

	// Limit to 10 recent
	recent := repoViews
	if len(recent) > 10 {
		recent = recent[:10]
	}

	h.render(w, r, "dashboard", PageData{
		Title:      "Dashboard",
		CurrentNav: "dashboard",
		Breadcrumbs: []Breadcrumb{{Label: "Dashboard", URL: "/ui/"}},
		Content: DashboardData{
			RepoCount:      len(repos),
			TagCount:       totalTags,
			TotalSize:      totalSize,
			TotalSizeHuman: humanBytes(totalSize),
			RecentRepos:    recent,
		},
	})
}

func (h *Handler) handleRepositories(w http.ResponseWriter, r *http.Request) {
	repos, _ := h.metadata.ListRepositories()

	var repoViews []RepoView
	for _, repoName := range repos {
		repoViews = append(repoViews, h.buildRepoView(repoName))
	}

	// Handle sort param
	sortBy := r.URL.Query().Get("sort")
	switch sortBy {
	case "updated":
		sort.Slice(repoViews, func(i, j int) bool {
			return repoViews[i].LastUpdated.After(repoViews[j].LastUpdated)
		})
	case "size":
		sort.Slice(repoViews, func(i, j int) bool {
			return repoViews[i].TotalSize > repoViews[j].TotalSize
		})
	default:
		sort.Slice(repoViews, func(i, j int) bool {
			return repoViews[i].FullName < repoViews[j].FullName
		})
	}

	h.render(w, r, "repositories", PageData{
		Title:      "Repositories",
		CurrentNav: "repositories",
		Breadcrumbs: []Breadcrumb{
			{Label: "Dashboard", URL: "/ui/"},
			{Label: "Repositories", URL: "/ui/repositories"},
		},
		Content: struct{ Repos []RepoView }{Repos: repoViews},
	})
}

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	repos, _ := h.metadata.ListRepositories()

	var matches []RepoView
	for _, repoName := range repos {
		if strings.Contains(strings.ToLower(repoName), q) {
			rv := h.buildRepoView(repoName)
			matches = append(matches, rv)
			if len(matches) >= 8 {
				break
			}
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.partials.ExecuteTemplate(w, "search-results", matches); err != nil {
		log.Printf("search template error: %v", err)
	}
}

func (h *Handler) handleRepository(w http.ResponseWriter, r *http.Request, path string) {
	repoName := strings.TrimPrefix(path, "/repositories/")
	repoName = strings.TrimSuffix(repoName, "/")

	tags, err := h.metadata.ListTags(repoName)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	var tagViews []TagView
	var totalSize int64
	var lastUpdated time.Time

	for _, tag := range tags {
		meta, err := h.metadata.GetManifest(repoName, tag)
		if err != nil {
			continue
		}

		size := meta.Size
		// Sum layer sizes for a better total
		for _, layerDigest := range meta.Layers {
			d := digest.Digest(layerDigest)
			if s, err := h.blobs.Size(d); err == nil {
				size += s
			}
		}

		tv := TagView{
			Name:        tag,
			Repo:        repoName,
			Digest:      string(meta.Digest),
			DigestShort: shortenDigest(string(meta.Digest)),
			Size:        size,
			SizeHuman:   humanBytes(size),
			Pushed:      meta.CreatedAt,
			PushedHuman: humanTime(meta.CreatedAt),
			MediaType:   meta.MediaType,
		}
		tagViews = append(tagViews, tv)
		totalSize += size
		if meta.CreatedAt.After(lastUpdated) {
			lastUpdated = meta.CreatedAt
		}
	}

	registryHost := r.Host
	if registryHost == "" {
		registryHost = "localhost:5000"
	}

	h.render(w, r, "repository", PageData{
		Title:      repoName,
		CurrentNav: "repositories",
		Breadcrumbs: []Breadcrumb{
			{Label: "Dashboard", URL: "/ui/"},
			{Label: "Repositories", URL: "/ui/repositories"},
			{Label: repoName, URL: "/ui/repositories/" + repoName},
		},
		Content: RepoDetailData{
			FullName:         repoName,
			RegistryHost:     registryHost,
			TagCount:         len(tags),
			TotalSize:        totalSize,
			TotalSizeHuman:   humanBytes(totalSize),
			LastUpdated:      lastUpdated,
			LastUpdatedHuman: humanTime(lastUpdated),
			Tags:             tagViews,
		},
	})
}

func (h *Handler) handleManifest(w http.ResponseWriter, r *http.Request, path string) {
	// path: /repositories/{repo...}/manifests/{ref}
	manifestIdx := strings.Index(path, "/manifests/")
	if manifestIdx < 0 {
		http.NotFound(w, r)
		return
	}

	repoName := strings.TrimPrefix(path[:manifestIdx], "/repositories/")
	ref := path[manifestIdx+len("/manifests/"):]

	meta, err := h.metadata.GetManifest(repoName, ref)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	var layers []LayerView
	var maxLayerSize int64
	for _, layerDigest := range meta.Layers {
		d := digest.Digest(layerDigest)
		size, _ := h.blobs.Size(d)
		if size > maxLayerSize {
			maxLayerSize = size
		}
		layers = append(layers, LayerView{
			Digest:      layerDigest,
			DigestShort: shortenDigest(layerDigest),
			Size:        size,
			SizeHuman:   humanBytes(size),
		})
	}

	// Compute size percentages
	for i := range layers {
		if maxLayerSize > 0 {
			layers[i].SizePercent = float64(layers[i].Size) / float64(maxLayerSize) * 100
		}
		if layers[i].SizePercent < 2 {
			layers[i].SizePercent = 2 // Minimum bar width
		}
	}

	h.render(w, r, "manifest", PageData{
		Title:      "Manifest: " + ref,
		CurrentNav: "repositories",
		Breadcrumbs: []Breadcrumb{
			{Label: "Dashboard", URL: "/ui/"},
			{Label: "Repositories", URL: "/ui/repositories"},
			{Label: repoName, URL: "/ui/repositories/" + repoName},
			{Label: ref, URL: ""},
		},
		Content: ManifestData{
			Repo:         repoName,
			Reference:    ref,
			Digest:       string(meta.Digest),
			MediaType:    meta.MediaType,
			Size:         meta.Size,
			SizeHuman:    humanBytes(meta.Size),
			Created:      meta.CreatedAt,
			CreatedHuman: humanTime(meta.CreatedAt),
			Layers:       layers,
			Annotations:  meta.Annotations,
		},
	})
}

func (h *Handler) handleReleases(w http.ResponseWriter, r *http.Request) {
	data := ReleasesData{}

	if h.releaseMgr != nil {
		allReleases, _ := h.releaseMgr.ListReleases()

		for _, rel := range allReleases {
			data.TotalCount++
			switch rel.State {
			case releases.StateAvailable:
				data.AvailableCount++
				data.Available = append(data.Available, rel)
			case releases.StateCloned, releases.StateExtracting:
				data.ClonedCount++
			case releases.StateReady:
				data.ReadyCount++
			case releases.StateFailed:
				data.FailedCount++
			case releases.StateCloning:
				data.AvailableCount++
				if p := h.releaseMgr.GetProgress(rel.Version); p != nil {
					data.ActiveClones = append(data.ActiveClones, *p)
				}
			}
		}

		// Grouped releases
		groups, _ := h.releaseMgr.ListGrouped()
		data.Groups = groups

		// Build JSON array of group names for Alpine.js
		var groupNames []string
		for _, g := range groups {
			groupNames = append(groupNames, g.MajorMinor)
		}
		groupsBytes, _ := json.Marshal(groupNames)
		data.GroupsJSON = template.JS(groupsBytes)

		// Recent releases
		recent, _ := h.releaseMgr.ListRecent(20)
		for _, rel := range recent {
			mm := ""
			parts := strings.SplitN(rel.Version, ".", 3)
			if len(parts) >= 2 {
				mm = parts[0] + "." + parts[1]
			}
			rv := ReleaseView{
				Version:       rel.Version,
				Architecture:  rel.Architecture,
				Tag:           rel.Tag,
				State:         string(rel.State),
				StateBadge:    string(rel.State),
				DiscoveredAt:  humanTime(rel.DiscoveredAt),
				MajorMinor:    mm,
				ArtifactCount: len(rel.Artifacts),
				Artifacts:     rel.Artifacts,
				Error:         rel.Error,
				HasError:      rel.Error != "",
			}
			data.RecentReleases = append(data.RecentReleases, rv)
		}

		// Sort available for clone dropdown
		sort.Slice(data.Available, func(i, j int) bool {
			return data.Available[i].Version > data.Available[j].Version
		})

		data.DiscoveryLog = h.releaseMgr.GetDiscoveryLog()
		data.AllActiveClones = h.releaseMgr.GetAllProgress()
	}

	// Populate event store data
	if h.eventStore != nil {
		data.RecentEvents, _ = h.eventStore.ListEvents(100, "")
		data.CloneHistory, _ = h.eventStore.ListCloneHistory(100)
		data.DownloadStats = h.eventStore.GetDownloadStats()
		data.RecentDownloads, _ = h.eventStore.ListRecentDownloads(100)
	}

	if h.certMgr != nil {
		data.HasCA = h.certMgr.HasCA()
	}

	h.render(w, r, "releases", PageData{
		Title:      "Releases",
		CurrentNav: "releases",
		Breadcrumbs: []Breadcrumb{
			{Label: "Dashboard", URL: "/ui/"},
			{Label: "Releases", URL: "/ui/releases"},
		},
		Content: data,
	})
}

func (h *Handler) handleReleaseDetail(w http.ResponseWriter, r *http.Request, tag string) {
	if h.releaseMgr == nil {
		http.NotFound(w, r)
		return
	}

	rel, err := h.releaseMgr.GetRelease(tag)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	mm := ""
	parts := strings.SplitN(rel.Version, ".", 3)
	if len(parts) >= 2 {
		mm = parts[0] + "." + parts[1]
	}

	rv := ReleaseView{
		Version:       rel.Version,
		Architecture:  rel.Architecture,
		Tag:           rel.Tag,
		State:         string(rel.State),
		StateBadge:    string(rel.State),
		DiscoveredAt:  humanTime(rel.DiscoveredAt),
		MajorMinor:    mm,
		ArtifactCount: len(rel.Artifacts),
		Artifacts:     rel.Artifacts,
		Error:         rel.Error,
		HasError:      rel.Error != "",
	}

	detail := ReleaseDetailData{
		Release: rv,
	}

	// Fetch manifest for cloned/extracting/ready/failed releases
	if rel.State != releases.StateAvailable && rel.State != releases.StateCloning {
		localRepo := h.releaseMgr.LocalRepo()
		if localRepo != "" {
			meta, err := h.metadata.GetManifest(localRepo, tag)
			if err == nil {
				var layers []LayerView
				var maxLayerSize int64
				var totalLayerSize int64
				for _, layerDigest := range meta.Layers {
					d := digest.Digest(layerDigest)
					size, _ := h.blobs.Size(d)
					if size > maxLayerSize {
						maxLayerSize = size
					}
					totalLayerSize += size
					layers = append(layers, LayerView{
						Digest:      layerDigest,
						DigestShort: shortenDigest(layerDigest),
						Size:        size,
						SizeHuman:   humanBytes(size),
					})
				}
				for i := range layers {
					if maxLayerSize > 0 {
						layers[i].SizePercent = float64(layers[i].Size) / float64(maxLayerSize) * 100
					}
					if layers[i].SizePercent < 2 {
						layers[i].SizePercent = 2
					}
				}

				detail.HasManifest = true
				detail.Manifest = ManifestData{
					Repo:         localRepo,
					Reference:    tag,
					Digest:       string(meta.Digest),
					MediaType:    meta.MediaType,
					Size:         meta.Size,
					SizeHuman:    humanBytes(meta.Size),
					Created:      meta.CreatedAt,
					CreatedHuman: humanTime(meta.CreatedAt),
					Layers:       layers,
					Annotations:  meta.Annotations,
				}
				detail.TotalSize = humanBytes(totalLayerSize + meta.Size)
			}
		}
	}

	// Build artifact views for ready releases
	if rel.State == releases.StateReady && len(rel.Artifacts) > 0 {
		for _, a := range rel.Artifacts {
			av := ArtifactView{
				Name:        a.Name,
				Type:        a.Type,
				Size:        humanBytes(a.Size),
				SHA256:      shortenDigest("sha256:" + a.SHA256),
				DownloadURL: "/files/releases/" + rel.Version + "/" + a.Name,
			}
			detail.Artifacts = append(detail.Artifacts, av)
		}
	}

	h.render(w, r, "release-detail", PageData{
		Title:      "Release " + rel.Version,
		CurrentNav: "releases",
		Breadcrumbs: []Breadcrumb{
			{Label: "Dashboard", URL: "/ui/"},
			{Label: "Releases", URL: "/ui/releases"},
			{Label: rel.Version, URL: ""},
		},
		Content: detail,
	})
}

// --- Helpers ---

func (h *Handler) buildRepoView(repoName string) RepoView {
	namespace, name := splitRepoName(repoName)
	tags, _ := h.metadata.ListTags(repoName)

	var totalSize int64
	var lastUpdated time.Time

	for _, tag := range tags {
		meta, err := h.metadata.GetManifest(repoName, tag)
		if err != nil {
			continue
		}
		totalSize += meta.Size
		if meta.CreatedAt.After(lastUpdated) {
			lastUpdated = meta.CreatedAt
		}
	}

	// Fall back to repo metadata for lastUpdated
	if lastUpdated.IsZero() {
		if rm, err := h.metadata.GetRepoMeta(repoName); err == nil {
			lastUpdated = rm.UpdatedAt
		}
	}

	return RepoView{
		Name:             name,
		Namespace:        namespace,
		FullName:         repoName,
		TagCount:         len(tags),
		TotalSize:        totalSize,
		TotalSizeHuman:   humanBytes(totalSize),
		LastUpdated:      lastUpdated,
		LastUpdatedHuman: humanTime(lastUpdated),
	}
}

func splitRepoName(name string) (namespace, repo string) {
	idx := strings.LastIndex(name, "/")
	if idx < 0 {
		return "", name
	}
	return name[:idx], name[idx+1:]
}

func shortenDigest(d string) string {
	if strings.HasPrefix(d, "sha256:") && len(d) > 19 {
		return "sha256:" + d[7:19]
	}
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

func humanBytes(b int64) string {
	if b == 0 {
		return "0 B"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	suffixes := []string{"KB", "MB", "GB", "TB"}
	return fmt.Sprintf("%.1f %s", float64(b)/float64(div), suffixes[exp])
}

func humanTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	case d < 30*24*time.Hour:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	default:
		return t.Format("Jan 2, 2006")
	}
}

func (h *Handler) handleSync(w http.ResponseWriter, r *http.Request) {
	data := SyncData{}

	if h.scheduler != nil {
		statuses := h.scheduler.GetAllJobStatuses()
		for _, s := range statuses {
			jv := SyncJobView{
				Name:         s.Name,
				Running:      s.Running,
				LastRun:      s.LastRun,
				LastRunHuman: humanTime(s.LastRun),
				Progress:     s.Progress,
			}
			if s.Source != nil {
				jv.Source = &SyncSourceView{
					Type:          s.Source.Type,
					URL:           s.Source.URL,
					Schedule:      s.Source.Schedule,
					Mode:          s.Source.Mode,
					Concurrency:   s.Source.Concurrency,
					Repositories:  s.Source.Repositories,
					Organizations: s.Source.Organizations,
				}
			}
			data.Jobs = append(data.Jobs, jv)
		}
	}

	h.render(w, r, "sync", PageData{
		Title:      "Sync",
		CurrentNav: "sync",
		Breadcrumbs: []Breadcrumb{
			{Label: "Dashboard", URL: "/ui/"},
			{Label: "Sync", URL: "/ui/sync"},
		},
		Content: data,
	})
}

func (h *Handler) handleSyncStatus(w http.ResponseWriter, r *http.Request) {
	data := SyncData{}

	if h.scheduler != nil {
		statuses := h.scheduler.GetAllJobStatuses()
		for _, s := range statuses {
			jv := SyncJobView{
				Name:         s.Name,
				Running:      s.Running,
				LastRun:      s.LastRun,
				LastRunHuman: humanTime(s.LastRun),
				Progress:     s.Progress,
			}
			if s.Source != nil {
				jv.Source = &SyncSourceView{
					Type:          s.Source.Type,
					URL:           s.Source.URL,
					Schedule:      s.Source.Schedule,
					Mode:          s.Source.Mode,
					Concurrency:   s.Source.Concurrency,
					Repositories:  s.Source.Repositories,
					Organizations: s.Source.Organizations,
				}
			}
			data.Jobs = append(data.Jobs, jv)
		}
	}

	tmpl, ok := h.pages["sync"]
	if !ok {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "sync-status-partial", PageData{Content: data}); err != nil {
		log.Printf("sync status template error: %v", err)
	}
}

func (h *Handler) handleSyncTrigger(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if h.scheduler == nil {
		http.Error(w, "Sync not configured", http.StatusNotFound)
		return
	}

	if err := h.scheduler.TriggerSync(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
}
