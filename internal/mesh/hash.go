package mesh

import (
	"hash/crc32"
	"sort"
	"sync"
)

// ConsistentHash implements consistent hashing with virtual nodes
type ConsistentHash struct {
	replicas int               // Number of virtual nodes per real node
	ring     []uint32          // Sorted hash ring
	nodes    map[uint32]string // Hash -> node ID
	members  map[string]bool   // Set of member node IDs
	mu       sync.RWMutex
}

// NewConsistentHash creates a new consistent hash ring
// replicas is the number of virtual nodes per real node (higher = better distribution)
func NewConsistentHash(replicas int) *ConsistentHash {
	if replicas < 1 {
		replicas = 150 // Default: 150 virtual nodes per real node
	}
	return &ConsistentHash{
		replicas: replicas,
		nodes:    make(map[uint32]string),
		members:  make(map[string]bool),
	}
}

// Add adds a node to the hash ring
func (c *ConsistentHash) Add(nodeID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.members[nodeID] {
		return // Already exists
	}

	c.members[nodeID] = true

	// Add virtual nodes
	for i := 0; i < c.replicas; i++ {
		hash := c.hash(virtualNodeKey(nodeID, i))
		c.ring = append(c.ring, hash)
		c.nodes[hash] = nodeID
	}

	// Keep ring sorted for binary search
	sort.Slice(c.ring, func(i, j int) bool {
		return c.ring[i] < c.ring[j]
	})
}

// Remove removes a node from the hash ring
func (c *ConsistentHash) Remove(nodeID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.members[nodeID] {
		return
	}

	delete(c.members, nodeID)

	// Remove virtual nodes
	for i := 0; i < c.replicas; i++ {
		hash := c.hash(virtualNodeKey(nodeID, i))
		delete(c.nodes, hash)
	}

	// Rebuild ring without removed hashes
	newRing := make([]uint32, 0, len(c.ring)-c.replicas)
	for _, h := range c.ring {
		if _, ok := c.nodes[h]; ok {
			newRing = append(newRing, h)
		}
	}
	c.ring = newRing
}

// Get returns the node responsible for the given key
func (c *ConsistentHash) Get(key string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.ring) == 0 {
		return ""
	}

	hash := c.hash(key)

	// Binary search for the first node with hash >= key hash
	idx := sort.Search(len(c.ring), func(i int) bool {
		return c.ring[i] >= hash
	})

	// Wrap around if we're past the end
	if idx >= len(c.ring) {
		idx = 0
	}

	return c.nodes[c.ring[idx]]
}

// GetN returns N nodes responsible for the given key (for replication)
func (c *ConsistentHash) GetN(key string, n int) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.ring) == 0 {
		return nil
	}

	if n > len(c.members) {
		n = len(c.members)
	}

	hash := c.hash(key)

	// Find starting position
	idx := sort.Search(len(c.ring), func(i int) bool {
		return c.ring[i] >= hash
	})

	if idx >= len(c.ring) {
		idx = 0
	}

	// Collect unique nodes
	seen := make(map[string]bool)
	var result []string

	for len(result) < n {
		nodeID := c.nodes[c.ring[idx]]
		if !seen[nodeID] {
			seen[nodeID] = true
			result = append(result, nodeID)
		}
		idx = (idx + 1) % len(c.ring)
	}

	return result
}

// Members returns all node IDs in the ring
func (c *ConsistentHash) Members() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make([]string, 0, len(c.members))
	for nodeID := range c.members {
		result = append(result, nodeID)
	}
	return result
}

// Size returns the number of nodes in the ring
func (c *ConsistentHash) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.members)
}

// hash computes a hash for the given key
func (c *ConsistentHash) hash(key string) uint32 {
	return crc32.ChecksumIEEE([]byte(key))
}

// virtualNodeKey generates a key for a virtual node
func virtualNodeKey(nodeID string, replica int) string {
	return nodeID + "#" + string(rune(replica))
}
