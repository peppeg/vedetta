package search

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBraveProvider_SearchSuccess(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Subscription-Token") != "test-api-key" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("q") != "spinoza speranza" {
			http.Error(w, "unexpected query", http.StatusBadRequest)
			return
		}

		resp := braveResponse{}
		resp.Web.Results = []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		}{
			{
				Title:       "Baruch Spinoza - Stanford Encyclopedia of Philosophy",
				URL:         "https://plato.stanford.edu/entries/spinoza/",
				Description: "Spinoza on the passions and hope as an inconstant joy.",
			},
			{
				Title:       "Hope in Psychology",
				URL:         "https://example.com/psychology-hope",
				Description: "Clinical aspects of hope.",
			},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	provider := NewBraveProvider("test-api-key", mockServer.URL)
	if provider.Name() != "brave" {
		t.Fatalf("expected provider name 'brave', got %q", provider.Name())
	}

	results, err := provider.Search(context.Background(), "spinoza speranza", Options{MaxResults: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	if results[0].Title != "Baruch Spinoza - Stanford Encyclopedia of Philosophy" {
		t.Errorf("unexpected title: %q", results[0].Title)
	}
	if results[0].Engine != "brave" {
		t.Errorf("expected engine 'brave', got %q", results[0].Engine)
	}
	if results[0].URL != "https://plato.stanford.edu/entries/spinoza/" {
		t.Errorf("unexpected URL: %q", results[0].URL)
	}
}

func TestBraveProvider_QuotaExhausted(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
	}))
	defer mockServer.Close()

	provider := NewBraveProvider("test-key", mockServer.URL)
	_, err := provider.Search(context.Background(), "test", Options{})
	if !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("expected ErrQuotaExhausted, got %v", err)
	}
}

func TestBraveProvider_ValidationErrors(t *testing.T) {
	// Empty query
	p := NewBraveProvider("some-key")
	_, err := p.Search(context.Background(), "   ", Options{})
	if err == nil {
		t.Fatalf("expected error for empty query, got nil")
	}

	// Missing API key
	pNoKey := NewBraveProvider("")
	_, err = pNoKey.Search(context.Background(), "query", Options{})
	if err == nil {
		t.Fatalf("expected error for missing API key, got nil")
	}
}
