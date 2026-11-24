package reader

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// FirecrawlClient integrates the Firecrawl cloud scraping API for SPA, PDF, and complex JS rendering.
type FirecrawlClient struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

type firecrawlScrapeReq struct {
	URL     string   `json:"url"`
	Formats []string `json:"formats"`
}

type firecrawlScrapeResp struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
	Data    struct {
		Markdown string `json:"markdown"`
		Metadata struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			SourceURL   string `json:"sourceURL"`
			StatusCode  int    `json:"statusCode"`
		} `json:"metadata"`
	} `json:"data"`
}

// NewFirecrawlClient creates a new Firecrawl client with API key and optional custom base URL.
func NewFirecrawlClient(apiKey string, baseURL ...string) *FirecrawlClient {
	base := "https://api.firecrawl.dev/v1"
	if len(baseURL) > 0 && strings.TrimSpace(baseURL[0]) != "" {
		base = strings.TrimRight(strings.TrimSpace(baseURL[0]), "/")
	}
	return &FirecrawlClient{
		apiKey:  strings.TrimSpace(apiKey),
		baseURL: base,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (fc *FirecrawlClient) Name() string {
	return "firecrawl"
}

// Read delegates extraction of the target URL to Firecrawl.
func (fc *FirecrawlClient) Read(ctx context.Context, targetURL string) (*Document, error) {
	if fc.apiKey == "" {
		return nil, fmt.Errorf("firecrawl api key is not configured")
	}

	// 1. SSRF validation
	if err := ValidateSafeURL(targetURL); err != nil {
		return nil, fmt.Errorf("ssrf validation failed: %w", err)
	}

	endpoint := fmt.Sprintf("%s/scrape", fc.baseURL)
	reqPayload := firecrawlScrapeReq{
		URL:     targetURL,
		Formats: []string{"markdown"},
	}

	jsonBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, fmt.Errorf("failed to encode firecrawl payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(jsonBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create firecrawl request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+fc.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Vedetta-Agent/1.0")

	resp, err := fc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("firecrawl request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("firecrawl auth failed (status %d): check FIRECRAWL_API_KEY", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("firecrawl rate limit exceeded (status 429)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("firecrawl returned non-200 status: %d", resp.StatusCode)
	}

	var parsed firecrawlScrapeResp
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("failed to decode firecrawl response: %w", err)
	}

	if !parsed.Success && parsed.Error != "" {
		return nil, fmt.Errorf("firecrawl scrape error: %s", parsed.Error)
	}
	if code := parsed.Data.Metadata.StatusCode; code >= 400 {
		return nil, targetStatusError("firecrawl", code, http.StatusText(code))
	}

	title := strings.TrimSpace(parsed.Data.Metadata.Title)
	if title == "" {
		title = "No Title"
	}

	content := strings.TrimSpace(parsed.Data.Markdown)
	if content == "" {
		return nil, fmt.Errorf("%w: firecrawl returned empty markdown content", ErrLowDensityContent)
	}

	return &Document{
		Title:       title,
		URL:         targetURL,
		Content:     content,
		ExtractedAt: time.Now(),
		Metadata: map[string]string{
			"description": parsed.Data.Metadata.Description,
			"source_url":  parsed.Data.Metadata.SourceURL,
			"source_type": "firecrawl",
		},
	}, nil
}
