package search

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTavilyProvider(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req tavilyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.APIKey != "tvly-test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if req.Query == "rate-limited" {
			http.Error(w, "quota exhausted", http.StatusTooManyRequests)
			return
		}

		resp := tavilyResponse{
			Results: []struct {
				Title   string  `json:"title"`
				URL     string  `json:"url"`
				Content string  `json:"content"`
				Score   float64 `json:"score"`
			}{
				{
					Title:   "Tavily Hope Result",
					URL:     "https://tavily.test/hope",
					Content: "Hope theory snippet",
					Score:   0.88,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	tp := NewTavilyProvider("tvly-test", mockServer.URL)
	if tp.Name() != "tavily" {
		t.Fatalf("expected tavily name, got %q", tp.Name())
	}

	results, err := tp.Search(context.Background(), "speranza", Options{MaxResults: 5})
	if err != nil {
		t.Fatalf("search error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Engine != "tavily" {
		t.Errorf("expected engine 'tavily', got %q", results[0].Engine)
	}
	if results[0].Score != 0.88 {
		t.Errorf("expected score 0.88, got %f", results[0].Score)
	}

	// Test 429
	_, err429 := tp.Search(context.Background(), "rate-limited", Options{})
	if !errors.Is(err429, ErrQuotaExhausted) {
		t.Fatalf("expected ErrQuotaExhausted, got %v", err429)
	}

	// Validation
	if _, err := tp.Search(context.Background(), "  ", Options{}); err == nil {
		t.Fatalf("expected error on empty query")
	}
	tpNoKey := NewTavilyProvider("")
	if _, err := tpNoKey.Search(context.Background(), "query", Options{}); err == nil {
		t.Fatalf("expected error on missing key")
	}
}

func TestSerperProvider(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-KEY") != "serp-test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req serperRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.Q == "rate-limited" {
			http.Error(w, "quota exhausted", http.StatusTooManyRequests)
			return
		}

		resp := serperResponse{
			Organic: []struct {
				Title    string `json:"title"`
				Link     string `json:"link"`
				Snippet  string `json:"snippet"`
				Position int    `json:"position"`
			}{
				{
					Title:    "Google Serper Hope",
					Link:     "https://serper.test/google-hope",
					Snippet:  "Serper snippet text",
					Position: 1,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	sp := NewSerperProvider("serp-test", mockServer.URL)
	if sp.Name() != "serper" {
		t.Fatalf("expected serper name, got %q", sp.Name())
	}

	results, err := sp.Search(context.Background(), "speranza", Options{MaxResults: 5, Language: "it"})
	if err != nil {
		t.Fatalf("search error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Engine != "google (serper)" {
		t.Errorf("expected engine 'google (serper)', got %q", results[0].Engine)
	}

	// Test 429
	_, err429 := sp.Search(context.Background(), "rate-limited", Options{})
	if !errors.Is(err429, ErrQuotaExhausted) {
		t.Fatalf("expected ErrQuotaExhausted, got %v", err429)
	}

	// Validation
	if _, err := sp.Search(context.Background(), "  ", Options{}); err == nil {
		t.Fatalf("expected error on empty query")
	}
	spNoKey := NewSerperProvider("")
	if _, err := spNoKey.Search(context.Background(), "query", Options{}); err == nil {
		t.Fatalf("expected error on missing key")
	}
}
