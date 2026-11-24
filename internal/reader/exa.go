package reader

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

// exaMaxChars caps the page text asked to Exa: excerpt selection keeps at most
// VEDETTA_EXCERPT_MAX_CHARS of it, but chooses them from the whole page.
const exaMaxChars = 200000

// ExaReader reads pages through the Exa contents API, for the pages that refuse
// the direct HTTP reader (docs/SPEC_EXA_PROFILO.md). Exa answers from its cache
// or crawls the page live.
type ExaReader struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

type exaContentsRequest struct {
	URLs []string `json:"urls"`
	Text struct {
		MaxCharacters int `json:"maxCharacters"`
	} `json:"text"`
	LivecrawlTimeout int `json:"livecrawlTimeout"`
}

type exaContentsResponse struct {
	Results []struct {
		Title string `json:"title"`
		URL   string `json:"url"`
		Text  string `json:"text"`
	} `json:"results"`
	Statuses []struct {
		Status string `json:"status"`
		Source string `json:"source"`
		Error  *struct {
			Tag            string `json:"tag"`
			HTTPStatusCode *int   `json:"httpStatusCode"`
		} `json:"error"`
	} `json:"statuses"`
	CostDollars struct {
		Total float64 `json:"total"`
	} `json:"costDollars"`
}

// NewExaReader creates an Exa contents reader with an optional custom base URL.
func NewExaReader(apiKey string, baseURL ...string) *ExaReader {
	base := "https://api.exa.ai"
	if len(baseURL) > 0 && strings.TrimSpace(baseURL[0]) != "" {
		base = strings.TrimRight(strings.TrimSpace(baseURL[0]), "/")
	}
	return &ExaReader{
		apiKey:  strings.TrimSpace(apiKey),
		baseURL: base,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (e *ExaReader) Name() string {
	return "exa"
}

// Read asks Exa for the text of targetURL.
func (e *ExaReader) Read(ctx context.Context, targetURL string) (*Document, error) {
	if e.apiKey == "" {
		return nil, fmt.Errorf("exa api key is not configured")
	}
	if err := ValidateSafeURL(targetURL); err != nil {
		return nil, fmt.Errorf("ssrf validation failed: %w", err)
	}

	body := exaContentsRequest{URLs: []string{targetURL}, LivecrawlTimeout: 15000}
	body.Text.MaxCharacters = exaMaxChars
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to encode exa payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/contents", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create exa request: %w", err)
	}
	req.Header.Set("x-api-key", e.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Vedetta-Agent/1.0")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exa request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("exa auth failed (status %d): check EXA_API_KEY", resp.StatusCode)
	case http.StatusPaymentRequired:
		return nil, fmt.Errorf("exa credits exhausted (status 402)")
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("exa rate limit exceeded (status 429)")
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("exa returned status %d: %s", resp.StatusCode, string(msg))
	}

	var parsed exaContentsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("failed to decode exa response: %w", err)
	}
	source := ""
	if len(parsed.Statuses) > 0 {
		st := parsed.Statuses[0]
		source = st.Source
		if st.Status != "success" {
			reason := "unknown error"
			if st.Error != nil {
				reason = st.Error.Tag
				if st.Error.HTTPStatusCode != nil {
					reason = fmt.Sprintf("%s (http %d)", reason, *st.Error.HTTPStatusCode)
				}
			}
			log.Printf("[exa] session=%s kind=contents url=%s status=error cost=$%.4f", SessionIDFromContext(ctx), targetURL, parsed.CostDollars.Total)
			return nil, fmt.Errorf("exa could not read the page: %s", reason)
		}
	}
	if len(parsed.Results) == 0 {
		return nil, fmt.Errorf("%w: exa returned no result", ErrLowDensityContent)
	}
	res := parsed.Results[0]
	content := strings.TrimSpace(res.Text)
	log.Printf("[exa] session=%s kind=contents url=%s source=%s chars=%d cost=$%.4f", SessionIDFromContext(ctx), targetURL, source, len(content), parsed.CostDollars.Total)
	if content == "" {
		return nil, fmt.Errorf("%w: exa returned empty text", ErrLowDensityContent)
	}
	title := strings.TrimSpace(res.Title)
	if title == "" {
		title = "No Title"
	}
	return &Document{
		Title:       title,
		URL:         targetURL,
		Content:     content,
		ExtractedAt: time.Now(),
		Metadata: map[string]string{
			"source_type": "exa",
			"exa_source":  source,
		},
	}, nil
}
