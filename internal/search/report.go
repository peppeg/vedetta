package search

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Report collects, over a whole session, the engines a meta-search provider
// said did not answer (SearXNG "unresponsive_engines": CAPTCHA, quota, timeout).
// A session whose results come from one engine only is otherwise
// indistinguishable from a session where every engine answered (idea I12).
type Report struct {
	mu      sync.Mutex
	engines map[string]*engineFailure
	queries int
	empty   int
}

type engineFailure struct {
	count  int
	reason string
}

// EngineFailure is one engine that did not answer, with how many searches it
// missed and the last reason it gave.
type EngineFailure struct {
	Engine string `json:"engine"`
	Count  int    `json:"count"`
	Reason string `json:"reason"`
}

func NewReport() *Report {
	return &Report{engines: make(map[string]*engineFailure)}
}

type reportContextKey struct{}

// WithReport attaches r to ctx so providers can record into it.
func WithReport(ctx context.Context, r *Report) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, reportContextKey{}, r)
}

// ReportFromContext returns the report attached to ctx, or nil.
func ReportFromContext(ctx context.Context) *Report {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(reportContextKey{}).(*Report)
	return r
}

// AddUnresponsive records that engine did not answer one search. SearXNG
// prefixes the reason with "Suspended: " once it stops querying the engine;
// both count as the same reason.
func (r *Report) AddUnresponsive(engine, reason string) {
	if r == nil {
		return
	}
	engine = strings.TrimSpace(engine)
	if engine == "" {
		return
	}
	reason = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(reason), "Suspended:"))
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.engines[engine]
	if f == nil {
		f = &engineFailure{}
		r.engines[engine] = f
	}
	f.count++
	if reason != "" {
		f.reason = reason
	}
}

// AddSearch records one search and whether it returned no results (or failed).
func (r *Report) AddSearch(empty bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries++
	if empty {
		r.empty++
	}
}

// Searches returns how many searches ran and how many came back empty.
func (r *Report) Searches() (queries, empty int) {
	if r == nil {
		return 0, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.queries, r.empty
}

// Unresponsive returns the engines that did not answer, most missed first.
func (r *Report) Unresponsive() []EngineFailure {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]EngineFailure, 0, len(r.engines))
	for name, f := range r.engines {
		out = append(out, EngineFailure{Engine: name, Count: f.count, Reason: f.reason})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Engine < out[j].Engine
	})
	return out
}

// String formats an engine failure as "google cse (too many requests ×3)".
func (f EngineFailure) String() string {
	reason := f.Reason
	if reason == "" {
		reason = "no answer"
	}
	return fmt.Sprintf("%s (%s ×%d)", f.Engine, reason, f.Count)
}
