package agent

import (
	"encoding/json"

	"vedetta/internal/cache"
	"vedetta/internal/search"
)

// answerMetaVersion is bumped whenever AnswerMeta changes shape, so offline tools
// (scripts/eval.py) can tell old and new records apart.
const answerMetaVersion = 7

// CitationRef maps a rendered citation number [n] to the fact it stands for.
type CitationRef struct {
	N         int    `json:"n"`
	FactID    string `json:"fact_id"`
	SourceURL string `json:"source_url"`
}

// AnswerMeta is the structured, Go-computed summary of an answer that is persisted
// next to the message, so evaluation never has to parse it back from the text.
type AnswerMeta struct {
	Version         int           `json:"v"`
	Provenance      Provenance    `json:"provenance"`
	Partial         bool          `json:"partial"`
	Citations       []CitationRef `json:"citations"`
	DroppedPhantoms int           `json:"dropped_phantoms"`
	// V25-T3.5 (v2): cited search previews [Sn] and link checks.
	SnippetCitations       []SnippetRef `json:"snippet_citations,omitempty"`
	DroppedPhantomSnippets int          `json:"dropped_phantom_snippets"`
	RetargetedSnippets     int          `json:"retargeted_snippets,omitempty"`
	DroppedPhantomURLs     int          `json:"dropped_phantom_urls"`
	RepairedURLs           int          `json:"repaired_urls"`
	// V25-QA A5 (v6): sentences the writer declared as its own reasoning.
	Deductions   int `json:"deductions"`
	FactsCount   int `json:"facts_count"`
	SourcesCount int `json:"sources_count"`
	SourcesRead  int `json:"sources_read"`
	Hops         int `json:"hops"`
	Nodes        int `json:"nodes"`
	// I12 (v3): how many searches ran, how many came back empty, and the
	// meta-search engines that did not answer.
	SearchQueries       int                    `json:"search_queries"`
	EmptySearches       int                    `json:"empty_searches"`
	UnresponsiveEngines []search.EngineFailure `json:"unresponsive_engines,omitempty"`
	// V25-R1 (v4): search and page cache hits of this session; absent when the cache is off.
	Cache *cache.Stats `json:"cache,omitempty"`
	// V25-T7.1b (v5): stages that ran without the model and why; absent when none did.
	LLMTrouble map[string]LLMTroubleCount `json:"llm_trouble,omitempty"`
	// v7: what the Deep a ciclo did (deeploop.go); absent in older answers.
	Loop *LoopStats `json:"loop,omitempty"`
	// v7: for each sentence linking a page read (or a link Vedetta opened), the passage
	// of that page closest to it (loopanchors.go); the judge reads it.
	Anchors []CitationAnchor `json:"anchors,omitempty"`
}

// marshalAnswerMeta encodes meta for storage; a nil meta yields nil (stored as "{}").
func marshalAnswerMeta(meta *AnswerMeta) json.RawMessage {
	if meta == nil {
		return nil
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return nil
	}
	return b
}
