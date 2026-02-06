package mesh

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gwest/fastregistry/internal/storage"
	"github.com/gwest/fastregistry/pkg/digest"
)

// Replicator handles async replication of blobs to replica nodes
type Replicator struct {
	cluster *Cluster
	blobs   *storage.BlobStore
	client  *http.Client
	queue   chan replicationJob
	wg      sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelFunc
}

type replicationJob struct {
	digest  digest.Digest
	targets []string // Node IDs to replicate to
}

// NewReplicator creates a new replicator
func NewReplicator(cluster *Cluster, blobs *storage.BlobStore, workers int) *Replicator {
	if workers < 1 {
		workers = 4
	}

	ctx, cancel := context.WithCancel(context.Background())

	r := &Replicator{
		cluster: cluster,
		blobs:   blobs,
		client: &http.Client{
			Timeout: 5 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		queue:  make(chan replicationJob, 10000),
		ctx:    ctx,
		cancel: cancel,
	}

	// Start workers
	for i := 0; i < workers; i++ {
		r.wg.Add(1)
		go r.worker()
	}

	return r
}

// Replicate queues a blob for replication to replica nodes
func (r *Replicator) Replicate(dgst digest.Digest) {
	key := string(dgst)

	// Get replica nodes (excluding self)
	targets := r.cluster.GetReplicaNodes(key)
	localID := r.cluster.LocalNode().NodeID

	var replicaTargets []string
	for _, nodeID := range targets {
		if nodeID != localID {
			replicaTargets = append(replicaTargets, nodeID)
		}
	}

	if len(replicaTargets) == 0 {
		return
	}

	select {
	case r.queue <- replicationJob{digest: dgst, targets: replicaTargets}:
	default:
		log.Printf("Replication queue full, dropping job for %s", dgst.ShortHex())
	}
}

// Stop stops the replicator
func (r *Replicator) Stop() {
	r.cancel()
	close(r.queue)
	r.wg.Wait()
}

func (r *Replicator) worker() {
	defer r.wg.Done()

	for {
		select {
		case <-r.ctx.Done():
			return
		case job, ok := <-r.queue:
			if !ok {
				return
			}
			r.processJob(job)
		}
	}
}

func (r *Replicator) processJob(job replicationJob) {
	// Get blob data
	reader, size, err := r.blobs.Get(job.digest)
	if err != nil {
		log.Printf("Replication failed for %s: %v", job.digest.ShortHex(), err)
		return
	}
	defer reader.Close()

	// Read blob into memory for multi-target replication
	data, err := io.ReadAll(reader)
	if err != nil {
		log.Printf("Replication failed for %s: %v", job.digest.ShortHex(), err)
		return
	}

	// Replicate to each target
	for _, nodeID := range job.targets {
		node := r.cluster.GetNode(nodeID)
		if node == nil || node.RegistryAddr == "" {
			continue
		}

		if err := r.replicateToNode(node, job.digest, data, size); err != nil {
			log.Printf("Replication to %s failed for %s: %v", nodeID, job.digest.ShortHex(), err)
		}
	}
}

func (r *Replicator) replicateToNode(node *NodeMeta, dgst digest.Digest, data []byte, size int64) error {
	url := fmt.Sprintf("http://%s/v2/_internal/blobs/%s", node.RegistryAddr, dgst)

	ctx, cancel := context.WithTimeout(r.ctx, 2*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, nil)
	if err != nil {
		return err
	}

	req.Body = io.NopCloser(newBytesReader(data))
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("node returned status %d", resp.StatusCode)
	}

	return nil
}

// bytesReader is a resettable bytes reader
type bytesReader struct {
	data   []byte
	offset int
}

func newBytesReader(data []byte) *bytesReader {
	return &bytesReader{data: data}
}

func (r *bytesReader) Read(p []byte) (n int, err error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	n = copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}

// AntiEntropySync performs anti-entropy synchronization
// This runs periodically to ensure replicas are consistent
type AntiEntropySync struct {
	cluster    *Cluster
	blobs      *storage.BlobStore
	metadata   *storage.MetadataStore
	replicator *Replicator
	interval   time.Duration
	ctx        context.Context
	cancel     context.CancelFunc
}

// NewAntiEntropySync creates a new anti-entropy synchronizer
func NewAntiEntropySync(cluster *Cluster, blobs *storage.BlobStore, metadata *storage.MetadataStore, replicator *Replicator) *AntiEntropySync {
	ctx, cancel := context.WithCancel(context.Background())

	return &AntiEntropySync{
		cluster:    cluster,
		blobs:      blobs,
		metadata:   metadata,
		replicator: replicator,
		interval:   5 * time.Minute,
		ctx:        ctx,
		cancel:     cancel,
	}
}

// Start starts the anti-entropy sync process
func (s *AntiEntropySync) Start() {
	go s.run()
}

// Stop stops the anti-entropy sync process
func (s *AntiEntropySync) Stop() {
	s.cancel()
}

func (s *AntiEntropySync) run() {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.sync()
		}
	}
}

func (s *AntiEntropySync) sync() {
	// Get all repositories
	repos, err := s.metadata.ListRepositories()
	if err != nil {
		log.Printf("Anti-entropy: failed to list repos: %v", err)
		return
	}

	localID := s.cluster.LocalNode().NodeID
	syncedCount := 0

	for _, repo := range repos {
		// Get all blobs for this repo
		digests, err := s.metadata.GetRepoBlobs(repo)
		if err != nil {
			continue
		}

		for _, dgst := range digests {
			key := string(dgst)

			// Check if we should have this blob
			replicas := s.cluster.GetReplicaNodes(key)
			shouldHave := false
			for _, nodeID := range replicas {
				if nodeID == localID {
					shouldHave = true
					break
				}
			}

			if shouldHave && !s.blobs.Exists(dgst) {
				// We should have it but don't - pull from another replica
				for _, nodeID := range replicas {
					if nodeID == localID {
						continue
					}

					node := s.cluster.GetNode(nodeID)
					if node == nil || node.RegistryAddr == "" {
						continue
					}

					if err := s.pullFromNode(node, dgst); err == nil {
						syncedCount++
						break
					}
				}
			}
		}
	}

	if syncedCount > 0 {
		log.Printf("Anti-entropy: synced %d blobs", syncedCount)
	}
}

func (s *AntiEntropySync) pullFromNode(node *NodeMeta, dgst digest.Digest) error {
	url := fmt.Sprintf("http://%s/v2/_internal/blobs/%s", node.RegistryAddr, dgst)

	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("node returned status %d", resp.StatusCode)
	}

	return s.blobs.Put(dgst, resp.Body, resp.ContentLength)
}
