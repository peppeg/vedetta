package search

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAcademicProvider_SearchSuccess(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if q != "hope snyder depression" {
			t.Errorf("expected clean query 'hope snyder depression', got %q", q)
		}

		resp := europePMCResponse{
			HitCount: 1,
			ResultList: struct {
				Result []struct {
					ID           string `json:"id"`
					Source       string `json:"source"`
					PMID         string `json:"pmid"`
					DOI          string `json:"doi"`
					Title        string `json:"title"`
					AuthorString string `json:"authorString"`
					JournalTitle string `json:"journalTitle"`
					PubYear      string `json:"pubYear"`
					AbstractText string `json:"abstractText"`
				} `json:"result"`
			}{
				Result: []struct {
					ID           string `json:"id"`
					Source       string `json:"source"`
					PMID         string `json:"pmid"`
					DOI          string `json:"doi"`
					Title        string `json:"title"`
					AuthorString string `json:"authorString"`
					JournalTitle string `json:"journalTitle"`
					PubYear      string `json:"pubYear"`
					AbstractText string `json:"abstractText"`
				}{
					{
						ID:           "32145678",
						Source:       "MED",
						PMID:         "32145678",
						DOI:          "10.1016/j.jad.2020.01.001",
						Title:        "The efficacy of hope therapy in clinical depression.",
						AuthorString: "Snyder CR, Cheavens J.",
						JournalTitle: "Journal of Clinical Psychology",
						PubYear:      "2020",
						AbstractText: "Hope therapy improves agency and pathways in patients with depression.",
					},
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	ap := NewAcademicProvider(mockServer.URL)
	if ap.Name() != "europepmc" {
		t.Fatalf("expected provider name 'europepmc', got %q", ap.Name())
	}

	results, err := ap.Search(context.Background(), "site:ncbi.nlm.nih.gov hope snyder depression", Options{MaxResults: 5})
	if err != nil {
		t.Fatalf("academic search failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	res := results[0]
	if res.Title != "The efficacy of hope therapy in clinical depression" {
		t.Errorf("unexpected title: %q", res.Title)
	}
	if res.URL != "https://pubmed.ncbi.nlm.nih.gov/32145678/" {
		t.Errorf("expected pubmed url, got %q", res.URL)
	}
	if res.Engine != "europepmc" {
		t.Errorf("expected engine 'europepmc', got %q", res.Engine)
	}
	if res.Score != 1.0 {
		t.Errorf("expected score 1.0, got %f", res.Score)
	}
	expectedSnippetPrefix := "[Snyder CR, Cheavens J. | Journal of Clinical Psychology (2020)]"
	if len(res.Content) < len(expectedSnippetPrefix) || res.Content[:len(expectedSnippetPrefix)] != expectedSnippetPrefix {
		t.Errorf("expected snippet prefix %q, got %q", expectedSnippetPrefix, res.Content)
	}
}

func TestAcademicProvider_QuotaAndErrors(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer mockServer.Close()

	ap := NewAcademicProvider(mockServer.URL)
	_, err := ap.Search(context.Background(), "cancer therapy", Options{})
	if !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("expected ErrQuotaExhausted, got %v", err)
	}

	// Empty query
	_, errEmpty := ap.Search(context.Background(), "   ", Options{})
	if errEmpty == nil {
		t.Fatalf("expected error on empty query")
	}
}
