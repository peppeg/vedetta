package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The model's memory (DL3, docs/SPEC_DL3_MEMORIA_DEL_MODELLO.md). Prompt and
// answer share it: a conversation that does not fit is refused (halogen,
// llama.cpp, vLLM), answered with nothing (Kilo) or cut silently (Ollama).
// Vedetta learns it, in this order, from a setting, the server's /models, the
// llama.cpp /props, a refusal that states it, or a prudent default.

// DefaultContextTokens is the memory assumed when nothing states it (D1).
const DefaultContextTokens = 131072

// ContextInfo is what Vedetta knows about the configured model's memory.
type ContextInfo struct {
	Tokens int // memory in tokens, never 0: DefaultContextTokens when unknown
	// Source: "setting", "models", "props", "refusal" or "default".
	Source string
	// OutputCap is the max_tokens the client sends, 0 when it sends none.
	OutputCap int
}

// Known reports whether the memory was stated rather than assumed.
func (i ContextInfo) Known() bool { return i.Source != "" && i.Source != "default" }

// WithContextTokens sets the model's memory by hand (n > 0); 0 reads it from
// the server. A setting wins over what the server states.
func (c *Client) WithContextTokens(n int) *Client {
	c.capMu.Lock()
	defer c.capMu.Unlock()
	if n < 0 {
		n = 0
	}
	c.contextTokens = n
	return c
}

// ContextInfo returns the model's memory and where it comes from. It may ask
// the server (/models, then /props), at most once per service and model in
// limitsTTL: the answer is shared by every client.
func (c *Client) ContextInfo(ctx context.Context) ContextInfo {
	c.capMu.Lock()
	set, mt, omit := c.contextTokens, c.maxTokens, c.omitMaxTokens
	c.capMu.Unlock()
	lim, _, _ := c.limits(ctx)
	info := ContextInfo{}
	switch {
	case omit || mt < 0:
	case mt > 0:
		info.OutputCap = mt
	default:
		info.OutputCap = lim.OutputCap
	}
	if set > 0 {
		info.Tokens, info.Source = set, "setting"
		return info
	}
	if n, ok := learnedContext(c.limitsKey()); ok && (lim.Context == 0 || n < lim.Context) {
		info.Tokens, info.Source = n, "refusal"
		return info
	}
	if lim.Context > 0 {
		info.Tokens, info.Source = lim.Context, lim.Source
		return info
	}
	info.Tokens, info.Source = DefaultContextTokens, "default"
	if lim.Ceiling > 0 && lim.Ceiling < info.Tokens {
		info.Tokens = lim.Ceiling
	}
	return info
}

// BytesPerToken turns the conversation's bytes into tokens, prudently: on dense
// Italian with tables halogen, Kilo's Nemotron and gpt-oss count 3.2–3.4 bytes
// per token, on web pages halogen 4.1 (DL3.0, 09/11/2026). Code, numbers and
// other scripts can cost more: a refusal covers them (ContextError).
const BytesPerToken = 3

// ContextMargin is the memory kept for the chat template and the tool
// definitions, in tokens.
const ContextMargin = 2000

// minMaxTokens: below this a request is let through as is; the server
// refuses it or answers short, either way the caller learns of it.
const minMaxTokens = 1024

// RequestChars is the size of what a request sends the model, in bytes:
// texts, reasoning, tool calls and tool definitions.
func RequestChars(messages []Message, tools []Tool) int {
	n := 0
	for _, m := range messages {
		n += len(m.Content) + len(m.ReasoningContent)
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	if len(tools) > 0 {
		if b, err := json.Marshal(tools); err == nil {
			n += len(b)
		}
	}
	return n
}

// fitMaxTokens lowers max_tokens to what the model's memory leaves beside the
// request: prompt and answer share it, and halogen or vLLM refuse a request
// whose sum does not fit. Only with a stated memory: an assumed one would cut
// the answer on servers that hold more.
func (c *Client) fitMaxTokens(ctx context.Context, n int, messages []Message, tools []Tool) int {
	info := c.ContextInfo(ctx)
	if !info.Known() {
		return n
	}
	room := info.Tokens - RequestChars(messages, tools)/BytesPerToken - ContextMargin
	if room < minMaxTokens {
		room = minMaxTokens
	}
	if room >= n {
		return n
	}
	log.Printf("[llm] model=%s max_tokens %d lowered to %d: the memory (%d, %s) holds the request beside it", c.model, n, room, info.Tokens, info.Source)
	return room
}

// modelLimits is what a service states about one model.
type modelLimits struct {
	Context int    // memory the model is loaded with, 0 when not stated
	Source  string // "models" or "props"
	// Ceiling is the most the model can hold (llama.cpp n_ctx_train, LM Studio
	// max_context_length): not what it is loaded with, so only a ceiling.
	Ceiling   int
	OutputCap int // most tokens it may write in one answer, 0 when not stated
}

// limitsTTL: a model reloaded with another memory (Lemonade) is read again soon.
const limitsTTL = 10 * time.Minute

// learnedTTL: how long a memory learned from a refusal is kept.
const learnedTTL = time.Hour

type limitsEntry struct {
	lim modelLimits
	at  time.Time
}

type learnedEntry struct {
	tokens int
	at     time.Time
}

var (
	limitsMu    sync.Mutex
	limitsCache = map[string]limitsEntry{}
	learnedMem  = map[string]learnedEntry{}
	limitsNow   = time.Now // replaced in tests
)

// ForgetLimits drops what services stated and what refusals taught. Tests
// call it: a closed test server's port can come back for the next one.
func ForgetLimits() {
	limitsMu.Lock()
	defer limitsMu.Unlock()
	limitsCache = map[string]limitsEntry{}
	learnedMem = map[string]learnedEntry{}
}

func (c *Client) limitsKey() string {
	return c.baseURL + "\x00" + strings.ToLower(c.model)
}

// limits returns what the service states about the model, from the shared
// cache or from /models (and /props when /models does not state the memory).
// final is false when the answer may change on a later try (no connection,
// 5xx, 429): such answers are not kept.
func (c *Client) limits(ctx context.Context) (modelLimits, bool, error) {
	key := c.limitsKey()
	limitsMu.Lock()
	if e, ok := limitsCache[key]; ok && limitsNow().Sub(e.at) < limitsTTL {
		limitsMu.Unlock()
		return e.lim, true, nil
	}
	limitsMu.Unlock()

	// Outside the lock: a slow /models must not block other calls (they may ask
	// too, the answer is the same).
	body, final, err := c.getJSON(ctx, "/models")
	if err != nil {
		if final {
			log.Printf("[llm] model=%s /models unusable (%v): memory and output cap not stated", c.model, err)
			c.storeLimits(key, modelLimits{})
		}
		return modelLimits{}, final, err
	}
	lim := parseModelLimits(body, c.model)
	if lim.Context > 0 {
		lim.Source = "models"
	} else if props, _, perr := c.getJSON(ctx, "/props"); perr == nil {
		if n := parsePropsContext(props); n > 0 {
			lim.Context, lim.Source = n, "props"
		}
	}
	log.Printf("[llm] model=%s memory=%d (%s) ceiling=%d output_cap=%d", c.model, lim.Context, orNone(lim.Source), lim.Ceiling, lim.OutputCap)
	c.storeLimits(key, lim)
	return lim, true, nil
}

func orNone(s string) string {
	if s == "" {
		return "not stated"
	}
	return s
}

func (c *Client) storeLimits(key string, lim modelLimits) {
	limitsMu.Lock()
	defer limitsMu.Unlock()
	limitsCache[key] = limitsEntry{lim: lim, at: limitsNow()}
}

// getJSON GETs path from the service with a short deadline. /props is
// llama.cpp's and sits at the server root, outside /v1.
func (c *Client) getJSON(ctx context.Context, path string) ([]byte, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	base := c.baseURL
	if path == "/props" {
		base = strings.TrimSuffix(base, "/v1")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, true, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, modelListMaxBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		final := resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests
		return nil, final, fmt.Errorf("%s returned status %d", path, resp.StatusCode)
	}
	return body, true, nil
}

// parseModelLimits finds model in a /models answer (OpenAI or Ollama shape).
// Memory, in order: OpenRouter/Kilo top_provider.context_length and
// context_length, vLLM max_model_len, meta.n_ctx (halogen), LM Studio
// loaded_context_length. Output cap: max_tokens_cap (halogen),
// max_completion_tokens, max_output_tokens, top_provider.max_completion_tokens.
func parseModelLimits(body []byte, model string) modelLimits {
	type entry struct {
		ID                  string `json:"id"`
		Name                string `json:"name"`
		ContextLength       int    `json:"context_length"`
		MaxModelLen         int    `json:"max_model_len"`
		LoadedContextLength int    `json:"loaded_context_length"`
		MaxContextLength    int    `json:"max_context_length"`
		MaxTokensCap        int    `json:"max_tokens_cap"`
		MaxCompletionTokens int    `json:"max_completion_tokens"`
		MaxOutputTokens     int    `json:"max_output_tokens"`
		Meta                struct {
			NCtx      int `json:"n_ctx"`
			NCtxTrain int `json:"n_ctx_train"`
		} `json:"meta"`
		TopProvider struct {
			ContextLength       int `json:"context_length"`
			MaxCompletionTokens int `json:"max_completion_tokens"`
		} `json:"top_provider"`
	}
	var payload struct {
		Data   []entry `json:"data"`
		Models []entry `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return modelLimits{}
	}
	for _, e := range append(payload.Data, payload.Models...) {
		if !strings.EqualFold(e.ID, model) && !strings.EqualFold(e.Name, model) {
			continue
		}
		return modelLimits{
			Context:   firstPositive(e.TopProvider.ContextLength, e.ContextLength, e.MaxModelLen, e.Meta.NCtx, e.LoadedContextLength),
			Ceiling:   firstPositive(e.Meta.NCtxTrain, e.MaxContextLength),
			OutputCap: firstPositive(e.MaxTokensCap, e.MaxCompletionTokens, e.MaxOutputTokens, e.TopProvider.MaxCompletionTokens),
		}
	}
	return modelLimits{}
}

// parsePropsContext reads the memory of a running llama.cpp server: the
// context of one slot, which is what one request gets.
func parsePropsContext(body []byte) int {
	var p struct {
		NCtx     int `json:"n_ctx"`
		Settings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return 0
	}
	return firstPositive(p.Settings.NCtx, p.NCtx)
}

// learnedContext returns a memory learned from a refusal, while fresh.
func learnedContext(key string) (int, bool) {
	limitsMu.Lock()
	defer limitsMu.Unlock()
	e, ok := learnedMem[key]
	if !ok || limitsNow().Sub(e.at) >= learnedTTL {
		return 0, false
	}
	return e.tokens, true
}

// learnContext records the memory a refusal stated; a smaller one wins.
func learnContext(key string, tokens int) {
	if tokens <= 0 {
		return
	}
	limitsMu.Lock()
	defer limitsMu.Unlock()
	if e, ok := learnedMem[key]; ok && limitsNow().Sub(e.at) < learnedTTL && e.tokens <= tokens {
		return
	}
	learnedMem[key] = learnedEntry{tokens: tokens, at: limitsNow()}
}

func firstPositive(ns ...int) int {
	for _, n := range ns {
		if n > 0 {
			return n
		}
	}
	return 0
}

// ErrContextExceeded: the conversation does not fit in the model's memory.
var ErrContextExceeded = errors.New("the conversation does not fit in the model's memory")

// ContextError is a server's refusal of a conversation that does not fit in
// the model's memory, or, Silent, Kilo's empty answer in its place
// (finish_reason "other", nothing written; DL3.0, 09/11/2026).
type ContextError struct {
	Context int          // the memory the server stated, 0 when it did not
	Prompt  int          // the prompt's tokens the server counted, 0 when it did not
	Silent  bool         // no refusal: the server closed without writing anything
	Status  *StatusError // the refusal; nil when Silent
}

func (e *ContextError) Error() string {
	if e.Silent {
		return "the server closed without writing anything (finish_reason other): probably the conversation does not fit in the model's memory"
	}
	return "the conversation does not fit in the model's memory: " + e.Status.Error()
}

func (e *ContextError) Is(target error) bool { return target == ErrContextExceeded }

func (e *ContextError) Unwrap() error {
	if e.Status == nil {
		return nil
	}
	return e.Status
}

// contextMarkers are the words of a refusal for a conversation too long:
// halogen "does not fit", llama.cpp exceed_context_size_error, vLLM, OpenAI
// and OpenRouter "maximum context length", Anthropic "prompt is too long".
var contextMarkers = []string{
	"does not fit", "exceed_context_size", "exceeds the available context",
	"maximum context length", "context length", "context_length_exceeded",
	"context size", "context window", "prompt is too long", "too many tokens",
}

var (
	contextNumberRe = regexp.MustCompile(`(?i)(?:the context is|maximum context length is|context length is|context size is|context window is)\s*(\d[\d,]*)`)
	promptNumberRe  = regexp.MustCompile(`(?i)(?:prompt is|it is|resulted in|messages resulted in)\s*(\d[\d,]*)\s*tokens`)
)

// contextRefusal recognizes a refusal for a conversation that does not fit,
// with the numbers it states.
func contextRefusal(code int, body string) (*ContextError, bool) {
	if code != http.StatusBadRequest && code != http.StatusRequestEntityTooLarge {
		return nil, false
	}
	low := strings.ToLower(body)
	found := false
	for _, m := range contextMarkers {
		if strings.Contains(low, m) {
			found = true
			break
		}
	}
	if !found {
		return nil, false
	}
	ce := &ContextError{Status: &StatusError{Code: code, Body: body}}
	// llama.cpp states the numbers as fields of the error.
	var j struct {
		Error struct {
			NCtx    int `json:"n_ctx"`
			NPrompt int `json:"n_prompt_tokens"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &j) == nil {
		ce.Context, ce.Prompt = j.Error.NCtx, j.Error.NPrompt
	}
	if ce.Context == 0 {
		ce.Context = firstNumber(contextNumberRe, body)
	}
	if ce.Prompt == 0 {
		ce.Prompt = firstNumber(promptNumberRe, body)
	}
	return ce, true
}

func firstNumber(re *regexp.Regexp, s string) int {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
	if err != nil {
		return 0
	}
	return n
}
