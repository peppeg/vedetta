package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// limitsServer answers /models and /props with the given bodies (404 when
// empty) and counts the calls.
type limitsServer struct {
	mu         sync.Mutex
	models     string
	props      string
	modelsHits int
	propsHits  int
}

func (s *limitsServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	ForgetLimits()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		body := ""
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			s.modelsHits++
			body = s.models
		case r.URL.Path == "/props":
			s.propsHits++
			body = s.props
		}
		if body == "" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestContextInfo_RealAnswers(t *testing.T) {
	for _, tc := range []struct {
		name, body, model string
		want              ContextInfo
	}{
		{"halogen", fixture(t, "halogen_models_2026-11-09.json"), "halogen-qwen3.8-flash-next", ContextInfo{262144, "models", 65536}},
		{"kilo gpt-oss", fixture(t, "kilo_models_2026-11-09.json"), "openai/gpt-oss-120b", ContextInfo{131072, "models", 65536}},
		{"kilo lightning", fixture(t, "kilo_models_2026-11-09.json"), "nvidia/nemotron-3.5-lightning", ContextInfo{262144, "models", 235929}},
		{"vllm", `{"data":[{"id":"m","max_model_len":32768}]}`, "m", ContextInfo{32768, "models", 0}},
		{"lm studio", `{"data":[{"id":"m","loaded_context_length":8192,"max_context_length":131072}]}`, "m", ContextInfo{8192, "models", 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &limitsServer{models: tc.body}
			srv := s.start(t)
			got := NewClient(srv.URL+"/v1", "", tc.model).WithMaxTokens(0).ContextInfo(context.Background())
			if got != tc.want {
				t.Fatalf("ContextInfo = %+v, want %+v", got, tc.want)
			}
			if s.propsHits != 0 {
				t.Fatalf("/props asked %d times, want never when /models states the memory", s.propsHits)
			}
		})
	}
}

func TestContextInfo_LlamaCppProps(t *testing.T) {
	// llama.cpp /v1/models states only the training memory: /props has the loaded one.
	s := &limitsServer{
		models: `{"data":[{"id":"m","meta":{"n_ctx_train":262144}}]}`,
		props:  `{"default_generation_settings":{"n_ctx":32768},"total_slots":1}`,
	}
	srv := s.start(t)
	got := NewClient(srv.URL+"/v1", "", "m").ContextInfo(context.Background())
	if got.Tokens != 32768 || got.Source != "props" {
		t.Fatalf("ContextInfo = %+v, want 32768 from props", got)
	}
}

func TestContextInfo_DefaultWhenNotStated(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"nothing stated", `{"data":[{"id":"m"}]}`, DefaultContextTokens},
		{"no models endpoint", "", DefaultContextTokens},
		{"training memory above the default", `{"data":[{"id":"m","meta":{"n_ctx_train":262144}}]}`, DefaultContextTokens},
		{"training memory below the default", `{"data":[{"id":"m","meta":{"n_ctx_train":65536}}]}`, 65536},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &limitsServer{models: tc.body}
			srv := s.start(t)
			got := NewClient(srv.URL+"/v1", "", "m").ContextInfo(context.Background())
			if got.Tokens != tc.want || got.Source != "default" || got.Known() {
				t.Fatalf("ContextInfo = %+v, want %d from default", got, tc.want)
			}
		})
	}
}

func TestContextInfo_SettingWins(t *testing.T) {
	s := &limitsServer{models: fixture(t, "halogen_models_2026-11-09.json")}
	srv := s.start(t)
	got := NewClient(srv.URL+"/v1", "", "halogen-qwen3.8-flash-next").WithMaxTokens(0).WithContextTokens(65536).ContextInfo(context.Background())
	if got.Tokens != 65536 || got.Source != "setting" || got.OutputCap != 65536 {
		t.Fatalf("ContextInfo = %+v, want 65536 from setting, output cap 65536", got)
	}
}

func TestContextInfo_OutputCapFollowsMaxTokens(t *testing.T) {
	s := &limitsServer{models: fixture(t, "halogen_models_2026-11-09.json")}
	srv := s.start(t)
	for _, tc := range []struct {
		n, want int
	}{{0, 65536}, {20000, 20000}, {-1, 0}} {
		got := NewClient(srv.URL+"/v1", "", "halogen-qwen3.8-flash-next").WithMaxTokens(tc.n).ContextInfo(context.Background())
		if got.OutputCap != tc.want {
			t.Fatalf("WithMaxTokens(%d): output cap %d, want %d", tc.n, got.OutputCap, tc.want)
		}
	}
}

func TestLimits_SharedAndRereadAfterTTL(t *testing.T) {
	now := time.Now()
	limitsNow = func() time.Time { return now }
	defer func() { limitsNow = time.Now }()
	s := &limitsServer{models: `{"data":[{"id":"m","max_model_len":32768}]}`}
	srv := s.start(t)
	// Two clients for the same service and model: one /models call.
	NewClient(srv.URL+"/v1", "", "m").ContextInfo(context.Background())
	NewClient(srv.URL+"/v1", "", "M").WithMaxTokens(0).ContextInfo(context.Background())
	if s.modelsHits != 1 {
		t.Fatalf("/models asked %d times, want once", s.modelsHits)
	}
	// The model is reloaded with more memory: read again after the TTL.
	s.mu.Lock()
	s.models = `{"data":[{"id":"m","max_model_len":65536}]}`
	s.mu.Unlock()
	now = now.Add(limitsTTL + time.Second)
	if got := NewClient(srv.URL+"/v1", "", "m").ContextInfo(context.Background()); got.Tokens != 65536 || s.modelsHits != 2 {
		t.Fatalf("after the TTL: %+v, /models hits %d; want 65536 and 2 hits", got, s.modelsHits)
	}
}

func TestContextInfo_LearnedFromRefusal(t *testing.T) {
	now := time.Now()
	limitsNow = func() time.Time { return now }
	defer func() { limitsNow = time.Now }()
	s := &limitsServer{models: `{"data":[{"id":"m","max_model_len":131072}]}`}
	srv := s.start(t)
	c := NewClient(srv.URL+"/v1", "", "m")
	learnContext(c.limitsKey(), 200000) // larger than stated: ignored
	if got := c.ContextInfo(context.Background()); got.Tokens != 131072 || got.Source != "models" {
		t.Fatalf("ContextInfo = %+v, want 131072 from models", got)
	}
	learnContext(c.limitsKey(), 32768)
	if got := c.ContextInfo(context.Background()); got.Tokens != 32768 || got.Source != "refusal" {
		t.Fatalf("ContextInfo = %+v, want 32768 from refusal", got)
	}
	now = now.Add(learnedTTL + time.Second)
	if got := c.ContextInfo(context.Background()); got.Tokens != 131072 {
		t.Fatalf("after an hour: %+v, want the stated 131072 again", got)
	}
}

// sentMaxTokens sends one request of size bytes and returns the max_tokens the
// server received (0 when omitted).
func sentMaxTokens(t *testing.T, models string, set, size int) int {
	t.Helper()
	f := &fakeServer{modelsBody: models}
	ForgetLimits()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	c := NewClient(srv.URL, "", "m").WithMaxTokens(0).WithContextTokens(set)
	msgs := []Message{{Role: "user", Content: strings.Repeat("a", size)}}
	if err := c.StreamChatWithOptions(context.Background(), msgs, ChatOptions{}, func(StreamToken) error { return nil }); err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if v, ok := f.maxTokens[0].(float64); ok {
		return int(v)
	}
	return 0
}

func TestFitMaxTokens(t *testing.T) {
	halogen := `{"data":[{"id":"m","max_model_len":262144,"max_tokens_cap":65536}]}`
	gptoss := `{"data":[{"id":"m","context_length":131072,"top_provider":{"context_length":131072,"max_completion_tokens":65536}}]}`
	kiloLightning := `{"data":[{"id":"m","context_length":262144,"top_provider":{"context_length":262144,"max_completion_tokens":235929}}]}`
	unstated := `{"data":[{"id":"m","max_tokens_cap":65536}]}`
	for _, tc := range []struct {
		name         string
		models       string
		set, size    int
		wantMaxToken int
	}{
		{"gpt-oss, 300,000 bytes", gptoss, 0, 300000, 131072 - 100000 - 2000},
		{"halogen at the Deep limit keeps its cap", halogen, 0, 583824, 65536},
		{"halogen small request", halogen, 0, 1000, 65536},
		{"Kilo cap of 90% of the memory", kiloLightning, 0, 100000, 262144 - 100000/3 - 2000},
		{"memory not stated: cap as is", unstated, 0, 300000, 65536},
		{"memory set by hand", unstated, 131072, 300000, 131072 - 100000 - 2000},
		{"no room left: floor", gptoss, 0, 400000, minMaxTokens},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sentMaxTokens(t, tc.models, tc.set, tc.size); got != tc.wantMaxToken {
				t.Fatalf("max_tokens = %d, want %d", got, tc.wantMaxToken)
			}
		})
	}
}

// Real refusals of 09/11/2026 (docs/dl3_ricognizione_2026-11-09/), and the
// formats of llama.cpp, vLLM and OpenRouter as their sources write them.
const (
	halogenOverContext = `{"error":{"message":"the prompt does not fit: it is 390915 tokens and the context is 262144, 128771 tokens over. Shorten the prompt, or raise the context (HALOGEN_CTX, and HALOGEN_ROPE_YARN for more than the native 262144).","type":"invalid_request_error","param":null,"code":null}}`
	halogenMaxTokens   = `{"error":{"message":"max_tokens 65536 does not fit: prompt is 258494 tokens and the context is 262144, leaving room for 3650.","type":"invalid_request_error","param":null,"code":null}}`
	llamaCppOver       = `{"error":{"code":400,"message":"the request exceeds the available context size, try increasing it","type":"exceed_context_size_error","n_prompt_tokens":5021,"n_ctx":4096}}`
	vllmOver           = `{"object":"error","message":"This model's maximum context length is 32768 tokens. However, you requested 40000 tokens (35000 in the messages, 5000 in the completion). Please reduce the length of the messages or completion.","type":"BadRequestError","param":null,"code":400}`
	openRouterOver     = `{"error":{"message":"This endpoint's maximum context length is 131072 tokens. However, you requested about 180322 tokens (180306 of text input, 16 in the output). Please reduce the length of either one, or use the \"middle-out\" transform to compress your prompt automatically.","code":400}}`
	kiloSilentStream   = "data: {\"id\":\"gen_01M6ZP7CKTHF5C985XBQ7GR71E\",\"object\":\"chat.completion.chunk\",\"created\":1794212347,\"model\":\"openai/gpt-oss-120b\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"logprobs\":null,\"finish_reason\":null}],\"system_fingerprint\":\"fp_102abcoxi3\"}\n\n" +
		"data: {\"id\":\"gen_01M6ZP7CKTHF5C985XBQ7GR71E\",\"object\":\"chat.completion.chunk\",\"created\":1794212347,\"model\":\"openai/gpt-oss-120b\",\"choices\":[{\"index\":0,\"delta\":{},\"logprobs\":null,\"finish_reason\":\"other\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2,\"cost\":0.000001}}\n\n"
)

func TestContextRefusal_Recognized(t *testing.T) {
	for _, tc := range []struct {
		name, body      string
		context, prompt int
	}{
		{"halogen over context", halogenOverContext, 262144, 390915},
		{"halogen max_tokens", halogenMaxTokens, 262144, 258494},
		{"llama.cpp", llamaCppOver, 4096, 5021},
		{"vLLM", vllmOver, 32768, 0},
		{"OpenRouter", openRouterOver, 131072, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ce, ok := contextRefusal(http.StatusBadRequest, tc.body)
			if !ok {
				t.Fatalf("not recognized: %s", tc.body)
			}
			if ce.Context != tc.context || ce.Prompt != tc.prompt {
				t.Fatalf("context=%d prompt=%d, want %d and %d", ce.Context, ce.Prompt, tc.context, tc.prompt)
			}
		})
	}
	for _, body := range []string{`{"error":"unsupported parameter: max_tokens"}`, `{"error":"model not found"}`} {
		if _, ok := contextRefusal(http.StatusBadRequest, body); ok {
			t.Fatalf("recognized as a memory refusal: %s", body)
		}
	}
	if _, ok := contextRefusal(http.StatusInternalServerError, halogenOverContext); ok {
		t.Fatalf("a 500 is not a refusal of the conversation")
	}
}

// refusingServer refuses every chat request with status and body, after
// answering /models with models.
func refusingServer(t *testing.T, models, body string, maxTokens *[]interface{}) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	ForgetLimits()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			_, _ = w.Write([]byte(models))
		case strings.HasSuffix(r.URL.Path, "/props"):
			http.NotFound(w, r)
		default:
			var req map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			*maxTokens = append(*maxTokens, req["max_tokens"])
			mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func chatErr(c *Client) error {
	return c.StreamChatWithOptions(context.Background(), []Message{{Role: "user", Content: "hi"}}, ChatOptions{}, func(StreamToken) error { return nil })
}

// The trap of 09/11: halogen's refusal names max_tokens, and was taken for a
// refusal of the field, dropped until the restart.
func TestContextRefusal_KeepsMaxTokens(t *testing.T) {
	var sent []interface{}
	srv := refusingServer(t, `{"data":[{"id":"m","max_model_len":262144,"max_tokens_cap":65536}]}`, halogenMaxTokens, &sent)
	c := NewClient(srv.URL+"/v1", "", "m").WithMaxTokens(0)
	for i := 0; i < 2; i++ {
		err := chatErr(c)
		if !errors.Is(err, ErrContextExceeded) {
			t.Fatalf("err = %v, want ErrContextExceeded", err)
		}
		var se *StatusError
		if !errors.As(err, &se) || se.Code != http.StatusBadRequest {
			t.Fatalf("err = %v, want it to carry the 400", err)
		}
	}
	if len(sent) != 2 || sent[0] != float64(65536) || sent[1] != float64(65536) {
		t.Fatalf("max_tokens sent = %v, want 65536 twice and no retry", sent)
	}
}

func TestContextRefusal_LearnsTheMemory(t *testing.T) {
	var sent []interface{}
	srv := refusingServer(t, `{"data":[{"id":"m"}]}`, llamaCppOver, &sent)
	c := NewClient(srv.URL+"/v1", "", "m")
	if before := c.ContextInfo(context.Background()); before.Source != "default" {
		t.Fatalf("before the refusal: %+v, want default", before)
	}
	if err := chatErr(c); !errors.Is(err, ErrContextExceeded) {
		t.Fatalf("err = %v, want ErrContextExceeded", err)
	}
	if got := c.ContextInfo(context.Background()); got.Tokens != 4096 || got.Source != "refusal" {
		t.Fatalf("after the refusal: %+v, want 4096 from refusal", got)
	}
}

func TestKiloSilentEmpty_IsContextError(t *testing.T) {
	f := &fakeServer{chunk: kiloSilentStream}
	ForgetLimits()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	err := chatErr(NewClient(srv.URL, "", "openai/gpt-oss-120b"))
	var ce *ContextError
	if !errors.As(err, &ce) || !ce.Silent || !errors.Is(err, ErrContextExceeded) {
		t.Fatalf("err = %v, want a silent ContextError", err)
	}
	// finish_reason "other" after some text is not the empty answer.
	f2 := &fakeServer{chunk: "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"other\"}]}\n\n"}
	ForgetLimits()
	srv2 := httptest.NewServer(http.HandlerFunc(f2.handler))
	defer srv2.Close()
	if err := chatErr(NewClient(srv2.URL, "", "m")); err != nil {
		t.Fatalf("err = %v, want nil when the server wrote something", err)
	}
}

func TestParseModelInfo_Context(t *testing.T) {
	got := map[string]int{}
	for _, m := range parseModelInfo([]byte(fixture(t, "kilo_models_2026-11-09.json"))) {
		got[m.ID] = m.Context
	}
	for _, m := range parseModelInfo([]byte(fixture(t, "halogen_models_2026-11-09.json"))) {
		got[m.ID] = m.Context
	}
	for _, m := range parseModelInfo([]byte(`{"data":[{"id":"odd","context_length":"lots"},{"id":"none"}]}`)) {
		got[m.ID] = m.Context
	}
	want := map[string]int{"openai/gpt-oss-120b": 131072, "nvidia/nemotron-3.5-lightning": 262144, "halogen-qwen3.8-flash-next": 262144, "odd": 0, "none": 0}
	for id, n := range want {
		if got[id] != n {
			t.Errorf("%s: context %d, want %d", id, got[id], n)
		}
	}
}
