package reader

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeReader_HTTPBuiltIn(t *testing.T) {
	got := ProbeReader(context.Background(), "http", ReaderConfig{})
	if got.Status != "ok" {
		t.Fatalf("expected http ok, got %q", got.Status)
	}
}

// Map R8: Jina is recognized by its home page; another page is "not Jina".
func TestProbeReader_JinaUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("[Usage 1] https://r.jina.ai/YOUR_URL\n[Usage 2] https://s.jina.ai/YOUR_SEARCH_QUERY\n[Homepage] https://jina.ai/reader\n"))
	}))
	defer srv.Close()

	got := ProbeReader(context.Background(), "jina", ReaderConfig{JinaBaseURL: srv.URL})
	if got.Status != "ok" {
		t.Fatalf("expected jina ok, got %q (detail %q)", got.Status, got.Detail)
	}

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>something else</html>"))
	}))
	defer other.Close()
	if got := ProbeReader(context.Background(), "jina", ReaderConfig{JinaBaseURL: other.URL}); got.Status != "down" || got.Problem != "not_jina" {
		t.Fatalf("another page: got %+v", got)
	}
}

func TestProbeReader_JinaDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed listener -> connection refused

	got := ProbeReader(context.Background(), "jina", ReaderConfig{JinaBaseURL: srv.URL})
	if got.Status != "down" {
		t.Fatalf("expected jina down, got %q", got.Status)
	}
	if got.Detail == "" {
		t.Fatal("expected a non-empty detail on a down probe")
	}
}

func TestProbeReader_Firecrawl(t *testing.T) {
	if got := ProbeReader(context.Background(), "firecrawl", ReaderConfig{FirecrawlAPIKey: "k"}); got.Status != "configured" {
		t.Fatalf("expected firecrawl configured with key, got %q", got.Status)
	}
	if got := ProbeReader(context.Background(), "firecrawl", ReaderConfig{}); got.Status != "unknown" {
		t.Fatalf("expected firecrawl unknown without key, got %q", got.Status)
	}
}
