package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newEffortServer returns a mock chat server accepting the given reasoning_effort
// values (plus the empty/omitted value when acceptEmpty is true), and records the
// sequence of received values.
func newEffortServer(accept map[string]bool, acceptEmpty bool) (*httptest.Server, *[]string) {
	got := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		v, _ := req["reasoning_effort"].(string)
		*got = append(*got, v)
		if accept[v] || (v == "" && acceptEmpty) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"reasoning_effort '` + v + `' unsupported; use minimal|low|medium|high|xhigh"}}`))
	}))
	return srv, got
}

func drain(t *testing.T, c *Client, eff Effort) error {
	return c.StreamChatWithOptions(context.Background(),
		[]Message{{Role: "user", Content: "hi"}},
		ChatOptions{Temperature: Float64Ptr(0.1), Effort: eff},
		func(token StreamToken) error { return nil })
}

func TestEffort_RetryOnUnsupportedNone(t *testing.T) {
	// halogen-like server: "none" rejected, "minimal" accepted.
	srv, got := newEffortServer(map[string]bool{"minimal": true}, false)
	defer srv.Close()
	c := NewClient(srv.URL, "", "qwen-test")

	if err := drain(t, c, EffortOff); err != nil {
		t.Fatalf("expected self-healing retry to succeed, got %v", err)
	}
	want := []string{"none", "minimal"}
	if len(*got) != len(want) || (*got)[0] != want[0] || (*got)[1] != want[1] {
		t.Fatalf("expected requests %v, got %v", want, *got)
	}
}

func TestEffort_FallsBackToOmitted(t *testing.T) {
	// Server rejects every explicit value but accepts the field omitted.
	srv, got := newEffortServer(map[string]bool{}, true)
	defer srv.Close()
	c := NewClient(srv.URL, "", "any")

	if err := drain(t, c, EffortOff); err != nil {
		t.Fatalf("expected fallback-to-omit to succeed, got %v", err)
	}
	last := (*got)[len(*got)-1]
	if last != "" {
		t.Fatalf("expected final request to omit reasoning_effort, got %q", last)
	}
}

func TestEffort_Probe(t *testing.T) {
	srv, _ := newEffortServer(map[string]bool{"minimal": true, "high": true}, false)
	defer srv.Close()
	c := NewClient(srv.URL, "", "qwen-test")

	accepted, err := c.ProbeEffort(context.Background())
	if err != nil {
		t.Fatalf("probe error: %v", err)
	}
	if len(accepted) != 2 || accepted[0] != "minimal" || accepted[1] != "high" {
		t.Fatalf("expected [minimal high], got %v", accepted)
	}
}

func TestEffort_DefaultOmitsField(t *testing.T) {
	srv, got := newEffortServer(map[string]bool{}, true)
	defer srv.Close()
	c := NewClient(srv.URL, "", "any")

	if err := drain(t, c, EffortDefault); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*got) != 1 || (*got)[0] != "" {
		t.Fatalf("EffortDefault must omit the field on the first request, got %v", *got)
	}
}
