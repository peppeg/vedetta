package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"vedetta/config"
	"vedetta/internal/agent"
)

func newExtendedConfigHandlers(t *testing.T, cfg *config.Config) *Handlers {
	t.Helper()
	engine := agent.NewEngine(&dummySearchProvider{}, nil, nil, nil)
	return NewHandlers(cfg, engine)
}

func postConfigUpdate(t *testing.T, h *Handlers, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("failed to marshal config update: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Config(rec, req)
	return rec
}

func TestConfig_ExtendedResponse(t *testing.T) {
	cfg := &config.Config{
		Version:               "1.2.3",
		ContactEmail:          "ops@example.com",
		DefaultSearchProvider: "brave",
		SearchProviders:       []string{"brave", "searxng"},
		ReaderProviders:       []string{"jina"},
		LLMProviders:          []string{"deepseek"},
		SearxngURL:            "http://localhost:8888",
		JinaURL:               "http://localhost:3001",
		LLMBaseURL:            "http://localhost:8000/v1",
		LLMModel:              "deepseek-chat",
		ConfigPath:            t.TempDir() + "/config.local",
	}
	h := newExtendedConfigHandlers(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	h.Config(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on GET /api/config, got %d", rec.Code)
	}
	var resp ConfigResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode GET /api/config response: %v", err)
	}
	if resp.Version != "1.2.3" {
		t.Errorf("version = %q, want %q", resp.Version, "1.2.3")
	}
	if resp.ContactEmail != "ops@example.com" {
		t.Errorf("contact_email = %q, want %q", resp.ContactEmail, "ops@example.com")
	}
	if resp.DefaultSearchProvider != "brave" {
		t.Errorf("default_search_provider = %q, want %q", resp.DefaultSearchProvider, "brave")
	}
}

func TestConfig_NoKeysLeaked(t *testing.T) {
	keys := map[string]string{
		"brave":     "brave-secret-value-123",
		"tavily":    "tavily-secret-value-456",
		"serper":    "serper-secret-value-789",
		"firecrawl": "firecrawl-secret-value-abc",
		"llm":       "llm-secret-value-def",
	}
	cfg := &config.Config{
		SearchProviders: []string{"searxng"},
		ReaderProviders: []string{"jina"},
		LLMProviders:    []string{"deepseek"},
		BraveAPIKey:     keys["brave"],
		TavilyAPIKey:    keys["tavily"],
		SerperAPIKey:    keys["serper"],
		FirecrawlAPIKey: keys["firecrawl"],
		LLMAPIKey:       keys["llm"],
		SearxngURL:      "http://localhost:8888",
		JinaURL:         "http://localhost:3001",
		LLMBaseURL:      "http://localhost:8000/v1",
		LLMModel:        "deepseek-chat",
		ConfigPath:      t.TempDir() + "/config.local",
	}
	h := newExtendedConfigHandlers(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	h.Config(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on GET /api/config, got %d", rec.Code)
	}
	body := rec.Body.String()
	for name, key := range keys {
		if strings.Contains(body, key) {
			t.Errorf("response body leaks %s key", name)
		}
	}

	var resp ConfigResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("failed to decode GET /api/config response: %v", err)
	}
	if !resp.HasBraveKey || !resp.HasTavilyKey || !resp.HasSerperKey || !resp.HasFirecrawlKey || !resp.HasLLMKey {
		t.Errorf("has_*_key flags not all true: %+v", resp)
	}
}

func TestConfig_MaskPreservesKeys(t *testing.T) {
	mask := "********"
	cfg := &config.Config{
		SearchProviders: []string{"searxng"},
		ReaderProviders: []string{"jina"},
		LLMProviders:    []string{"deepseek"},
		BraveAPIKey:     "keep-brave",
		TavilyAPIKey:    "keep-tavily",
		SerperAPIKey:    "keep-serper",
		FirecrawlAPIKey: "keep-firecrawl",
		LLMAPIKey:       "keep-llm",
		SearxngURL:      "http://localhost:8888",
		JinaURL:         "http://localhost:3001",
		LLMBaseURL:      "http://localhost:8000/v1",
		LLMModel:        "deepseek-chat",
		ConfigPath:      t.TempDir() + "/config.local",
	}
	h := newExtendedConfigHandlers(t, cfg)

	rec := postConfigUpdate(t, h, ConfigUpdateRequest{
		BraveAPIKey:     &mask,
		TavilyAPIKey:    &mask,
		SerperAPIKey:    &mask,
		FirecrawlAPIKey: &mask,
		LLMAPIKey:       &mask,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on masked POST /api/config, got %d: %s", rec.Code, rec.Body.String())
	}

	h.cfgMu.RLock()
	gotBrave := h.cfg.BraveAPIKey
	gotTavily := h.cfg.TavilyAPIKey
	gotSerper := h.cfg.SerperAPIKey
	gotFirecrawl := h.cfg.FirecrawlAPIKey
	gotLLM := h.cfg.LLMAPIKey
	h.cfgMu.RUnlock()

	if gotBrave != "keep-brave" || gotTavily != "keep-tavily" || gotSerper != "keep-serper" ||
		gotFirecrawl != "keep-firecrawl" || gotLLM != "keep-llm" {
		t.Errorf("masked update mutated in-memory keys: brave=%q tavily=%q serper=%q firecrawl=%q llm=%q",
			gotBrave, gotTavily, gotSerper, gotFirecrawl, gotLLM)
	}

	data, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatalf("failed to read persisted config: %v", err)
	}
	var f config.LocalConfigFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("failed to decode persisted config: %v", err)
	}
	if f.BraveAPIKey != "keep-brave" || f.TavilyAPIKey != "keep-tavily" || f.SerperAPIKey != "keep-serper" ||
		f.FirecrawlAPIKey != "keep-firecrawl" || f.LLMAPIKey != "keep-llm" {
		t.Errorf("masked update mutated persisted keys: %+v", f)
	}
}

func TestConfig_ExtendedFieldsPersisted(t *testing.T) {
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
	h := newExtendedConfigHandlers(t, cfg)

	email := "new@example.com"
	provider := "searxng"
	rec := postConfigUpdate(t, h, ConfigUpdateRequest{
		ContactEmail:          &email,
		DefaultSearchProvider: &provider,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on POST /api/config, got %d: %s", rec.Code, rec.Body.String())
	}

	data, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatalf("failed to read persisted config: %v", err)
	}
	var f config.LocalConfigFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("failed to decode persisted config: %v", err)
	}
	if f.ContactEmail != email {
		t.Errorf("persisted contact_email = %q, want %q", f.ContactEmail, email)
	}
	if f.DefaultSearchProvider != provider {
		t.Errorf("persisted default_search_provider = %q, want %q", f.DefaultSearchProvider, provider)
	}
}

func TestConfig_SearchProvidersChangeHealsDefault(t *testing.T) {
	cfg := &config.Config{
		SearchProviders:       []string{"brave", "searxng"},
		ReaderProviders:       []string{"jina"},
		LLMProviders:          []string{"deepseek"},
		DefaultSearchProvider: "brave",
		SearxngURL:            "http://localhost:8888",
		JinaURL:               "http://localhost:3001",
		LLMBaseURL:            "http://localhost:8000/v1",
		LLMModel:              "deepseek-chat",
		ConfigPath:            t.TempDir() + "/config.local",
	}
	h := newExtendedConfigHandlers(t, cfg)

	// No default_search_provider in the body: the default must auto-heal to the
	// first still-enabled provider instead of failing with 400.
	rec := postConfigUpdate(t, h, ConfigUpdateRequest{SearchProviders: []string{"searxng"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on provider-list change without explicit default, got %d: %s", rec.Code, rec.Body.String())
	}

	h.cfgMu.RLock()
	got := h.cfg.DefaultSearchProvider
	h.cfgMu.RUnlock()
	if got != "searxng" {
		t.Errorf("default_search_provider = %q, want healed value %q", got, "searxng")
	}
}

func TestConfig_InvalidDefaultProvider400(t *testing.T) {
	cfg := &config.Config{
		SearchProviders:       []string{"searxng"},
		ReaderProviders:       []string{"jina"},
		LLMProviders:          []string{"deepseek"},
		DefaultSearchProvider: "searxng",
		SearxngURL:            "http://localhost:8888",
		JinaURL:               "http://localhost:3001",
		LLMBaseURL:            "http://localhost:8000/v1",
		LLMModel:              "deepseek-chat",
		ConfigPath:            t.TempDir() + "/config.local",
	}
	h := newExtendedConfigHandlers(t, cfg)

	bad := "nope"
	rec := postConfigUpdate(t, h, ConfigUpdateRequest{DefaultSearchProvider: &bad})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on unknown default provider, got %d: %s", rec.Code, rec.Body.String())
	}

	h.cfgMu.RLock()
	got := h.cfg.DefaultSearchProvider
	h.cfgMu.RUnlock()
	if got != "searxng" {
		t.Errorf("default_search_provider mutated to %q after rejected update, want %q", got, "searxng")
	}
}

func newLLMRegistryConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		SearchProviders: []string{"searxng"},
		ReaderProviders: []string{"jina"},
		LLMProviders:    []string{"deepseek"},
		SearxngURL:      "http://localhost:8888",
		JinaURL:         "http://localhost:3001",
		LLMBaseURL:      "http://localhost:8000/v1",
		LLMModel:        "deepseek-chat",
		ConfigPath:      t.TempDir() + "/config.local",
	}
}

func TestConfig_LLMEndpointsRoundTrip(t *testing.T) {
	cfg := newLLMRegistryConfig(t)
	h := newExtendedConfigHandlers(t, cfg)

	keyA := "secret-a"
	keyB := "secret-b"
	defaultID := "b"
	rec := postConfigUpdate(t, h, ConfigUpdateRequest{
		LLMEndpoints: []LLMEndpointUpdate{
			{ID: "a", Name: "Endpoint A", BaseURL: "https://a.example/v1", Model: "model-a", APIKey: &keyA},
			{ID: "b", Name: "Endpoint B", BaseURL: "https://b.example/v1", Model: "model-b", APIKey: &keyB},
		},
		DefaultLLMEndpoint: &defaultID,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on endpoint registry POST, got %d: %s", rec.Code, rec.Body.String())
	}

	// GET must expose both endpoints (without keys) and the default id.
	getRec := httptest.NewRecorder()
	h.Config(getRec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on GET, got %d", getRec.Code)
	}
	var resp ConfigResponse
	if err := json.Unmarshal(getRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode GET response: %v", err)
	}
	if len(resp.LLMEndpoints) != 2 {
		t.Fatalf("len(llm_endpoints) = %d, want 2", len(resp.LLMEndpoints))
	}
	if resp.DefaultLLMEndpoint != "b" {
		t.Errorf("default_llm_endpoint = %q, want %q", resp.DefaultLLMEndpoint, "b")
	}
	if !resp.LLMEndpoints[0].HasKey || !resp.LLMEndpoints[1].HasKey {
		t.Errorf("has_key flags = (%v, %v), want (true, true)", resp.LLMEndpoints[0].HasKey, resp.LLMEndpoints[1].HasKey)
	}
	if resp.LLMEndpoints[0].Name != "Endpoint A" || resp.LLMEndpoints[1].Name != "Endpoint B" {
		t.Errorf("endpoint names not round-tripped: %+v", resp.LLMEndpoints)
	}

	// Legacy mirror must follow the default endpoint.
	h.cfgMu.RLock()
	legacyBase, legacyModel, legacyKey := h.cfg.LLMBaseURL, h.cfg.LLMModel, h.cfg.LLMAPIKey
	h.cfgMu.RUnlock()
	if legacyBase != "https://b.example/v1" || legacyModel != "model-b" || legacyKey != "secret-b" {
		t.Errorf("legacy mirror = (%q, %q, %q), want default endpoint values", legacyBase, legacyModel, legacyKey)
	}

	// Persisted file must be coherent.
	data, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatalf("failed to read persisted config: %v", err)
	}
	var f config.LocalConfigFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("failed to decode persisted config: %v", err)
	}
	if len(f.LLMEndpoints) != 2 {
		t.Fatalf("persisted len(llm_endpoints) = %d, want 2", len(f.LLMEndpoints))
	}
	if f.LLMDefaultID != "b" {
		t.Errorf("persisted llm_default_id = %q, want %q", f.LLMDefaultID, "b")
	}
	if f.LLMBaseURL != "https://b.example/v1" || f.LLMAPIKey != "secret-b" {
		t.Errorf("persisted legacy mirror = (%q, %q), want default endpoint values", f.LLMBaseURL, f.LLMAPIKey)
	}
}

func TestConfig_LLMEndpointsKeyNeverReturned(t *testing.T) {
	cfg := newLLMRegistryConfig(t)
	h := newExtendedConfigHandlers(t, cfg)

	secret := "super-secret-llm-key"
	rec := postConfigUpdate(t, h, ConfigUpdateRequest{
		LLMEndpoints: []LLMEndpointUpdate{
			{ID: "main", BaseURL: "https://main.example/v1", Model: "model-main", APIKey: &secret},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on endpoint POST, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("POST response body leaks the endpoint api_key")
	}

	getRec := httptest.NewRecorder()
	h.Config(getRec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on GET, got %d", getRec.Code)
	}
	body := getRec.Body.String()
	if strings.Contains(body, secret) {
		t.Errorf("GET response body leaks the endpoint api_key")
	}

	var resp ConfigResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("failed to decode GET response: %v", err)
	}
	if len(resp.LLMEndpoints) != 1 {
		t.Fatalf("len(llm_endpoints) = %d, want 1", len(resp.LLMEndpoints))
	}
	if !resp.LLMEndpoints[0].HasKey {
		t.Errorf("has_key = false, want true")
	}
}

func TestConfig_LLMEndpointsMaskKeepsKey(t *testing.T) {
	cfg := newLLMRegistryConfig(t)
	cfg.LLMEndpoints = []config.LLMEndpoint{
		{ID: "main", Name: "Main", BaseURL: "http://original/v1", Model: "orig-model", APIKey: "orig-key"},
	}
	cfg.LLMDefaultID = "main"
	cfg.LLMBaseURL = "http://original/v1"
	cfg.LLMModel = "orig-model"
	cfg.LLMAPIKey = "orig-key"
	h := newExtendedConfigHandlers(t, cfg)

	mask := "********"
	rec := postConfigUpdate(t, h, ConfigUpdateRequest{
		LLMEndpoints: []LLMEndpointUpdate{
			// same host, another path: the key stays (map R6, T9.3)
			{ID: "main", Name: "Main", BaseURL: "http://original/v2", Model: "new-model", APIKey: &mask},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on masked endpoint POST, got %d: %s", rec.Code, rec.Body.String())
	}

	h.cfgMu.RLock()
	gotList := append([]config.LLMEndpoint(nil), h.cfg.LLMEndpoints...)
	gotLegacyKey := h.cfg.LLMAPIKey
	h.cfgMu.RUnlock()
	if len(gotList) != 1 || gotList[0].APIKey != "orig-key" {
		t.Errorf("masked update mutated endpoint key: %+v", gotList)
	}
	if gotLegacyKey != "orig-key" {
		t.Errorf("masked update mutated legacy LLMAPIKey: %q", gotLegacyKey)
	}

	data, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatalf("failed to read persisted config: %v", err)
	}
	var f config.LocalConfigFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("failed to decode persisted config: %v", err)
	}
	if len(f.LLMEndpoints) != 1 || f.LLMEndpoints[0].APIKey != "orig-key" {
		t.Errorf("masked update mutated persisted endpoint key: %+v", f.LLMEndpoints)
	}
	if f.LLMAPIKey != "orig-key" {
		t.Errorf("masked update mutated persisted legacy key: %q", f.LLMAPIKey)
	}
}

// Map R6 (T9.3): a saved key is never sent to another host. Changing the host of a
// model without typing a key drops the saved one, in the registry and through the
// legacy single-model fields.
func TestConfig_KeyDroppedWhenHostChanges(t *testing.T) {
	mask := "********"
	for _, viaLegacy := range []bool{false, true} {
		cfg := newLLMRegistryConfig(t)
		cfg.LLMEndpoints = []config.LLMEndpoint{
			{ID: "main", Name: "Main", BaseURL: "https://api.example.org/v1", Model: "m", APIKey: "orig-key"},
		}
		cfg.LLMDefaultID = "main"
		cfg.LLMBaseURL, cfg.LLMModel, cfg.LLMAPIKey = "https://api.example.org/v1", "m", "orig-key"
		h := newExtendedConfigHandlers(t, cfg)

		var req ConfigUpdateRequest
		if viaLegacy {
			u := "https://evil.example.net/v1"
			req = ConfigUpdateRequest{LLMBaseURL: &u}
		} else {
			req = ConfigUpdateRequest{LLMEndpoints: []LLMEndpointUpdate{
				{ID: "main", Name: "Main", BaseURL: "https://evil.example.net/v1", Model: "m", APIKey: &mask},
			}}
		}
		if rec := postConfigUpdate(t, h, req); rec.Code != http.StatusOK {
			t.Fatalf("legacy=%t: %d %s", viaLegacy, rec.Code, rec.Body.String())
		}
		h.cfgMu.RLock()
		got := h.cfg.LLMEndpoints[0]
		legacy := h.cfg.LLMAPIKey
		h.cfgMu.RUnlock()
		if got.APIKey != "" || legacy != "" {
			t.Errorf("legacy=%t: the key followed the model to another host: endpoint %q, legacy %q", viaLegacy, got.APIKey, legacy)
		}
		data, _ := os.ReadFile(cfg.ConfigPath)
		if strings.Contains(string(data), "orig-key") {
			t.Errorf("legacy=%t: the old key is still saved", viaLegacy)
		}
	}
}

func TestConfig_LLMEndpointsInvalid400(t *testing.T) {
	cases := []struct {
		name string
		req  ConfigUpdateRequest
	}{
		{
			name: "duplicate id",
			req: ConfigUpdateRequest{LLMEndpoints: []LLMEndpointUpdate{
				{ID: "dup", BaseURL: "https://a/v1", Model: "m"},
				{ID: "dup", BaseURL: "https://b/v1", Model: "m"},
			}},
		},
		{
			name: "empty base_url",
			req: ConfigUpdateRequest{LLMEndpoints: []LLMEndpointUpdate{
				{ID: "a", BaseURL: "", Model: "m"},
			}},
		},
		{
			name: "dangling default",
			req:  ConfigUpdateRequest{DefaultLLMEndpoint: strPtr(t, "ghost")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newLLMRegistryConfig(t)
			h := newExtendedConfigHandlers(t, cfg)

			rec := postConfigUpdate(t, h, tc.req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}

			// No commit: the in-memory config must be untouched.
			h.cfgMu.RLock()
			gotBaseURL := h.cfg.LLMBaseURL
			gotDefault := h.cfg.LLMDefaultID
			h.cfgMu.RUnlock()
			if gotBaseURL != "http://localhost:8000/v1" {
				t.Errorf("LLMBaseURL mutated to %q after rejected update", gotBaseURL)
			}
			if gotDefault != "" {
				t.Errorf("LLMDefaultID mutated to %q after rejected update", gotDefault)
			}
		})
	}
}

func strPtr(t *testing.T, s string) *string {
	t.Helper()
	return &s
}

// DL3: the settings page sends no context_tokens; saving must keep the stored one.
func TestConfig_LLMEndpointsKeepContextTokens(t *testing.T) {
	cfg := newLLMRegistryConfig(t)
	cfg.LLMEndpoints = []config.LLMEndpoint{
		{ID: "main", Name: "Main", BaseURL: "http://original/v1", Model: "m", ContextTokens: 32768},
		{ID: "other", Name: "Other", BaseURL: "http://other/v1", Model: "o"},
	}
	cfg.LLMDefaultID = "main"
	h := newExtendedConfigHandlers(t, cfg)

	set := 65536
	rec := postConfigUpdate(t, h, ConfigUpdateRequest{
		LLMEndpoints: []LLMEndpointUpdate{
			{ID: "main", Name: "Main renamed", BaseURL: "http://original/v1", Model: "m"},
			{ID: "other", Name: "Other", BaseURL: "http://other/v1", Model: "o", ContextTokens: &set},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	h.cfgMu.RLock()
	got := append([]config.LLMEndpoint(nil), h.cfg.LLMEndpoints...)
	h.cfgMu.RUnlock()
	if len(got) != 2 || got[0].ContextTokens != 32768 || got[1].ContextTokens != 65536 {
		t.Fatalf("endpoints = %+v, want main kept at 32768 and other set to 65536", got)
	}
}
