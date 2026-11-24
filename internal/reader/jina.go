package reader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type JinaReader struct {
	baseURL    string
	httpClient *http.Client
}

type jinaResponse struct {
	Code int `json:"code"`
	Data struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		URL         string `json:"url"`
		Content     string `json:"content"`
		Warning     string `json:"warning"`
	} `json:"data"`
}

// jinaTargetErrorRe reads the status of the site from Jina's warning: Jina
// answers 200 with the site's error page and says "Target URL returned error 404:
// Not Found" (seen on the Jina at home, 11/11/2026).
var jinaTargetErrorRe = regexp.MustCompile(`returned error (\d{3})`)

func NewJinaReader(baseURL string) *JinaReader {
	baseURL = strings.TrimRight(baseURL, "/")
	return &JinaReader{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

func (j *JinaReader) Name() string {
	return "jina-reader"
}

func (j *JinaReader) Read(ctx context.Context, targetURL string) (*Document, error) {
	// First enforce SSRF validation on target URL
	if err := ValidateSafeURL(targetURL); err != nil {
		return nil, fmt.Errorf("ssrf validation failed: %w", err)
	}

	endpoint := fmt.Sprintf("%s/%s", j.baseURL, targetURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create jina request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Vedetta-Agent/1.0")

	resp, err := j.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jina request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jina reader returned status %d", resp.StatusCode)
	}

	var data jinaResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("failed to decode jina response: %w", err)
	}
	if m := jinaTargetErrorRe.FindStringSubmatch(data.Data.Warning); m != nil {
		if code, _ := strconv.Atoi(m[1]); code >= 400 {
			return nil, targetStatusError("jina", code, strings.TrimSpace(data.Data.Warning))
		}
	}

	title := strings.TrimSpace(data.Data.Title)
	if title == "" {
		title = "No Title"
	}

	return &Document{
		Title:       title,
		URL:         targetURL,
		Content:     strings.TrimSpace(data.Data.Content),
		ExtractedAt: time.Now(),
		Metadata: map[string]string{
			"description": data.Data.Description,
		},
	}, nil
}
