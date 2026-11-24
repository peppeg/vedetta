package config

import "testing"

func TestNormalizeLLMEndpoints_BackwardCompat(t *testing.T) {
	cfg := &Config{
		LLMBaseURL: "http://localhost:8000/v1",
		LLMModel:   "deepseek-chat",
		LLMAPIKey:  "legacy-secret",
	}

	Normalize(cfg)

	if len(cfg.LLMEndpoints) != 1 {
		t.Fatalf("len(LLMEndpoints) = %d, want 1", len(cfg.LLMEndpoints))
	}
	ep := cfg.LLMEndpoints[0]
	if ep.ID != "default" {
		t.Errorf("synthesized endpoint id = %q, want %q", ep.ID, "default")
	}
	if ep.Name != "default" {
		t.Errorf("synthesized endpoint name = %q, want %q", ep.Name, "default")
	}
	if ep.BaseURL != "http://localhost:8000/v1" {
		t.Errorf("synthesized endpoint base_url = %q, want legacy value", ep.BaseURL)
	}
	if ep.Model != "deepseek-chat" {
		t.Errorf("synthesized endpoint model = %q, want legacy value", ep.Model)
	}
	if ep.APIKey != "legacy-secret" {
		t.Errorf("synthesized endpoint api_key = %q, want legacy value", ep.APIKey)
	}
	if cfg.LLMDefaultID != "default" {
		t.Errorf("LLMDefaultID = %q, want %q", cfg.LLMDefaultID, "default")
	}
}

func TestNormalizeLLMEndpoints_MirrorFromDefault(t *testing.T) {
	cfg := &Config{
		LLMBaseURL: "stale-url",
		LLMModel:   "stale-model",
		LLMAPIKey:  "stale-key",
		LLMEndpoints: []LLMEndpoint{
			{ID: "a", Name: "A", BaseURL: "http://a/v1", Model: "model-a", APIKey: "key-a"},
			{ID: "b", Name: "B", BaseURL: "http://b/v1", Model: "model-b", APIKey: "key-b"},
		},
		LLMDefaultID: "b",
	}

	Normalize(cfg)

	if cfg.LLMBaseURL != "http://b/v1" {
		t.Errorf("LLMBaseURL = %q, want mirrored %q", cfg.LLMBaseURL, "http://b/v1")
	}
	if cfg.LLMModel != "model-b" {
		t.Errorf("LLMModel = %q, want mirrored %q", cfg.LLMModel, "model-b")
	}
	if cfg.LLMAPIKey != "key-b" {
		t.Errorf("LLMAPIKey = %q, want mirrored %q", cfg.LLMAPIKey, "key-b")
	}
}

func TestNormalizeLLMEndpoints_DefaultFallback(t *testing.T) {
	cfg := &Config{
		LLMEndpoints: []LLMEndpoint{
			{ID: "a", Name: "A", BaseURL: "http://a/v1", Model: "model-a", APIKey: "key-a"},
			{ID: "b", Name: "B", BaseURL: "http://b/v1", Model: "model-b", APIKey: "key-b"},
		},
		LLMDefaultID: "ghost",
	}

	Normalize(cfg)

	if cfg.LLMDefaultID != "a" {
		t.Errorf("LLMDefaultID = %q, want fallback to first endpoint %q", cfg.LLMDefaultID, "a")
	}
	if cfg.LLMBaseURL != "http://a/v1" || cfg.LLMModel != "model-a" || cfg.LLMAPIKey != "key-a" {
		t.Errorf("legacy mirror = (%q, %q, %q), want first endpoint values",
			cfg.LLMBaseURL, cfg.LLMModel, cfg.LLMAPIKey)
	}
}

func TestSaveLoadLocalConfig_LLMEndpoints(t *testing.T) {
	path := t.TempDir() + "/config.local"

	want := &Config{
		LLMEndpoints: []LLMEndpoint{
			{ID: "a", Name: "A", BaseURL: "http://a/v1", Model: "model-a", APIKey: "key-a"},
			{ID: "b", Name: "B", BaseURL: "http://b/v1", Model: "model-b", APIKey: "key-b"},
		},
		LLMDefaultID: "b",
	}
	if err := SaveLocalConfig(path, want); err != nil {
		t.Fatalf("SaveLocalConfig failed: %v", err)
	}

	got := &Config{}
	loadLocalConfig(path, got)

	if len(got.LLMEndpoints) != 2 {
		t.Fatalf("len(LLMEndpoints) = %d, want 2", len(got.LLMEndpoints))
	}
	if got.LLMDefaultID != "b" {
		t.Errorf("LLMDefaultID = %q, want %q", got.LLMDefaultID, "b")
	}
	if got.LLMEndpoints[0].ID != "a" || got.LLMEndpoints[0].APIKey != "key-a" {
		t.Errorf("endpoint a = %+v, want id=a api_key=key-a", got.LLMEndpoints[0])
	}
	if got.LLMEndpoints[1].ID != "b" || got.LLMEndpoints[1].APIKey != "key-b" {
		t.Errorf("endpoint b = %+v, want id=b api_key=key-b", got.LLMEndpoints[1])
	}
	// Legacy mirror is persisted so older builds keep working.
	if got.LLMBaseURL != "http://b/v1" || got.LLMModel != "model-b" || got.LLMAPIKey != "key-b" {
		t.Errorf("legacy mirror after load = (%q, %q, %q), want default endpoint values",
			got.LLMBaseURL, got.LLMModel, got.LLMAPIKey)
	}
}

func TestContextTokensFor(t *testing.T) {
	cfg := &Config{
		LLMEndpoints: []LLMEndpoint{
			{ID: "home", BaseURL: "http://a/v1", Model: "a"},
			{ID: "kilo", BaseURL: "http://b/v1", Model: "b", ContextTokens: 65536},
			{ID: "lemonade", BaseURL: "http://c/v1", Model: "c"},
		},
		LLMDefaultID:     "home",
		LLMContextTokens: 32768,
	}
	for _, tc := range []struct {
		id   string
		want int
	}{{"home", 32768}, {"kilo", 65536}, {"lemonade", 0}} {
		ep, _ := EndpointByID(cfg.LLMEndpoints, tc.id)
		if got := ContextTokensFor(cfg, ep); got != tc.want {
			t.Errorf("ContextTokensFor(%s) = %d, want %d", tc.id, got, tc.want)
		}
	}
	if got := MainContextTokens(cfg); got != 32768 {
		t.Errorf("MainContextTokens = %d, want 32768 from the variable", got)
	}
	cfg.LLMEndpoints[0].ContextTokens = 262144
	if got := MainContextTokens(cfg); got != 262144 {
		t.Errorf("MainContextTokens = %d, want the endpoint's 262144 over the variable", got)
	}
}

func TestSaveLoadLocalConfig_ContextTokens(t *testing.T) {
	path := t.TempDir() + "/config.local"
	want := &Config{
		LLMEndpoints: []LLMEndpoint{{ID: "a", BaseURL: "http://a/v1", Model: "m", ContextTokens: 32768}},
		LLMDefaultID: "a",
	}
	if err := SaveLocalConfig(path, want); err != nil {
		t.Fatalf("SaveLocalConfig failed: %v", err)
	}
	got := &Config{}
	loadLocalConfig(path, got)
	if len(got.LLMEndpoints) != 1 || got.LLMEndpoints[0].ContextTokens != 32768 {
		t.Fatalf("endpoints after load = %+v, want context_tokens 32768", got.LLMEndpoints)
	}
}
