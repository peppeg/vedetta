// Package cache keeps search results and read pages on disk so that repeated
// searches do not hit the search engines again (normal use) and benchmark runs
// can replay the same material (V25-R1).
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"vedetta/internal/reader"
	"vedetta/internal/search"
)

// Kinds of entry; each kind lives in its own subdirectory.
const (
	KindSearch = "search"
	KindPage   = "page"
)

// Entry is one cached search or page read. A page entry holds either a
// document or a stable read error (ErrorKind).
type Entry struct {
	Key       string           `json:"key"`
	Kind      string           `json:"kind"`
	CreatedAt time.Time        `json:"created_at"`
	ExpiresAt time.Time        `json:"expires_at"`
	Results   []search.Result  `json:"results,omitempty"`
	Document  *reader.Document `json:"document,omitempty"`
	ErrorKind string           `json:"error_kind,omitempty"`
	Error     string           `json:"error,omitempty"`
}

// Store keeps one JSON file per entry under dir/<kind>/<hash[:2]>/<hash>.json.
// Plain files can be inspected with cat or jq, and a corpus is frozen by
// copying the directory.
type Store struct {
	dir string
}

// NewStore creates dir if needed.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cache dir %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(kind, key string) string {
	sum := sha256.Sum256([]byte(key))
	h := hex.EncodeToString(sum[:])
	return filepath.Join(s.dir, kind, h[:2], h+".json")
}

// Get returns the entry stored for key. A missing, unreadable or corrupt file
// is a miss, never an error for the caller.
func (s *Store) Get(kind, key string) (Entry, bool) {
	p := s.path(kind, key)
	raw, err := os.ReadFile(p)
	if err != nil {
		return Entry{}, false
	}
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		log.Printf("[cache] corrupt entry %s ignored: %v", p, err)
		return Entry{}, false
	}
	if e.Key != key {
		return Entry{}, false
	}
	return e, true
}

// Put writes e atomically (temporary file + rename), so a parallel reader never
// sees a half-written entry. Concurrent writers of the same key: the last wins.
func (s *Store) Put(e Entry) error {
	p := s.path(e.Kind, e.Key)
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
