package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"vedetta/config"
	"vedetta/internal/agent"
	"vedetta/internal/llm"
	"vedetta/internal/search"
)

type dummySearchProvider struct{}

func (d *dummySearchProvider) Name() string { return "dummy" }
func (d *dummySearchProvider) Search(ctx context.Context, query string, opts search.Options) ([]search.Result, error) {
	return []search.Result{
		{Title: "Test", URL: "https://example.com/test", Content: "Content snippet"},
	}, nil
}

func TestSearchHandler_DeepDispatch(t *testing.T) {
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"deep answer\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer llmSrv.Close()

	cfg := &config.Config{
		Port: "3030",
	}
	client := llm.NewClient(llmSrv.URL, "", "test-model")
	engine := agent.NewEngine(&dummySearchProvider{}, nil, client, nil)
	handlers := NewHandlers(cfg, engine)

	reqBody, _ := json.Marshal(map[string]interface{}{
		"query": "What is deep research?",
		"mode":  "deep",
		"lang":  "en",
	})

	req := httptest.NewRequest(http.MethodPost, "/api/search", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handlers.Search(rec, req)

	res := rec.Body.String()
	// The Deep a ciclo answers: step events, then the model's text.
	if !strings.Contains(res, "event: step") {
		t.Errorf("expected step events in deep stream, got: %s", res)
	}
	if !strings.Contains(res, "deep answer") {
		t.Errorf("expected the model's answer in the stream, got: %s", res)
	}
}

func TestConfigHandler_GetAndPost(t *testing.T) {
	cfg := &config.Config{
		SearchProviders:       []string{"searxng"},
		ReaderProviders:       []string{"jina"},
		LLMProviders:          []string{"deepseek"},
		SearchFallbackOnEmpty: false,
		SearxngURL:            "http://localhost:8888",
		JinaURL:               "http://localhost:3001",
		LLMBaseURL:            "http://localhost:8000/v1",
		LLMModel:              "deepseek-chat",
		LLMAPIKey:             "secret-llm-key",
		ConfigPath:            t.TempDir() + "/config.local",
	}
	engine := agent.NewEngine(&dummySearchProvider{}, nil, nil, nil)
	handlers := NewHandlers(cfg, engine)

	// 1. GET /api/config
	reqGet := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	recGet := httptest.NewRecorder()
	handlers.Config(recGet, reqGet)

	if recGet.Code != http.StatusOK {
		t.Fatalf("expected status 200 on GET /api/config, got %d", recGet.Code)
	}
	var respGet ConfigResponse
	if err := json.NewDecoder(recGet.Body).Decode(&respGet); err != nil {
		t.Fatalf("failed to decode GET /api/config response: %v", err)
	}
	if len(respGet.SearchProviders) != 1 || respGet.SearchProviders[0] != "searxng" {
		t.Errorf("expected search_providers ['searxng'], got %v", respGet.SearchProviders)
	}
	if strings.Contains(recGet.Body.String(), "llm_synth_temperature") {
		t.Errorf("the writer's temperature is gone (map R2), the response still has it")
	}

	// 2. POST /api/config (update reader providers)
	updateBody, _ := json.Marshal(ConfigUpdateRequest{
		ReaderProviders: []string{"http", "jina"},
	})
	reqPost := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(updateBody))
	reqPost.Header.Set("Content-Type", "application/json")
	recPost := httptest.NewRecorder()
	handlers.Config(recPost, reqPost)

	if recPost.Code != http.StatusOK {
		t.Fatalf("expected status 200 on POST /api/config, got %d: %s", recPost.Code, recPost.Body.String())
	}
	var respPost ConfigResponse
	if err := json.NewDecoder(recPost.Body).Decode(&respPost); err != nil {
		t.Fatalf("failed to decode POST /api/config response: %v", err)
	}
	if len(respPost.ReaderProviders) != 2 || respPost.ReaderProviders[0] != "http" {
		t.Errorf("expected reader_providers ['http', 'jina'], got %v", respPost.ReaderProviders)
	}
	if engine.ReaderProviderName() != "http" {
		t.Errorf("expected engine.ReaderProviderName 'http', got %q", engine.ReaderProviderName())
	}

	// Regression: NewClient(baseURL, apiKey, model) argument order must not be swapped.
	if got := engine.LLMClient().Model(); got != "deepseek-chat" {
		t.Errorf("expected rebuilt LLM model 'deepseek-chat', got %q (arg order swapped?)", got)
	}
	if got := engine.LLMClient().BaseURL(); got != "http://localhost:8000/v1" {
		t.Errorf("expected rebuilt LLM base URL 'http://localhost:8000/v1', got %q", got)
	}
	if !engine.LLMClient().HasAPIKey() {
		t.Errorf("expected rebuilt LLM client to retain the API key")
	}
}

// TestConfigHandler_TransactionalFailure verifies that a failed provider rebuild
// leaves the live configuration untouched (no partial mutation).
func TestConfigHandler_TransactionalFailure(t *testing.T) {
	cfg := &config.Config{
		SearchProviders: []string{"searxng"},
		ReaderProviders: []string{"jina"},
		LLMProviders:    []string{"deepseek"},
		SearxngURL:      "http://localhost:8888",
		JinaURL:         "http://localhost:3001",
		LLMBaseURL:      "http://localhost:8000/v1",
		LLMModel:        "deepseek-chat",
		ConfigPath:      t.TempDir() + "/config.local",
	}
	engine := agent.NewEngine(&dummySearchProvider{}, nil, nil, nil)
	handlers := NewHandlers(cfg, engine)

	// Unknown reader provider forces reader.NewReader to fail.
	body, _ := json.Marshal(ConfigUpdateRequest{
		ReaderProviders: []string{"not-a-real-reader"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handlers.Config(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on invalid reader provider, got %d", rec.Code)
	}

	// Live config must be unchanged and engine must keep the original reader.
	handlers.cfgMu.RLock()
	gotReaders := append([]string(nil), handlers.cfg.ReaderProviders...)
	handlers.cfgMu.RUnlock()
	if len(gotReaders) != 1 || gotReaders[0] != "jina" {
		t.Errorf("expected reader providers to remain ['jina'], got %v", gotReaders)
	}
	if engine.ReaderProviderName() == "not-a-real-reader" {
		t.Errorf("engine reader must not have been reconfigured to the invalid provider")
	}
}

// TestConfigHandler_AuthToken verifies the optional VEDETTA_CONFIG_TOKEN guard.
func TestConfigHandler_AuthToken(t *testing.T) {
	cfg := &config.Config{
		SearchProviders: []string{"searxng"},
		ReaderProviders: []string{"jina"},
		LLMProviders:    []string{"deepseek"},
		SearxngURL:      "http://localhost:8888",
		JinaURL:         "http://localhost:3001",
		LLMBaseURL:      "http://localhost:8000/v1",
		LLMModel:        "deepseek-chat",
		ConfigToken:     "s3cr3t-token",
		ConfigPath:      t.TempDir() + "/config.local",
	}
	engine := agent.NewEngine(&dummySearchProvider{}, nil, nil, nil)
	handlers := NewHandlers(cfg, engine)

	// Without token -> 401
	reqNoToken := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	recNoToken := httptest.NewRecorder()
	handlers.Config(recNoToken, reqNoToken)
	if recNoToken.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", recNoToken.Code)
	}

	// With wrong token -> 401
	reqWrong := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	reqWrong.Header.Set("X-Vedetta-Token", "nope")
	recWrong := httptest.NewRecorder()
	handlers.Config(recWrong, reqWrong)
	if recWrong.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong token, got %d", recWrong.Code)
	}

	// With correct token -> 200
	reqOK := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	reqOK.Header.Set("X-Vedetta-Token", "s3cr3t-token")
	recOK := httptest.NewRecorder()
	handlers.Config(recOK, reqOK)
	if recOK.Code != http.StatusOK {
		t.Fatalf("expected 200 with correct token, got %d", recOK.Code)
	}
}

// Map R3: Settings switches the experimental levers; the engine takes them at
// once (for the next search) and config.local keeps the choice.
func TestConfigHandler_ExperimentalLevers(t *testing.T) {
	path := t.TempDir() + "/config.local"
	cfg := &config.Config{
		SearchProviders: []string{"searxng"},
		ReaderProviders: []string{"jina"},
		LLMProviders:    []string{"deepseek"},
		SearxngURL:      "http://localhost:8888",
		JinaURL:         "http://localhost:3001",
		LLMBaseURL:      "http://localhost:8000/v1",
		LLMModel:        "deepseek-chat",
		ConfigPath:      path,
	}
	engine := agent.NewEngine(&dummySearchProvider{}, nil, nil, nil)
	handlers := NewHandlers(cfg, engine)

	on := true
	body, _ := json.Marshal(ConfigUpdateRequest{DeepLoopPush: &on})
	rec := httptest.NewRecorder()
	handlers.Config(rec, httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST: %d %s", rec.Code, rec.Body.String())
	}
	var resp ConfigResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.DeepLoopPush || resp.DeepLoopReviewLines {
		t.Errorf("response: push=%t review_lines=%t, want true and false", resp.DeepLoopPush, resp.DeepLoopReviewLines)
	}
	if !engine.DeepLoopPush() || engine.DeepLoopReviewLines() {
		t.Errorf("engine: push=%t review_lines=%t", engine.DeepLoopPush(), engine.DeepLoopReviewLines())
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"deep_loop_push": true`) || strings.Contains(string(raw), "deep_loop_review_lines") || strings.Contains(string(raw), "deep_loop_grant") {
		t.Errorf("config.local should keep only the chosen lever: %s", raw)
	}

	body, _ = json.Marshal(ConfigUpdateRequest{DeepLoopGrant: &on})
	rec = httptest.NewRecorder()
	handlers.Config(rec, httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body)))
	if raw, _ = os.ReadFile(path); rec.Code != http.StatusOK || !engine.DeepLoopGrant() || !strings.Contains(string(raw), `"deep_loop_grant": true`) {
		t.Errorf("special permission: code %d, engine %t, saved %s", rec.Code, engine.DeepLoopGrant(), raw)
	}
}
