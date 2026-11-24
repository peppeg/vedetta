package search

import (
	"strings"
	"testing"
)

func TestRegistry_Lookup(t *testing.T) {
	cfg := ProviderConfig{
		SearxngURL: "http://localhost:8888",
	}

	p, err := Get("searxng", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Name() != "searxng" {
		t.Fatalf("expected searxng provider, got %q", p.Name())
	}

	_, errUnknown := Get("non-existent-provider", cfg)
	if errUnknown == nil {
		t.Fatalf("expected error for unknown provider")
	}
}

func TestRegistry_NewProvider(t *testing.T) {
	// 1. Default: SearXNG (single provider in chain)
	cfgDefault := ProviderConfig{
		SearxngURL: "http://localhost:8888",
	}
	provDef, err := NewProvider(cfgDefault)
	if err != nil {
		t.Fatalf("unexpected error for default config: %v", err)
	}
	if provDef.Name() != "searxng" {
		t.Errorf("expected provider name 'searxng', got %q", provDef.Name())
	}
	compDef, ok := provDef.(*CompositeProvider)
	if !ok {
		t.Fatalf("expected CompositeProvider wrapper")
	}
	if len(compDef.Providers()) != 1 {
		t.Errorf("expected chain length 1 for default config, got %d", len(compDef.Providers()))
	}

	// 2. Brave without key -> error
	cfgBraveNoKey := ProviderConfig{
		Providers: []string{"brave"},
	}
	_, errNoKey := NewProvider(cfgBraveNoKey)
	if errNoKey == nil || !strings.Contains(errNoKey.Error(), "BRAVE_API_KEY") {
		t.Fatalf("expected missing BRAVE_API_KEY error, got %v", errNoKey)
	}

	// 3. Single provider without fallback (REV 2 B-R1.2: no automatic SearXNG fallback!)
	cfgBraveSingle := ProviderConfig{
		Providers:   []string{"brave"},
		BraveAPIKey: "test-brave-key",
	}
	provBraveSingle, err := NewProvider(cfgBraveSingle)
	if err != nil {
		t.Fatalf("unexpected error for single brave config: %v", err)
	}
	compSingle, ok := provBraveSingle.(*CompositeProvider)
	if !ok {
		t.Fatalf("expected CompositeProvider wrapper")
	}
	if len(compSingle.Providers()) != 1 {
		t.Fatalf("expected chain length 1 (no auto-fallback to searxng!), got %d", len(compSingle.Providers()))
	}
	if compSingle.Primary().Name() != "brave" {
		t.Errorf("expected primary brave, got %q", compSingle.Primary().Name())
	}
	if compSingle.Fallback() != nil {
		t.Errorf("expected nil fallback when not configured, got %v", compSingle.Fallback())
	}

	// 4. Multi-provider chain: serper as primary, brave as secondary (REV 2 B-R1.2: no serper demotion)
	cfgChain := ProviderConfig{
		Providers:    []string{"serper", "brave", "searxng"},
		SerperAPIKey: "test-serp-key",
		BraveAPIKey:  "test-brave-key",
		SearxngURL:   "http://localhost:8888",
	}
	provChain, err := NewProvider(cfgChain)
	if err != nil {
		t.Fatalf("unexpected error for chain config: %v", err)
	}
	compChain, ok := provChain.(*CompositeProvider)
	if !ok {
		t.Fatalf("expected CompositeProvider wrapper")
	}
	chain := compChain.Providers()
	if len(chain) != 3 {
		t.Fatalf("expected chain length 3, got %d", len(chain))
	}
	if chain[0].Name() != "serper" {
		t.Errorf("expected serper primary, got %q", chain[0].Name())
	}
	if chain[1].Name() != "brave" {
		t.Errorf("expected brave secondary, got %q", chain[1].Name())
	}
	if chain[2].Name() != "searxng" {
		t.Errorf("expected searxng tertiary, got %q", chain[2].Name())
	}

	// 5. Legacy migration: ProviderName + FallbackProvider
	cfgLegacy := ProviderConfig{
		ProviderName:     "brave",
		FallbackProvider: "searxng",
		BraveAPIKey:      "test-brave-key",
		SearxngURL:       "http://localhost:8888",
	}
	provLegacy, err := NewProvider(cfgLegacy)
	if err != nil {
		t.Fatalf("unexpected error for legacy config: %v", err)
	}
	compLegacy, ok := provLegacy.(*CompositeProvider)
	if !ok {
		t.Fatalf("expected CompositeProvider wrapper")
	}
	if len(compLegacy.Providers()) != 2 {
		t.Fatalf("expected chain length 2 from legacy config, got %d", len(compLegacy.Providers()))
	}
}
