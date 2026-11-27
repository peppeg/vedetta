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

// Map R8: the first run looks for SearXNG, degoog and Jina Reader at the usual
// addresses: the service names of our compose (degoog: the name its own compose
// uses), this machine, and the machine that hosts the container. It never scans
// the home network.
var (
	searxngCandidates = []string{
		"http://searxng:8080",
		"http://localhost:8888", "http://localhost:8080",
		"http://host.docker.internal:8888", "http://host.docker.internal:8080",
		"http://host.containers.internal:8888", "http://host.containers.internal:8080",
	}
	degoogCandidates = []string{
		"http://degoog:4444",
		"http://127.0.0.1:4444", "http://localhost:4444", // degoog's container may not answer on ::1, where localhost goes first
		"http://host.docker.internal:4444", "http://host.containers.internal:4444",
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
	URL      string `json:"url"`
	Provider string `json:"provider,omitempty"` // search: "searxng" or "degoog"
	Status   string `json:"status"`             // ok, down
	Problem  string `json:"problem,omitempty"`  // the doctor's code (search.ProbeStatus, reader.ProbeStatus), or no_results
	Version  string `json:"version,omitempty"`
	Results  *int   `json:"results,omitempty"` // SearXNG and degoog: results of one real search
	Current  bool   `json:"current,omitempty"` // the address set now
	Typed    bool   `json:"typed,omitempty"`   // the address the user wrote
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
// like the ones set now; the usual addresses only when a SearXNG, a degoog or a
// Jina answers there. The typed search address may be a SearXNG or a degoog.
func (h *Handlers) Discover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.authorizeConfig(w, r) {
		return
	}
	h.cfgMu.RLock()
	curSearx, curDegoog, curJina, inContainer := h.cfg.SearxngURL, h.cfg.DegoogURL, h.cfg.JinaURL, h.cfg.Publish != ""
	h.cfgMu.RUnlock()

	q := r.URL.Query()
	resp := DiscoverResponse{InContainer: inContainer}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		var searx, dg []FoundService
		var both sync.WaitGroup
		both.Add(2)
		go func() {
			defer both.Done()
			searx = discoverKind(r.Context(), q.Get("searxng"), curSearx, searxngCandidates, probeSearchAt)
		}()
		go func() {
			defer both.Done()
			dg = dropLoopbackTwin(discoverKind(r.Context(), "", curDegoog, degoogCandidates, probeDegoogAt))
		}()
		both.Wait()
		resp.Search = mergeFound(searx, dg)
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

// probeSearchAt asks the SearXNG doctor; when it finds degoog there, the
// address is degoog's and the degoog doctor answers.
func probeSearchAt(ctx context.Context, u string) FoundService {
	ps := search.ProbeProvider(ctx, "searxng", search.ProviderConfig{SearxngURL: u})
	if ps.Problem == "is_degoog" && ps.Detail != "" {
		return probeDegoogAt(ctx, ps.Detail)
	}
	return FoundService{URL: u, Provider: "searxng", Status: ps.Status, Problem: ps.Problem, Version: ps.Version}
}

func probeDegoogAt(ctx context.Context, u string) FoundService {
	ps := search.ProbeProvider(ctx, "degoog", search.ProviderConfig{DegoogURL: u})
	return FoundService{URL: search.DegoogBase(u), Provider: "degoog", Status: ps.Status, Problem: ps.Problem}
}

// dropLoopbackTwin keeps one of http://127.0.0.1:P and http://localhost:P when
// both work: on one machine they are the same service.
func dropLoopbackTwin(list []FoundService) []FoundService {
	working := map[string]bool{}
	for _, f := range list {
		if f.Status == "ok" {
			working[f.URL] = true
		}
	}
	out := list[:0:0]
	for _, f := range list {
		if rest, ok := strings.CutPrefix(f.URL, "http://localhost:"); ok && f.Status == "ok" && !f.Typed && !f.Current && working["http://127.0.0.1:"+rest] {
			continue
		}
		out = append(out, f)
	}
	return out
}

// mergeFound joins the SearXNG and degoog lists: one entry per service and
// address (keeping the typed and current marks), working ones first.
func mergeFound(lists ...[]FoundService) []FoundService {
	var all []FoundService
	at := map[string]int{}
	for _, list := range lists {
		for _, f := range list {
			k := f.Provider + " " + f.URL
			if i, ok := at[k]; ok {
				all[i].Typed = all[i].Typed || f.Typed
				all[i].Current = all[i].Current || f.Current
				continue
			}
			at[k] = len(all)
			all = append(all, f)
		}
	}
	var ok, rest []FoundService
	for _, f := range all {
		if f.Status == "ok" {
			ok = append(ok, f)
		} else {
			rest = append(rest, f)
		}
	}
	return append(ok, rest...)
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
		case f.Problem == "json_off" || f.Problem == "limiter" || f.Problem == "degoog_key" || f.Problem == "degoog_limit":
			// a SearXNG or a degoog at a usual address that Vedetta cannot use yet: worth saying why
			rest = append(rest, f)
		}
	}
	return append(ok, rest...)
}

// addResults sends one real search to each working SearXNG or degoog (at most
// three): one that answers with nothing usually has its engines blocked.
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
			probe := search.ProbeSearxngResults
			if f.Provider == "degoog" {
				probe = search.ProbeDegoogResults
			}
			n, err := probe(rctx, f.URL)
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
