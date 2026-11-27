package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ProbeStatus is the honest health report for a single search provider.
// Status is one of: ok, down, unknown, configured.
type ProbeStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Problem names what is wrong, as a code the page says in words (map R8):
	// unreachable, not_searxng, use_api (a SearXNG-style search such as degoog
	// answers under /api: Detail is that address), json_off, limiter,
	// no_results; limiter_on is a warning on a working SearXNG.
	Problem string `json:"problem,omitempty"`
	Version string `json:"version,omitempty"`
}

// ProbeProvider probes the given search provider name using the supplied
// configuration. States are never faked: a keyed provider with no cheap
// endpoint to test is reported as "configured", a keyless provider that is
// not probed is reported as "unknown".
func ProbeProvider(ctx context.Context, name string, cfg ProviderConfig) ProbeStatus {
	clean := strings.ToLower(strings.TrimSpace(name))
	switch clean {
	case "searxng":
		return probeSearxng(ctx, cfg.SearxngURL)
	case "brave":
		return probeKeyed("brave", cfg.BraveAPIKey)
	case "tavily":
		return probeKeyed("tavily", cfg.TavilyAPIKey)
	case "serper":
		return probeKeyed("serper", cfg.SerperAPIKey)
	case "exa":
		return probeKeyed("exa", cfg.ExaAPIKey)
	case "academic", "europepmc":
		return ProbeStatus{Name: clean, Status: "unknown", Detail: "keyless provider; endpoint not probed"}
	default:
		return ProbeStatus{Name: clean, Status: "unknown", Detail: "unknown provider"}
	}
}

// probeSearxng is the SearXNG doctor (map R8). Two cheap requests, no search
// sent to the engines: /config says whether it is SearXNG, its version and its
// limiter; a search with an empty query in JSON answers 400 ("no query") when the
// JSON format is on and 403 when it is off.
func probeSearxng(ctx context.Context, rawURL string) ProbeStatus {
	base := strings.TrimRight(strings.TrimSpace(rawURL), "/")
	if base == "" {
		base = "http://localhost:8888"
	}
	st := ProbeStatus{Name: "searxng"}
	down := func(problem, detail string) ProbeStatus {
		st.Status, st.Problem, st.Detail = "down", problem, detail
		return st
	}
	client := &http.Client{Timeout: 5 * time.Second}
	get := func(path string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "Vedetta-Agent/1.0")
		req.Header.Set("Accept", "application/json")
		return client.Do(req)
	}

	resp, err := get("/config")
	if err != nil {
		return down("unreachable", err.Error())
	}
	var info struct {
		Version *string         `json:"version"`
		Engines json.RawMessage `json:"engines"`
		Limiter json.RawMessage `json:"limiter"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return down("limiter", "status 429")
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &info) != nil || info.Version == nil || len(info.Engines) == 0 {
		// Not SearXNG. degoog, and any service that copies SearXNG's JSON
		// search, has no /config but answers the empty JSON search with 400
		// like a SearXNG with JSON on. degoog's search sits under /api, so a
		// base address without it is pointed there.
		switch {
		case answersNoQuery(get, "/search?q=&format=json"):
			st.Status, st.Detail = "ok", "answers like SearXNG, without /config (degoog or similar)"
			return st
		case answersNoQuery(get, "/api/search?q=&format=json"):
			return down("use_api", base+"/api")
		}
		return down("not_searxng", fmt.Sprintf("status %d", resp.StatusCode))
	}
	st.Version = *info.Version

	resp, err = get("/search?q=&format=json")
	if err != nil {
		return down("unreachable", err.Error())
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusForbidden:
		return down("json_off", "status 403")
	case resp.StatusCode == http.StatusTooManyRequests:
		return down("limiter", "status 429")
	case resp.StatusCode != http.StatusBadRequest && (resp.StatusCode < 200 || resp.StatusCode >= 300):
		return down("", fmt.Sprintf("status %d", resp.StatusCode))
	}
	st.Status = "ok"
	if limiterOn(info.Limiter) {
		st.Problem = "limiter_on"
	}
	return st
}

// answersNoQuery says whether path answers an empty query as a JSON search
// does: status 400 with a JSON object. A web page answers with HTML, or with
// 200 or 404.
func answersNoQuery(get func(string) (*http.Response, error), path string) bool {
	resp, err := get(path)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(resp.Header.Get("Content-Type"), "json") {
		return false
	}
	var obj map[string]json.RawMessage
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&obj) == nil
}

// limiterOn reads /config's limiter: a bool in older SearXNG, an object with
// "enabled" in newer ones.
func limiterOn(raw json.RawMessage) bool {
	var on bool
	if json.Unmarshal(raw, &on) == nil {
		return on
	}
	var obj struct {
		Enabled bool `json:"enabled"`
	}
	return json.Unmarshal(raw, &obj) == nil && obj.Enabled
}

// ProbeSearxngResults sends one real search (map R8, first run only): a SearXNG
// that answers with no results usually has its engines blocked.
func ProbeSearxngResults(ctx context.Context, rawURL string) (int, error) {
	base := strings.TrimRight(strings.TrimSpace(rawURL), "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/search?q=open+source&format=json", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "Vedetta-Agent/1.0")
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 12 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("status %d", resp.StatusCode)
	}
	var out struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return 0, err
	}
	return len(out.Results), nil
}

// probeKeyed reports a provider that is usable only with an API key. Presence
// of the key is "configured"; absence is "unknown" because we cannot verify
// reachability without performing a real (billable) request.
func probeKeyed(name, key string) ProbeStatus {
	if strings.TrimSpace(key) != "" {
		return ProbeStatus{Name: name, Status: "configured", Detail: "key present"}
	}
	return ProbeStatus{Name: name, Status: "unknown", Detail: "no api key"}
}
