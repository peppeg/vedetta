package search

import (
	"context"
	"errors"
)

type Result struct {
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Content string  `json:"content"`
	Engine  string  `json:"engine,omitempty"`
	Score   float64 `json:"score,omitempty"`
}

var (
	// ErrQuotaExhausted indicates that the provider rate limit or monthly quota has been exhausted.
	ErrQuotaExhausted = errors.New("search provider quota exhausted (HTTP 429)")

	// ErrAllProvidersFailed indicates that every provider in the configured chain failed.
	ErrAllProvidersFailed = errors.New("all configured search providers failed")
)

type Options struct {
	SessionID            string
	Categories           []string
	Language             string
	MaxResults           int
	AuthoritativeDomains []string
	BiomedicalQuery      bool
	Facet                string // retrieval facet from the planner ("general"/"authoritative"/"offbeat"/"audience"); informative
}

type Provider interface {
	Name() string
	Search(ctx context.Context, query string, opts Options) ([]Result, error)
}

// FallbackReporter allows providers (e.g. CompositeProvider) to report
// if and why a fallback provider was engaged, optionally scoped to a sessionID.
type FallbackReporter interface {
	FallbackStatus(sessionID ...string) (used bool, reason string)
}

// FallbackResetter allows resetting fallback state before a new search session.
type FallbackResetter interface {
	ResetFallback(sessionID ...string)
}

// Reranker defines the interface for reranking candidate search results.
type Reranker interface {
	Rerank(query string, results []Result, minScore float64) []Result
}

// RootAwareReranker is an optional interface for rerankers that can contextualize
// sub-queries using the user's root query intent.
type RootAwareReranker interface {
	Reranker
	RerankWithRoot(query string, rootQuery string, results []Result, minScore float64) []Result
}

