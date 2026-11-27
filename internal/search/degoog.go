package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DegoogProvider searches through degoog (github.com/degoog-org/degoog), a
// self-hosted metasearch engine. It asks degoog's own JSON search at
// /api/search, the one degoog's web page uses. degoog can also answer in
// SearXNG's shape, but only when its admin turns that on, and Vedetta would
// lose the language: degoog reads "lang", not SearXNG's "language".
type DegoogProvider struct {
	baseURL    string
	httpClient *http.Client
}

// degoogResponse is the part of degoog's answer that Vedetta reads (degoog 1.1).
type degoogResponse struct {
	Results []struct {
		URL     string `json:"url"`
		Title   string `json:"title"`
		Snippet string `json:"snippet"`
		// Content repeats Snippet for Open WebUI; degoog plans to drop it.
		Content string  `json:"content"`
		Source  string  `json:"source"` // the first engine that found the result
		Score   float64 `json:"score"`
	} `json:"results"`
	// EngineTimings has one entry per engine asked. Any status but "ok" means
	// the engine did not answer ("rate_limited", "blocked", "interstitial"); an
	// "ok" engine with no results did answer.
	EngineTimings []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"engineTimings"`
}

// degoog waits up to 10 seconds for each engine (its default), all at once.
const degoogTimeout = 25 * time.Second

func NewDegoogProvider(baseURL string) *DegoogProvider {
	return &DegoogProvider{baseURL: DegoogBase(baseURL), httpClient: &http.Client{Timeout: degoogTimeout}}
}

func (d *DegoogProvider) Name() string {
	return "degoog"
}

// DegoogBase drops what is often pasted after degoog's address: a final slash,
// /search (its web page), /api or /api/search.
func DegoogBase(raw string) string {
	b := strings.TrimRight(strings.TrimSpace(raw), "/")
	for _, suffix := range []string{"/search", "/api"} {
		b = strings.TrimRight(strings.TrimSuffix(b, suffix), "/")
	}
	return b
}

// degoogLang turns Vedetta's language into degoog's "lang": the primary
// subtag in lowercase ("it", "en"). "auto", and anything that is not a
// language, gives "": degoog then searches with no language. Sent as it is,
// "auto" would reach the engines (Bing would get setlang=auto).
func degoogLang(lang string) string {
	l := strings.ToLower(strings.TrimSpace(lang))
	if i := strings.IndexAny(l, "-_"); i >= 0 {
		l = l[:i]
	}
	if len(l) < 2 || len(l) > 3 {
		return ""
	}
	for _, r := range l {
		if r < 'a' || r > 'z' {
			return ""
		}
	}
	return l
}

// Search asks degoog. Categories are not sent: the Deep always searches the
// whole web, and degoog has no categories, only kinds (web, news, images).
func (d *DegoogProvider) Search(ctx context.Context, query string, opts Options) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("empty query")
	}
	q := url.Values{}
	q.Set("q", query)
	if l := degoogLang(opts.Language); l != "" {
		q.Set("lang", l)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+"/api/search?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("invalid degoog url: %w", err)
	}
	req.Header.Set("User-Agent", "Vedetta-Agent/1.0")
	req.Header.Set("Accept", "application/json")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("degoog request failed: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return nil, ErrQuotaExhausted
	default:
		return nil, fmt.Errorf("degoog returned status %d", resp.StatusCode)
	}

	var data degoogResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("failed to decode degoog response: %w", err)
	}
	if rep := ReportFromContext(ctx); rep != nil {
		for _, e := range data.EngineTimings {
			status := strings.TrimSpace(e.Status)
			if status == "" || strings.EqualFold(status, "ok") {
				continue
			}
			rep.AddUnresponsive(e.Name, strings.NewReplacer("_", " ", "-", " ").Replace(status))
		}
	}

	maxRes := opts.MaxResults
	if maxRes <= 0 {
		maxRes = 10
	}
	var results []Result
	seen := make(map[string]bool)
	for _, item := range data.Results {
		u := strings.TrimSpace(item.URL)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		text := strings.TrimSpace(item.Snippet)
		if text == "" {
			text = strings.TrimSpace(item.Content)
		}
		results = append(results, Result{
			Title:   strings.TrimSpace(item.Title),
			URL:     u,
			Content: text,
			Engine:  strings.TrimSpace(item.Source),
			Score:   item.Score,
		})
		if len(results) >= maxRes {
			break
		}
	}
	return results, nil
}

// probeDegoog is the degoog doctor. One request that asks no engine: a search
// with no query. degoog answers it with 400 in JSON; first it checks its rate
// limit (429) and, when its admin wants one, the API key (401).
func probeDegoog(ctx context.Context, rawURL string) ProbeStatus {
	st := ProbeStatus{Name: "degoog"}
	base := DegoogBase(rawURL)
	if base == "" {
		st.Status, st.Detail = "unknown", "no address"
		return st
	}
	down := func(problem, detail string) ProbeStatus {
		st.Status, st.Problem, st.Detail = "down", problem, detail
		return st
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/search?q=", nil)
	if err != nil {
		return down("unreachable", err.Error())
	}
	req.Header.Set("User-Agent", "Vedetta-Agent/1.0")
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return down("unreachable", err.Error())
	}
	defer resp.Body.Close()
	isJSON := strings.Contains(resp.Header.Get("Content-Type"), "json")
	var obj map[string]json.RawMessage
	isJSON = isJSON && json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&obj) == nil
	switch {
	case resp.StatusCode == http.StatusBadRequest && isJSON:
		st.Status = "ok"
		return st
	case resp.StatusCode == http.StatusUnauthorized && isJSON:
		return down("degoog_key", "status 401")
	case resp.StatusCode == http.StatusTooManyRequests:
		return down("degoog_limit", "status 429")
	}
	return down("not_degoog", fmt.Sprintf("status %d", resp.StatusCode))
}

// ProbeDegoogResults sends one real search (first run only): a degoog that
// answers with no results usually has its engines blocked.
func ProbeDegoogResults(ctx context.Context, rawURL string) (int, error) {
	res, err := NewDegoogProvider(rawURL).Search(ctx, "open source", Options{MaxResults: 50})
	return len(res), err
}
