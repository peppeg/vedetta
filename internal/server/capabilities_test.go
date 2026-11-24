package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"vedetta/config"
	"vedetta/internal/agent"
)

// newCapLLMServer serves an OpenAI-compatible /models endpoint and a
// /chat/completions endpoint that only accepts the given reasoning_effort
// values. Every request is counted in hits when hits is non-nil.
func newCapLLMServer(accept map[string]bool, hits *int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt64(hits, 1)
		}
		switch r.URL.Path {
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
		case "/chat/completions":
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			v, _ := req["reasoning_effort"].(string)
			if accept[v] {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"reasoning_effort unsupported"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// fakeSearxngOK answers like a working SearXNG for the doctor (map R8): /config,
// and 400 "no query" for the empty JSON search.
func fakeSearxngOK() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config":
			w.Write([]byte(`{"version":"2026.11.18","engines":[{"name":"duckduckgo web","enabled":true}],"limiter":{"enabled":false}}`))
		case "/search":
			w.WriteHeader(http.StatusBadRequest)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
}

// newCapabilitiesHandlers builds a Handlers wired to a disposable engine and,
// when llmURL/searxURL are non-empty, points the config at the test servers.
func newCapabilitiesHandlers(cfg *config.Config, llmURL, searxURL string) *Handlers {
	if llmURL != "" {
		cfg.LLMEndpoints = []config.LLMEndpoint{{ID: "default", Name: "default", BaseURL: llmURL, Model: "m", APIKey: "k"}}
		cfg.LLMDefaultID = "default"
		cfg.LLMBaseURL = llmURL
		cfg.LLMModel = "m"
		cfg.LLMAPIKey = "k"
	}
	if searxURL != "" {
		cfg.SearxngURL = searxURL
	}
	return NewHandlers(cfg, agent.NewEngine(&dummySearchProvider{}, nil, nil, nil))
}

func TestCapabilities_Statuses(t *testing.T) {
	llmSrv := newCapLLMServer(map[string]bool{"minimal": true, "high": true}, nil)
	defer llmSrv.Close()
	searxSrv := fakeSearxngOK()
	defer searxSrv.Close()

	cfg := &config.Config{
		SearchProviders: []string{"searxng", "brave"},
		ReaderProviders: []string{"http"},
		BraveAPIKey:     "x",
	}
	h := newCapabilitiesHandlers(cfg, llmSrv.URL, searxSrv.URL)

	rec := httptest.NewRecorder()
	h.Capabilities(rec, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp CapabilitiesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.LLM) != 1 {
		t.Fatalf("expected 1 llm endpoint, got %d", len(resp.LLM))
	}
	if !resp.LLM[0].Reachable {
		t.Fatalf("expected llm reachable")
	}
	if got := resp.LLM[0].EffortValues; len(got) != 2 || got[0] != "minimal" || got[1] != "high" {
		t.Fatalf("expected effort_values [minimal high], got %v", got)
	}
	if resp.LLM[0].StructuredOutput != "unknown" {
		t.Fatalf("expected structured_output unknown, got %q", resp.LLM[0].StructuredOutput)
	}

	status := map[string]string{}
	for _, c := range resp.Search {
		status[c.Name] = c.Status
	}
	if status["searxng"] != "ok" {
		t.Fatalf("expected searxng ok, got %v", status)
	}
	if status["brave"] != "configured" {
		t.Fatalf("expected brave configured, got %v", status)
	}
	if len(resp.Reader) != 1 || resp.Reader[0].Status != "ok" {
		t.Fatalf("expected reader http ok, got %+v", resp.Reader)
	}
}

func TestCapabilities_ProbeForcesRefresh(t *testing.T) {
	var hits int64
	llmSrv := newCapLLMServer(map[string]bool{}, &hits)
	defer llmSrv.Close()
	searxSrv := fakeSearxngOK()
	defer searxSrv.Close()

	cfg := &config.Config{SearchProviders: []string{"searxng"}, ReaderProviders: []string{"http"}}
	h := newCapabilitiesHandlers(cfg, llmSrv.URL, searxSrv.URL)

	call := func(probe bool) CapabilitiesResponse {
		url := "/api/capabilities"
		if probe {
			url += "?probe=1"
		}
		rec := httptest.NewRecorder()
		h.Capabilities(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var resp CapabilitiesResponse
		_ = json.NewDecoder(rec.Body).Decode(&resp)
		if resp.ProbedAt == "" {
			t.Fatal("expected a non-empty probed_at")
		}
		return resp
	}

	call(false)
	afterFirst := atomic.LoadInt64(&hits)
	call(false)
	if atomic.LoadInt64(&hits) != afterFirst {
		t.Fatalf("expected the second non-forced call to reuse the cache")
	}

	call(true)
	if atomic.LoadInt64(&hits) <= afterFirst {
		t.Fatalf("expected ?probe=1 to run fresh probes (hits %d -> %d)", afterFirst, atomic.LoadInt64(&hits))
	}
}

func TestCapabilities_Auth(t *testing.T) {
	llmSrv := newCapLLMServer(map[string]bool{}, nil)
	defer llmSrv.Close()

	cfg := &config.Config{ConfigToken: "tok", ReaderProviders: []string{"http"}}
	h := newCapabilitiesHandlers(cfg, llmSrv.URL, "")

	rec := httptest.NewRecorder()
	h.Capabilities(rec, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/capabilities", nil)
	req.Header.Set("X-Vedetta-Token", "tok")
	h.Capabilities(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with token, got %d", rec.Code)
	}
}

func TestCapabilities_ConcurrentRefresh(t *testing.T) {
	llmSrv := newCapLLMServer(map[string]bool{"minimal": true}, nil)
	defer llmSrv.Close()
	searxSrv := fakeSearxngOK()
	defer searxSrv.Close()

	cfg := &config.Config{SearchProviders: []string{"searxng"}, ReaderProviders: []string{"http"}}
	h := newCapabilitiesHandlers(cfg, llmSrv.URL, searxSrv.URL)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.Capabilities(rec, httptest.NewRequest(http.MethodGet, "/api/capabilities?probe=1", nil))
			if rec.Code != http.StatusOK {
				t.Errorf("expected 200, got %d", rec.Code)
			}
		}()
	}
	wg.Wait()
}
