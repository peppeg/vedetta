package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// exaSnippetChars caps the highlight used as result snippet: Exa treats its
// own maxCharacters as a hint and returned up to ~2,500 characters in EXA.0.
const exaSnippetChars = 600

// ExaProvider implements search.Provider using the Exa search API
// (docs/SPEC_EXA_PROFILO.md). Results carry one highlight as snippet: without
// it Exa returns only title and URL, and Vedetta ranks on the snippet.
type ExaProvider struct {
	apiKey       string
	baseURL      string
	searchType   string // "auto" (default) or "lite"
	userLocation string // ISO country code, "" = not sent
	httpClient   *http.Client
}

type exaSearchRequest struct {
	Query        string      `json:"query"`
	Type         string      `json:"type"`
	NumResults   int         `json:"numResults"`
	UserLocation string      `json:"userLocation,omitempty"`
	Contents     exaContents `json:"contents"`
}

type exaContents struct {
	Highlights struct {
		MaxCharacters int `json:"maxCharacters"`
	} `json:"highlights"`
}

type exaSearchResponse struct {
	Results []struct {
		Title      string   `json:"title"`
		URL        string   `json:"url"`
		Highlights []string `json:"highlights"`
	} `json:"results"`
	CostDollars struct {
		Total float64 `json:"total"`
	} `json:"costDollars"`
}

// NewExaProvider constructs an Exa provider; empty searchType means "auto".
func NewExaProvider(apiKey, searchType, userLocation string, customBaseURL ...string) *ExaProvider {
	baseURL := "https://api.exa.ai"
	if len(customBaseURL) > 0 && customBaseURL[0] != "" {
		baseURL = strings.TrimRight(customBaseURL[0], "/")
	}
	searchType = strings.ToLower(strings.TrimSpace(searchType))
	if searchType == "" {
		searchType = "auto"
	}
	return &ExaProvider{
		apiKey:       strings.TrimSpace(apiKey),
		baseURL:      baseURL,
		searchType:   searchType,
		userLocation: strings.ToUpper(strings.TrimSpace(userLocation)),
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

func (e *ExaProvider) Name() string {
	return "exa"
}

func (e *ExaProvider) Search(ctx context.Context, query string, opts Options) ([]Result, error) {
	cleanQuery := strings.TrimSpace(query)
	if cleanQuery == "" {
		return nil, fmt.Errorf("empty query")
	}
	if e.apiKey == "" {
		return nil, fmt.Errorf("exa search api key is not configured")
	}

	maxRes := opts.MaxResults
	if maxRes <= 0 {
		maxRes = 10
	}
	if maxRes > 20 {
		maxRes = 20
	}

	body := exaSearchRequest{Query: cleanQuery, Type: e.searchType, NumResults: maxRes, UserLocation: e.userLocation}
	body.Contents.Highlights.MaxCharacters = exaSnippetChars
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to encode exa request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/search", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create exa request: %w", err)
	}
	req.Header.Set("x-api-key", e.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Vedetta-Search/1.0")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exa request failed: %w", err)
	}
	defer resp.Body.Close()

	// 402: credits or budget exhausted; 429: rate limit.
	if resp.StatusCode == http.StatusPaymentRequired || resp.StatusCode == http.StatusTooManyRequests {
		return nil, ErrQuotaExhausted
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("exa returned status %d: %s", resp.StatusCode, string(msg))
	}

	var data exaSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("failed to decode exa response: %w", err)
	}

	var results []Result
	seen := make(map[string]bool)
	for _, item := range data.Results {
		targetURL := strings.TrimSpace(item.URL)
		if targetURL == "" || seen[targetURL] {
			continue
		}
		seen[targetURL] = true
		var snippet string
		if len(item.Highlights) > 0 {
			snippet = truncateRunes(strings.Join(strings.Fields(item.Highlights[0]), " "), exaSnippetChars)
		}
		results = append(results, Result{
			Title:   strings.TrimSpace(item.Title),
			URL:     targetURL,
			Content: snippet,
			Engine:  "exa",
			Score:   1.0,
		})
	}
	log.Printf("[exa] session=%s kind=search results=%d cost=$%.4f", opts.SessionID, len(results), data.CostDollars.Total)
	return results, nil
}

// truncateRunes cuts s to at most n runes, at the last space when there is one.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n])
	if i := strings.LastIndex(cut, " "); i > n/2 {
		cut = cut[:i]
	}
	return cut + "…"
}
