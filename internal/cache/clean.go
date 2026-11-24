package cache

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Cleaning the cache (map R2): what has expired is deleted by itself,
// and Settings can empty everything by hand. Only the files the store writes are
// touched: <dir>/<kind>/<hash[:2]>/<hash>.json and the store's leftover temporary
// files, so a cache directory set to a wrong place deletes nothing else.

// ErrReplay refuses to empty a benchmark corpus.
var ErrReplay = errors.New("the cache is a benchmark corpus (replay mode): it is not emptied")

var entryName = regexp.MustCompile(`^[0-9a-f]{64}\.json$`)

// tmpAge is how old a leftover temporary file must be before it is deleted: a
// younger one may be a write in progress.
const tmpAge = time.Hour

// Usage is what the cache holds on disk.
type Usage struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// walk calls fn for each file the store wrote: entries (tmp false) and temporary
// files (tmp true), in the kind directories only.
func (s *Store) walk(fn func(path string, info fs.FileInfo, tmp bool)) {
	for _, kind := range []string{KindSearch, KindPage} {
		root := filepath.Join(s.dir, kind)
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // a missing kind directory or an unreadable folder: nothing to do there
			}
			if d.IsDir() {
				if p != root && filepath.Dir(p) != root {
					return filepath.SkipDir // the store nests one level only
				}
				return nil
			}
			name := d.Name()
			tmp := strings.HasPrefix(name, ".tmp-")
			if !tmp && !entryName.MatchString(name) {
				return nil
			}
			if filepath.Dir(p) == root {
				return nil // the store never writes at the top of a kind directory
			}
			info, err := d.Info()
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			fn(p, info, tmp)
			return nil
		})
	}
}

// Usage counts the entries and their bytes.
func (s *Store) Usage() Usage {
	var u Usage
	s.walk(func(_ string, info fs.FileInfo, tmp bool) {
		if !tmp {
			u.Files++
		}
		u.Bytes += info.Size()
	})
	return u
}

// prune deletes the entries expired at now, and temporary files older than tmpAge.
// An entry that cannot be read is left alone (Clear removes it).
func (s *Store) prune(now time.Time) (removed int, freed int64) {
	s.walk(func(p string, info fs.FileInfo, tmp bool) {
		if tmp {
			if now.Sub(info.ModTime()) > tmpAge && os.Remove(p) == nil {
				freed += info.Size()
			}
			return
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return
		}
		var e struct {
			Key       string    `json:"key"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		if json.Unmarshal(raw, &e) != nil || e.Key == "" || e.ExpiresAt.IsZero() || now.Before(e.ExpiresAt) {
			return
		}
		if os.Remove(p) == nil {
			removed++
			freed += info.Size()
		}
	})
	return removed, freed
}

// clear deletes every entry and temporary file.
func (s *Store) clear() (removed int, freed int64) {
	s.walk(func(p string, info fs.FileInfo, tmp bool) {
		if os.Remove(p) == nil {
			if !tmp {
				removed++
			}
			freed += info.Size()
		}
	})
	return removed, freed
}

// Usage is what the cache holds on disk.
func (c *Cache) Usage() Usage { return c.store.Usage() }

// SearchTTL and PageTTL are how long a search and a page are reused in record mode.
func (c *Cache) SearchTTL() time.Duration { return c.searchTTL }
func (c *Cache) PageTTL() time.Duration   { return c.pageTTL }

// Prune deletes what has expired. In record mode an expired entry is never used
// again, not even as an earlier page of a follow-up (Session.Page). Replay never
// prunes: a corpus is used whole, expired or not.
func (c *Cache) Prune() (removed int, freed int64) {
	if c.mode != ModeRecord {
		return 0, 0
	}
	removed, freed = c.store.prune(c.now())
	if removed > 0 {
		log.Printf("[cache] removed %d expired entries (%d bytes)", removed, freed)
	}
	return removed, freed
}

// PruneEvery prunes now and then every d, for ever: run it in its own goroutine.
func (c *Cache) PruneEvery(d time.Duration) {
	for {
		c.Prune()
		time.Sleep(d)
	}
}

// Clear empties the cache by hand. The next searches read their pages again, the
// earlier pages of a follow-up too.
func (c *Cache) Clear() (removed int, freed int64, err error) {
	if c.mode == ModeReplay {
		return 0, 0, ErrReplay
	}
	removed, freed = c.store.clear()
	log.Printf("[cache] emptied by hand: %d entries (%d bytes)", removed, freed)
	return removed, freed, nil
}
