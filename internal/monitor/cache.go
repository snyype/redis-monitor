package monitor

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// memoryCache is the in-process stand-in for the application cache the PHP service
// leans on. Its whole job is to stop a dashboard left open on auto-refresh from
// turning into a SCAN loop against production: an overview is reused for
// metrics_cache_seconds and the deep stats payload for stats_cache_seconds.
//
// Entries expire lazily on read. There is no janitor because the key space is
// tiny and bounded — three buckets times the selectable databases.
type memoryCache struct {
	mu      sync.RWMutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	value     interface{}
	expiresAt time.Time
}

func newMemoryCache() *memoryCache {
	return &memoryCache{entries: make(map[string]cacheEntry)}
}

func (c *memoryCache) Get(key string) (interface{}, bool) {
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()

	if !ok {
		return nil, false
	}

	if time.Now().After(entry.expiresAt) {
		c.mu.Lock()
		// Only drop it if nothing has replaced it in the meantime.
		if current, still := c.entries[key]; still && current.expiresAt.Equal(entry.expiresAt) {
			delete(c.entries, key)
		}
		c.mu.Unlock()

		return nil, false
	}

	return entry.value, true
}

func (c *memoryCache) Put(key string, value interface{}, ttl time.Duration) {
	if ttl <= 0 {
		return
	}

	c.mu.Lock()
	c.entries[key] = cacheEntry{value: value, expiresAt: time.Now().Add(ttl)}
	c.mu.Unlock()
}

// Forget drops one entry.
func (c *memoryCache) Forget(key string) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// ForgetPrefix drops every entry under a bucket, used after a delete so the charts
// and the namespace dropdown stop showing keys that are gone.
func (c *memoryCache) ForgetPrefix(prefix string) {
	c.mu.Lock()
	for key := range c.entries {
		if strings.HasPrefix(key, prefix) {
			delete(c.entries, key)
		}
	}
	c.mu.Unlock()
}

func cacheKey(bucket string, db int) string {
	return "redis-monitor:" + bucket + ":" + strconv.Itoa(db)
}
