package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The stream of a halogen answer that calls two tools (probe of 29/10/2026):
// id and name come first, the arguments in pieces, finish_reason "tool_calls".
func TestStreamChat_ToolCalls(t *testing.T) {
	var got map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range []string{
			`{"choices":[{"delta":{"reasoning_content":"I should search."}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"search","arguments":""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\": \"komodo"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":" release\"}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"read","arguments":"{\"url\":\"https://x.test\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		} {
			_, _ = w.Write([]byte("data: " + l + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	tools := []Tool{{Type: "function", Function: ToolFunction{Name: "search", Description: "Search the web.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`)}}}
	var calls []ToolCall
	var reasoning string
	client := NewClient(server.URL, "", "m")
	err := client.StreamChatWithOptions(context.Background(), []Message{{Role: "user", Content: "q"}}, ChatOptions{Tools: tools},
		func(tok StreamToken) error {
			switch tok.Type {
			case TokenToolCalls:
				calls = tok.ToolCalls
			case TokenReasoning:
				reasoning += tok.Text
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if ts, ok := got["tools"].([]interface{}); !ok || len(ts) != 1 {
		t.Fatalf("tools not sent: %v", got["tools"])
	}
	if reasoning != "I should search." {
		t.Errorf("reasoning = %q", reasoning)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].ID != "call_a" || calls[0].Function.Name != "search" || calls[0].Function.Arguments != `{"query": "komodo release"}` {
		t.Errorf("call 0 = %+v", calls[0])
	}
	if calls[1].ID != "call_b" || calls[1].Function.Name != "read" {
		t.Errorf("call 1 = %+v", calls[1])
	}
}

// A tool result goes back as a "tool" message, the assistant turn keeps its calls.
func TestMessage_ToolRoundTripJSON(t *testing.T) {
	msgs := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: ToolCallFunction{Name: "search", Arguments: `{"query":"x"}`}}}},
		{Role: "tool", ToolCallID: "c1", Content: "results"},
		{Role: "user", Content: "plain"},
	}
	b, _ := json.Marshal(msgs)
	want := `[{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"search","arguments":"{\"query\":\"x\"}"}}]},` +
		`{"role":"tool","content":"results","tool_call_id":"c1"},{"role":"user","content":"plain"}]`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
}
