package llm

import "strings"

type TokenType string

const (
	TokenContent   TokenType = "content"
	TokenReasoning TokenType = "reasoning"
	// TokenTruncated comes once, last, when the server cut the answer at its
	// length limit (finish_reason "length"). It carries no text.
	TokenTruncated TokenType = "truncated"
	// TokenToolCalls comes once, after the text, when the model asked to call
	// tools (tools.go). It carries the calls, not text.
	TokenToolCalls TokenType = "tool_calls"
)

type StreamToken struct {
	Type      TokenType
	Text      string
	ToolCalls []ToolCall
	// NoReturn marks reasoning that came in a gateway's "reasoning" field (Kilo,
	// 12/11/2026), not in "reasoning_content": it is shown and saved, and is not
	// sent back to the model with the next turn.
	NoReturn bool
}

// StreamFilter parses tokens on the fly, correctly separating <think> blocks
// from final content, handling partial tags across chunk boundaries.
type StreamFilter struct {
	inThink bool
	buffer  string
}

func NewStreamFilter() *StreamFilter {
	return &StreamFilter{}
}

func (sf *StreamFilter) Process(text string, isExplicitReasoning bool) []StreamToken {
	if isExplicitReasoning {
		return []StreamToken{{Type: TokenReasoning, Text: text}}
	}

	var tokens []StreamToken
	sf.buffer += text

	for len(sf.buffer) > 0 {
		if !sf.inThink {
			// Look for <think> tag
			idx := strings.Index(sf.buffer, "<think>")
			if idx == -1 {
				// Check for possible partial tag at the end (e.g. "<thi")
				potentialTag := false
				for i := 1; i < len("<think>"); i++ {
					if strings.HasSuffix(sf.buffer, "<think>"[:i]) {
						potentialTag = true
						safeLen := len(sf.buffer) - i
						if safeLen > 0 {
							tokens = append(tokens, StreamToken{Type: TokenContent, Text: sf.buffer[:safeLen]})
							sf.buffer = sf.buffer[safeLen:]
						}
						break
					}
				}
				if !potentialTag {
					tokens = append(tokens, StreamToken{Type: TokenContent, Text: sf.buffer})
					sf.buffer = ""
				}
				break
			}

			// We found <think>
			if idx > 0 {
				tokens = append(tokens, StreamToken{Type: TokenContent, Text: sf.buffer[:idx]})
			}
			sf.inThink = true
			sf.buffer = sf.buffer[idx+len("<think>"):]
		} else {
			// In think mode, look for </think>
			idx := strings.Index(sf.buffer, "</think>")
			if idx == -1 {
				// Check for potential partial closing tag
				potentialTag := false
				for i := 1; i < len("</think>"); i++ {
					if strings.HasSuffix(sf.buffer, "</think>"[:i]) {
						potentialTag = true
						safeLen := len(sf.buffer) - i
						if safeLen > 0 {
							tokens = append(tokens, StreamToken{Type: TokenReasoning, Text: sf.buffer[:safeLen]})
							sf.buffer = sf.buffer[safeLen:]
						}
						break
					}
				}
				if !potentialTag {
					tokens = append(tokens, StreamToken{Type: TokenReasoning, Text: sf.buffer})
					sf.buffer = ""
				}
				break
			}

			// Found </think>
			if idx > 0 {
				tokens = append(tokens, StreamToken{Type: TokenReasoning, Text: sf.buffer[:idx]})
			}
			sf.inThink = false
			sf.buffer = sf.buffer[idx+len("</think>"):]
			// Trim leading whitespace/newlines right after </think>
			sf.buffer = strings.TrimLeft(sf.buffer, "\r\n")
		}
	}

	return tokens
}

func (sf *StreamFilter) Flush() []StreamToken {
	if len(sf.buffer) == 0 {
		return nil
	}
	tt := TokenContent
	if sf.inThink {
		tt = TokenReasoning
	}
	t := []StreamToken{{Type: tt, Text: sf.buffer}}
	sf.buffer = ""
	return t
}
