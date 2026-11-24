package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"vedetta/config"
	"vedetta/internal/agent"
	"vedetta/internal/cache"
)

func cacheHandlers(t *testing.T, mode, token string) (*Handlers, *cache.Store) {
	t.Helper()
	st, err := cache.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if err := st.Put(cache.Entry{Key: k, Kind: cache.KindSearch, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	engine := agent.NewEngine(&dummySearchProvider{}, nil, nil, nil)
	engine.SetCache(cache.New(st, mode, 24*time.Hour, 168*time.Hour))
	cfg := &config.Config{ConfigToken: token, CachePrune: true, ConfigPath: t.TempDir() + "/config.local"}
	return NewHandlers(cfg, engine), st
}

func decodeCache(t *testing.T, rec *httptest.ResponseRecorder) CacheResponse {
	t.Helper()
	var v CacheResponse
	if err := json.NewDecoder(rec.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return v
}

func TestCacheAPI_StateAndClear(t *testing.T) {
	h, st := cacheHandlers(t, cache.ModeRecord, "")
	rec := httptest.NewRecorder()
	h.Cache(rec, httptest.NewRequest(http.MethodGet, "/api/cache", nil))
	got := decodeCache(t, rec)
	if rec.Code != 200 || got.Mode != "record" || got.Files != 3 || got.Bytes == 0 || got.PageTTLHours != 168 || got.SearchTTLHours != 24 || !got.ExpiredRemoved {
		t.Fatalf("state = %d %+v", rec.Code, got)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cache/clear", nil)
	req.Header.Set("Origin", "http://"+req.Host)
	h.CacheClear(rec, req)
	got = decodeCache(t, rec)
	if rec.Code != 200 || got.Removed != 3 || got.FreedBytes == 0 || got.Files != 0 {
		t.Fatalf("clear = %d %+v", rec.Code, got)
	}
	if u := st.Usage(); u.Files != 0 {
		t.Fatalf("store still holds %d entries", u.Files)
	}
}

func TestCacheAPI_ClearIsGuarded(t *testing.T) {
	// another site
	h, st := cacheHandlers(t, cache.ModeRecord, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cache/clear", nil)
	req.Header.Set("Origin", "https://evil.example")
	h.CacheClear(rec, req)
	if rec.Code != http.StatusForbidden || st.Usage().Files != 3 {
		t.Fatalf("other site: %d, %d entries left", rec.Code, st.Usage().Files)
	}
	// a GET does not clear
	rec = httptest.NewRecorder()
	h.CacheClear(rec, httptest.NewRequest(http.MethodGet, "/api/cache/clear", nil))
	if rec.Code != http.StatusMethodNotAllowed || st.Usage().Files != 3 {
		t.Fatalf("GET clear: %d", rec.Code)
	}

	// the settings code, when there is one
	h, st = cacheHandlers(t, cache.ModeRecord, "s3cr3t")
	rec = httptest.NewRecorder()
	h.CacheClear(rec, httptest.NewRequest(http.MethodPost, "/api/cache/clear", nil))
	if rec.Code != http.StatusUnauthorized || st.Usage().Files != 3 {
		t.Fatalf("no code: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.Cache(rec, httptest.NewRequest(http.MethodGet, "/api/cache", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("state without code: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/cache/clear", nil)
	req.Header.Set("X-Vedetta-Token", "s3cr3t")
	h.CacheClear(rec, req)
	if rec.Code != 200 || st.Usage().Files != 0 {
		t.Fatalf("with code: %d, %d entries left", rec.Code, st.Usage().Files)
	}

	// a benchmark corpus is never emptied
	h, st = cacheHandlers(t, cache.ModeReplay, "")
	rec = httptest.NewRecorder()
	h.CacheClear(rec, httptest.NewRequest(http.MethodPost, "/api/cache/clear", nil))
	if rec.Code != http.StatusConflict || st.Usage().Files != 3 {
		t.Fatalf("replay: %d, %d entries left", rec.Code, st.Usage().Files)
	}
}

func TestCacheAPI_Off(t *testing.T) {
	engine := agent.NewEngine(&dummySearchProvider{}, nil, nil, nil)
	h := NewHandlers(&config.Config{ConfigPath: t.TempDir() + "/config.local"}, engine)
	rec := httptest.NewRecorder()
	h.Cache(rec, httptest.NewRequest(http.MethodGet, "/api/cache", nil))
	if got := decodeCache(t, rec); rec.Code != 200 || got.Mode != "off" || got.Files != 0 {
		t.Fatalf("off: %d %+v", rec.Code, got)
	}
	rec = httptest.NewRecorder()
	h.CacheClear(rec, httptest.NewRequest(http.MethodPost, "/api/cache/clear", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("clear when off: %d", rec.Code)
	}
}
