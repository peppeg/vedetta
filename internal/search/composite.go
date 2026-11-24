package search

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// CompositeConfig defines options for creating a CompositeProvider.
type CompositeConfig struct {
	Name            string
	Providers       []Provider
	Primary         Provider // Legacy compat
	Fallback        Provider // Legacy compat
	Academic        Provider
	FallbackOnEmpty bool
}

type sessionFallback struct {
	used     bool
	reason   string
	lastSeen time.Time
}

const maxSessionFallbackEntries = 512

// CompositeProvider implements search.Provider with an N-element provider chain,
// transparent per-session fallback, and conditioned academic routing per sub-query.
type CompositeProvider struct {
	name            string
	chain           []Provider
	academic        Provider
	fallbackOnEmpty bool
	mu              sync.RWMutex
	sessions        map[string]*sessionFallback
	globalUsed      bool
	globalReason    string
}

// NewCompositeProvider creates a new composite provider.
func NewCompositeProvider(cfg CompositeConfig) *CompositeProvider {
	var chain []Provider
	if len(cfg.Providers) > 0 {
		chain = append(chain, cfg.Providers...)
	} else {
		if cfg.Primary != nil {
			chain = append(chain, cfg.Primary)
		}
		if cfg.Fallback != nil {
			chain = append(chain, cfg.Fallback)
		}
	}

	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		if len(chain) > 0 && chain[0] != nil {
			name = chain[0].Name()
		} else {
			name = "composite"
		}
	}

	return &CompositeProvider{
		name:            name,
		chain:           chain,
		academic:        cfg.Academic,
		fallbackOnEmpty: cfg.FallbackOnEmpty,
		sessions:        make(map[string]*sessionFallback),
	}
}

func (c *CompositeProvider) Name() string {
	return c.name
}

func (c *CompositeProvider) Primary() Provider {
	if len(c.chain) > 0 {
		return c.chain[0]
	}
	return nil
}

func (c *CompositeProvider) Fallback() Provider {
	if len(c.chain) > 1 {
		return c.chain[1]
	}
	return nil
}

func (c *CompositeProvider) Providers() []Provider {
	res := make([]Provider, len(c.chain))
	copy(res, c.chain)
	return res
}

func (c *CompositeProvider) Academic() Provider {
	return c.academic
}

// FallbackStatus returns whether fallback occurred and the reason, optionally scoped to a sessionID.
func (c *CompositeProvider) FallbackStatus(sessionID ...string) (bool, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(sessionID) > 0 && sessionID[0] != "" {
		if sf, ok := c.sessions[sessionID[0]]; ok {
			return sf.used, sf.reason
		}
		return false, ""
	}
	return c.globalUsed, c.globalReason
}

// ResetFallback clears fallback state, optionally scoped to a sessionID.
func (c *CompositeProvider) ResetFallback(sessionID ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(sessionID) > 0 && sessionID[0] != "" {
		delete(c.sessions, sessionID[0])
		return
	}
	c.globalUsed = false
	c.globalReason = ""
	c.sessions = make(map[string]*sessionFallback)
}

func (c *CompositeProvider) recordFallback(sessionID string, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.globalUsed = true
	if c.globalReason == "" {
		c.globalReason = reason
	} else if !strings.Contains(c.globalReason, reason) {
		c.globalReason += "; " + reason
	}

	if sessionID != "" {
		sf, ok := c.sessions[sessionID]
		if !ok {
			sf = &sessionFallback{}
			c.sessions[sessionID] = sf
		}
		sf.used = true
		sf.lastSeen = time.Now()
		if sf.reason == "" {
			sf.reason = reason
		} else if !strings.Contains(sf.reason, reason) {
			sf.reason += "; " + reason
		}
		c.pruneSessionsLocked()
	}
}

// pruneSessionsLocked bounds the per-session map to avoid unbounded growth for
// sessions that are never revisited. Must be called with c.mu held.
func (c *CompositeProvider) pruneSessionsLocked() {
	if len(c.sessions) <= maxSessionFallbackEntries {
		return
	}
	var oldestID string
	var oldest time.Time
	for id, sf := range c.sessions {
		if oldestID == "" || sf.lastSeen.Before(oldest) {
			oldestID = id
			oldest = sf.lastSeen
		}
	}
	delete(c.sessions, oldestID)
}

// shouldRouteToAcademic implements B-R1.1: requires BOTH BiomedicalQuery flag on the subquery
// AND at least one biomedical repository domain in AuthoritativeDomains.
func (c *CompositeProvider) shouldRouteToAcademic(opts Options) bool {
	if c.academic == nil || !opts.BiomedicalQuery {
		return false
	}
	for _, d := range opts.AuthoritativeDomains {
		ld := strings.ToLower(d)
		if strings.Contains(ld, "ncbi") || strings.Contains(ld, "pubmed") || strings.Contains(ld, "europepmc") {
			return true
		}
	}
	return false
}

func (c *CompositeProvider) Search(ctx context.Context, query string, opts Options) ([]Result, error) {
	cleanQuery := strings.TrimSpace(query)
	if cleanQuery == "" {
		return nil, fmt.Errorf("empty query")
	}

	sessID := opts.SessionID

	// 1. Academic routing if strictly conditioned on BiomedicalQuery and authoritative domains
	var academicResults []Result
	if c.shouldRouteToAcademic(opts) {
		acRes, acErr := c.academic.Search(ctx, cleanQuery, opts)
		if acErr != nil {
			log.Printf("[composite] academic provider %s search notice for %q: %v", c.academic.Name(), cleanQuery, acErr)
		} else {
			academicResults = acRes
		}
	}

	// 2. Web search across provider chain
	if len(c.chain) == 0 {
		if len(academicResults) > 0 {
			return academicResults, nil
		}
		return nil, fmt.Errorf("no search providers configured in chain")
	}

	// Single provider in chain: hard error on failure, empty response on 0 results (Rule 6)
	if len(c.chain) == 1 {
		p := c.chain[0]
		res, pErr := p.Search(ctx, cleanQuery, opts)
		if pErr != nil {
			if len(academicResults) > 0 {
				c.recordFallback(sessID, fmt.Sprintf("%s degradato (%v)", p.Name(), pErr))
				return academicResults, nil
			}
			return nil, pErr
		}
		return mergeResults(academicResults, res, opts.MaxResults), nil
	}

	// Chain of N providers
	var lastErr error
	var webResults []Result
	foundValid := false

	for idx, p := range c.chain {
		res, pErr := p.Search(ctx, cleanQuery, opts)
		if pErr != nil {
			lastErr = pErr
			reason := fmt.Sprintf("%s degradato (%v)", p.Name(), pErr)
			c.recordFallback(sessID, reason)
			log.Printf("[composite] chain provider [%d] %s returned error: %v", idx, p.Name(), pErr)
			continue
		}

		if len(res) == 0 {
			if c.fallbackOnEmpty && idx < len(c.chain)-1 {
				reason := fmt.Sprintf("%s nessun risultato", p.Name())
				c.recordFallback(sessID, reason)
				log.Printf("[composite] chain provider [%d] %s returned 0 results, fallbackOnEmpty active, advancing", idx, p.Name())
				continue
			}
			foundValid = true
			webResults = res
			break
		}

		foundValid = true
		webResults = res
		break
	}

	if !foundValid {
		if len(academicResults) > 0 {
			// Record primary failure even when academic succeeded
			if lastErr != nil {
				c.recordFallback(sessID, fmt.Sprintf("chain degradata (%v)", lastErr))
			}
			return academicResults, nil
		}
		if lastErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrAllProvidersFailed, lastErr)
		}
		return nil, ErrAllProvidersFailed
	}

	return mergeResults(academicResults, webResults, opts.MaxResults), nil
}

func mergeResults(academic, web []Result, maxResults int) []Result {
	seen := make(map[string]bool)
	var final []Result

	for _, r := range academic {
		cleanURL := strings.TrimSpace(r.URL)
		if cleanURL == "" || seen[cleanURL] {
			continue
		}
		seen[cleanURL] = true
		final = append(final, r)
	}

	for _, r := range web {
		cleanURL := strings.TrimSpace(r.URL)
		if cleanURL == "" || seen[cleanURL] {
			continue
		}
		seen[cleanURL] = true
		final = append(final, r)
	}

	maxLimit := maxResults
	if maxLimit <= 0 {
		maxLimit = 15
	}
	capLimit := maxLimit * 2
	if capLimit < 20 {
		capLimit = 20
	}
	if len(final) > capLimit {
		final = final[:capLimit]
	}
	return final
}
