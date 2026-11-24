package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"vedetta/internal/reader"
)

// a cache whose clock the test sets, with one fresh and one expired page and search
func cleanFixture(t *testing.T, mode string) (*Cache, string, time.Time) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	put := func(kind, key string, expires time.Time) {
		e := Entry{Key: key, Kind: kind, CreatedAt: now.Add(-time.Hour), ExpiresAt: expires}
		if kind == KindPage {
			e.Document = &reader.Document{URL: key, Content: "body of " + key}
		}
		if err := s.Put(e); err != nil {
			t.Fatal(err)
		}
	}
	put(KindPage, "https://fresh.example", now.Add(time.Hour))
	put(KindPage, "https://old.example", now.Add(-time.Minute))
	put(KindSearch, "fresh query", now.Add(time.Hour))
	put(KindSearch, "old query", now)
	c := New(s, mode, 24*time.Hour, 168*time.Hour)
	c.now = func() time.Time { return now }
	return c, dir, now
}

func TestPrune_RemovesOnlyWhatExpired(t *testing.T) {
	c, _, _ := cleanFixture(t, ModeRecord)
	if u := c.Usage(); u.Files != 4 || u.Bytes == 0 {
		t.Fatalf("usage before = %+v", u)
	}
	removed, freed := c.Prune()
	if removed != 2 || freed == 0 {
		t.Fatalf("removed %d entries, %d bytes; want 2", removed, freed)
	}
	if _, ok := c.store.Get(KindPage, "https://fresh.example"); !ok {
		t.Error("the fresh page was removed")
	}
	if _, ok := c.store.Get(KindSearch, "fresh query"); !ok {
		t.Error("the fresh search was removed")
	}
	if _, ok := c.store.Get(KindPage, "https://old.example"); ok {
		t.Error("the expired page is still there")
	}
	if _, ok := c.store.Get(KindSearch, "old query"); ok {
		t.Error("a search expiring right now is still there")
	}
	if u := c.Usage(); u.Files != 2 {
		t.Errorf("usage after = %+v", u)
	}
}

func TestPrune_ReplayKeepsTheCorpus(t *testing.T) {
	c, _, _ := cleanFixture(t, ModeReplay)
	if removed, _ := c.Prune(); removed != 0 || c.Usage().Files != 4 {
		t.Fatalf("replay pruned %d entries", removed)
	}
	if _, _, err := c.Clear(); err != ErrReplay {
		t.Fatalf("Clear in replay: err = %v, want ErrReplay", err)
	}
	if c.Usage().Files != 4 {
		t.Fatal("Clear in replay removed entries")
	}
}

func TestClear_RemovesEverythingTheStoreWrote(t *testing.T) {
	c, dir, now := cleanFixture(t, ModeRecord)
	// a leftover temporary file of a write cut short
	sub := filepath.Dir(c.store.path(KindPage, "https://fresh.example"))
	tmp := filepath.Join(sub, ".tmp-123")
	if err := os.WriteFile(tmp, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, freed, err := c.Clear()
	if err != nil || removed != 4 || freed == 0 {
		t.Fatalf("Clear = %d, %d, %v", removed, freed, err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Error("the temporary file is still there")
	}
	if u := c.Usage(); u.Files != 0 || u.Bytes != 0 {
		t.Errorf("usage after Clear = %+v", u)
	}
	// the cache keeps working after it
	if err := c.store.Put(Entry{Key: "k", Kind: KindSearch, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
}

func TestClean_TouchesOnlyTheStoreFiles(t *testing.T) {
	c, dir, now := cleanFixture(t, ModeRecord)
	// files that are not the store's: a note at the top, a JSON with another name,
	// a file at the top of a kind directory, a folder nested too deep
	others := []string{
		filepath.Join(dir, "notes.json"),
		filepath.Join(dir, KindPage, "README.json"),
		filepath.Join(dir, KindPage, "ab", "config.json"),
		filepath.Join(dir, KindPage, "ab", "deep", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.json"),
		filepath.Join(dir, KindSearch, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.json"),
	}
	for _, p := range others {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(`{"key":"x","expires_at":"2000-01-01T00:00:00Z"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// a young temporary file may be a write in progress: prune leaves it
	young := filepath.Join(filepath.Dir(c.store.path(KindSearch, "fresh query")), ".tmp-young")
	if err := os.WriteFile(young, []byte("in progress"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(filepath.Dir(c.store.path(KindSearch, "fresh query")), ".tmp-old")
	if err := os.WriteFile(old, []byte("cut short"), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := now.Add(-2 * time.Hour)
	os.Chtimes(old, stamp, stamp)
	os.Chtimes(young, now, now)

	c.Prune()
	if _, err := os.Stat(young); err != nil {
		t.Error("prune removed a temporary file younger than an hour")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("prune left an old temporary file")
	}
	c.Clear()
	for _, p := range others {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed", p)
		}
	}
}
