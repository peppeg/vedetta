package cache

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"vedetta/internal/reader"
	"vedetta/internal/search"
)

func TestStore_PutGet(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(KindSearch, "k"); ok {
		t.Fatal("empty store returned a hit")
	}
	want := Entry{Key: "k", Kind: KindSearch, CreatedAt: time.Now().UTC().Truncate(time.Second),
		Results: []search.Result{{Title: "T", URL: "https://example.org", Engine: "duckduckgo"}}}
	if err := s.Put(want); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get(KindSearch, "k")
	if !ok || len(got.Results) != 1 || got.Results[0].Engine != "duckduckgo" || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
	if _, ok := s.Get(KindPage, "k"); ok {
		t.Fatal("same key under another kind must be a miss")
	}

	page := Entry{Key: "p", Kind: KindPage, Document: &reader.Document{URL: "https://example.org", Content: "body"}}
	if err := s.Put(page); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Get(KindPage, "p"); !ok || got.Document == nil || got.Document.Content != "body" {
		t.Fatalf("page entry: %+v ok=%v", got, ok)
	}
}

func TestStore_CorruptFileIsMiss(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	if err := s.Put(Entry{Key: "k", Kind: KindSearch}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path(KindSearch, "k"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(KindSearch, "k"); ok {
		t.Fatal("corrupt file must be a miss")
	}
}

func TestStore_ConcurrentWrites(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("k%d", i%4) // several writers per key
			if err := s.Put(Entry{Key: key, Kind: KindPage, Error: fmt.Sprint(i)}); err != nil {
				t.Error(err)
			}
			s.Get(KindPage, key)
		}(i)
	}
	wg.Wait()
	for i := 0; i < 4; i++ {
		if _, ok := s.Get(KindPage, fmt.Sprintf("k%d", i)); !ok {
			t.Errorf("k%d missing after concurrent writes", i)
		}
	}
}
