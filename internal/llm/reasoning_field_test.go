package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// streamReasoning runs one chat against a server that streams lines and returns the
// reasoning tokens it gave.
func streamReasoning(t *testing.T, lines []string) []StreamToken {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range lines {
			_, _ = w.Write([]byte("data: " + l + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	var got []StreamToken
	err := NewClient(server.URL, "", "m").StreamChatWithOptions(context.Background(), []Message{{Role: "user", Content: "q"}}, ChatOptions{},
		func(tok StreamToken) error {
			if tok.Type == TokenReasoning {
				got = append(got, tok)
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// Kilo Gateway streams the model's reasoning in "reasoning", with a copy in
// "reasoning_details" (probe of 12/11/2026, deepseek/deepseek-v4.1-flash-1017):
// it is reasoning to show and save, marked as not to be sent back.
func TestStreamChat_ReasoningFieldOfGateways(t *testing.T) {
	got := streamReasoning(t, []string{
		`{"choices":[{"delta":{"role":"assistant","reasoning":"We","reasoning_details":[{"type":"reasoning.text","text":"We","format":"unknown","index":0}]}}]}`,
		`{"choices":[{"delta":{"reasoning":" need 3h25.","reasoning_details":[{"type":"reasoning.text","text":" need 3h25.","format":"unknown","index":0}]}}]}`,
		`{"choices":[{"delta":{"content":"3 hours 25 minutes."},"finish_reason":"stop"}]}`,
	})
	text := ""
	for _, tok := range got {
		text += tok.Text
		if !tok.NoReturn {
			t.Errorf("reasoning from the \"reasoning\" field must be marked NoReturn: %+v", tok)
		}
	}
	if text != "We need 3h25." {
		t.Errorf("reasoning = %q", text)
	}
}

// "reasoning_content" stays as it was: reasoning the server takes back. A server
// that sends both fields is read once.
func TestStreamChat_ReasoningContentStillReturnable(t *testing.T) {
	got := streamReasoning(t, []string{
		`{"choices":[{"delta":{"reasoning_content":"Plan."}}]}`,
		`{"choices":[{"delta":{"reasoning_content":" Both.","reasoning":" Both."}}]}`,
		`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
	})
	text := ""
	for _, tok := range got {
		text += tok.Text
		if tok.NoReturn {
			t.Errorf("reasoning_content must not be marked NoReturn: %+v", tok)
		}
	}
	if text != "Plan. Both." {
		t.Errorf("reasoning = %q", text)
	}
}
