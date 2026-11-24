package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Tool calls of an assistant turn, and the call a "tool" message answers (tools.go).
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	// The reasoning of an assistant turn, sent back when the model's template keeps
	// it across tool calls (Qwen3.8, DeepSeek V4: docs/APPUNTI_MOTORI_INFERENZA.md).
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type Client struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client

	capMu          sync.Mutex
	effortRejected map[string]bool // physical reasoning_effort values rejected with 400
	omitEffort     bool            // server does not support the field at all

	// Output budget (maxtokens.go): maxTokens is the configured value; with 0
	// the output cap comes from /models (context.go).
	maxTokens     int
	omitMaxTokens bool // server refused max_tokens with 400
	// The model's memory set by hand, 0 = read from the server (context.go).
	contextTokens int

	// T7.1 retries before the response (retry.go). now, sleep and jitter are
	// replaced in tests.
	retry       RetryPolicy
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
	jitter      func() float64
	pauseMu     sync.Mutex
	pausedUntil time.Time    // the server asked this client to wait until then
	pauseErr    *StatusError // the answer that asked for the pause
}

type chatRequest struct {
	Model           string    `json:"model"`
	Messages        []Message `json:"messages"`
	Stream          bool      `json:"stream"`
	Temperature     *float64  `json:"temperature,omitempty"`
	TopP            *float64  `json:"top_p,omitempty"`
	TopK            *int      `json:"top_k,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
	MaxTokens       int       `json:"max_tokens,omitempty"`
	Tools           []Tool    `json:"tools,omitempty"`
}

type ChatOptions struct {
	Temperature *float64
	// Effort is a logical reasoning-effort level; the client maps it to a value the
	// server accepts (or omits the field). See effort.go.
	Effort Effort
	// Tools the model may call; its calls come back as one TokenToolCalls.
	Tools []Tool
	// TopP and TopK are sent only when set. ModelSampling sends no temperature
	// either: the server applies the model's recommended sampling.
	TopP          *float64
	TopK          *int
	ModelSampling bool
}

// Float64Ptr returns a pointer to the given float64 value, useful for setting ChatOptions.Temperature.
func Float64Ptr(v float64) *float64 {
	return &v
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Role             string `json:"role,omitempty"`
			Content          string `json:"content,omitempty"`
			ReasoningContent string `json:"reasoning_content,omitempty"`
			// Reasoning is the same thing as some gateways name it (Kilo sends
			// "reasoning" and a copy in "reasoning_details", never "reasoning_content").
			Reasoning string          `json:"reasoning,omitempty"`
			ToolCalls []toolCallDelta `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason,omitempty"`
	} `json:"choices"`
	// Error is how a server says, after answering 200, that the rest will not come.
	Error json.RawMessage `json:"error,omitempty"`
}

// Model returns the configured model name (read-only introspection).
func (c *Client) Model() string {
	return c.model
}

// BaseURL returns the configured base URL (read-only introspection).
func (c *Client) BaseURL() string {
	return c.baseURL
}

// HasAPIKey reports whether an API key is configured (read-only introspection).
func (c *Client) HasAPIKey() bool {
	return c.apiKey != ""
}

func NewClient(baseURL, apiKey, model string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   15 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 120 * time.Second, // Allow reasoning model prefill time
			},
		},
		maxTokens: -1, // WithMaxTokens turns it on
		retry:     DefaultRetryPolicy(),
		now:       time.Now,
		sleep:     sleepCtx,
		jitter:    defaultJitter,
	}
}

func (c *Client) StreamChat(ctx context.Context, messages []Message, onToken func(token StreamToken) error) error {
	return c.StreamChatWithOptions(ctx, messages, ChatOptions{Temperature: Float64Ptr(0.6)}, onToken)
}

func (c *Client) StreamChatWithOptions(ctx context.Context, messages []Message, opts ChatOptions, onToken func(token StreamToken) error) error {
	endpoint := c.baseURL + "/chat/completions"

	var temp *float64
	if opts.Temperature != nil {
		temp = opts.Temperature
	} else if !opts.ModelSampling {
		defaultTemp := 0.6
		temp = &defaultTemp
	}

	// F3.1/F3.4: map the logical effort to a value the server accepts (or omit it).
	effort, hasEffort := c.selectEffort(opts.Effort)
	maxTokens := c.selectMaxTokens(ctx)
	if maxTokens > 0 {
		maxTokens = c.fitMaxTokens(ctx, maxTokens, messages, opts.Tools)
	}

	reqBody := chatRequest{
		Model:           c.model,
		Messages:        messages,
		Stream:          true,
		Temperature:     temp,
		ReasoningEffort: effort,
		MaxTokens:       maxTokens,
		Tools:           opts.Tools,
		TopP:            opts.TopP,
		TopK:            opts.TopK,
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, attempts, err := c.post(ctx, endpoint, payload)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		snippet := strings.TrimSpace(string(body))
		// F3.2 self-healing: a 400 mentioning reasoning_effort means the value is
		// unsupported. Remember it and retry with the next candidate (or omit it).
		if resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(snippet), "reasoning_effort") && hasEffort {
			c.rejectEffort(effort)
			return c.StreamChatWithOptions(ctx, messages, opts, onToken)
		}
		// A conversation that does not fit in the memory: said apart, and the
		// memory it states is remembered. It must not look like a refusal of
		// max_tokens below: halogen's says "max_tokens 65536 does not fit"
		// (DL3.0), and max_tokens would be dropped until the restart.
		if ce, ok := contextRefusal(resp.StatusCode, snippet); ok {
			ce.Status.Attempts = attempts
			learnContext(c.limitsKey(), ce.Context)
			log.Printf("[llm] model=%s conversation does not fit in the memory (context=%d prompt=%d): %s", c.model, ce.Context, ce.Prompt, snippet)
			return ce
		}
		// Same for max_tokens: a server that refuses it gets requests without it.
		if resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(snippet), "max_tokens") && maxTokens > 0 {
			log.Printf("[llm] model=%s server refused max_tokens=%d (%s): omitted from now on", c.model, maxTokens, snippet)
			c.rejectMaxTokens()
			return c.StreamChatWithOptions(ctx, messages, opts, onToken)
		}
		return &StatusError{Code: resp.StatusCode, Body: snippet, Attempts: attempts}
	}
	defer resp.Body.Close()

	filter := NewStreamFilter()
	reader := bufio.NewReader(resp.Body)
	finishReason := ""
	wrote := false // the server sent text, reasoning or tool calls
	var calls toolCallAccumulator

	done := false       // [DONE], the end of the stream
	var streamErr error // the server said, in the stream, that the rest will not come
	for !done && streamErr == nil {
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			if err == context.Canceled || ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("reading llm stream: %w", err)
		}
		last := err == io.EOF // the last line may have no line end

		data, isData := strings.CutPrefix(strings.TrimSpace(line), "data:")
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			done = true
		}
		var chunk streamChunk
		if !isData || done || json.Unmarshal([]byte(data), &chunk) != nil {
			if last {
				break
			}
			continue
		}
		switch e := string(chunk.Error); e {
		case "", "null", `""`, "{}":
		default:
			streamErr = c.streamError(`{"error":` + e + `}`)
		}

		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta
			if fr := chunk.Choices[0].FinishReason; fr != nil && *fr != "" {
				finishReason = *fr
			}

			// Handle reasoning_content (OpenAI standard for thinking)
			if delta.ReasoningContent != "" {
				tokens := filter.Process(delta.ReasoningContent, true)
				for _, tok := range tokens {
					if err := onToken(tok); err != nil {
						return err
					}
				}
			} else if delta.Reasoning != "" {
				// A gateway's name for it: shown and saved as reasoning, marked as
				// not to be sent back. A server that sends both is read once, above.
				if err := onToken(StreamToken{Type: TokenReasoning, Text: delta.Reasoning, NoReturn: true}); err != nil {
					return err
				}
			}

			calls.add(delta.ToolCalls)
			if delta.Content != "" || delta.ReasoningContent != "" || delta.Reasoning != "" || len(delta.ToolCalls) > 0 {
				wrote = true
			}

			// Handle standard content (which may contain inline <think>)
			if delta.Content != "" {
				tokens := filter.Process(delta.Content, false)
				for _, tok := range tokens {
					if err := onToken(tok); err != nil {
						return err
					}
				}
			}
		}
		if last {
			break
		}
	}

	// Flush any leftover buffer
	for _, tok := range filter.Flush() {
		if err := onToken(tok); err != nil {
			return err
		}
	}

	// Without this a cut answer would pass for a finished one: the server says it has
	// failed, or the connection closes with neither [DONE] nor a reason to stop.
	if streamErr == nil && finishReason == "error" {
		streamErr = c.streamError("finish_reason error")
	}
	if streamErr == nil && !done && finishReason == "" {
		log.Printf("[llm] model=%s the stream ended before the answer did (no finish_reason, no [DONE])", c.model)
		streamErr = fmt.Errorf("reading llm stream: %w", io.ErrUnexpectedEOF)
	}
	if streamErr != nil {
		return streamErr
	}

	if tc := calls.calls(); len(tc) > 0 {
		if err := onToken(StreamToken{Type: TokenToolCalls, ToolCalls: tc}); err != nil {
			return err
		}
	}

	// Kilo answers a conversation that does not fit with nothing and
	// finish_reason "other", status 200 (DL3.0, 09/11/2026).
	if finishReason == "other" && !wrote {
		log.Printf("[llm] model=%s closed without writing anything (finish_reason=other): probably the conversation does not fit in the memory", c.model)
		return &ContextError{Silent: true}
	}

	// The server stopped the answer at its length limit: say it to the caller,
	// which would otherwise take a cut answer for a finished one.
	if finishReason == "length" {
		log.Printf("[llm] model=%s answer cut by the server's length limit (finish_reason=length, max_tokens=%d)", c.model, maxTokens)
		if err := onToken(StreamToken{Type: TokenTruncated}); err != nil {
			return err
		}
	}

	return nil
}

// streamError is an error the server sent inside the stream, after the 200. One that
// says the conversation does not fit is told apart, as before the stream.
func (c *Client) streamError(body string) error {
	body = truncate(body, 4096)
	if ce, ok := contextRefusal(http.StatusBadRequest, body); ok {
		learnContext(c.limitsKey(), ce.Context)
		log.Printf("[llm] model=%s conversation does not fit in the memory, said in the stream (context=%d prompt=%d): %s", c.model, ce.Context, ce.Prompt, body)
		return ce
	}
	body = truncate(body, 400)
	log.Printf("[llm] model=%s error in the stream: %s", c.model, body)
	return fmt.Errorf("llm error in the stream: %s", body)
}

// selectEffort picks the first physical value the server has not rejected.
// Returns ("", false) when the field must be omitted (portable baseline).
func (c *Client) selectEffort(e Effort) (string, bool) {
	c.capMu.Lock()
	defer c.capMu.Unlock()
	if c.omitEffort {
		return "", false
	}
	for _, cand := range effortCandidates(e) {
		if cand == "" {
			return "", false
		}
		if !c.effortRejected[cand] {
			return cand, true
		}
	}
	return "", false
}

// rejectEffort records that a physical value (or the whole field) is unsupported.
func (c *Client) rejectEffort(v string) {
	c.capMu.Lock()
	defer c.capMu.Unlock()
	if v == "" {
		c.omitEffort = true
		return
	}
	if c.effortRejected == nil {
		c.effortRejected = make(map[string]bool)
	}
	c.effortRejected[v] = true
}

// ProbeEffort probes the server for accepted reasoning_effort values and records them
// on the client (F3.3). It issues one small request per candidate value and returns the
// accepted subset. Errors are non-fatal for the caller's normal operation.
func (c *Client) ProbeEffort(ctx context.Context) ([]string, error) {
	var accepted []string
	temp := Float64Ptr(0.0)
	msgs := []Message{{Role: "user", Content: "ok"}}
	for _, v := range AllEffortValues {
		payload, err := json.Marshal(chatRequest{
			Model:           c.model,
			Messages:        msgs,
			Stream:          true,
			Temperature:     temp,
			ReasoningEffort: v,
		})
		if err != nil {
			return accepted, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
		if err != nil {
			return accepted, err
		}
		req.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return accepted, err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			accepted = append(accepted, v)
		} else if resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(string(body)), "reasoning_effort") {
			c.rejectEffort(v)
		}
	}
	return accepted, nil
}
