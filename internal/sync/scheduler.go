package sync

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gwest/fastregistry/config"
	"github.com/gwest/fastregistry/internal/storage"
)

// Scheduler manages sync jobs
type Scheduler struct {
	sources  []config.SyncSource
	blobs    *storage.BlobStore
	metadata *storage.MetadataStore
	jobs     map[string]*syncJob
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
}

type syncJob struct {
	source   config.SyncSource
	client   *QuayClient
	lastRun  time.Time
	running  bool
	progress *Progress
}

// NewScheduler creates a new sync scheduler
func NewScheduler(sources []config.SyncSource, blobs *storage.BlobStore, metadata *storage.MetadataStore) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())

	s := &Scheduler{
		sources:  sources,
		blobs:    blobs,
		metadata: metadata,
		jobs:     make(map[string]*syncJob),
		ctx:      ctx,
		cancel:   cancel,
	}

	// Initialize jobs
	for _, src := range sources {
		var client *QuayClient
		switch src.Type {
		case "quay":
			client = NewQuayClient(src, blobs, metadata)
		default:
			log.Printf("Warning: unknown sync source type: %s", src.Type)
			continue
		}

		s.jobs[src.Name] = &syncJob{
			source: src,
			client: client,
		}
	}

	return s
}

// Start begins the scheduler
func (s *Scheduler) Start() {
	go s.run()
}

// Stop stops the scheduler
func (s *Scheduler) Stop() {
	s.cancel()
}

func (s *Scheduler) run() {
	// Check every minute for jobs that need to run
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	// Run initial sync for "once" mode jobs
	for name, job := range s.jobs {
		if job.source.Mode == "once" && job.lastRun.IsZero() {
			go s.runJob(name)
		}
	}

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.checkJobs()
		}
	}
}

func (s *Scheduler) checkJobs() {
	now := time.Now()

	for name, job := range s.jobs {
		if job.running || job.source.Mode != "continuous" {
			continue
		}

		// Parse cron-like schedule (simplified: */15 * * * * = every 15 min)
		if shouldRun(job.source.Schedule, job.lastRun, now) {
			go s.runJob(name)
		}
	}
}

// shouldRun checks if a job should run based on its schedule
func shouldRun(schedule string, lastRun, now time.Time) bool {
	if schedule == "" {
		return false
	}

	// Parse simplified cron: */N * * * * means every N minutes
	parts := strings.Fields(schedule)
	if len(parts) == 0 {
		return false
	}

	// Handle */N format for first field (minutes)
	if strings.HasPrefix(parts[0], "*/") {
		intervalStr := strings.TrimPrefix(parts[0], "*/")
		interval, err := strconv.Atoi(intervalStr)
		if err != nil {
			return false
		}

		// Check if enough time has passed since last run
		if lastRun.IsZero() {
			return true
		}
		return now.Sub(lastRun) >= time.Duration(interval)*time.Minute
	}

	return false
}

func (s *Scheduler) runJob(name string) {
	s.mu.Lock()
	job, ok := s.jobs[name]
	if !ok || job.running {
		s.mu.Unlock()
		return
	}
	job.running = true
	job.progress = NewProgress()
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		job.running = false
		job.lastRun = time.Now()
		s.mu.Unlock()
	}()

	log.Printf("Starting sync job: %s", name)

	ctx, cancel := context.WithTimeout(s.ctx, 4*time.Hour)
	defer cancel()

	// List repositories
	repos, err := job.client.ListRepositories(ctx)
	if err != nil {
		log.Printf("Error listing repositories for %s: %v", name, err)
		return
	}

	job.progress.TotalRepos = len(repos)
	log.Printf("Found %d repositories to sync", len(repos))

	// Sync each repository
	for _, repo := range repos {
		select {
		case <-ctx.Done():
			log.Printf("Sync job %s cancelled", name)
			return
		default:
		}

		if err := job.client.SyncRepository(ctx, repo, job.progress); err != nil {
			log.Printf("Error syncing %s: %v", repo, err)
			job.progress.FailedRepos++
			continue
		}
		job.progress.SyncedRepos++
	}

	duration := time.Since(job.progress.StartTime)
	log.Printf("Sync job %s completed: %d/%d repos, %d/%d tags, %d bytes in %v",
		name,
		job.progress.SyncedRepos, job.progress.TotalRepos,
		job.progress.SyncedTags, job.progress.TotalTags,
		job.progress.BytesSynced,
		duration,
	)
}

// TriggerSync manually triggers a sync job
func (s *Scheduler) TriggerSync(name string) error {
	s.mu.Lock()
	job, ok := s.jobs[name]
	if !ok {
		s.mu.Unlock()
		return ErrJobNotFound
	}
	if job.running {
		s.mu.Unlock()
		return ErrJobRunning
	}
	s.mu.Unlock()

	go s.runJob(name)
	return nil
}

// GetJobStatus returns the status of a sync job
func (s *Scheduler) GetJobStatus(name string) (*JobStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[name]
	if !ok {
		return nil, ErrJobNotFound
	}

	status := &JobStatus{
		Name:    name,
		Running: job.running,
		LastRun: job.lastRun,
	}

	if job.progress != nil {
		status.Progress = job.progress
	}

	return status, nil
}

// ListJobs returns all configured sync jobs
func (s *Scheduler) ListJobs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var names []string
	for name := range s.jobs {
		names = append(names, name)
	}
	return names
}

// JobStatus represents the status of a sync job
type JobStatus struct {
	Name     string
	Running  bool
	LastRun  time.Time
	Progress *Progress
}

// Errors
var (
	ErrJobNotFound = newError("job not found")
	ErrJobRunning  = newError("job already running")
)

type syncError struct {
	msg string
}

func newError(msg string) error {
	return &syncError{msg: msg}
}

func (e *syncError) Error() string {
	return e.msg
}
