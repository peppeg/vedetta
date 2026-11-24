package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)



// BraveProvider implements search.Provider using the official Brave Search API.
type BraveProvider struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

type braveResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

// NewBraveProvider constructs a new Brave Search API provider.
func NewBraveProvider(apiKey string, customBaseURL ...string) *BraveProvider {
	baseURL := "https://api.search.brave.com/res/v1/web/search"
	if len(customBaseURL) > 0 && customBaseURL[0] != "" {
		baseURL = strings.TrimRight(customBaseURL[0], "/")
	}
	return &BraveProvider{
		apiKey:  strings.TrimSpace(apiKey),
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (b *BraveProvider) Name() string {
	return "brave"
}

func (b *BraveProvider) Search(ctx context.Context, query string, opts Options) ([]Result, error) {
	cleanQuery := strings.TrimSpace(query)
	if cleanQuery == "" {
		return nil, fmt.Errorf("empty query")
	}
	if b.apiKey == "" {
		return nil, fmt.Errorf("brave search api key is not configured")
	}

	u, err := url.Parse(b.baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid brave base url: %w", err)
	}

	maxRes := opts.MaxResults
	if maxRes <= 0 {
		maxRes = 10
	}
	if maxRes > 20 {
		maxRes = 20
	}

	q := u.Query()
	q.Set("q", cleanQuery)
	q.Set("count", strconv.Itoa(maxRes))
	if opts.Language != "" {
		q.Set("search_lang", opts.Language)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create brave request: %w", err)
	}
	req.Header.Set("X-Subscription-Token", b.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Vedetta-Search/1.0")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("brave request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, ErrQuotaExhausted
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("brave returned status %d: %s", resp.StatusCode, string(body))
	}

	var data braveResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("failed to decode brave response: %w", err)
	}

	var results []Result
	seen := make(map[string]bool)

	for _, item := range data.Web.Results {
		targetURL := strings.TrimSpace(item.URL)
		if targetURL == "" || seen[targetURL] {
			continue
		}
		seen[targetURL] = true

		results = append(results, Result{
			Title:   strings.TrimSpace(item.Title),
			URL:     targetURL,
			Content: strings.TrimSpace(item.Description),
			Engine:  "brave",
			Score:   1.0,
		})
	}

	return results, nil
}
