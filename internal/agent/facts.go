package agent

import (
	"sync"
)

// ConfidenceLevel indicates how directly the claim is backed by the source text.
type ConfidenceLevel string

const (
	ConfidenceExplicit ConfidenceLevel = "explicit"
	ConfidenceReported ConfidenceLevel = "reported"
)

// AtomicFact represents an individual, self-contained statement extracted from a source document.
type AtomicFact struct {
	Claim           string   `json:"claim"`
	Entities        []string `json:"entities"`
	Attribute       string   `json:"attribute"`              // e.g. "clock speed", "release date", "core count"
	DateOfFact      string   `json:"date_of_fact,omitempty"` // ISO-8601 or partial (e.g. "2026-05")
	VerbatimSnippet string   `json:"verbatim_snippet"`       // Exact or near-exact quote from document (<= 25 words)
	Confidence      string   `json:"confidence"`             // "explicit" or "reported"
	// Set by Go after alignment, never by the model (V25-T3.1): Evidence is the original
	// document text matching the snippet, Passage the paragraph(s) around it.
	Evidence string `json:"evidence,omitempty"`
	Passage  string `json:"passage,omitempty"`
}

// FactStatus represents the epistemic status of a fact after cross-source reconciliation.
type FactStatus string

const (
	FactStatusConsensus   FactStatus = "consensus"   // >= 2 independent sources agreeing
	FactStatusConflicting FactStatus = "conflicting" // >= 2 sources diverging on the same attribute
	FactStatusUnconfirmed FactStatus = "unconfirmed" // 1 single source (grounded verbatim, but uncorroborated)
)

// ConflictGroup aggregates conflicting facts across sources for the same entity and attribute.
type ConflictGroup struct {
	Entity    string     `json:"entity"`
	Attribute string     `json:"attribute"`
	Status    FactStatus `json:"status"`
	Facts     []FactCard `json:"facts"`
}

// FactCard is an immutable, validated fact registered with a deterministic ID in the system.
type FactCard struct {
	ID     string `json:"id"`      // Deterministic ID assigned by Go backend (e.g. "F1", "F2")
	NodeID string `json:"node_id"` // Associated query/DAG node ID (e.g. "n1", "n2")
	AtomicFact
	SourceURL   string `json:"source_url"`
	SourceTitle string `json:"source_title"`
}

// FactStore manages thread-safe storage, numbering, and retrieval of facts across concurrent workers.
type FactStore struct {
	mu        sync.RWMutex
	facts     []FactCard
	byID      map[string]FactCard
	byNode    map[string][]FactCard
	statuses  map[string]FactStatus
	conflicts []ConflictGroup
	nextID    int
}

// NewFactStore creates an empty, initialized FactStore.
func NewFactStore() *FactStore {
	return &FactStore{
		facts:     make([]FactCard, 0),
		byID:      make(map[string]FactCard),
		byNode:    make(map[string][]FactCard),
		statuses:  make(map[string]FactStatus),
		conflicts: make([]ConflictGroup, 0),
		nextID:    1,
	}
}

// Get retrieves a fact card by its deterministic ID.
func (s *FactStore) Get(id string) (FactCard, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	card, ok := s.byID[id]
	return card, ok
}

// All returns a snapshot of all registered facts in order of registration.
func (s *FactStore) All() []FactCard {
	s.mu.RLock()
	defer s.mu.RUnlock()

	copied := make([]FactCard, len(s.facts))
	copy(copied, s.facts)
	return copied
}
