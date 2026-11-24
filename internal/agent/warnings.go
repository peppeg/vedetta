package agent

// WarningKind classifies client-facing warnings so the UI can react appropriately
// (e.g. resume call-to-action only for a partial answer, distinct labels for
// search vs reader fallback). See Vedetta v2.4, F1.
type WarningKind string

const (
	// WarnKindPartial indicates the synthesis was cut short by a limit/error.
	WarnKindPartial WarningKind = "partial"
	// WarnKindSearchFallback indicates the search provider chain degraded.
	WarnKindSearchFallback WarningKind = "search_fallback"
	// WarnKindReaderFallback indicates the reader chain degraded (e.g. http -> jina).
	WarnKindReaderFallback WarningKind = "reader_fallback"
	// WarnKindPlannerFallback indicates the query planner fell back to heuristics.
	WarnKindPlannerFallback WarningKind = "planner_fallback"
	// WarnKindGroundingLoss indicates no verified facts were extracted.
	WarnKindGroundingLoss WarningKind = "grounding_loss"
	// WarnKindAnchoredGap indicates a follow-up had no session evidence to anchor to, so a new search started.
	WarnKindAnchoredGap WarningKind = "anchored_gap"
	// WarnKindModelIncompatible indicates the inference server rejected a request (e.g. HTTP 400 on reasoning_effort).
	WarnKindModelIncompatible WarningKind = "model_incompatible"
	// WarnKindLLMUnavailable indicates some stages ran without the model (server unavailable or too slow), V25-T7.1b.
	WarnKindLLMUnavailable WarningKind = "llm_unavailable"
)
