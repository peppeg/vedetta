package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"vedetta/internal/reader"
	"vedetta/internal/search"
)

// Map R8: the first run looks for SearXNG and Jina Reader at the usual addresses:
// the service names of our compose, this machine, and the machine that hosts the
// container. It never scans the home network.
var (
	searxngCandidates = []string{
		"http://searxng:8080",
		"http://localhost:8888", "http://localhost:8080",
		"http://host.docker.internal:8888", "http://host.docker.internal:8080",
		"http://host.containers.internal:8888", "http://host.containers.internal:8080",
	}
	jinaCandidates = []string{
		"http://jina:8081",
		"http://localhost:3001", "http://localhost:3000",
		"http://host.docker.internal:3001", "http://host.docker.internal:3000",
		"http://host.containers.internal:3001", "http://host.containers.internal:3000",
	}
	discoverProbeTime   = 3 * time.Second
	discoverResultsTime = 12 * time.Second
)

// FoundService is one address the first run tried and can show.
type FoundService struct {
	URL     string `json:"url"`
	Status  string `json:"status"`            // ok, down
	Problem string `json:"problem,omitempty"` // the doctor's code (search.ProbeStatus, reader.ProbeStatus), or no_results
	Version string `json:"version,omitempty"`
	Results *int   `json:"results,omitempty"` // SearXNG: results of one real search
	Current bool   `json:"current,omitempty"` // the address set now
	Typed   bool   `json:"typed,omitempty"`   // the address the user wrote
}

type DiscoverResponse struct {
	Search      []FoundService `json:"search"`
	Reader      []FoundService `json:"reader"`
	InContainer bool           `json:"in_container"` // our compose runs Vedetta: "localhost" is the container
}

// normalizeServiceURL adds http:// to a bare host:port and drops the final slash;
// "" when it is not an http(s) address.
func normalizeServiceURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		return ""
	}
	return strings.TrimRight(u, "/")
}

// Discover answers GET /api/setup/discover[?searxng=…&jina=…] (map R8). The
// addresses in the query are the ones the user typed: they are always shown,
// like the ones set now; the usual addresses only when a SearXNG or a Jina
// answers there.
func (h *Handlers) Discover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.authorizeConfig(w, r) {
		return
	}
	h.cfgMu.RLock()
	curSearx, curJina, inContainer := h.cfg.SearxngURL, h.cfg.JinaURL, h.cfg.Publish != ""
	h.cfgMu.RUnlock()

	q := r.URL.Query()
	resp := DiscoverResponse{InContainer: inContainer}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		resp.Search = discoverKind(r.Context(), q.Get("searxng"), curSearx, searxngCandidates, probeSearxngAt)
		addResults(r.Context(), resp.Search)
	}()
	go func() {
		defer wg.Done()
		resp.Reader = discoverKind(r.Context(), q.Get("jina"), curJina, jinaCandidates, probeJinaAt)
	}()
	wg.Wait()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func probeSearxngAt(ctx context.Context, u string) FoundService {
	ps := search.ProbeProvider(ctx, "searxng", search.ProviderConfig{SearxngURL: u})
	return FoundService{URL: u, Status: ps.Status, Problem: ps.Problem, Version: ps.Version}
}

func probeJinaAt(ctx context.Context, u string) FoundService {
	ps := reader.ProbeReader(ctx, "jina", reader.ReaderConfig{JinaBaseURL: u})
	return FoundService{URL: u, Status: ps.Status, Problem: ps.Problem}
}

// discoverKind probes the typed address, the current one and the usual ones in
// parallel, and keeps what is worth showing, working ones first.
func discoverKind(ctx context.Context, typed, current string, candidates []string, probe func(context.Context, string) FoundService) []FoundService {
	typed, current = normalizeServiceURL(typed), normalizeServiceURL(current)
	var order []string
	seen := map[string]bool{}
	for _, u := range append([]string{typed, current}, candidates...) {
		if u = normalizeServiceURL(u); u != "" && !seen[u] {
			seen[u] = true
			order = append(order, u)
		}
	}
	found := make([]FoundService, len(order))
	var wg sync.WaitGroup
	for i, u := range order {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, discoverProbeTime)
			defer cancel()
			found[i] = probe(pctx, u)
			found[i].Typed, found[i].Current = u == typed, u == current
		}(i, u)
	}
	wg.Wait()

	var ok, rest []FoundService
	for _, f := range found {
		switch {
		case f.Status == "ok":
			ok = append(ok, f)
		case f.Typed || f.Current:
			rest = append(rest, f)
		case f.Problem == "json_off" || f.Problem == "limiter":
			// a SearXNG at a usual address that Vedetta cannot use yet: worth saying why
			rest = append(rest, f)
		}
	}
	return append(ok, rest...)
}

// addResults sends one real search to each working SearXNG (at most three): one
// that answers with nothing usually has its engines blocked.
func addResults(ctx context.Context, list []FoundService) {
	var wg sync.WaitGroup
	for i := range list {
		if i >= 3 || list[i].Status != "ok" {
			continue
		}
		wg.Add(1)
		go func(f *FoundService) {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(ctx, discoverResultsTime)
			defer cancel()
			n, err := search.ProbeSearxngResults(rctx, f.URL)
			if err != nil {
				return
			}
			f.Results = &n
			if n == 0 {
				f.Problem = "no_results"
			}
		}(&list[i])
	}
	wg.Wait()
}
