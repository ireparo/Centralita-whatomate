package crm

import (
	"sync"
	"time"
)

// LookupCache is a tiny in-memory TTL cache for CRM lookup responses.
//
// Why a cache: every incoming call hits the CRM with a lookup. Without
// caching, a single call burst (e.g. 10 simultaneous incoming calls from
// a customer that keeps trying because the line was busy) would generate
// 10 identical CRM requests. The default TTL of 5 minutes is conservative
// — short enough to pick up a freshly-created customer, long enough to
// soak typical bursts.
//
// The cache is keyed by normalized phone (E.164 without "+"). It stores
// both "found" and "not found" results — both worth caching, but with
// different TTLs (negative results expire faster so a customer that
// becomes a real customer is picked up quickly).
type LookupCache struct {
	mu      sync.RWMutex
	entries map[string]cacheEntry
	posTTL  time.Duration
	negTTL  time.Duration
}

type cacheEntry struct {
	resp      *LookupResponse
	expiresAt time.Time
}

// NewLookupCache returns a new cache. positive is the TTL for found
// customers, negative is the (typically shorter) TTL for "not found".
//
// Pass 0 for either to use the defaults (5m positive, 30s negative).
func NewLookupCache(positive, negative time.Duration) *LookupCache {
	if positive <= 0 {
		positive = 5 * time.Minute
	}
	if negative <= 0 {
		negative = 30 * time.Second
	}
	return &LookupCache{
		entries: make(map[string]cacheEntry),
		posTTL:  positive,
		negTTL:  negative,
	}
}

// Get returns the cached response for a phone, or (nil, false) if absent
// or expired.
func (c *LookupCache) Get(phone string) (*LookupResponse, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.RLock()
	entry, ok := c.entries[phone]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expiresAt) {
		c.mu.Lock()
		delete(c.entries, phone)
		c.mu.Unlock()
		return nil, false
	}
	return entry.resp, true
}

// Put stores a response in the cache. The TTL depends on whether the
// response was a hit or a miss.
func (c *LookupCache) Put(phone string, resp *LookupResponse) {
	if c == nil || resp == nil {
		return
	}
	ttl := c.negTTL
	if resp.Found {
		ttl = c.posTTL
	}
	c.mu.Lock()
	c.entries[phone] = cacheEntry{resp: resp, expiresAt: time.Now().Add(ttl)}
	c.mu.Unlock()
}

// Invalidate removes a phone from the cache, e.g. when an admin manually
// links a contact to a CRM customer.
func (c *LookupCache) Invalidate(phone string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.entries, phone)
	c.mu.Unlock()
}

// Size returns the current number of entries (mostly for tests / metrics).
func (c *LookupCache) Size() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}
