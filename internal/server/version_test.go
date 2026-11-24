package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"vedetta/config"
	"vedetta/internal/agent"
)

func newVersionTestHandlers(version string) *Handlers {
	cfg := &config.Config{
		Version: version,
	}
	engine := agent.NewEngine(&dummySearchProvider{}, nil, nil, nil)
	return NewHandlers(cfg, engine)
}

func decodeHealthVersion(t *testing.T, h *Handlers) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	h.Health(rec, req)
	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode /api/health response: %v", err)
	}
	return body.Version
}

func decodeConfigVersion(t *testing.T, h *Handlers) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	h.Config(rec, req)
	var body ConfigResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode /api/config response: %v", err)
	}
	return body.Version
}

func TestHealth_VersionFromSingleSource(t *testing.T) {
	h := newVersionTestHandlers("1.2.3")
	if got := decodeHealthVersion(t, h); got != "1.2.3" {
		t.Fatalf("/api/health version = %q, want %q", got, "1.2.3")
	}
}

func TestConfig_VersionMatchesHealth(t *testing.T) {
	h := newVersionTestHandlers("1.2.3")
	configVersion := decodeConfigVersion(t, h)
	healthVersion := decodeHealthVersion(t, h)
	if configVersion != "1.2.3" {
		t.Fatalf("/api/config version = %q, want %q", configVersion, "1.2.3")
	}
	if configVersion != healthVersion {
		t.Fatalf("version mismatch: /api/config=%q /api/health=%q", configVersion, healthVersion)
	}
}

func TestVersion_FallbackWhenEmpty(t *testing.T) {
	h := newVersionTestHandlers("")
	if got := decodeHealthVersion(t, h); got != unknownVersion {
		t.Fatalf("/api/health version with empty cfg = %q, want %q", got, unknownVersion)
	}
	if got := decodeConfigVersion(t, h); got != unknownVersion {
		t.Fatalf("/api/config version with empty cfg = %q, want %q", got, unknownVersion)
	}
	if got := decodeHealthVersion(t, h); got == "0.2.0" {
		t.Fatal("/api/health still exposes hardcoded 0.2.0")
	}
}
