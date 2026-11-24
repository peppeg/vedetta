package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vedetta/config"
	"vedetta/internal/agent"
)

// F36-T5: an unregistered/unknown search_provider override is rejected with 400
// before any SSE stream is opened.
func TestSearch_UnregisteredProvider400(t *testing.T) {
	cfg := &config.Config{
		Port:            "3030",
		SearchProviders: []string{"searxng"},
	}
	engine := agent.NewEngine(&dummySearchProvider{}, nil, nil, nil)
	handlers := NewHandlers(cfg, engine)

	reqBody, _ := json.Marshal(map[string]interface{}{
		"query":           "override provider test",
		"mode":            "fast",
		"search_provider": "nope",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/search", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handlers.Search(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown search provider") {
		t.Errorf("expected unknown search provider error, got: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "event:") {
		t.Errorf("SSE stream must not start on invalid override, got: %s", rec.Body.String())
	}
}

func TestShouldApplySearchOverride(t *testing.T) {
	cases := []struct {
		defaultProvider string
		requested       string
		want            bool
	}{
		{"searxng", "", false},
		{"searxng", "searxng", false},
		{"searxng", "SearXNG", false},
		{"searxng", "  searxng  ", false},
		{"searxng", "brave", true},
		{"", "brave", true},
		{"", "", false},
	}
	for _, c := range cases {
		if got := shouldApplySearchOverride(c.defaultProvider, c.requested); got != c.want {
			t.Errorf("shouldApplySearchOverride(%q, %q) = %v, want %v", c.defaultProvider, c.requested, got, c.want)
		}
	}
}

// F36-T5: an unknown llm_endpoint override is rejected with 400 before streaming.
func TestSearch_UnknownLLMEndpoint400(t *testing.T) {
	cfg := &config.Config{
		Port: "3030",
		LLMEndpoints: []config.LLMEndpoint{
			{ID: "default", Name: "default", BaseURL: "http://localhost:9999", Model: "m"},
		},
	}
	engine := agent.NewEngine(&dummySearchProvider{}, nil, nil, nil)
	handlers := NewHandlers(cfg, engine)

	reqBody, _ := json.Marshal(map[string]interface{}{
		"query":        "override endpoint test",
		"mode":         "fast",
		"llm_endpoint": "ghost",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/search", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handlers.Search(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown llm endpoint") {
		t.Errorf("expected unknown llm endpoint error, got: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "event:") {
		t.Errorf("SSE stream must not start on invalid override, got: %s", rec.Body.String())
	}
}
