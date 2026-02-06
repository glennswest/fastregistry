package mesh

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"
)

// Cluster manages the mesh cluster using gossip protocol
type Cluster struct {
	config     *ClusterConfig
	memberlist *memberlist.Memberlist
	hash       *ConsistentHash
	localNode  *NodeMeta
	delegates  *clusterDelegate
	events     *clusterEvents
	mu         sync.RWMutex

	// Callbacks
	onJoin  func(nodeID string)
	onLeave func(nodeID string)
}

// ClusterConfig holds cluster configuration
type ClusterConfig struct {
	NodeID            string        // Unique node identifier
	BindAddr          string        // Address to bind for gossip (e.g., ":7946")
	AdvertiseAddr     string        // Address to advertise to other nodes
	AdvertisePort     int           // Port to advertise
	Peers             []string      // Initial peers to join
	ReplicationFactor int           // Number of replicas for data
	SecretKey         []byte        // Optional encryption key
	RegistryAddr      string        // This node's registry address (for routing)
}

// NodeMeta holds metadata about a node
type NodeMeta struct {
	NodeID       string `json:"node_id"`
	RegistryAddr string `json:"registry_addr"` // Address for registry API
	GRPCAddr     string `json:"grpc_addr"`     // Address for internal gRPC
	JoinedAt     int64  `json:"joined_at"`
}

// NewCluster creates a new mesh cluster
func NewCluster(cfg *ClusterConfig) (*Cluster, error) {
	c := &Cluster{
		config: cfg,
		hash:   NewConsistentHash(150),
		localNode: &NodeMeta{
			NodeID:       cfg.NodeID,
			RegistryAddr: cfg.RegistryAddr,
			JoinedAt:     time.Now().Unix(),
		},
	}

	// Create memberlist config
	mlConfig := memberlist.DefaultLANConfig()
	mlConfig.Name = cfg.NodeID
	mlConfig.BindPort = 7946
	mlConfig.AdvertisePort = cfg.AdvertisePort

	// Parse bind address
	if cfg.BindAddr != "" {
		host, port, err := net.SplitHostPort(cfg.BindAddr)
		if err == nil {
			if host != "" {
				mlConfig.BindAddr = host
			}
			if port != "" {
				fmt.Sscanf(port, "%d", &mlConfig.BindPort)
			}
		}
	}

	if cfg.AdvertiseAddr != "" {
		mlConfig.AdvertiseAddr = cfg.AdvertiseAddr
	}

	if cfg.AdvertisePort > 0 {
		mlConfig.AdvertisePort = cfg.AdvertisePort
	} else {
		mlConfig.AdvertisePort = mlConfig.BindPort
	}

	// Set up encryption if key is provided
	if len(cfg.SecretKey) > 0 {
		mlConfig.SecretKey = cfg.SecretKey
	}

	// Set up delegates for metadata and events
	c.delegates = &clusterDelegate{cluster: c}
	c.events = &clusterEvents{cluster: c}

	mlConfig.Delegate = c.delegates
	mlConfig.Events = c.events

	// Reduce logging
	mlConfig.LogOutput = &logWriter{prefix: "memberlist"}

	// Create memberlist
	ml, err := memberlist.Create(mlConfig)
	if err != nil {
		return nil, fmt.Errorf("creating memberlist: %w", err)
	}

	c.memberlist = ml

	// Add self to hash ring
	c.hash.Add(cfg.NodeID)

	return c, nil
}

// Join joins the cluster by connecting to known peers
func (c *Cluster) Join(peers []string) error {
	if len(peers) == 0 {
		peers = c.config.Peers
	}

	if len(peers) == 0 {
		log.Printf("No peers specified, running as single node")
		return nil
	}

	n, err := c.memberlist.Join(peers)
	if err != nil {
		return fmt.Errorf("joining cluster: %w", err)
	}

	log.Printf("Joined cluster via %d nodes", n)
	return nil
}

// Leave gracefully leaves the cluster
func (c *Cluster) Leave(timeout time.Duration) error {
	return c.memberlist.Leave(timeout)
}

// Shutdown shuts down the cluster
func (c *Cluster) Shutdown() error {
	return c.memberlist.Shutdown()
}

// LocalNode returns the local node's metadata
func (c *Cluster) LocalNode() *NodeMeta {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.localNode
}

// Members returns all cluster members
func (c *Cluster) Members() []*NodeMeta {
	c.mu.RLock()
	defer c.mu.RUnlock()

	members := c.memberlist.Members()
	result := make([]*NodeMeta, 0, len(members))

	for _, m := range members {
		meta := &NodeMeta{}
		if len(m.Meta) > 0 {
			json.Unmarshal(m.Meta, meta)
		}
		if meta.NodeID == "" {
			meta.NodeID = m.Name
		}
		result = append(result, meta)
	}

	return result
}

// GetNode returns metadata for a specific node
func (c *Cluster) GetNode(nodeID string) *NodeMeta {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, m := range c.memberlist.Members() {
		if m.Name == nodeID {
			meta := &NodeMeta{}
			if len(m.Meta) > 0 {
				json.Unmarshal(m.Meta, meta)
			}
			if meta.NodeID == "" {
				meta.NodeID = m.Name
			}
			return meta
		}
	}

	return nil
}

// GetPrimaryNode returns the primary node for a given key (digest)
func (c *Cluster) GetPrimaryNode(key string) string {
	return c.hash.Get(key)
}

// GetReplicaNodes returns nodes that should hold replicas for a key
func (c *Cluster) GetReplicaNodes(key string) []string {
	return c.hash.GetN(key, c.config.ReplicationFactor)
}

// IsLocalPrimary returns true if this node is the primary for a key
func (c *Cluster) IsLocalPrimary(key string) bool {
	return c.hash.Get(key) == c.config.NodeID
}

// NumMembers returns the number of cluster members
func (c *Cluster) NumMembers() int {
	return c.memberlist.NumMembers()
}

// OnJoin sets a callback for when a node joins
func (c *Cluster) OnJoin(fn func(nodeID string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onJoin = fn
}

// OnLeave sets a callback for when a node leaves
func (c *Cluster) OnLeave(fn func(nodeID string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onLeave = fn
}

// clusterDelegate implements memberlist.Delegate
type clusterDelegate struct {
	cluster *Cluster
}

func (d *clusterDelegate) NodeMeta(limit int) []byte {
	d.cluster.mu.RLock()
	defer d.cluster.mu.RUnlock()

	data, _ := json.Marshal(d.cluster.localNode)
	if len(data) > limit {
		return nil
	}
	return data
}

func (d *clusterDelegate) NotifyMsg(msg []byte) {
	// Handle custom messages if needed
}

func (d *clusterDelegate) GetBroadcasts(overhead, limit int) [][]byte {
	return nil
}

func (d *clusterDelegate) LocalState(join bool) []byte {
	return nil
}

func (d *clusterDelegate) MergeRemoteState(buf []byte, join bool) {
}

// clusterEvents implements memberlist.EventDelegate
type clusterEvents struct {
	cluster *Cluster
}

func (e *clusterEvents) NotifyJoin(node *memberlist.Node) {
	log.Printf("Node joined: %s (%s)", node.Name, node.Addr)

	e.cluster.hash.Add(node.Name)

	e.cluster.mu.RLock()
	callback := e.cluster.onJoin
	e.cluster.mu.RUnlock()

	if callback != nil {
		go callback(node.Name)
	}
}

func (e *clusterEvents) NotifyLeave(node *memberlist.Node) {
	log.Printf("Node left: %s (%s)", node.Name, node.Addr)

	e.cluster.hash.Remove(node.Name)

	e.cluster.mu.RLock()
	callback := e.cluster.onLeave
	e.cluster.mu.RUnlock()

	if callback != nil {
		go callback(node.Name)
	}
}

func (e *clusterEvents) NotifyUpdate(node *memberlist.Node) {
	log.Printf("Node updated: %s", node.Name)
}

// logWriter filters memberlist logs
type logWriter struct {
	prefix string
}

func (w *logWriter) Write(p []byte) (n int, err error) {
	// Only log important messages
	// log.Printf("[%s] %s", w.prefix, string(p))
	return len(p), nil
}
