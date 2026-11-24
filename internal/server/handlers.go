package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"vedetta/config"
	"vedetta/internal/agent"
	"vedetta/internal/llm"
	"vedetta/internal/logquiet"
	"vedetta/internal/reader"
	"vedetta/internal/search"
	"vedetta/internal/storage"
)

type Handlers struct {
	cfgMu  sync.RWMutex
	cfg    *config.Config
	engine *agent.Engine

	// Capabilities probe cache (TTL-bounded; see Capabilities).
	capsMu    sync.Mutex
	capsCache *CapabilitiesResponse
	capsAt    time.Time
	svcCache  *ServicesResponse
	svcAt     time.Time

	runs *runRegistry

	logins  loginLimiter // failed access codes (map R6)
	checked codeCache    // codes already checked against a saved hash (map R6b)
}

func NewHandlers(cfg *config.Config, engine *agent.Engine) *Handlers {
	return &Handlers{
		cfg:    cfg,
		engine: engine,
		runs:   newRunRegistry(),
	}
}

// searchTimeLimit is the longest a search can last, in the background as on the
// connection that follows it.
func searchTimeLimit(cfg *config.Config) time.Duration {
	limit := 1200 * time.Second
	if cfg != nil {
		if d := time.Duration(cfg.DeepSessionTimeoutSecs+cfg.SynthesisTimeoutSecs+60) * time.Second; d > limit {
			limit = d
		}
	}
	return limit
}

type SearchRequest struct {
	SessionID      string `json:"session_id,omitempty"`
	Query          string `json:"query"`
	Mode           string `json:"mode,omitempty"`
	Language       string `json:"language,omitempty"`        // UI locale (interface)
	AnswerLanguage string `json:"answer_language,omitempty"` // optional response-language override
	Resume         bool   `json:"resume,omitempty"`

	// Per-search optional overrides (F3.6-5). Empty means "use the engine
	// global/default". They never mutate server-side configuration.
	SearchProvider string `json:"search_provider,omitempty"`
	LLMEndpoint    string `json:"llm_endpoint,omitempty"`
	// Reader picks the first reader of the chain for this search (S4).
	Reader string `json:"reader,omitempty"`
}

const unknownVersion = "unknown"

func versionOrDefault(v string) string {
	if strings.TrimSpace(v) == "" {
		return unknownVersion
	}
	return strings.TrimSpace(v)
}

// capabilitiesCacheTTL bounds how long a /api/capabilities snapshot is reused.
const capabilitiesCacheTTL = 60 * time.Second

// CapabilityLLM is the probed state of a single LLM endpoint.
type CapabilityLLM struct {
	ID               string   `json:"id"`
	BaseURL          string   `json:"base_url"`
	Model            string   `json:"model"`
	Reachable        bool     `json:"reachable"`
	EffortValues     []string `json:"effort_values,omitempty"`
	StructuredOutput string   `json:"structured_output"`
	Models           []string `json:"models,omitempty"`
}

// CapabilityComponent is the probed state of a search or reader provider.
type CapabilityComponent struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// CapabilitiesResponse is the on-demand capabilities report. States are honest:
// ok/down/unknown/configured, never faked.
type CapabilitiesResponse struct {
	ProbedAt string                `json:"probed_at"`
	LLM      []CapabilityLLM       `json:"llm"`
	Search   []CapabilityComponent `json:"search"`
	Reader   []CapabilityComponent `json:"reader"`
}

// Capabilities reports the live state of the configured LLM/search/reader
// components. Results are cached for capabilitiesCacheTTL; ?probe=1 forces a
// fresh probe. Network probes run outside the config lock.
func (h *Handlers) Capabilities(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConfig(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	force := r.URL.Query().Get("probe") == "1"

	// Check the cache under the lock without holding it during network probes.
	h.capsMu.Lock()
	if !force && h.capsCache != nil && time.Since(h.capsAt) < capabilitiesCacheTTL {
		cached := h.capsCache
		h.capsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cached)
		return
	}
	h.capsMu.Unlock()

	// Snapshot the configuration, then release the read lock before probing.
	h.cfgMu.RLock()
	endpoints := make([]config.LLMEndpoint, len(h.cfg.LLMEndpoints))
	copy(endpoints, h.cfg.LLMEndpoints)
	searchProviders := append([]string(nil), h.cfg.SearchProviders...)
	readerProviders := append([]string(nil), h.cfg.ReaderProviders...)
	searchCfg := search.ProviderConfig{
		SearxngURL:   h.cfg.SearxngURL,
		BraveAPIKey:  h.cfg.BraveAPIKey,
		TavilyAPIKey: h.cfg.TavilyAPIKey,
		SerperAPIKey: h.cfg.SerperAPIKey,
		ExaAPIKey:    h.cfg.ExaAPIKey,
	}
	readerCfg := reader.ReaderConfig{
		JinaBaseURL:     h.cfg.JinaURL,
		FirecrawlAPIKey: h.cfg.FirecrawlAPIKey,
		ExaAPIKey:       h.cfg.ExaAPIKey,
		Browser:         BrowserReaderConfig(h.cfg),
	}
	h.cfgMu.RUnlock()

	resp := CapabilitiesResponse{
		ProbedAt: time.Now().UTC().Format(time.RFC3339Nano),
		LLM:      probeLLMEndpoints(r.Context(), endpoints),
		Search:   make([]CapabilityComponent, 0, len(searchProviders)),
		Reader:   make([]CapabilityComponent, 0, len(readerProviders)),
	}

	for _, name := range searchProviders {
		ps := search.ProbeProvider(r.Context(), name, searchCfg)
		resp.Search = append(resp.Search, CapabilityComponent{Name: ps.Name, Status: ps.Status, Detail: ps.Detail})
	}
	for _, name := range readerProviders {
		ps := reader.ProbeReader(r.Context(), name, readerCfg)
		resp.Reader = append(resp.Reader, CapabilityComponent{Name: ps.Name, Status: ps.Status, Detail: ps.Detail})
	}

	// Store the fresh snapshot for subsequent cache hits.
	h.capsMu.Lock()
	h.capsCache = &resp
	h.capsAt = time.Now()
	h.capsMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// probeLLMEndpoints probes every configured LLM endpoint with disposable
// clients, so probing never mutates the live engine's client state.
func probeLLMEndpoints(ctx context.Context, endpoints []config.LLMEndpoint) []CapabilityLLM {
	out := make([]CapabilityLLM, 0, len(endpoints))
	for _, ep := range endpoints {
		client := llm.NewClient(ep.BaseURL, ep.APIKey, ep.Model)
		cap := CapabilityLLM{
			ID:               ep.ID,
			BaseURL:          ep.BaseURL,
			Model:            ep.Model,
			StructuredOutput: "unknown",
		}
		if models, err := client.ListModels(ctx); err == nil {
			cap.Reachable = true
			cap.Models = models
		}
		// Bound the effort sweep: a partial result is fine when the context expires.
		probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		efforts, _ := client.ProbeEffort(probeCtx)
		cancel()
		cap.EffortValues = efforts
		out = append(out, cap)
	}
	return out
}

func (h *Handlers) Health(w http.ResponseWriter, r *http.Request) {
	h.cfgMu.RLock()
	version := h.cfg.Version
	port := h.cfg.Port
	searxngURL := h.cfg.SearxngURL
	jinaURL := h.cfg.JinaURL
	llmBaseURL := h.cfg.LLMBaseURL
	llmModel := h.cfg.LLMModel
	h.cfgMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	// map R6: with an access code, a device not in gets only the state and the version
	// (enough for a container health check), not the addresses of the services
	if !h.accessOK(r) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "version": versionOrDefault(version)})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"version": versionOrDefault(version),
		"config": map[string]string{
			"port":         port,
			"searxng_url":  searxngURL,
			"jina_url":     jinaURL,
			"llm_base_url": llmBaseURL,
			"llm_model":    llmModel,
		},
	})
}

func (h *Handlers) ListSessions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if h.engine.Storage() == nil {
		_ = json.NewEncoder(w).Encode([]interface{}{})
		return
	}
	sessions, err := h.engine.Storage().FindSessions(sessionQueryOf(r))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to list sessions: "+err.Error())
		return
	}
	// Which ones are still searching: the page marks them, the user may be elsewhere.
	views := make([]sessionListItem, len(sessions))
	for i, sess := range sessions {
		run := h.runs.get(sess.ID)
		views[i] = sessionListItem{SessionSummary: sess, Live: run != nil && !run.isFinished()}
	}
	_ = json.NewEncoder(w).Encode(views)
}

// sessionQueryOf reads the list's parameters (map R4): days=N keeps the sessions of
// the last N days (the sidebar), all=1 lists every one (the Library), q= keeps those
// whose title or questions have its words. With none of them: the latest 100, as before.
func sessionQueryOf(r *http.Request) storage.SessionQuery {
	v := r.URL.Query()
	q := storage.SessionQuery{Limit: 100, Match: strings.TrimSpace(v.Get("q"))}
	if v.Get("all") == "1" || q.Match != "" {
		q.Limit = 0
	}
	if d, err := strconv.Atoi(v.Get("days")); err == nil && d > 0 {
		q.Limit = 0
		q.Since = time.Now().AddDate(0, 0, -d)
	}
	return q
}

type liveSearch struct {
	Query string `json:"query"`
}

type sessionListItem struct {
	storage.SessionSummary
	Live bool `json:"live,omitempty"`
}

type sessionView struct {
	*storage.SessionDetail
	Live *liveSearch `json:"live,omitempty"`
}

// streamRun gives the events of a session's search from ?from=N on: how a client
// that lost the connection, or reloaded the page, comes back to a search.
func (h *Handlers) streamRun(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	run := h.runs.get(sessionID)
	if run == nil {
		// Over for a while, or never started: the answer, if any, is in the session.
		writeJSONError(w, http.StatusNotFound, "no search running for this session")
		return
	}
	from, _ := strconv.Atoi(r.URL.Query().Get("from"))
	sse, err := NewSSEWriter(w)
	if err != nil {
		http.Error(w, "streaming not supported: "+err.Error(), http.StatusInternalServerError)
		return
	}
	run.stream(r.Context(), sse, from)
}

// stopRun stops a session's search and keeps the session: the engine saves what it
// has, and the stream ends as usual.
func (h *Handlers) stopRun(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	run := h.runs.get(sessionID)
	if run == nil {
		writeJSONError(w, http.StatusNotFound, "no search running for this session")
		return
	}
	run.cancel()
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "stopping", "id": sessionID})
}

func (h *Handlers) SessionItem(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	id = strings.TrimSpace(id)

	if base, ok := strings.CutSuffix(id, "/stream"); ok {
		h.streamRun(w, r, base)
		return
	}
	if base, ok := strings.CutSuffix(id, "/stop"); ok {
		h.stopRun(w, r, base)
		return
	}

	if id == "" {
		if r.Method == http.MethodGet {
			h.ListSessions(w, r)
			return
		}
		http.Error(w, `{"error":"session id required"}`, http.StatusBadRequest)
		return
	}

	if h.engine.Storage() == nil {
		http.Error(w, `{"error":"storage not enabled"}`, http.StatusServiceUnavailable)
		return
	}

	switch r.Method {
	case http.MethodGet:
		detail, err := h.engine.Storage().GetSession(id)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to get session: "+err.Error())
			return
		}
		if detail == nil {
			http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
			return
		}
		// A search still running has no stored message yet: say so, or the session
		// looks empty to whoever opens it.
		var live *liveSearch
		if run := h.runs.get(id); run != nil && !run.isFinished() {
			live = &liveSearch{Query: run.query}
		}
		_ = json.NewEncoder(w).Encode(sessionView{SessionDetail: detail, Live: live})

	case http.MethodDelete:
		h.runs.cancelSession(id, 10*time.Second)
		if err := h.engine.Storage().DeleteSession(id); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to delete session: "+err.Error())
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted", "id": id})

	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func (h *Handlers) Search(w http.ResponseWriter, r *http.Request) {
	var req SearchRequest

	// map R6 (audit S2): a search starts only with a POST; with a GET, a picture in
	// any page could start a Deep, on a paid model too
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed: a search starts with POST"}`, http.StatusMethodNotAllowed)
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid json body"}`, http.StatusBadRequest)
		return
	}

	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" {
		http.Error(w, `{"error":"query is required"}`, http.StatusBadRequest)
		return
	}

	// One way only since 17/11/2026 (map R1): every mode an older client sends is the Deep.
	req.Mode = "deep"

	// Per-search overrides (F3.6-5): resolve optional provider/endpoint handles
	// under a short read lock. We only read configuration here; engine-global
	// providers are never mutated.
	var searchOverride search.Provider
	var llmOverride *llm.Client
	var readerOverride reader.Reader
	if strings.TrimSpace(req.SearchProvider) != "" || strings.TrimSpace(req.LLMEndpoint) != "" || strings.TrimSpace(req.Reader) != "" {
		h.cfgMu.RLock()
		var searchChain, readerChain []string
		var searchErr, readerErr error
		var readerCfg reader.ReaderConfig
		var endpoints []config.LLMEndpoint
		var searxngURL, braveKey, tavilyKey, serperKey, exaKey, exaType, exaLocation string
		var fallbackOnEmpty bool
		retryPolicy := llm.DefaultRetryPolicy()
		maxTokens := 0
		contextTokens := func(ep config.LLMEndpoint) int { return ep.ContextTokens }
		if h.cfg != nil {
			retryPolicy = llm.NewRetryPolicy(h.cfg.LLMRetries, h.cfg.LLMRetryMaxWaitSecs)
			maxTokens = h.cfg.LLMMaxTokens
			cfgCopy := *h.cfg
			contextTokens = func(ep config.LLMEndpoint) int { return config.ContextTokensFor(&cfgCopy, ep) }
			if name := strings.TrimSpace(req.SearchProvider); shouldApplySearchOverride(h.cfg.DefaultSearchProvider, name) {
				searchChain, searchErr = requestChain(h.cfg, "search", name)
			}
			if name := strings.TrimSpace(req.Reader); name != "" && !(len(h.cfg.ReaderProviders) > 0 && strings.EqualFold(h.cfg.ReaderProviders[0], name)) {
				readerChain, readerErr = requestChain(h.cfg, "reader", name)
				readerCfg = reader.ReaderConfig{
					Providers:        readerChain,
					JinaBaseURL:      h.cfg.JinaURL,
					FirecrawlAPIKey:  h.cfg.FirecrawlAPIKey,
					FirecrawlBaseURL: h.cfg.FirecrawlBaseURL,
					ExaAPIKey:        h.cfg.ExaAPIKey,
					PDF:              PDFReaderConfig(h.cfg),
					Browser:          BrowserReaderConfig(h.cfg),
				}
			}
			endpoints = append(endpoints, h.cfg.LLMEndpoints...)
			searxngURL = h.cfg.SearxngURL
			braveKey = h.cfg.BraveAPIKey
			tavilyKey = h.cfg.TavilyAPIKey
			serperKey = h.cfg.SerperAPIKey
			exaKey, exaType, exaLocation = h.cfg.ExaAPIKey, h.cfg.ExaSearchType, h.cfg.ExaUserLocation
			fallbackOnEmpty = h.cfg.SearchFallbackOnEmpty
		}
		h.cfgMu.RUnlock()

		if searchErr != nil {
			writeJSONError(w, http.StatusBadRequest, searchErr.Error())
			return
		}
		if readerErr != nil {
			writeJSONError(w, http.StatusBadRequest, readerErr.Error())
			return
		}
		if len(readerChain) > 0 {
			cr, err := reader.NewReader(readerCfg)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "reader: "+err.Error())
				return
			}
			readerOverride = cr
		}
		if len(searchChain) > 0 {
			// The chosen service first, then the allowed fallbacks (R1).
			// NewProvider (not Get) preserves the academic routing wrapper.
			sp, spErr := search.NewProvider(search.ProviderConfig{
				Providers:       searchChain,
				FallbackOnEmpty: fallbackOnEmpty,
				SearxngURL:      searxngURL,
				BraveAPIKey:     braveKey,
				TavilyAPIKey:    tavilyKey,
				SerperAPIKey:    serperKey,
				ExaAPIKey:       exaKey,
				ExaSearchType:   exaType,
				ExaUserLocation: exaLocation,
			})
			if spErr != nil {
				writeJSONError(w, http.StatusBadRequest, "search provider: "+spErr.Error())
				return
			}
			searchOverride = sp
		}

		if id := strings.TrimSpace(req.LLMEndpoint); id != "" {
			ep, ok := config.EndpointByID(endpoints, id)
			if !ok {
				writeJSONError(w, http.StatusBadRequest, "unknown llm endpoint: "+id)
				return
			}
			llmOverride = llm.NewClient(ep.BaseURL, ep.APIKey, ep.Model).WithRetryPolicy(retryPolicy).WithMaxTokens(maxTokens).
				WithContextTokens(contextTokens(ep))
		}
	}

	params := agent.SearchParams{
		SessionID:      strings.TrimSpace(req.SessionID),
		Query:          req.Query,
		Mode:           req.Mode,
		UILanguage:     agent.NormalizeLanguage(req.Language),
		AnswerLanguage: strings.TrimSpace(req.AnswerLanguage),
		Resume:         req.Resume,

		SearchProviderOverride: searchOverride,
		LLMClientOverride:      llmOverride,
		ContentReaderOverride:  readerOverride,
	}

	h.cfgMu.RLock()
	limit := searchTimeLimit(h.cfg)
	h.cfgMu.RUnlock()

	// Off this connection's context. One search at a time in a conversation: a second one
	// would take its place here, and the first could no longer be stopped.
	run := h.runs.start(params.Query, params.SessionID, limit, func(ctx context.Context, emit agent.Emitter) error {
		return h.engine.ExecuteDeepPath(ctx, params, emit)
	})
	if run == nil {
		writeJSONError(w, http.StatusConflict, "busy: a search is already running in this conversation")
		return
	}
	sse, err := NewSSEWriter(w)
	if err != nil {
		run.cancel()
		http.Error(w, "streaming not supported: "+err.Error(), http.StatusInternalServerError)
		return
	}
	run.stream(r.Context(), sse, 0)
}

type LLMEndpointView struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	HasKey  bool   `json:"has_key"`
}

// LLMEndpointUpdate is the write-side counterpart of LLMEndpointView. APIKey is
// optional: when nil, empty or the mask sentinel, the stored key is preserved
// for existing ids and left empty for new ones.
type LLMEndpointUpdate struct {
	ID      string  `json:"id"`
	Name    string  `json:"name,omitempty"`
	BaseURL string  `json:"base_url"`
	Model   string  `json:"model"`
	APIKey  *string `json:"api_key,omitempty"`
	Paid    *bool   `json:"paid,omitempty"`
	// ContextTokens sets the model's memory by hand (0 = read it from the
	// server); nil keeps what is stored.
	ContextTokens *int `json:"context_tokens,omitempty"`
	// ClearKey removes the stored key (Settings "togli"); an empty APIKey keeps it.
	ClearKey bool `json:"clear_key,omitempty"`
}

// RuntimeView is a read-only snapshot of non-policy runtime parameters (F3.6-9).
// Never exposes secrets: ConfigToken/EmbeddingsAPIKey only as booleans.
type RuntimeView struct {
	Port             string `json:"port"`
	ConfigTokenSet   bool   `json:"config_token_set"`
	PresetIT         bool   `json:"preset_it"`
	LLMEffortDeep    string `json:"llm_effort_deep"`
	EmbeddingsURL    string `json:"embeddings_url"`
	HasEmbeddingsKey bool   `json:"has_embeddings_key"`
	FirecrawlBaseURL string `json:"firecrawl_base_url"`
}

// LimitsView is a read-only snapshot of the main budget/timeout parameters
// (F3.6-9). It excludes core policy (PlanFacets/RRF/DomainCap/reranker).
type LimitsView struct {
	SynthesisTimeoutSecs   int `json:"synthesis_timeout_secs"`
	DeepSessionTimeoutSecs int `json:"deep_session_timeout_secs"`
	SynthContentStallSecs  int `json:"synth_content_stall_secs"`
}

type ConfigResponse struct {
	Version               string            `json:"version"`
	ContactEmail          string            `json:"contact_email,omitempty"`
	DefaultSearchProvider string            `json:"default_search_provider,omitempty"`
	SearchProviders       []string          `json:"search_providers"`
	ReaderProviders       []string          `json:"reader_providers"`
	LLMProviders          []string          `json:"llm_providers"`
	SearchFallbackOnEmpty bool              `json:"search_fallback_on_empty"`
	HasBraveKey           bool              `json:"has_brave_key"`
	HasTavilyKey          bool              `json:"has_tavily_key"`
	HasSerperKey          bool              `json:"has_serper_key"`
	HasFirecrawlKey       bool              `json:"has_firecrawl_key"`
	HasExaKey             bool              `json:"has_exa_key"`
	BrowserCDPAddress     string            `json:"browser_cdp_address"` // scheme://host only: the address may carry a token
	HasLLMKey             bool              `json:"has_llm_key"`
	SearxngURL            string            `json:"searxng_url"`
	JinaURL               string            `json:"jina_url"`
	LLMBaseURL            string            `json:"llm_base_url"`
	LLMModel              string            `json:"llm_model"`
	LLMEndpoints          []LLMEndpointView `json:"llm_endpoints"`
	DefaultLLMEndpoint    string            `json:"default_llm_endpoint,omitempty"`
	ActiveSearchProvider  string            `json:"active_search_provider"`
	ActiveReaderProvider  string            `json:"active_reader_provider"`
	Runtime               RuntimeView       `json:"runtime"`
	Limits                LimitsView        `json:"limits"`
	Services              map[string]bool   `json:"services,omitempty"`
	// Sperimentale (map R3): the levers as they are now.
	DeepLoopReviewLines bool `json:"deep_loop_review_lines"`
	DeepLoopPush        bool `json:"deep_loop_push"`
	DeepLoopGrant       bool `json:"deep_loop_grant"`
	QuietLog            bool `json:"quiet_log"` // map R7
	PaidFallback        bool `json:"paid_fallback"`
	SetupDone           bool `json:"setup_done"` // map R8: false shows the first run
}

type ConfigUpdateRequest struct {
	ContactEmail          *string             `json:"contact_email,omitempty"`
	DefaultSearchProvider *string             `json:"default_search_provider,omitempty"`
	SearchProviders       []string            `json:"search_providers,omitempty"`
	ReaderProviders       []string            `json:"reader_providers,omitempty"`
	LLMProviders          []string            `json:"llm_providers,omitempty"`
	SearchFallbackOnEmpty *bool               `json:"search_fallback_on_empty,omitempty"`
	BraveAPIKey           *string             `json:"brave_api_key,omitempty"`
	TavilyAPIKey          *string             `json:"tavily_api_key,omitempty"`
	SerperAPIKey          *string             `json:"serper_api_key,omitempty"`
	FirecrawlAPIKey       *string             `json:"firecrawl_api_key,omitempty"`
	ExaAPIKey             *string             `json:"exa_api_key,omitempty"`
	BrowserCDPURL         *string             `json:"browser_cdp_url,omitempty"`
	SearxngURL            *string             `json:"searxng_url,omitempty"`
	JinaURL               *string             `json:"jina_url,omitempty"`
	LLMBaseURL            *string             `json:"llm_base_url,omitempty"`
	LLMModel              *string             `json:"llm_model,omitempty"`
	LLMAPIKey             *string             `json:"llm_api_key,omitempty"`
	LLMEndpoints          []LLMEndpointUpdate `json:"llm_endpoints,omitempty"`
	DefaultLLMEndpoint    *string             `json:"default_llm_endpoint,omitempty"`
	// Settings (SPEC_UI_IMPOSTAZIONI_E_BARRA S2): on/off per service, keyed
	// "search:brave"; when present, the chains are rebuilt from the choices.
	Services     map[string]bool `json:"services,omitempty"`
	PaidFallback *bool           `json:"paid_fallback,omitempty"`
	// Sperimentale (map R3): a choice made in Settings, saved and stronger than .env.
	DeepLoopReviewLines *bool `json:"deep_loop_review_lines,omitempty"`
	DeepLoopPush        *bool `json:"deep_loop_push,omitempty"`
	DeepLoopGrant       *bool `json:"deep_loop_grant,omitempty"`
	// Map R7: "Registro con meno dettagli", chosen in Settings.
	QuietLog *bool `json:"quiet_log,omitempty"`
	// Map R8: true ends the first run, false opens it again.
	SetupDone *bool `json:"setup_done,omitempty"`
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// shouldApplySearchOverride reports whether a per-search search-provider
// override must be applied. An empty request or a request equal to the
// configured default provider (case-insensitive) means "use the global chain",
// preserving the configured multi-provider order and fallback.
func shouldApplySearchOverride(defaultProvider, requested string) bool {
	req := strings.ToLower(strings.TrimSpace(requested))
	if req == "" {
		return false
	}
	return req != strings.ToLower(strings.TrimSpace(defaultProvider))
}

// authorizeConfig enforces the optional VEDETTA_CONFIG_TOKEN guard.
// When no token is configured the endpoint stays open (backward compatible).
func (h *Handlers) authorizeConfig(w http.ResponseWriter, r *http.Request) bool {
	ok, blocked := h.configCheck(r)
	if blocked {
		writeJSONError(w, http.StatusTooManyRequests, tooManyCodes)
		return false
	}
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized: invalid or missing config token")
		return false
	}
	return true
}

// configCheck reports whether the request may see and change the configuration:
// always when no token is set, otherwise only with the token. blocked: too many wrong
// codes came from there, and this one was not looked at.
func (h *Handlers) configCheck(r *http.Request) (ok, blocked bool) {
	_, settings := h.codes()
	if !settings.set() {
		return true, false
	}
	p := presentedCode(r)
	if p == "" {
		return false, false
	}
	return h.logins.try(clientKey(r), time.Now(), func() bool { return h.codeMatches(p, settings) })
}

// applyConfigUpdate overlays the request onto a candidate configuration copy. It
// refuses a "${NAME}" key that may not go to its address (keyRefAllowed).
func applyConfigUpdate(cfg *config.Config, req *ConfigUpdateRequest) error {
	if req.ContactEmail != nil {
		cfg.ContactEmail = strings.TrimSpace(*req.ContactEmail)
	}
	if req.DefaultSearchProvider != nil {
		cfg.DefaultSearchProvider = strings.ToLower(strings.TrimSpace(*req.DefaultSearchProvider))
	}
	if req.BraveAPIKey != nil && *req.BraveAPIKey != "********" {
		cfg.BraveAPIKey = strings.TrimSpace(*req.BraveAPIKey)
	}
	if req.TavilyAPIKey != nil && *req.TavilyAPIKey != "********" {
		cfg.TavilyAPIKey = strings.TrimSpace(*req.TavilyAPIKey)
	}
	if req.SerperAPIKey != nil && *req.SerperAPIKey != "********" {
		cfg.SerperAPIKey = strings.TrimSpace(*req.SerperAPIKey)
	}
	if req.FirecrawlAPIKey != nil && *req.FirecrawlAPIKey != "********" {
		cfg.FirecrawlAPIKey = strings.TrimSpace(*req.FirecrawlAPIKey)
	}
	if req.ExaAPIKey != nil && *req.ExaAPIKey != "********" {
		cfg.ExaAPIKey = strings.TrimSpace(*req.ExaAPIKey)
	}
	if req.BrowserCDPURL != nil && *req.BrowserCDPURL != "********" {
		cfg.BrowserCDPURL = strings.TrimSpace(*req.BrowserCDPURL)
	}
	if req.LLMAPIKey != nil && *req.LLMAPIKey != "********" {
		base := cfg.LLMBaseURL
		if req.LLMBaseURL != nil && strings.TrimSpace(*req.LLMBaseURL) != "" {
			base = strings.TrimSpace(*req.LLMBaseURL)
		}
		key, env := config.ResolveKeyRef(*req.LLMAPIKey)
		if env != "" && !keyRefAllowed(env, base, cfg.LLMEndpoints) {
			return errKeyRef(env)
		}
		cfg.LLMAPIKey = key
	}
	if req.SearxngURL != nil && strings.TrimSpace(*req.SearxngURL) != "" {
		cfg.SearxngURL = strings.TrimSpace(*req.SearxngURL)
	}
	if req.JinaURL != nil && strings.TrimSpace(*req.JinaURL) != "" {
		cfg.JinaURL = strings.TrimSpace(*req.JinaURL)
	}
	if req.LLMBaseURL != nil && strings.TrimSpace(*req.LLMBaseURL) != "" {
		cfg.LLMBaseURL = strings.TrimSpace(*req.LLMBaseURL)
	}
	if req.LLMModel != nil && strings.TrimSpace(*req.LLMModel) != "" {
		cfg.LLMModel = strings.TrimSpace(*req.LLMModel)
	}
	if req.SearchFallbackOnEmpty != nil {
		cfg.SearchFallbackOnEmpty = *req.SearchFallbackOnEmpty
	}
	if req.DeepLoopReviewLines != nil {
		cfg.DeepLoopReviewLines, cfg.DeepLoopReviewLinesChosen = *req.DeepLoopReviewLines, true
	}
	if req.DeepLoopPush != nil {
		cfg.DeepLoopPush, cfg.DeepLoopPushChosen = *req.DeepLoopPush, true
	}
	if req.DeepLoopGrant != nil {
		cfg.DeepLoopGrant, cfg.DeepLoopGrantChosen = *req.DeepLoopGrant, true
	}
	if req.QuietLog != nil {
		cfg.QuietLog, cfg.QuietLogChosen = *req.QuietLog, true
	}
	if req.SetupDone != nil {
		cfg.SetupDone = *req.SetupDone
	}
	if len(req.SearchProviders) > 0 {
		var cleanSearch []string
		for _, sp := range req.SearchProviders {
			sp = strings.ToLower(strings.TrimSpace(sp))
			if sp != "" && sp != "none" {
				cleanSearch = append(cleanSearch, sp)
			}
		}
		if len(cleanSearch) > 0 {
			cfg.SearchProviders = cleanSearch
			cfg.SearchProvider = cleanSearch[0]
		}
	}
	if len(req.ReaderProviders) > 0 {
		var cleanReader []string
		for _, rp := range req.ReaderProviders {
			rp = strings.ToLower(strings.TrimSpace(rp))
			if rp != "" {
				cleanReader = append(cleanReader, rp)
			}
		}
		if len(cleanReader) > 0 {
			cfg.ReaderProviders = cleanReader
		}
	}
	if len(req.LLMProviders) > 0 {
		cfg.LLMProviders = req.LLMProviders
	}

	// Step (b): full-replace the endpoint registry when provided. Keys are
	// preserved per id unless a real new key is supplied.
	if req.LLMEndpoints != nil {
		newList := make([]config.LLMEndpoint, 0, len(req.LLMEndpoints))
		for _, upd := range req.LLMEndpoints {
			id := strings.TrimSpace(upd.ID)
			ep := config.LLMEndpoint{
				ID:      id,
				Name:    strings.TrimSpace(upd.Name),
				BaseURL: strings.TrimSpace(upd.BaseURL),
				Model:   strings.TrimSpace(upd.Model),
			}
			existing, had := config.EndpointByID(cfg.LLMEndpoints, id)
			if upd.APIKey != nil && *upd.APIKey != "" && *upd.APIKey != "********" {
				// "${NAME}" keeps the key in the environment; only the reference is saved.
				ep.APIKey, ep.KeyEnv = config.ResolveKeyRef(*upd.APIKey)
				if ep.KeyEnv != "" && !keyRefAllowed(ep.KeyEnv, ep.BaseURL, cfg.LLMEndpoints) {
					return errKeyRef(ep.KeyEnv)
				}
			} else if had && sameServiceHost(existing.BaseURL, ep.BaseURL) {
				// map R6 (T9.3): a saved key follows its model only on the same host;
				// a new address needs the key typed again
				ep.APIKey, ep.KeyEnv = existing.APIKey, existing.KeyEnv
			}
			if upd.ClearKey {
				ep.APIKey, ep.KeyEnv = "", ""
			}
			if upd.Paid != nil {
				paid := *upd.Paid
				ep.Paid = &paid
			} else if had {
				ep.Paid = existing.Paid
			}
			if upd.ContextTokens != nil && *upd.ContextTokens >= 0 {
				ep.ContextTokens = *upd.ContextTokens
			} else if had {
				ep.ContextTokens = existing.ContextTokens
			}
			newList = append(newList, ep)
		}
		cfg.LLMEndpoints = newList
	}

	// Step (c): explicit default selection.
	if req.DefaultLLMEndpoint != nil {
		cfg.LLMDefaultID = strings.TrimSpace(*req.DefaultLLMEndpoint)
	}

	// Step (d): when the registry is not part of the request, propagate legacy
	// changes into the default endpoint so the legacy client keeps working.
	if req.LLMEndpoints == nil {
		if idx := defaultEndpointIndex(cfg); idx >= 0 {
			if req.LLMBaseURL != nil && strings.TrimSpace(*req.LLMBaseURL) != "" {
				newKey := req.LLMAPIKey != nil && *req.LLMAPIKey != "********"
				if !newKey && !sameServiceHost(cfg.LLMEndpoints[idx].BaseURL, cfg.LLMBaseURL) {
					cfg.LLMEndpoints[idx].APIKey, cfg.LLMEndpoints[idx].KeyEnv = "", ""
					cfg.LLMAPIKey = ""
				}
				cfg.LLMEndpoints[idx].BaseURL = cfg.LLMBaseURL
			}
			if req.LLMModel != nil && strings.TrimSpace(*req.LLMModel) != "" {
				cfg.LLMEndpoints[idx].Model = cfg.LLMModel
			}
			if req.LLMAPIKey != nil && *req.LLMAPIKey != "********" {
				cfg.LLMEndpoints[idx].APIKey = cfg.LLMAPIKey
				_, cfg.LLMEndpoints[idx].KeyEnv = config.ResolveKeyRef(*req.LLMAPIKey)
			}
		}
	}
	return nil
}

// ownKeyNames are the variables Vedetta reads for its own services: none of them is a
// model's key.
var ownKeyNames = map[string]bool{"LLM_API_KEY": true, "BRAVE_API_KEY": true, "TAVILY_API_KEY": true, "SERPER_API_KEY": true,
	"SERP_API_KEY": true, "FIRECRAWL_API_KEY": true, "EXA_API_KEY": true, "EMBEDDINGS_API_KEY": true}

var keyNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*_(?:KEY|TOKEN)$`)

// keyRefAllowed says whether a "${NAME}" key sent by the page may go to an address.
// Whoever can change the settings chooses both, and a variable holds what its owner
// wrote in .env: the key of a known service goes only to that service, one already in
// use only to the host it is used with, and what is not a model's key to nobody
// (external audit 24/11/2026, SEC-01). The references saved in config.local are the
// owner's, and are not looked at.
func keyRefAllowed(name, baseURL string, endpoints []config.LLMEndpoint) bool {
	if !keyNameRe.MatchString(name) || strings.HasPrefix(name, "VEDETTA_") || ownKeyNames[name] {
		return false
	}
	for _, p := range llmPresets {
		if p.KeyEnv == name {
			return sameServiceHost(p.BaseURL, baseURL)
		}
	}
	for _, ep := range endpoints {
		if ep.KeyEnv == name && !sameServiceHost(ep.BaseURL, baseURL) {
			return false
		}
	}
	return true
}

func errKeyRef(name string) error {
	return fmt.Errorf("the variable %s may not be sent to this address", name)
}

// sameServiceHost reports whether two service addresses have the same host (the path
// and the port may change): a saved key is never sent to another host (map R6, T9.3),
// nor in the clear to one that had it encrypted.
func sameServiceHost(a, b string) bool {
	ua, errA := url.Parse(strings.TrimSpace(a))
	ub, errB := url.Parse(strings.TrimSpace(b))
	if errA != nil || errB != nil || ua.Hostname() == "" || (ua.Scheme == "https" && ub.Scheme != "https") {
		return false
	}
	return strings.EqualFold(ua.Hostname(), ub.Hostname())
}

// defaultEndpointIndex returns the slice index of the effective default
// endpoint, falling back to the first element, or -1 when the list is empty.
func defaultEndpointIndex(cfg *config.Config) int {
	if cfg == nil || len(cfg.LLMEndpoints) == 0 {
		return -1
	}
	for i, ep := range cfg.LLMEndpoints {
		if ep.ID == cfg.LLMDefaultID {
			return i
		}
	}
	return 0
}

// isDefaultSearchProviderValid reports whether the resolved default provider is
// enabled in the candidate configuration. When no provider is configured only
// the built-in "searxng" fallback is accepted.
func isDefaultSearchProviderValid(cfg *config.Config) bool {
	want := strings.ToLower(strings.TrimSpace(cfg.DefaultSearchProvider))
	if want == "" {
		return false
	}
	if len(cfg.SearchProviders) == 0 {
		return want == "searxng"
	}
	for _, sp := range cfg.SearchProviders {
		if strings.ToLower(strings.TrimSpace(sp)) == want {
			return true
		}
	}
	return false
}

// validateLLMEndpoints checks the endpoint registry before commit. The
// requestedDefault is the raw value from the request (may be nil): an explicit
// dangling default is a hard error even though Normalize would heal it for
// legacy configs.
func validateLLMEndpoints(cfg *config.Config, requestedDefault *string) error {
	if len(cfg.LLMEndpoints) == 0 {
		return fmt.Errorf("at least one llm endpoint is required")
	}
	seen := make(map[string]bool, len(cfg.LLMEndpoints))
	for i, ep := range cfg.LLMEndpoints {
		id := strings.TrimSpace(ep.ID)
		if id == "" {
			return fmt.Errorf("llm endpoint #%d has empty id", i+1)
		}
		if seen[id] {
			return fmt.Errorf("duplicate llm endpoint id: %s", id)
		}
		seen[id] = true
		if strings.TrimSpace(ep.BaseURL) == "" {
			return fmt.Errorf("llm endpoint %s has empty base_url", id)
		}
		if strings.TrimSpace(ep.Model) == "" {
			return fmt.Errorf("llm endpoint %s has empty model", id)
		}
	}
	if _, ok := config.EndpointByID(cfg.LLMEndpoints, cfg.LLMDefaultID); !ok {
		return fmt.Errorf("default llm endpoint %q not found", cfg.LLMDefaultID)
	}
	if requestedDefault != nil {
		id := strings.TrimSpace(*requestedDefault)
		if id != "" {
			if _, ok := config.EndpointByID(cfg.LLMEndpoints, id); !ok {
				return fmt.Errorf("default llm endpoint %q not found", id)
			}
		}
	}
	return nil
}

// writeConfigResponse serializes the current configuration without exposing secrets.
func (h *Handlers) writeConfigResponse(w http.ResponseWriter) {
	h.cfgMu.RLock()
	cfg := *h.cfg
	searchProviders := append([]string(nil), h.cfg.SearchProviders...)
	readerProviders := append([]string(nil), h.cfg.ReaderProviders...)
	llmProviders := append([]string(nil), h.cfg.LLMProviders...)
	llmEndpoints := make([]config.LLMEndpoint, len(h.cfg.LLMEndpoints))
	copy(llmEndpoints, h.cfg.LLMEndpoints)
	h.cfgMu.RUnlock()

	endpointViews := make([]LLMEndpointView, 0, len(llmEndpoints))
	for _, ep := range llmEndpoints {
		endpointViews = append(endpointViews, LLMEndpointView{
			ID:      ep.ID,
			Name:    ep.Name,
			BaseURL: ep.BaseURL,
			Model:   ep.Model,
			HasKey:  ep.APIKey != "",
		})
	}

	resp := ConfigResponse{
		Version:               versionOrDefault(cfg.Version),
		ContactEmail:          cfg.ContactEmail,
		DefaultSearchProvider: cfg.DefaultSearchProvider,
		SearchProviders:       searchProviders,
		ReaderProviders:       readerProviders,
		LLMProviders:          llmProviders,
		SearchFallbackOnEmpty: cfg.SearchFallbackOnEmpty,
		HasBraveKey:           cfg.BraveAPIKey != "",
		HasTavilyKey:          cfg.TavilyAPIKey != "",
		HasSerperKey:          cfg.SerperAPIKey != "",
		HasFirecrawlKey:       cfg.FirecrawlAPIKey != "",
		HasExaKey:             cfg.ExaAPIKey != "",
		BrowserCDPAddress:     reader.RedactBrowserURL(cfg.BrowserCDPURL),
		HasLLMKey:             cfg.LLMAPIKey != "",
		SearxngURL:            cfg.SearxngURL,
		JinaURL:               cfg.JinaURL,
		LLMBaseURL:            cfg.LLMBaseURL,
		LLMModel:              cfg.LLMModel,
		LLMEndpoints:          endpointViews,
		Services:              copyServices(cfg.Services),
		PaidFallback:          cfg.PaidFallback,
		DeepLoopReviewLines:   h.engine.DeepLoopReviewLines(),
		DeepLoopPush:          h.engine.DeepLoopPush(),
		DeepLoopGrant:         h.engine.DeepLoopGrant(),
		QuietLog:              logquiet.On(),
		SetupDone:             cfg.SetupDone,
		DefaultLLMEndpoint:    cfg.LLMDefaultID,
		ActiveSearchProvider:  h.engine.SearchProviderName(),
		ActiveReaderProvider:  h.engine.ReaderProviderName(),
		Runtime:               newRuntimeView(&cfg),
		Limits:                newLimitsView(&cfg),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func newRuntimeView(cfg *config.Config) RuntimeView {
	return RuntimeView{
		Port:             cfg.Port,
		ConfigTokenSet:   cfg.ConfigToken != "" || cfg.ConfigTokenHash != "",
		PresetIT:         cfg.PresetIT,
		LLMEffortDeep:    cfg.LLMEffortDeep,
		EmbeddingsURL:    cfg.EmbeddingsURL,
		HasEmbeddingsKey: cfg.EmbeddingsAPIKey != "",
		FirecrawlBaseURL: cfg.FirecrawlBaseURL,
	}
}

func newLimitsView(cfg *config.Config) LimitsView {
	return LimitsView{
		SynthesisTimeoutSecs: cfg.SynthesisTimeoutSecs, DeepSessionTimeoutSecs: cfg.DeepSessionTimeoutSecs,
		SynthContentStallSecs: cfg.SynthContentStallSecs,
	}
}

func (h *Handlers) Config(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConfig(w, r) {
		return
	}

	switch r.Method {
	case http.MethodGet:
		h.writeConfigResponse(w)

	case http.MethodPost:
		// Saving can point a key at any address, and the settings code is optional.
		if !sameOrigin(r) {
			writeJSONError(w, http.StatusForbidden, notOwnPage)
			return
		}
		var req ConfigUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
			return
		}

		// Hold the write lock across the whole transaction (candidate build, provider
		// validation, commit). Constructors are in-memory, so this is cheap and
		// prevents lost updates between concurrent POSTs.
		h.cfgMu.Lock()
		candidate := *h.cfg
		candidate.SearchProviders = append([]string(nil), h.cfg.SearchProviders...)
		candidate.ReaderProviders = append([]string(nil), h.cfg.ReaderProviders...)
		candidate.LLMProviders = append([]string(nil), h.cfg.LLMProviders...)
		candidate.LLMEndpoints = append([]config.LLMEndpoint(nil), h.cfg.LLMEndpoints...)

		if err := applyConfigUpdate(&candidate, &req); err != nil {
			h.cfgMu.Unlock()
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := applyServiceChoices(&candidate, &req); err != nil {
			h.cfgMu.Unlock()
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		// Resolve derived defaults before validating the candidate.
		config.Normalize(&candidate)

		// The endpoint registry must be structurally valid (unique ids, non-empty
		// base_url/model, resolvable default). Validate before any commit.
		if err := validateLLMEndpoints(&candidate, req.DefaultLLMEndpoint); err != nil {
			h.cfgMu.Unlock()
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		// The default search provider must be one of the enabled providers
		// (case-insensitive). An empty provider list only accepts the built-in default.
		if !isDefaultSearchProviderValid(&candidate) {
			if req.DefaultSearchProvider != nil {
				h.cfgMu.Unlock()
				writeJSONError(w, http.StatusBadRequest, "unknown default search provider: "+candidate.DefaultSearchProvider)
				return
			}
			// The enabled-provider list changed without an explicit default: heal it.
			if len(candidate.SearchProviders) > 0 {
				candidate.DefaultSearchProvider = candidate.SearchProviders[0]
			} else {
				candidate.DefaultSearchProvider = "searxng"
			}
		}

		// Rebuild SearchProvider (validate before committing).
		newSP, err := search.NewProvider(search.ProviderConfig{
			Providers:       candidate.SearchProviders,
			FallbackOnEmpty: candidate.SearchFallbackOnEmpty,
			SearxngURL:      candidate.SearxngURL,
			BraveAPIKey:     candidate.BraveAPIKey,
			TavilyAPIKey:    candidate.TavilyAPIKey,
			SerperAPIKey:    candidate.SerperAPIKey,
			ExaAPIKey:       candidate.ExaAPIKey,
			ExaSearchType:   candidate.ExaSearchType,
			ExaUserLocation: candidate.ExaUserLocation,
		})
		if err != nil {
			h.cfgMu.Unlock()
			writeJSONError(w, http.StatusBadRequest, "failed to initialize search provider: "+err.Error())
			return
		}

		// Rebuild ReaderProvider.
		newCR, err := reader.NewReader(reader.ReaderConfig{
			Providers:        candidate.ReaderProviders,
			JinaBaseURL:      candidate.JinaURL,
			FirecrawlAPIKey:  candidate.FirecrawlAPIKey,
			FirecrawlBaseURL: candidate.FirecrawlBaseURL,
			ExaAPIKey:        candidate.ExaAPIKey,
			PDF:              PDFReaderConfig(&candidate),
			Browser:          BrowserReaderConfig(&candidate),
		})
		if err != nil {
			h.cfgMu.Unlock()
			writeJSONError(w, http.StatusBadRequest, "failed to initialize content reader: "+err.Error())
			return
		}

		// Rebuild LLM client. NOTE: NewClient(baseURL, apiKey, model) — order matters.
		newLLM := llm.NewClient(candidate.LLMBaseURL, candidate.LLMAPIKey, candidate.LLMModel).
			WithRetryPolicy(llm.NewRetryPolicy(candidate.LLMRetries, candidate.LLMRetryMaxWaitSecs)).
			WithMaxTokens(candidate.LLMMaxTokens).
			WithContextTokens(config.MainContextTokens(&candidate))

		// All rebuilds succeeded: commit the candidate atomically, and save it before
		// another save can start.
		*h.cfg = candidate
		configPath := candidate.ConfigPath
		if strings.TrimSpace(configPath) == "" {
			configPath = "data/config.local"
		}
		if err := config.SaveLocalConfig(configPath, &candidate); err != nil {
			log.Printf("[config] warning: failed to persist local config: %v", err)
		}
		h.cfgMu.Unlock()
		h.capsMu.Lock()
		h.svcCache, h.capsCache = nil, nil
		h.capsMu.Unlock()

		h.engine.SetSearchProvider(newSP)
		h.engine.SetContentReader(newCR)
		h.engine.SetLLMClient(newLLM)
		h.engine.SetDeepLoopReviewLines(candidate.DeepLoopReviewLines)
		h.engine.SetDeepLoopPush(candidate.DeepLoopPush)
		h.engine.SetDeepLoopGrant(candidate.DeepLoopGrant)
		logquiet.Set(candidate.QuietLog)

		h.writeConfigResponse(w)

	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
