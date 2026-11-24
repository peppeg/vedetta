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

// SerperProvider implements search.Provider using the Serper Google Search API.
type SerperProvider struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

type serperRequest struct {
	Q   string `json:"q"`
	Num int    `json:"num,omitempty"`
	GL  string `json:"gl,omitempty"`
	HL  string `json:"hl,omitempty"`
}

type serperResponse struct {
	Organic []struct {
		Title    string `json:"title"`
		Link     string `json:"link"`
		Snippet  string `json:"snippet"`
		Position int    `json:"position"`
	} `json:"organic"`
}

// NewSerperProvider constructs a new Serper Google Search API provider.
func NewSerperProvider(apiKey string, customBaseURL ...string) *SerperProvider {
	baseURL := "https://google.serper.dev/search"
	if len(customBaseURL) > 0 && customBaseURL[0] != "" {
		baseURL = strings.TrimRight(customBaseURL[0], "/")
	}
	return &SerperProvider{
		apiKey:  strings.TrimSpace(apiKey),
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (s *SerperProvider) Name() string {
	return "serper"
}

func (s *SerperProvider) Search(ctx context.Context, query string, opts Options) ([]Result, error) {
	cleanQuery := strings.TrimSpace(query)
	if cleanQuery == "" {
		return nil, fmt.Errorf("empty query")
	}
	if s.apiKey == "" {
		return nil, fmt.Errorf("serper api key is not configured")
	}

	maxRes := opts.MaxResults
	if maxRes <= 0 {
		maxRes = 10
	}
	if maxRes > 20 {
		maxRes = 20
	}

	reqBody := serperRequest{
		Q:   cleanQuery,
		Num: maxRes,
	}
	if opts.Language != "" {
		reqBody.HL = opts.Language
	}

	rawJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to encode serper request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL, bytes.NewReader(rawJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to create serper request: %w", err)
	}
	req.Header.Set("X-API-KEY", s.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Vedetta-Search/1.0")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("serper request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, ErrQuotaExhausted
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("serper returned status %d: %s", resp.StatusCode, string(body))
	}

	var data serperResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("failed to decode serper response: %w", err)
	}

	var results []Result
	seen := make(map[string]bool)

	for _, item := range data.Organic {
		targetURL := strings.TrimSpace(item.Link)
		if targetURL == "" || seen[targetURL] {
			continue
		}
		seen[targetURL] = true

		results = append(results, Result{
			Title:   strings.TrimSpace(item.Title),
			URL:     targetURL,
			Content: strings.TrimSpace(item.Snippet),
			Engine:  "google (serper)",
			Score:   1.0,
		})
	}

	return results, nil
}
