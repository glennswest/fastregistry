package storage

import (
	"container/list"
	"sync"
	"time"
)

// LRUCache is a thread-safe LRU cache for manifest metadata
type LRUCache struct {
	capacity int
	items    map[string]*list.Element
	lru      *list.List
	mu       sync.RWMutex
}

type cacheEntry struct {
	key       string
	value     interface{}
	expiresAt time.Time
}

// NewLRUCache creates a new LRU cache with the given capacity
func NewLRUCache(capacity int) *LRUCache {
	return &LRUCache{
		capacity: capacity,
		items:    make(map[string]*list.Element),
		lru:      list.New(),
	}
}

// Get retrieves a value from the cache
func (c *LRUCache) Get(key string) (interface{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		entry := elem.Value.(*cacheEntry)

		// Check expiration
		if !entry.expiresAt.IsZero() && time.Now().After(entry.expiresAt) {
			c.lru.Remove(elem)
			delete(c.items, key)
			return nil, false
		}

		// Move to front (most recently used)
		c.lru.MoveToFront(elem)
		return entry.value, true
	}
	return nil, false
}

// Set adds or updates a value in the cache
func (c *LRUCache) Set(key string, value interface{}) {
	c.SetWithTTL(key, value, 0)
}

// SetWithTTL adds or updates a value with a TTL
func (c *LRUCache) SetWithTTL(key string, value interface{}, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}

	entry := &cacheEntry{
		key:       key,
		value:     value,
		expiresAt: expiresAt,
	}

	if elem, ok := c.items[key]; ok {
		// Update existing entry
		c.lru.MoveToFront(elem)
		elem.Value = entry
		return
	}

	// Add new entry
	elem := c.lru.PushFront(entry)
	c.items[key] = elem

	// Evict if over capacity
	for c.lru.Len() > c.capacity {
		c.evictOldest()
	}
}

// Delete removes a value from the cache
func (c *LRUCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		c.lru.Remove(elem)
		delete(c.items, key)
	}
}

// evictOldest removes the least recently used entry
func (c *LRUCache) evictOldest() {
	elem := c.lru.Back()
	if elem != nil {
		entry := elem.Value.(*cacheEntry)
		c.lru.Remove(elem)
		delete(c.items, entry.key)
	}
}

// Len returns the number of items in the cache
func (c *LRUCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lru.Len()
}

// Clear removes all items from the cache
func (c *LRUCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items = make(map[string]*list.Element)
	c.lru = list.New()
}

// NegativeCache tracks "not found" results to avoid repeated lookups
type NegativeCache struct {
	cache *LRUCache
	ttl   time.Duration
}

// NewNegativeCache creates a negative cache with the given capacity and TTL
func NewNegativeCache(capacity int, ttl time.Duration) *NegativeCache {
	return &NegativeCache{
		cache: NewLRUCache(capacity),
		ttl:   ttl,
	}
}

// MarkNotFound records that a key was not found
func (nc *NegativeCache) MarkNotFound(key string) {
	nc.cache.SetWithTTL(key, struct{}{}, nc.ttl)
}

// IsNotFound returns true if the key was recently marked as not found
func (nc *NegativeCache) IsNotFound(key string) bool {
	_, ok := nc.cache.Get(key)
	return ok
}

// Clear removes the not-found marker for a key
func (nc *NegativeCache) Clear(key string) {
	nc.cache.Delete(key)
}
