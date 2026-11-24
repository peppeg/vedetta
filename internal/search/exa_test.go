package search

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExaProvider_SearchSuccess(t *testing.T) {
	var got exaSearchRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/search" {
			http.Error(w, "wrong endpoint", http.StatusNotFound)
			return
		}
		if r.Header.Get("x-api-key") != "test-key" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"title":"Regole operative","url":"https://www.gse.it/regole.pdf","highlights":["PARTE I.\n I REQUISITI   per l'accesso"]},
			{"title":"Duplicate","url":"https://www.gse.it/regole.pdf","highlights":["x"]},
			{"title":"No highlight","url":"https://www.mase.gov.it/faq"},
			{"title":"Long","url":"https://example.com/long","highlights":["` + strings.Repeat("parola ", 400) + `"]}
		],"costDollars":{"total":0.007}}`))
	}))
	defer server.Close()

	p := NewExaProvider("test-key", "", "it", server.URL)
	if p.Name() != "exa" {
		t.Fatalf("name = %q", p.Name())
	}
	results, err := p.Search(context.Background(), "  regole agrivoltaico ", Options{MaxResults: 50})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Query != "regole agrivoltaico" || got.Type != "auto" || got.NumResults != 20 || got.UserLocation != "IT" ||
		got.Contents.Highlights.MaxCharacters != exaSnippetChars {
		t.Errorf("request = %+v", got)
	}
	if len(results) != 3 {
		t.Fatalf("want 3 results (duplicate dropped), got %d", len(results))
	}
	if results[0].Content != "PARTE I. I REQUISITI per l'accesso" || results[0].Engine != "exa" {
		t.Errorf("first result = %+v", results[0])
	}
	if results[1].Content != "" {
		t.Errorf("no highlight: content = %q", results[1].Content)
	}
	if n := len([]rune(results[2].Content)); n > exaSnippetChars+1 || !strings.HasSuffix(results[2].Content, "…") {
		t.Errorf("long highlight not cut: %d runes", n)
	}
}

func TestExaProvider_Errors(t *testing.T) {
	for _, tt := range []struct {
		status    int
		wantQuota bool
	}{
		{http.StatusPaymentRequired, true},
		{http.StatusTooManyRequests, true},
		{http.StatusUnauthorized, false},
		{http.StatusInternalServerError, false},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", tt.status)
		}))
		_, err := NewExaProvider("k", "lite", "", server.URL).Search(context.Background(), "q", Options{})
		server.Close()
		if err == nil {
			t.Fatalf("status %d: want error", tt.status)
		}
		if errors.Is(err, ErrQuotaExhausted) != tt.wantQuota {
			t.Errorf("status %d: quota error %t, want %t (%v)", tt.status, errors.Is(err, ErrQuotaExhausted), tt.wantQuota, err)
		}
		if strings.Contains(err.Error(), "k\"") {
			t.Errorf("error leaks the key: %v", err)
		}
	}
	if _, err := NewExaProvider("", "", "").Search(context.Background(), "q", Options{}); err == nil {
		t.Error("missing key: want error")
	}
}

func TestExaProvider_Registry(t *testing.T) {
	if _, err := Get("exa", ProviderConfig{}); err == nil {
		t.Error("exa without key: want error")
	}
	p, err := Get("exa", ProviderConfig{ExaAPIKey: "k", ExaSearchType: "lite"})
	if err != nil || p.Name() != "exa" || p.(*ExaProvider).searchType != "lite" {
		t.Errorf("exa from registry: %v %v", p, err)
	}
	if st := ProbeProvider(context.Background(), "exa", ProviderConfig{ExaAPIKey: "k"}); st.Status != "configured" {
		t.Errorf("probe = %+v", st)
	}
}
