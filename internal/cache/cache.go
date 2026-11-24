package cache

import (
	"log"
	"sync/atomic"
	"time"
)

// Modes. "off" never builds a Cache.
const (
	ModeRecord = "record" // reuse until expiry, store what is new (normal use)
	ModeReplay = "replay" // always reuse, ignoring expiry; store only misses (benchmark)
)

// shortTTL is the record-mode expiry of outcomes that may be temporary: empty
// searches, stable read errors, blocked or very short pages.
const shortTTL = time.Hour

// Cache holds the store and the policy shared by every search session.
type Cache struct {
	store     *Store
	mode      string
	searchTTL time.Duration
	pageTTL   time.Duration
	now       func() time.Time
}

// New returns a cache in record or replay mode. Any other mode is treated as
// record; callers decide whether the cache is on at all.
func New(store *Store, mode string, searchTTL, pageTTL time.Duration) *Cache {
	if mode != ModeReplay {
		mode = ModeRecord
	}
	return &Cache{store: store, mode: mode, searchTTL: searchTTL, pageTTL: pageTTL, now: time.Now}
}

// Mode reports record or replay.
func (c *Cache) Mode() string { return c.mode }

func (c *Cache) fresh(e Entry) bool {
	return c.mode == ModeReplay || c.now().Before(e.ExpiresAt)
}

func (c *Cache) put(e Entry, ttl time.Duration) {
	now := c.now().UTC()
	e.CreatedAt = now
	e.ExpiresAt = now.Add(ttl)
	if err := c.store.Put(e); err != nil {
		log.Printf("[cache] write %s failed: %v", e.Key, err)
	}
}

// Stats counts what one search session found in the cache.
type Stats struct {
	Mode         string `json:"mode"`
	SearchHits   int    `json:"search_hits"`
	SearchMisses int    `json:"search_misses"`
	PageHits     int    `json:"page_hits"`
	PageMisses   int    `json:"page_misses"`
}

// Session wraps the providers of one user search and counts its hits and
// misses. Create one per search with Cache.Session.
type Session struct {
	c                        *Cache
	searchHits, searchMisses atomic.Int64
	pageHits, pageMisses     atomic.Int64
}

// Session starts the counters of one user search.
func (c *Cache) Session() *Session { return &Session{c: c} }

// Stats returns the counters so far.
func (s *Session) Stats() Stats {
	return Stats{
		Mode:         s.c.mode,
		SearchHits:   int(s.searchHits.Load()),
		SearchMisses: int(s.searchMisses.Load()),
		PageHits:     int(s.pageHits.Load()),
		PageMisses:   int(s.pageMisses.Load()),
	}
}
