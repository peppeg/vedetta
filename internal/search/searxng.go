package search

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type SearxngProvider struct {
	baseURL    string
	httpClient *http.Client
}

type searxngResponse struct {
	Results []struct {
		URL     string  `json:"url"`
		Title   string  `json:"title"`
		Content string  `json:"content"`
		Engine  string  `json:"engine"`
		Score   float64 `json:"score"`
	} `json:"results"`
	// UnresponsiveEngines lists [engine, reason] pairs for engines that did not
	// answer this search (CAPTCHA, quota, timeout, parsing error).
	UnresponsiveEngines [][]string `json:"unresponsive_engines"`
}

func NewSearxngProvider(baseURL string) *SearxngProvider {
	baseURL = strings.TrimRight(baseURL, "/")
	return &SearxngProvider{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (s *SearxngProvider) Name() string {
	return "searxng"
}

func (s *SearxngProvider) Search(ctx context.Context, query string, opts Options) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("empty query")
	}

	u, err := url.Parse(s.baseURL + "/search")
	if err != nil {
		return nil, fmt.Errorf("invalid searxng url: %w", err)
	}

	q := u.Query()
	q.Set("q", query)
	q.Set("format", "json")
	if opts.Language != "" {
		q.Set("language", opts.Language)
	} else {
		q.Set("language", "auto")
	}
	if len(opts.Categories) > 0 {
		q.Set("categories", strings.Join(opts.Categories, ","))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", "Vedetta-Agent/1.0")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("searxng request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("searxng returned status %d", resp.StatusCode)
	}

	var data searxngResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("failed to decode searxng response: %w", err)
	}
	if rep := ReportFromContext(ctx); rep != nil {
		for _, pair := range data.UnresponsiveEngines {
			if len(pair) == 0 {
				continue
			}
			reason := ""
			if len(pair) > 1 {
				reason = pair[1]
			}
			rep.AddUnresponsive(pair[0], reason)
		}
	}

	maxRes := opts.MaxResults
	if maxRes <= 0 {
		maxRes = 10
	}

	var results []Result
	seen := make(map[string]bool)

	for _, item := range data.Results {
		cleanURL := strings.TrimSpace(item.URL)
		if cleanURL == "" || seen[cleanURL] {
			continue
		}
		seen[cleanURL] = true

		results = append(results, Result{
			Title:   strings.TrimSpace(item.Title),
			URL:     cleanURL,
			Content: strings.TrimSpace(item.Content),
			Engine:  item.Engine,
			Score:   item.Score,
		})

		if len(results) >= maxRes {
			break
		}
	}

	// Automatic Fallback: If specialized categories (e.g. science/scholar, social media)
	// yielded 0 results (due to rate limits, CAPTCHA, or empty niche index),
	// retry seamlessly on the "general" web category.
	if len(results) == 0 && len(opts.Categories) > 0 && !containsCategory(opts.Categories, "general") {
		fallbackOpts := opts
		fallbackOpts.Categories = []string{"general"}
		return s.Search(ctx, query, fallbackOpts)
	}

	return results, nil
}

func containsCategory(cats []string, target string) bool {
	for _, c := range cats {
		if strings.EqualFold(strings.TrimSpace(c), target) {
			return true
		}
	}
	return false
}
