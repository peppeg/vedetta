package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"vedetta/config"
	"vedetta/internal/agent"
	"vedetta/internal/storage"
)

// Map R4: /api/sessions gives the latest 100 by default, every session with all=1,
// those with the words of q, and the last N days with days=N.
func TestListSessions_Parameters(t *testing.T) {
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := 0; i < 105; i++ {
		if _, err := store.CreateSession("ricerca qualunque", "deep"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.CreateSession("Qualità dell'aria", "deep"); err != nil {
		t.Fatal(err)
	}
	h := NewHandlers(&config.Config{ConfigPath: t.TempDir() + "/config.local"}, agent.NewEngine(&dummySearchProvider{}, nil, nil, store))
	count := func(url string) int {
		rec := httptest.NewRecorder()
		h.ListSessions(rec, httptest.NewRequest(http.MethodGet, url, nil))
		var list []map[string]interface{}
		if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
			t.Fatalf("%s: %v", url, err)
		}
		return len(list)
	}
	for url, want := range map[string]int{
		"/api/sessions":               100,
		"/api/sessions?all=1":         106,
		"/api/sessions?q=qualita":     1,
		"/api/sessions?days=7":        106,
		"/api/sessions?all=1&q=nulla": 0,
	} {
		if got := count(url); got != want {
			t.Errorf("%s: %d sessions, want %d", url, got, want)
		}
	}
}
