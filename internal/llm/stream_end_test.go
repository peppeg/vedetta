package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A cut answer must not pass for a finished one (external audit 24/11/2026): the
// server says it has failed inside the stream, or the connection closes before the
// end. What was written reaches the caller all the same.
func TestStreamEnd(t *testing.T) {
	text := `data: {"choices":[{"delta":{"content":"prima parte"}}]}` + "\n\n"
	for _, c := range []struct {
		name, body, want string
		err              bool
	}{
		{"whole", text + `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n", "prima parte", false},
		{"no [DONE], but a reason to stop", text + `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n", "prima parte", false},
		{"no reason to stop, but [DONE]", text + "data: [DONE]\n\n", "prima parte", false},
		{"the last line has no line end", text + `data: {"choices":[{"delta":{"content":" e fine"},"finish_reason":"stop"}]}`, "prima parte e fine", false},
		{"[DONE] without a line end", text + "data: [DONE]", "prima parte", false},
		{"an empty error field is no error", `data: {"error":null,"choices":[{"delta":{"content":"prima parte"}}]}` + "\n\ndata: [DONE]\n\n", "prima parte", false},
		{"the connection closes", text, "prima parte", true},
		{"an error object", text + `data: {"error":{"message":"upstream died","code":502}}` + "\n\ndata: [DONE]\n\n", "prima parte", true},
		{"an error string", text + `data: {"error":"upstream died"}` + "\n\n", "prima parte", true},
		{"an error with a choice", text + `data: {"error":{"code":502},"choices":[{"delta":{"content":""},"finish_reason":"error"}]}` + "\n\ndata: [DONE]\n\n", "prima parte", true},
		{"finish_reason error", text + `data: {"choices":[{"delta":{},"finish_reason":"error"}]}` + "\n\ndata: [DONE]\n\n", "prima parte", true},
		{"the text held for a half tag is not lost", `data: {"choices":[{"delta":{"content":"prima parte <th"}}]}` + "\n\n", "prima parte <th", true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, c.body)
		}))
		var got strings.Builder
		err := NewClient(srv.URL, "", "m").StreamChat(context.Background(), []Message{{Role: "user", Content: "q"}}, func(tok StreamToken) error {
			if tok.Type == TokenContent {
				got.WriteString(tok.Text)
			}
			return nil
		})
		srv.Close()
		if (err != nil) != c.err || got.String() != c.want {
			t.Errorf("%s: got %q err %v, want %q err %t", c.name, got.String(), err, c.want, c.err)
		}
	}
}

// Tool calls of a stream that did not finish are not run.
func TestStreamEnd_NoCallsFromACutStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"read","arguments":"{\"url\":\"https://x"}}]}}]}`+"\n\n")
	}))
	defer srv.Close()
	err := NewClient(srv.URL, "", "m").StreamChat(context.Background(), []Message{{Role: "user", Content: "q"}}, func(tok StreamToken) error {
		if tok.Type == TokenToolCalls {
			t.Errorf("calls of a cut stream reached the caller: %+v", tok.ToolCalls)
		}
		return nil
	})
	if err == nil {
		t.Error("a cut stream gave no error")
	}
}

// A conversation that does not fit, said inside the stream, is told apart as before it.
func TestStreamEnd_ContextSaidInTheStream(t *testing.T) {
	ForgetLimits()
	defer ForgetLimits()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `data: {"error":{"code":400,"message":"the request exceeds the available context size, try increasing it","type":"exceed_context_size_error","n_prompt_tokens":9000,"n_ctx":8192}}`+"\n\n")
	}))
	defer srv.Close()
	err := NewClient(srv.URL, "", "m").StreamChat(context.Background(), []Message{{Role: "user", Content: "q"}}, func(StreamToken) error { return nil })
	var ce *ContextError
	if !errors.As(err, &ce) {
		t.Fatalf("got %v, want a ContextError", err)
	}
	if ce.Context != 8192 || ce.Prompt != 9000 {
		t.Errorf("numbers read: context=%d prompt=%d, want 8192 and 9000", ce.Context, ce.Prompt)
	}
}
