package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"vedetta/internal/llm"
	"vedetta/internal/reader"
	"vedetta/internal/search"
)

// F36-T5 / F36-T6 — per-search overrides must apply to that call only and never
// mutate engine-global provider state.

// namedSpyProvider wraps spySearchProvider with a custom name so telemetry can
// be asserted on override-originated results.

// spySearchProvider records the Options of every search call, keyed by query.
type spySearchProvider struct {
	mu   sync.Mutex
	opts map[string]search.Options
}

func (s *spySearchProvider) Name() string { return "spy" }

func (s *spySearchProvider) Search(ctx context.Context, query string, opts search.Options) ([]search.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opts == nil {
		s.opts = make(map[string]search.Options)
	}
	s.opts[query] = opts
	return []search.Result{{
		Title:   "Doc " + query,
		URL:     "https://example.com/" + url.QueryEscape(query),
		Content: "Generic content about " + query,
	}}, nil
}

type namedSpyProvider struct {
	*spySearchProvider
	name string
}

func (n *namedSpyProvider) Name() string { return n.name }

// newTestLLMServer returns an OpenAI-compatible SSE endpoint emitting answer.
func newTestLLMServer(t *testing.T, answer string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		payload := map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": answer}},
			},
		}
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runStandard executes a fast path and returns the events emitted.
func runStandard(t *testing.T, engine *Engine, params SearchParams) []Event {
	t.Helper()
	var mu sync.Mutex
	var events []Event
	emit := func(ev Event) error {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := engine.ExecuteDeepPath(ctx, params, emit); err != nil {
		t.Fatalf("ExecuteDeepPath failed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return events
}

func joinTokens(events []Event) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Type == EventToken {
			if s, ok := ev.Data.(string); ok {
				b.WriteString(s)
			}
		}
	}
	return b.String()
}

// F36-T5: a per-search llm_endpoint override routes synthesis to that client.
func TestExecuteDeepPath_LLMOverrideUsed(t *testing.T) {
	globalSrv := newTestLLMServer(t, "GLOBAL")
	overrideSrv := newTestLLMServer(t, "OVERRIDE")

	sp := &mockSearchProvider{results: []search.Result{
		{Title: "Result 1", URL: "https://example.com/1", Content: "Snippet 1"},
	}}
	rd := &mockReader{docs: map[string]reader.Document{
		"https://example.com/1": {Title: "Result 1", URL: "https://example.com/1", Content: "Body"},
	}}
	globalClient := llm.NewClient(globalSrv.URL, "", "global-model")
	overrideClient := llm.NewClient(overrideSrv.URL, "", "override-model")
	engine := NewEngine(sp, rd, globalClient, nil)

	events := runStandard(t, engine, SearchParams{
		Query:             "override llm",
		UILanguage:        "it",
		AnswerLanguage:    "it",
		Mode:              "standard",
		LLMClientOverride: overrideClient,
	})

	text := joinTokens(events)
	if !strings.Contains(text, "OVERRIDE") {
		t.Errorf("expected override LLM output, got tokens: %q", text)
	}
	if strings.Contains(text, "GLOBAL") {
		t.Errorf("global LLM output leaked into override run: %q", text)
	}
	if engine.LLMClient() != globalClient {
		t.Errorf("engine global LLM client was mutated by override")
	}
}

// F36-T6: sources telemetry must report the override provider name, not the global one.
func TestOverride_TelemetryUsesOverrideName(t *testing.T) {
	srv := newTestLLMServer(t, "answer")
	global := &namedSpyProvider{spySearchProvider: &spySearchProvider{}, name: "global-spy"}
	override := &namedSpyProvider{spySearchProvider: &spySearchProvider{}, name: "override-spy"}
	client := llm.NewClient(srv.URL, "", "test-model")
	engine := NewEngine(global, &mockReader{}, client, nil)

	events := runStandard(t, engine, SearchParams{
		Query:                  "telemetry override",
		UILanguage:             "it",
		AnswerLanguage:         "it",
		Mode:                   "standard",
		SearchProviderOverride: override,
	})

	var got SourcesPayload
	found := false
	for _, ev := range events {
		if ev.Type != EventSources {
			continue
		}
		sp, ok := ev.Data.(SourcesPayload)
		if !ok {
			t.Fatalf("unexpected EventSources payload type %T", ev.Data)
		}
		got = sp
		found = true
	}
	if !found {
		t.Fatalf("no EventSources emitted")
	}
	if got.SearchProvider != "override-spy" {
		t.Errorf("expected telemetry provider override-spy, got %q", got.SearchProvider)
	}
	for engineName := range got.Engines {
		if engineName == "global-spy" {
			t.Errorf("global provider name leaked into telemetry engines: %v", got.Engines)
		}
	}
}

// F36-T6: concurrent runs with mixed overrides must not mutate global state.
func TestOverride_ConcurrentNoGlobalMutation(t *testing.T) {
	srv := newTestLLMServer(t, "answer")
	global := &spySearchProvider{}
	globalClient := llm.NewClient(srv.URL, "", "global-model")
	engine := NewEngine(global, &mockReader{}, globalClient, nil)

	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			params := SearchParams{
				Query:          fmt.Sprintf("concurrent-%d", i),
				UILanguage:     "it",
				AnswerLanguage: "it",
				Mode:           "standard",
			}
			if i%2 == 0 {
				params.SearchProviderOverride = &spySearchProvider{}
				params.LLMClientOverride = llm.NewClient(srv.URL, "", "override-model")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := engine.ExecuteDeepPath(ctx, params, func(Event) error { return nil }); err != nil {
				t.Errorf("worker %d failed: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if engine.SearchProvider() != search.Provider(global) {
		t.Errorf("global search provider mutated under concurrency")
	}
	if engine.LLMClient() != globalClient {
		t.Errorf("global LLM client mutated under concurrency")
	}
}
