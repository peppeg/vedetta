package llm

import (
	"encoding/json"
	"sort"
)

// Tool is a function the model may call (OpenAI "tools" format).
type Tool struct {
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema of the arguments
}

// ToolCall is one call the model asked for; Arguments is a JSON object as text.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// toolCallDelta is a piece of a tool call in a stream chunk: the server sends the
// id and name once and the arguments in pieces, all under the same index.
type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

// toolCallAccumulator joins the pieces of the tool calls of one streamed answer.
type toolCallAccumulator struct {
	byIndex map[int]*ToolCall
}

func (a *toolCallAccumulator) add(deltas []toolCallDelta) {
	for _, d := range deltas {
		if a.byIndex == nil {
			a.byIndex = make(map[int]*ToolCall)
		}
		c, ok := a.byIndex[d.Index]
		if !ok {
			c = &ToolCall{Type: "function"}
			a.byIndex[d.Index] = c
		}
		if d.ID != "" {
			c.ID = d.ID
		}
		if d.Type != "" {
			c.Type = d.Type
		}
		c.Function.Name += d.Function.Name
		c.Function.Arguments += d.Function.Arguments
	}
}

// calls returns the complete calls in index order.
func (a *toolCallAccumulator) calls() []ToolCall {
	if len(a.byIndex) == 0 {
		return nil
	}
	idx := make([]int, 0, len(a.byIndex))
	for i := range a.byIndex {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	out := make([]ToolCall, 0, len(idx))
	for _, i := range idx {
		out = append(out, *a.byIndex[i])
	}
	return out
}
