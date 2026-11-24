package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ModelInfo is one entry of a service's model list. A nil field means the service
// does not say, which is not the same as no.
type ModelInfo struct {
	ID       string `json:"id"`
	Free     *bool  `json:"free,omitempty"`
	Tools    *bool  `json:"tools,omitempty"`
	MayTrain *bool  `json:"may_train,omitempty"`
	// Context is the memory the service states for the model, in tokens; 0 when
	// it does not say (DL3).
	Context int `json:"context,omitempty"`
}

// modelListMaxBytes: OpenRouter's list is already over 800 KB, and a list cut in
// the middle parses as no models at all.
const modelListMaxBytes = 8 << 20

// ListModels queries the OpenAI-compatible /models endpoint and returns the
// available model identifiers. Parsing is tolerant: it accepts the OpenAI
// shape ({"data":[{"id":...}]}) as well as the Ollama-style shape
// ({"models":[{"name":...}]}). A short deadline: a probe must never hang the caller.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	infos, err := c.ListModelInfo(ctx)
	if err != nil {
		return nil, err
	}
	var models []string
	for _, m := range infos {
		models = append(models, m.ID)
	}
	return models, nil
}

// ListModelInfo is ListModels with what the service says about each model. It
// makes no chat request, so it costs nothing.
func (c *Client) ListModelInfo(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create models request: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, modelListMaxBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("models endpoint returned status %d", resp.StatusCode)
	}
	return parseModelInfo(body), nil
}

// parseModelInfo extracts the models from a tolerant union of the common
// OpenAI-compatible /models response shapes. The details are optional and read
// one by one: a field of an unexpected kind only makes that detail unknown.
func parseModelInfo(body []byte) []ModelInfo {
	var payload struct {
		Data []struct {
			ID       string          `json:"id"`
			IsFree   json.RawMessage `json:"isFree"`
			Pricing  json.RawMessage `json:"pricing"`
			Params   json.RawMessage `json:"supported_parameters"`
			MayTrain json.RawMessage `json:"mayTrainOnYourPrompts"`
			// The memory, as parseModelLimits reads it; raw, so an odd kind
			// only leaves it unknown.
			ContextLength json.RawMessage `json:"context_length"`
			MaxModelLen   json.RawMessage `json:"max_model_len"`
			LoadedContext json.RawMessage `json:"loaded_context_length"`
			TopProvider   json.RawMessage `json:"top_provider"`
			Meta          json.RawMessage `json:"meta"`
		} `json:"data"`
		Models []struct {
			Name string `json:"name"`
			ID   string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}

	var models []ModelInfo
	seen := make(map[string]bool)
	add := func(m ModelInfo) {
		m.ID = strings.TrimSpace(m.ID)
		if m.ID == "" || seen[m.ID] {
			return
		}
		seen[m.ID] = true
		models = append(models, m)
	}
	for _, d := range payload.Data {
		add(ModelInfo{ID: d.ID, Free: modelFree(d.ID, d.IsFree, d.Pricing), Tools: modelTools(d.Params), MayTrain: rawBool(d.MayTrain),
			Context: firstPositive(rawField(d.TopProvider, "context_length"), rawInt(d.ContextLength), rawInt(d.MaxModelLen), rawField(d.Meta, "n_ctx"), rawInt(d.LoadedContext))})
	}
	for _, m := range payload.Models {
		if strings.TrimSpace(m.Name) != "" {
			add(ModelInfo{ID: m.Name})
			continue
		}
		add(ModelInfo{ID: m.ID})
	}
	return models
}

// rawInt reads a whole number, 0 when the value is not one.
func rawInt(raw json.RawMessage) int {
	var n float64
	if len(raw) == 0 || json.Unmarshal(raw, &n) != nil || n < 0 || n != float64(int(n)) {
		return 0
	}
	return int(n)
}

// rawField reads a whole number from a field of an object.
func rawField(raw json.RawMessage, name string) int {
	var obj map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &obj) != nil {
		return 0
	}
	return rawInt(obj[name])
}

func rawBool(raw json.RawMessage) *bool {
	var b *bool
	if json.Unmarshal(raw, &b) != nil {
		return nil
	}
	return b
}

// modelFree: Kilo says it, OpenRouter gives the prices. A negative price means
// "variable" (automatic routing), which is not free.
func modelFree(id string, isFree, pricing json.RawMessage) *bool {
	yes := true
	if strings.HasSuffix(id, ":free") {
		return &yes
	}
	if b := rawBool(isFree); b != nil {
		return b
	}
	var prices map[string]any
	if json.Unmarshal(pricing, &prices) != nil {
		return nil
	}
	in, okIn := modelPrice(prices["prompt"])
	out, okOut := modelPrice(prices["completion"])
	if !okIn || !okOut {
		return nil
	}
	free := in == 0 && out == 0
	return &free
}

func modelPrice(v any) (float64, bool) {
	switch p := v.(type) {
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		return f, err == nil
	case float64:
		return p, true
	}
	return 0, false
}

func modelTools(raw json.RawMessage) *bool {
	var params []string
	if json.Unmarshal(raw, &params) != nil || params == nil {
		return nil
	}
	tools := false
	for _, p := range params {
		tools = tools || p == "tools"
	}
	return &tools
}
