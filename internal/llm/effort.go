package llm

import "strings"

// Effort is a logical reasoning-effort level, independent of any server-specific
// enum. The client maps it to a physical `reasoning_effort` value the server accepts,
// or omits the field entirely (the portable baseline).
type Effort string

const (
	// EffortDefault omits the reasoning_effort field (most portable).
	EffortDefault Effort = ""
	// EffortOff requests no/minimal reasoning for deterministic JSON tasks.
	EffortOff Effort = "off"
	// EffortLow requests light reasoning.
	EffortLow Effort = "low"
	// EffortMedium requests moderate reasoning.
	EffortMedium Effort = "medium"
	// EffortHigh requests strong reasoning.
	EffortHigh Effort = "high"
	// EffortMax requests the highest available reasoning.
	EffortMax Effort = "max"
)

// effortCandidates returns the ordered physical `reasoning_effort` values to try for a
// logical level, from the most preferred to the portable fallback ("" = omit the field).
// Different OpenAI-compatible servers accept different subsets:
//
//	DeepSeek/dwarfstar accepts "none"; halogen/Qwen accepts "minimal|low|medium|high|xhigh".
func effortCandidates(e Effort) []string {
	switch e {
	case EffortOff:
		return []string{"none", "minimal", ""}
	case EffortLow:
		return []string{"low", "minimal", ""}
	case EffortMedium:
		return []string{"medium", "low", ""}
	case EffortHigh:
		return []string{"high", "medium", ""}
	case EffortMax:
		return []string{"xhigh", "high", ""}
	default:
		return []string{""}
	}
}

// AllEffortValues is the set of physical values probed by ProbeEffort.
var AllEffortValues = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

// ParseEffort converts a configuration string into a logical Effort level.
// Unknown or empty values map to EffortDefault (omit the field).
func ParseEffort(s string) Effort {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "none", "minimal":
		return EffortOff
	case "low":
		return EffortLow
	case "medium", "mid":
		return EffortMedium
	case "high":
		return EffortHigh
	case "max", "xhigh":
		return EffortMax
	default:
		return EffortDefault
	}
}
