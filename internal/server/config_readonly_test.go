package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vedetta/config"
	"vedetta/internal/agent"
)

func newReadOnlyConfigHandlers(t *testing.T, cfg *config.Config) *Handlers {
	t.Helper()
	engine := agent.NewEngine(&dummySearchProvider{}, nil, nil, nil)
	return NewHandlers(cfg, engine)
}

func getConfigBody(t *testing.T, h *Handlers) (string, ConfigResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	h.cfgMu.RLock()
	token := h.cfg.ConfigToken
	h.cfgMu.RUnlock()
	if token != "" {
		req.Header.Set("X-Vedetta-Token", token)
	}
	rec := httptest.NewRecorder()
	h.Config(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on GET /api/config, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	var resp ConfigResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("failed to decode GET /api/config response: %v", err)
	}
	return body, resp
}

func TestConfig_ReadOnlyRuntimeAndLimits(t *testing.T) {
	cfg := &config.Config{
		Port:                 "3030",
		ConfigToken:          "super-secret-token",
		PresetIT:             true,
		EmbeddingsURL:        "http://embed.local",
		EmbeddingsAPIKey:     "embed-secret",
		FirecrawlBaseURL:     "http://fc.local",
		SynthesisTimeoutSecs: 900,
		LLMEffortDeep:        "high",
		ConfigPath:           t.TempDir() + "/config.local",
	}
	h := newReadOnlyConfigHandlers(t, cfg)

	_, resp := getConfigBody(t, h)

	if resp.Runtime.Port != "3030" {
		t.Errorf("runtime.port = %q, want %q", resp.Runtime.Port, "3030")
	}
	if !resp.Runtime.ConfigTokenSet {
		t.Errorf("runtime.config_token_set = false, want true")
	}
	if !resp.Runtime.PresetIT {
		t.Errorf("runtime.preset_it = false, want true")
	}
	if resp.Runtime.LLMEffortDeep != "high" {
		t.Errorf("runtime.llm_effort_deep = %q, want %q", resp.Runtime.LLMEffortDeep, "high")
	}
	if !resp.Runtime.HasEmbeddingsKey {
		t.Errorf("runtime.has_embeddings_key = false, want true")
	}
	if resp.Runtime.EmbeddingsURL != "http://embed.local" {
		t.Errorf("runtime.embeddings_url = %q, want %q", resp.Runtime.EmbeddingsURL, "http://embed.local")
	}
	if resp.Runtime.FirecrawlBaseURL != "http://fc.local" {
		t.Errorf("runtime.firecrawl_base_url = %q, want %q", resp.Runtime.FirecrawlBaseURL, "http://fc.local")
	}
	if resp.Limits.SynthesisTimeoutSecs != 900 {
		t.Errorf("limits.synthesis_timeout_secs = %d, want 900", resp.Limits.SynthesisTimeoutSecs)
	}
}

func TestConfig_ReadOnlyNeverLeaksSecrets(t *testing.T) {
	cfg := &config.Config{
		ConfigToken:      "super-secret-token",
		EmbeddingsAPIKey: "embed-secret",
		ConfigPath:       t.TempDir() + "/config.local",
	}
	h := newReadOnlyConfigHandlers(t, cfg)

	body, resp := getConfigBody(t, h)

	if strings.Contains(body, "super-secret-token") {
		t.Errorf("response body leaks the config token")
	}
	if strings.Contains(body, "embed-secret") {
		t.Errorf("response body leaks the embeddings api key")
	}
	if !resp.Runtime.ConfigTokenSet {
		t.Errorf("runtime.config_token_set = false, want true")
	}
	if !resp.Runtime.HasEmbeddingsKey {
		t.Errorf("runtime.has_embeddings_key = false, want true")
	}
}

func TestConfig_NoCorePolicyExposed(t *testing.T) {
	cfg := &config.Config{
		ConfigPath: t.TempDir() + "/config.local",
	}
	h := newReadOnlyConfigHandlers(t, cfg)

	body, _ := getConfigBody(t, h)

	forbidden := []string{
		"plan_facets", "PlanFacets",
		"rrf", "rrf_k", "RRF", "RRFK",
		"domain_cap", "DomainCap",
		"intent_rerank", "IntentRerank",
		"authority_boost", "AuthorityBoost",
		"project_penalty", "ProjectPenalty",
		"municipal_penalty", "MunicipalPenalty",
	}
	for _, token := range forbidden {
		if strings.Contains(body, token) {
			t.Errorf("response body exposes core policy token %q: %s", token, body)
		}
	}
}

func TestConfig_RuntimeLimitsAlwaysPresent(t *testing.T) {
	cfg := &config.Config{
		ConfigPath: t.TempDir() + "/config.local",
	}
	h := newReadOnlyConfigHandlers(t, cfg)

	body, _ := getConfigBody(t, h)

	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("failed to decode response into map: %v", err)
	}
	runtimeRaw, ok := raw["runtime"]
	if !ok {
		t.Fatalf("top-level key %q missing from response: %s", "runtime", body)
	}
	if _, ok := runtimeRaw.(map[string]any); !ok {
		t.Fatalf("runtime is not a JSON object: %T", runtimeRaw)
	}
	limitsRaw, ok := raw["limits"]
	if !ok {
		t.Fatalf("top-level key %q missing from response: %s", "limits", body)
	}
	limits, ok := limitsRaw.(map[string]any)
	if !ok {
		t.Fatalf("limits is not a JSON object: %T", limitsRaw)
	}
	// The research's time limits are always there, even when zero (not omitted).
	for _, key := range []string{"synthesis_timeout_secs", "deep_session_timeout_secs", "synth_content_stall_secs"} {
		v, ok := limits[key]
		if !ok {
			t.Errorf("limits.%s missing from response", key)
			continue
		}
		if n, ok := v.(float64); !ok || n != 0 {
			t.Errorf("limits.%s = %v, want 0", key, v)
		}
	}
}
