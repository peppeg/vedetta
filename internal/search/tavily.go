package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// TavilyProvider implements search.Provider using the Tavily Search API.
type TavilyProvider struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

type tavilyRequest struct {
	APIKey        string `json:"api_key"`
	Query         string `json:"query"`
	MaxResults    int    `json:"max_results,omitempty"`
	SearchDepth   string `json:"search_depth,omitempty"`
	IncludeAnswer bool   `json:"include_answer"`
	IncludeImages bool   `json:"include_images"`
}

type tavilyResponse struct {
	Results []struct {
		Title   string  `json:"title"`
		URL     string  `json:"url"`
		Content string  `json:"content"`
		Score   float64 `json:"score"`
	} `json:"results"`
}

// NewTavilyProvider constructs a new Tavily Search API provider.
func NewTavilyProvider(apiKey string, customBaseURL ...string) *TavilyProvider {
	baseURL := "https://api.tavily.com/search"
	if len(customBaseURL) > 0 && customBaseURL[0] != "" {
		baseURL = strings.TrimRight(customBaseURL[0], "/")
	}
	return &TavilyProvider{
		apiKey:  strings.TrimSpace(apiKey),
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (t *TavilyProvider) Name() string {
	return "tavily"
}

func (t *TavilyProvider) Search(ctx context.Context, query string, opts Options) ([]Result, error) {
	cleanQuery := strings.TrimSpace(query)
	if cleanQuery == "" {
		return nil, fmt.Errorf("empty query")
	}
	if t.apiKey == "" {
		return nil, fmt.Errorf("tavily search api key is not configured")
	}

	maxRes := opts.MaxResults
	if maxRes <= 0 {
		maxRes = 10
	}
	if maxRes > 20 {
		maxRes = 20
	}

	reqBody := tavilyRequest{
		APIKey:        t.apiKey,
		Query:         cleanQuery,
		MaxResults:    maxRes,
		SearchDepth:   "basic",
		IncludeAnswer: false,
		IncludeImages: false,
	}

	rawJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to encode tavily request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL, bytes.NewReader(rawJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to create tavily request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Vedetta-Search/1.0")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tavily request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, ErrQuotaExhausted
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("tavily returned status %d: %s", resp.StatusCode, string(body))
	}

	var data tavilyResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("failed to decode tavily response: %w", err)
	}

	var results []Result
	seen := make(map[string]bool)

	for _, item := range data.Results {
		targetURL := strings.TrimSpace(item.URL)
		if targetURL == "" || seen[targetURL] {
			continue
		}
		seen[targetURL] = true

		score := item.Score
		if score <= 0 {
			score = 1.0
		}

		results = append(results, Result{
			Title:   strings.TrimSpace(item.Title),
			URL:     targetURL,
			Content: strings.TrimSpace(item.Content),
			Engine:  "tavily",
			Score:   score,
		})
	}

	return results, nil
}
