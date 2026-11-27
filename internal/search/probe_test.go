package search

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeSearxng answers like SearXNG: /config, and the empty JSON search with 400
// ("no query") or the given code.
func fakeSearxng(searchCode int, limiter string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"version":"2026.11.18+b867c56a5","engines":[{"name":"duckduckgo web","enabled":true}],"limiter":` + limiter + `}`))
		case "/search":
			if r.URL.Query().Get("format") != "json" {
				t := http.StatusOK
				w.WriteHeader(t)
				return
			}
			w.WriteHeader(searchCode)
			w.Write([]byte(`{"error": "No query"}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
}

// Map R8: the doctor says what is wrong, not just "it answers".
func TestProbeProvider_SearxngDoctor(t *testing.T) {
	cases := []struct {
		name, limiter        string
		code                 int
		status, problem, ver string
	}{
		{"json on", `{"enabled":false}`, http.StatusBadRequest, "ok", "", "2026.11.18+b867c56a5"},
		{"json off", `{"enabled":false}`, http.StatusForbidden, "down", "json_off", "2026.11.18+b867c56a5"},
		{"limiter blocks", `{"enabled":true}`, http.StatusTooManyRequests, "down", "limiter", "2026.11.18+b867c56a5"},
		{"limiter on, still answers", `{"enabled":true}`, http.StatusBadRequest, "ok", "limiter_on", "2026.11.18+b867c56a5"},
		{"older searxng, limiter as a bool", `true`, http.StatusBadRequest, "ok", "limiter_on", "2026.11.18+b867c56a5"},
	}
	for _, c := range cases {
		srv := fakeSearxng(c.code, c.limiter)
		got := ProbeProvider(context.Background(), "searxng", ProviderConfig{SearxngURL: srv.URL + "/"})
		srv.Close()
		if got.Status != c.status || got.Problem != c.problem || got.Version != c.ver || got.Name != "searxng" {
			t.Errorf("%s: got %+v", c.name, got)
		}
	}

	// any web page that is not SearXNG
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>router login</html>"))
	}))
	defer page.Close()
	if got := ProbeProvider(context.Background(), "searxng", ProviderConfig{SearxngURL: page.URL}); got.Status != "down" || got.Problem != "not_searxng" {
		t.Errorf("a web page: got %+v", got)
	}
}

// degoog answers like a real degoog 1.x on 27/11/2026: no /config, its web page
// at /search, the SearXNG-style JSON search under /api.
func TestProbeProvider_DegoogAnswersLikeSearxng(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/config":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"Unauthorized","code":401}`))
		case "/api/search":
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("q") == "" {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":"Missing or invalid query parameter 'q'"}`))
				return
			}
			w.Write([]byte(`{"results":[{"url":"https://a.example","source":"Bing"}],"engineTimings":[]}`))
		case "/search", "/":
			w.Header().Set("Content-Type", "text/html; charset=UTF-8")
			w.Write([]byte("<!doctype html><html>degoog</html>"))
		default:
			w.Header().Set("Content-Type", "text/html; charset=UTF-8")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("<!doctype html><html>not found</html>"))
		}
	}))
	defer srv.Close()

	got := ProbeProvider(context.Background(), "searxng", ProviderConfig{SearxngURL: srv.URL + "/api/"})
	if got.Status != "ok" || got.Problem != "" || got.Detail == "" || got.Name != "searxng" {
		t.Errorf("degoog at /api: got %+v", got)
	}
	if n, err := ProbeSearxngResults(context.Background(), srv.URL+"/api"); err != nil || n != 1 {
		t.Errorf("degoog test search: got %d results, err %v", n, err)
	}

	got = ProbeProvider(context.Background(), "searxng", ProviderConfig{SearxngURL: srv.URL})
	if got.Status != "down" || got.Problem != "use_api" || got.Detail != srv.URL+"/api" {
		t.Errorf("degoog without /api: got %+v", got)
	}
}

func TestProbeSearxngResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") == "" || r.URL.Query().Get("format") != "json" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"results":[{"url":"https://a.example"},{"url":"https://b.example"}]}`))
	}))
	defer srv.Close()
	if n, err := ProbeSearxngResults(context.Background(), srv.URL); err != nil || n != 2 {
		t.Errorf("got %d results, err %v", n, err)
	}
}

func TestProbeProvider_SearxngDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed listener -> connection refused

	got := ProbeProvider(context.Background(), "searxng", ProviderConfig{SearxngURL: srv.URL})
	if got.Status != "down" || got.Problem != "unreachable" {
		t.Fatalf("expected status down, unreachable, got %+v", got)
	}
	if got.Detail == "" {
		t.Fatal("expected a non-empty detail on a down probe")
	}
}

func TestProbeProvider_Keyed(t *testing.T) {
	if got := ProbeProvider(context.Background(), "brave", ProviderConfig{BraveAPIKey: "k"}); got.Status != "configured" {
		t.Fatalf("expected brave configured with key, got %q", got.Status)
	}
	if got := ProbeProvider(context.Background(), "tavily", ProviderConfig{}); got.Status != "unknown" {
		t.Fatalf("expected tavily unknown without key, got %q", got.Status)
	}
	if got := ProbeProvider(context.Background(), "serper", ProviderConfig{SerperAPIKey: "k"}); got.Status != "configured" {
		t.Fatalf("expected serper configured with key, got %q", got.Status)
	}
}

func TestProbeProvider_AcademicUnknown(t *testing.T) {
	got := ProbeProvider(context.Background(), "academic", ProviderConfig{})
	if got.Status != "unknown" {
		t.Fatalf("expected academic unknown, got %q", got.Status)
	}
	if got.Detail == "" {
		t.Fatal("expected a detail explaining why academic is not probed")
	}
}

func TestProbeProvider_UnknownName(t *testing.T) {
	got := ProbeProvider(context.Background(), "does-not-exist", ProviderConfig{})
	if got.Status != "unknown" {
		t.Fatalf("expected unknown for an unknown provider, got %q", got.Status)
	}
}
