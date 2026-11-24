package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveKeyRef(t *testing.T) {
	t.Setenv("VEDETTA_TEST_KEY", "sk-real-value")
	if v, env := ResolveKeyRef("${VEDETTA_TEST_KEY}"); v != "sk-real-value" || env != "VEDETTA_TEST_KEY" {
		t.Fatalf("got %q %q", v, env)
	}
	if v, env := ResolveKeyRef(" plain-key "); v != "plain-key" || env != "" {
		t.Fatalf("plain key: got %q %q", v, env)
	}
	// A missing variable is an empty key ("manca la chiave"), not a failure.
	if v, env := ResolveKeyRef("${VEDETTA_TEST_MISSING}"); v != "" || env != "VEDETTA_TEST_MISSING" {
		t.Fatalf("missing var: got %q %q", v, env)
	}
}

func TestIsLocalAddress(t *testing.T) {
	cases := map[string]bool{
		"http://localhost:11434/v1":       true,
		"http://127.0.0.1:8080":           true,
		"http://192.168.1.20:1234/v1":     true,
		"http://100.64.10.20:8731/v1":     true, // Tailscale
		"http://halogen:8731/v1":          true,
		"https://openrouter.ai/api/v1":    false,
		"https://api.entrim.ai/v1":        false,
		"https://api.kilo.ai/api/gateway": false,
	}
	for u, want := range cases {
		if got := IsLocalAddress(u); got != want {
			t.Errorf("%s: got %v want %v", u, got, want)
		}
	}
}

func TestNormalize_PaidDefault(t *testing.T) {
	cfg := &Config{LLMEndpoints: []LLMEndpoint{
		{ID: "home", BaseURL: "http://100.64.10.20:8731/v1", Model: "m", APIKey: "k"},
		{ID: "cloud", BaseURL: "https://openrouter.ai/api/v1", Model: "m", APIKey: "k"},
		{ID: "cloud-nokey", BaseURL: "https://example.org/v1", Model: "m"},
	}}
	Normalize(cfg)
	want := map[string]bool{"home": false, "cloud": true, "cloud-nokey": false}
	for _, ep := range cfg.LLMEndpoints {
		if ep.Paid == nil || *ep.Paid != want[ep.ID] {
			t.Errorf("%s: paid=%v want %v", ep.ID, ep.Paid, want[ep.ID])
		}
	}
}

// The reference, never the value, goes to disk; reading it back resolves it.
func TestSaveLocalConfig_KeepsReference(t *testing.T) {
	t.Setenv("VEDETTA_TEST_CLOUD_KEY", "sk-should-not-be-on-disk")
	path := filepath.Join(t.TempDir(), "config.local")
	paid := true
	cfg := &Config{
		LLMEndpoints: []LLMEndpoint{{ID: "entrim", BaseURL: "https://api.entrim.ai/v1", Model: "m",
			APIKey: "sk-should-not-be-on-disk", KeyEnv: "VEDETTA_TEST_CLOUD_KEY", Paid: &paid}},
		LLMDefaultID: "entrim",
		Services:     map[string]bool{"search:brave": true},
		PaidFallback: true,
	}
	Normalize(cfg)
	if err := SaveLocalConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "sk-should-not-be-on-disk") {
		t.Fatalf("the key value was written to disk:\n%s", raw)
	}
	if !strings.Contains(string(raw), "${VEDETTA_TEST_CLOUD_KEY}") {
		t.Fatalf("the reference is missing:\n%s", raw)
	}
	back := &Config{}
	loadLocalConfig(path, back)
	Normalize(back)
	ep := back.LLMEndpoints[0]
	if ep.APIKey != "sk-should-not-be-on-disk" || ep.KeyEnv != "VEDETTA_TEST_CLOUD_KEY" || back.LLMAPIKey != "sk-should-not-be-on-disk" {
		t.Fatalf("reload: key=%q env=%q legacy=%q", ep.APIKey, ep.KeyEnv, back.LLMAPIKey)
	}
	if !back.PaidFallback || !back.Services["search:brave"] || ep.Paid == nil || !*ep.Paid {
		t.Fatalf("reload lost settings: %+v %+v", back.Services, ep)
	}
}
