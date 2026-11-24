package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeServer answers /models with modelsBody (404 when empty) and records the
// max_tokens of every chat request. refuse makes it answer 400 to requests
// that carry max_tokens; chunk is the SSE body of a chat answer.
type fakeServer struct {
	mu         sync.Mutex
	modelsBody string
	refuse     bool
	chunk      string
	maxTokens  []interface{}
	modelsHits int
}

func (f *fakeServer) handler(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/props") {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/models") {
		f.mu.Lock()
		f.modelsHits++
		f.mu.Unlock()
		if f.modelsBody == "" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(f.modelsBody))
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req map[string]interface{}
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	f.maxTokens = append(f.maxTokens, req["max_tokens"])
	f.mu.Unlock()
	if _, ok := req["max_tokens"]; ok && f.refuse {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"unsupported parameter: max_tokens"}`))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := f.chunk
	if chunk == "" {
		chunk = "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\n"
	}
	_, _ = w.Write([]byte(chunk + "data: [DONE]\n\n"))
}

func chatTokens(t *testing.T, c *Client) []StreamToken {
	t.Helper()
	var toks []StreamToken
	err := c.StreamChatWithOptions(context.Background(), []Message{{Role: "user", Content: "hi"}}, ChatOptions{}, func(tok StreamToken) error {
		toks = append(toks, tok)
		return nil
	})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	return toks
}

const halogenModels = `{"object":"list","data":[{"id":"other","max_tokens_cap":1000},{"id":"m","max_model_len":262144,"max_tokens_cap":65536,"max_tokens_default":8192}]}`

func TestMaxTokens_AutoUsesModelOutputCap(t *testing.T) {
	f := &fakeServer{modelsBody: halogenModels}
	ForgetLimits()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	c := NewClient(srv.URL, "", "m").WithMaxTokens(0)
	chatTokens(t, c)
	chatTokens(t, c)
	if len(f.maxTokens) != 2 || f.maxTokens[0] != float64(65536) || f.maxTokens[1] != float64(65536) {
		t.Fatalf("max_tokens = %v, want 65536 twice", f.maxTokens)
	}
	if f.modelsHits != 1 {
		t.Fatalf("/models asked %d times, want once", f.modelsHits)
	}
}

func TestMaxTokens_AutoOmittedWhenNotStated(t *testing.T) {
	for name, body := range map[string]string{
		"no models endpoint":  "",
		"context length only": `{"data":[{"id":"m","max_model_len":262144}]}`,
		"model not listed":    `{"data":[{"id":"x","max_tokens_cap":4096}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeServer{modelsBody: body}
			ForgetLimits()
			srv := httptest.NewServer(http.HandlerFunc(f.handler))
			defer srv.Close()
			chatTokens(t, NewClient(srv.URL, "", "m").WithMaxTokens(0))
			if f.maxTokens[0] != nil {
				t.Fatalf("max_tokens = %v, want omitted", f.maxTokens[0])
			}
		})
	}
}

func TestMaxTokens_ExplicitAndNever(t *testing.T) {
	f := &fakeServer{modelsBody: halogenModels}
	ForgetLimits()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	chatTokens(t, NewClient(srv.URL, "", "m").WithMaxTokens(20000))
	chatTokens(t, NewClient(srv.URL, "", "m").WithMaxTokens(-1))
	if f.maxTokens[0] != float64(20000) || f.maxTokens[1] != nil {
		t.Fatalf("max_tokens = %v, want [20000 omitted]", f.maxTokens)
	}
	// An explicit max_tokens must still fit in the memory: /models is asked once
	// for it (DL3.3). A client that never sends max_tokens does not ask.
	if f.modelsHits != 1 {
		t.Fatalf("/models asked %d times, want once", f.modelsHits)
	}
}

func TestMaxTokens_RefusedIsOmittedFromThenOn(t *testing.T) {
	f := &fakeServer{modelsBody: halogenModels, refuse: true}
	ForgetLimits()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	c := NewClient(srv.URL, "", "m").WithMaxTokens(0)
	chatTokens(t, c)
	chatTokens(t, c)
	want := []interface{}{float64(65536), nil, nil}
	if len(f.maxTokens) != 3 {
		t.Fatalf("requests = %v, want %v", f.maxTokens, want)
	}
	for i := range want {
		if f.maxTokens[i] != want[i] {
			t.Fatalf("requests = %v, want %v", f.maxTokens, want)
		}
	}
}

func TestFinishReasonLength_SendsTruncatedLast(t *testing.T) {
	f := &fakeServer{chunk: "data: {\"choices\":[{\"delta\":{\"content\":\"half an ans\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n"}
	ForgetLimits()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	toks := chatTokens(t, NewClient(srv.URL, "", "m"))
	if len(toks) == 0 || toks[len(toks)-1].Type != TokenTruncated {
		t.Fatalf("tokens = %+v, want TokenTruncated last", toks)
	}
}

func TestFinishReasonStop_NoTruncated(t *testing.T) {
	f := &fakeServer{}
	ForgetLimits()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	for _, tok := range chatTokens(t, NewClient(srv.URL, "", "m")) {
		if tok.Type == TokenTruncated {
			t.Fatalf("TokenTruncated on a finished answer")
		}
	}
}

func TestMaxTokens_OffUntilAsked(t *testing.T) {
	f := &fakeServer{modelsBody: halogenModels}
	ForgetLimits()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	chatTokens(t, NewClient(srv.URL, "", "m"))
	if f.maxTokens[0] != nil || f.modelsHits != 0 {
		t.Fatalf("max_tokens = %v, /models hits %d: want omitted and never asked", f.maxTokens[0], f.modelsHits)
	}
}

func TestMaxTokens_ServerDownAsksAgain(t *testing.T) {
	f := &fakeServer{}
	down := true
	ForgetLimits()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") && down {
			f.mu.Lock()
			f.modelsHits++
			f.mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		f.handler(w, r)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "", "m").WithMaxTokens(0)
	chatTokens(t, c)
	down = false
	f.modelsBody = halogenModels
	chatTokens(t, c)
	if f.maxTokens[0] != nil || f.maxTokens[1] != float64(65536) {
		t.Fatalf("max_tokens = %v, want [omitted 65536]", f.maxTokens)
	}
}

func TestParseOutputCap_OpenRouterShape(t *testing.T) {
	body := `{"data":[{"id":"deepseek/v4","context_length":163840,"top_provider":{"max_completion_tokens":32768}}]}`
	if got := parseOutputCap([]byte(body), "deepseek/v4"); got != 32768 {
		t.Fatalf("cap = %d, want 32768", got)
	}
}
