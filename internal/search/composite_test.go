package search

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type mockProvider struct {
	name     string
	results  []Result
	err      error
	searches []string
}

func (m *mockProvider) Name() string {
	return m.name
}

func (m *mockProvider) Search(ctx context.Context, query string, opts Options) ([]Result, error) {
	m.searches = append(m.searches, query)
	if m.err != nil {
		return nil, m.err
	}
	return m.results, nil
}

func TestCompositeProvider_SingleProvider_HardErrorOnFailure(t *testing.T) {
	mockErr := errors.New("simulated network failure")
	primary := &mockProvider{
		name: "brave",
		err:  mockErr,
	}

	comp := NewCompositeProvider(CompositeConfig{
		Providers: []Provider{primary},
	})

	_, err := comp.Search(context.Background(), "test query", Options{})
	if err == nil {
		t.Fatalf("expected hard error when chain length is 1 and provider fails, got nil")
	}
	if !errors.Is(err, mockErr) {
		t.Fatalf("expected mockErr %v, got %v", mockErr, err)
	}
}

func TestCompositeProvider_SingleProvider_EmptyResponseOnZeroResults(t *testing.T) {
	primary := &mockProvider{
		name:    "brave",
		results: []Result{},
	}

	comp := NewCompositeProvider(CompositeConfig{
		Providers: []Provider{primary},
	})

	res, err := comp.Search(context.Background(), "unknown query", Options{})
	if err != nil {
		t.Fatalf("expected nil error on 0 results for single provider, got %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("expected empty results, got %d", len(res))
	}
}

func TestCompositeProvider_ChainN_FallbackOnQuotaExhausted(t *testing.T) {
	p1 := &mockProvider{
		name: "brave",
		err:  ErrQuotaExhausted,
	}
	p2 := &mockProvider{
		name: "tavily",
		err:  ErrQuotaExhausted,
	}
	p3 := &mockProvider{
		name: "searxng",
		results: []Result{
			{Title: "Searxng Item", URL: "https://searx.com/item", Engine: "duckduckgo"},
		},
	}

	comp := NewCompositeProvider(CompositeConfig{
		Providers: []Provider{p1, p2, p3},
	})

	res, err := comp.Search(context.Background(), "query", Options{SessionID: "sess-123"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res) != 1 || res[0].Title != "Searxng Item" {
		t.Fatalf("expected item from p3 searxng, got %+v", res)
	}

	used, reason := comp.FallbackStatus("sess-123")
	if !used {
		t.Errorf("expected fallback to be marked used")
	}
	if !strings.Contains(reason, "brave degradato") || !strings.Contains(reason, "tavily degradato") {
		t.Errorf("expected reason to list degraded providers, got %q", reason)
	}
}

func TestCompositeProvider_ChainN_FallbackOnEmpty_Configurable(t *testing.T) {
	p1 := &mockProvider{
		name:    "brave",
		results: []Result{},
	}
	p2 := &mockProvider{
		name: "searxng",
		results: []Result{
			{Title: "Searxng Fallback Item", URL: "https://searx.com/item", Engine: "qwant"},
		},
	}

	// 1. With FallbackOnEmpty = false (default)
	compDefault := NewCompositeProvider(CompositeConfig{
		Providers:       []Provider{p1, p2},
		FallbackOnEmpty: false,
	})
	resDef, err := compDefault.Search(context.Background(), "query", Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resDef) != 0 {
		t.Fatalf("expected empty results without advancing chain when FallbackOnEmpty=false, got %d", len(resDef))
	}
	usedDef, _ := compDefault.FallbackStatus()
	if usedDef {
		t.Errorf("fallback should not be marked used when FallbackOnEmpty=false")
	}

	// 2. With FallbackOnEmpty = true
	compActive := NewCompositeProvider(CompositeConfig{
		Providers:       []Provider{p1, p2},
		FallbackOnEmpty: true,
	})
	resActive, err := compActive.Search(context.Background(), "query", Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resActive) != 1 || resActive[0].Title != "Searxng Fallback Item" {
		t.Fatalf("expected fallback results when FallbackOnEmpty=true, got %+v", resActive)
	}
	usedActive, reason := compActive.FallbackStatus()
	if !usedActive || !strings.Contains(reason, "nessun risultato") {
		t.Errorf("expected fallback on empty to be recorded, got used=%t, reason=%q", usedActive, reason)
	}
}

func TestCompositeProvider_PerSessionIsolation(t *testing.T) {
	p1 := &mockProvider{
		name: "brave",
		err:  ErrQuotaExhausted,
	}
	p2 := &mockProvider{
		name: "searxng",
		results: []Result{
			{Title: "Searxng Item", URL: "https://searx.com/item", Engine: "duckduckgo"},
		},
	}

	comp := NewCompositeProvider(CompositeConfig{
		Providers: []Provider{p1, p2},
	})

	// Session 1 triggers fallback
	_, err := comp.Search(context.Background(), "query 1", Options{SessionID: "session-A"})
	if err != nil {
		t.Fatalf("unexpected search error: %v", err)
	}

	usedA, reasonA := comp.FallbackStatus("session-A")
	if !usedA || !strings.Contains(reasonA, "brave degradato") {
		t.Errorf("expected session-A to have fallback recorded")
	}

	// Session 2 has not run or had fallback
	usedB, _ := comp.FallbackStatus("session-B")
	if usedB {
		t.Errorf("session-B should not have fallback recorded")
	}

	// Resetting Session A leaves Session B clean
	comp.ResetFallback("session-A")
	usedAAfter, _ := comp.FallbackStatus("session-A")
	if usedAAfter {
		t.Errorf("session-A should be reset")
	}
}

func TestCompositeProvider_AcademicGating_B_R1_1(t *testing.T) {
	primary := &mockProvider{
		name: "searxng",
		results: []Result{
			{Title: "Philosophy of Hope", URL: "https://plato.stanford.edu/hope", Engine: "duckduckgo"},
		},
	}
	academic := &mockProvider{
		name: "europepmc",
		results: []Result{
			{Title: "Clinical Hope Trial", URL: "https://pubmed.ncbi.nlm.nih.gov/12345/", Engine: "europepmc"},
		},
	}

	comp := NewCompositeProvider(CompositeConfig{
		Providers: []Provider{primary},
		Academic:  academic,
	})

	// Case 1: Sub-query with BiomedicalQuery=false in mixed plan (e.g. philosophical sub-query)
	// Even though AuthoritativeDomains has ncbi.nlm.nih.gov, Europe PMC must NOT be called
	academic.searches = nil
	res1, err := comp.Search(context.Background(), "site:plato.stanford.edu Baruch Spinoza", Options{
		AuthoritativeDomains: []string{"plato.stanford.edu", "ncbi.nlm.nih.gov"},
		BiomedicalQuery:      false, // Philosophical sub-query
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res1) != 1 || res1[0].Engine != "duckduckgo" {
		t.Errorf("expected only primary result, got %+v", res1)
	}
	if len(academic.searches) != 0 {
		t.Errorf("academic provider MUST NOT be called when BiomedicalQuery=false")
	}

	// Case 2: Sub-query with BiomedicalQuery=true and biomedical domain in plan
	academic.searches = nil
	res2, err := comp.Search(context.Background(), "hope theory depression trial", Options{
		AuthoritativeDomains: []string{"plato.stanford.edu", "ncbi.nlm.nih.gov"},
		BiomedicalQuery:      true, // Clinical sub-query
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res2) != 2 {
		t.Fatalf("expected 2 merged results (academic + primary), got %d", len(res2))
	}
	if len(academic.searches) != 1 {
		t.Errorf("academic provider MUST be called when BiomedicalQuery=true with biomedical domain")
	}
	if res2[0].Engine != "europepmc" {
		t.Errorf("expected first result to be academic, got %q", res2[0].Engine)
	}

	// Case 3: Category "science" alone does NOT trigger Europe PMC if BiomedicalQuery is false
	academic.searches = nil
	_, _ = comp.Search(context.Background(), "quantum computing physics", Options{
		Categories:           []string{"science"},
		AuthoritativeDomains: []string{"arxiv.org"},
		BiomedicalQuery:      false,
	})
	if len(academic.searches) != 0 {
		t.Errorf("category 'science' alone must NOT trigger Europe PMC without BiomedicalQuery")
	}
}

func TestCompositeProvider_PrimaryErrorNotMaskedByAcademic(t *testing.T) {
	primary := &mockProvider{
		name: "brave",
		err:  ErrQuotaExhausted,
	}
	academic := &mockProvider{
		name: "europepmc",
		results: []Result{
			{Title: "Clinical Trial", URL: "https://pubmed.ncbi.nlm.nih.gov/999/", Engine: "europepmc"},
		},
	}

	comp := NewCompositeProvider(CompositeConfig{
		Providers: []Provider{primary},
		Academic:  academic,
	})

	res, err := comp.Search(context.Background(), "hope depression", Options{
		SessionID:            "sess-mask-test",
		AuthoritativeDomains: []string{"ncbi.nlm.nih.gov"},
		BiomedicalQuery:      true,
	})
	if err != nil {
		t.Fatalf("search should succeed using academic results, got error: %v", err)
	}
	if len(res) != 1 || res[0].Engine != "europepmc" {
		t.Fatalf("expected 1 academic result, got %+v", res)
	}

	// Error of primary must still be recorded in fallback telemetry!
	used, reason := comp.FallbackStatus("sess-mask-test")
	if !used {
		t.Errorf("fallback must be marked used even when academic succeeded if primary failed")
	}
	if !strings.Contains(reason, "brave degradato") {
		t.Errorf("expected reason to note brave degradation, got %q", reason)
	}
}

// TestCompositeProvider_SessionMapBounded verifies that the per-session map does not
// grow without bound for sessions that are never revisited.
func TestCompositeProvider_SessionMapBounded(t *testing.T) {
	p1 := &mockProvider{name: "brave", err: ErrQuotaExhausted}
	p2 := &mockProvider{name: "searxng", results: []Result{{Title: "r", URL: "https://example.com", Engine: "searxng"}}}
	comp := NewCompositeProvider(CompositeConfig{Providers: []Provider{p1, p2}})

	for i := 0; i < maxSessionFallbackEntries*2; i++ {
		sid := fmt.Sprintf("session-%d", i)
		if _, err := comp.Search(context.Background(), "query", Options{SessionID: sid}); err != nil {
			t.Fatalf("unexpected error for %s: %v", sid, err)
		}
	}

	comp.mu.RLock()
	size := len(comp.sessions)
	comp.mu.RUnlock()
	if size > maxSessionFallbackEntries {
		t.Errorf("session map exceeded bound: got %d, max %d", size, maxSessionFallbackEntries)
	}
}
