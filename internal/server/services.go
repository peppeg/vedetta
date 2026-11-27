package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"vedetta/config"
	"vedetta/internal/llm"
	"vedetta/internal/reader"
	"vedetta/internal/search"
)

// SPEC_UI_IMPOSTAZIONI_E_BARRA: one catalog of every service Vedetta knows.
// Settings turn services on and hold their keys; the top bar picks, among the
// services that are on, what the next search uses.

// ServiceDef is one entry of the catalog.
type ServiceDef struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"` // "search" or "reader"
	Name   string `json:"name"`
	Online bool   `json:"online"`
	Paid   bool   `json:"paid"`
	Needs  string `json:"needs,omitempty"` // "", "url" or "key"
}

var serviceCatalog = []ServiceDef{
	{ID: "searxng", Kind: "search", Name: "SearXNG", Needs: "url"},
	{ID: "degoog", Kind: "search", Name: "degoog", Needs: "url"},
	{ID: "brave", Kind: "search", Name: "Brave Search", Online: true, Paid: true, Needs: "key"},
	{ID: "tavily", Kind: "search", Name: "Tavily", Online: true, Paid: true, Needs: "key"},
	{ID: "serper", Kind: "search", Name: "Serper (Google)", Online: true, Paid: true, Needs: "key"},
	{ID: "exa", Kind: "search", Name: "Exa", Online: true, Paid: true, Needs: "key"},
	{ID: "http", Kind: "reader", Name: "Lettore interno"},
	{ID: "jina", Kind: "reader", Name: "Jina Reader", Needs: "url"},
	{ID: "browser", Kind: "reader", Name: "Browser (Chromium)", Needs: "url"},
	{ID: "firecrawl", Kind: "reader", Name: "Firecrawl", Online: true, Paid: true, Needs: "key"},
	{ID: "exa", Kind: "reader", Name: "Exa", Online: true, Paid: true, Needs: "key"},
}

// LLMPreset fills the address, key reference and price of a well-known
// OpenAI-compatible service when the user adds a model in Settings.
type LLMPreset struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	KeyEnv  string `json:"key_env,omitempty"`
	Paid    bool   `json:"paid"`
	// Tested is false until the preset has been used for real with Vedetta.
	Tested bool `json:"tested"`
}

var llmPresets = []LLMPreset{
	{ID: "openrouter", Name: "OpenRouter", BaseURL: "https://openrouter.ai/api/v1", KeyEnv: "OPENROUTER_API_KEY", Paid: true},
	{ID: "kilo", Name: "Kilo Gateway", BaseURL: "https://api.kilo.ai/api/gateway", KeyEnv: "KILO_API_KEY", Paid: true, Tested: true},
	{ID: "entrim", Name: "Entrim", BaseURL: "https://api.entrim.ai/v1", KeyEnv: "ENTRIM_API_KEY", Paid: true, Tested: true},
	{ID: "ollama-cloud", Name: "Ollama Cloud", BaseURL: "https://ollama.com/v1", KeyEnv: "OLLAMA_API_KEY", Paid: true},
	{ID: "ollama", Name: "Ollama (in casa)", BaseURL: "http://localhost:11434/v1"},
	{ID: "lmstudio", Name: "LM Studio (in casa)", BaseURL: "http://localhost:1234/v1"},
	{ID: "custom", Name: "Altro compatibile OpenAI"},
}

func serviceKey(kind, id string) string { return kind + ":" + id }

func catalogEntry(kind, id string) (ServiceDef, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	switch id {
	case "direct":
		id = "http"
	case "jina-reader":
		id = "jina"
	}
	for _, d := range serviceCatalog {
		if d.Kind == kind && d.ID == id {
			return d, true
		}
	}
	return ServiceDef{}, false
}

func chainOf(cfg *config.Config, kind string) []string {
	if kind == "search" {
		if len(cfg.SearchProviders) == 0 {
			return []string{"searxng"}
		}
		return cfg.SearchProviders
	}
	return cfg.ReaderProviders
}

func inList(list []string, id string) bool {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), id) {
			return true
		}
	}
	return false
}

// serviceOn tells whether a service is switched on. An explicit choice in
// Settings wins; otherwise free services are on and paid ones are off, except
// that a service already listed in a chain counts as on (files written before
// the switch existed, benchmark profiles).
func serviceOn(cfg *config.Config, d ServiceDef) bool {
	if v, ok := cfg.Services[serviceKey(d.Kind, d.ID)]; ok {
		return v
	}
	if inList(chainOf(cfg, d.Kind), d.ID) {
		return true
	}
	return !d.Paid
}

// serviceReady tells whether a service has what it needs to run; when it does
// not, the reason is "missing_key" or "missing_url".
func serviceReady(cfg *config.Config, d ServiceDef) (bool, string) {
	key := ""
	switch d.ID {
	case "brave":
		key = cfg.BraveAPIKey
	case "tavily":
		key = cfg.TavilyAPIKey
	case "serper":
		key = cfg.SerperAPIKey
	case "exa":
		key = cfg.ExaAPIKey
	case "firecrawl":
		key = cfg.FirecrawlAPIKey
	case "browser":
		if strings.TrimSpace(cfg.BrowserCDPURL) == "" {
			return false, "missing_url"
		}
		return true, ""
	case "degoog":
		if search.DegoogBase(cfg.DegoogURL) == "" {
			return false, "missing_url"
		}
		return true, ""
	}
	if d.Needs == "key" && strings.TrimSpace(key) == "" {
		return false, "missing_key"
	}
	return true, ""
}

// serviceUsable: on and ready.
func serviceUsable(cfg *config.Config, d ServiceDef) bool {
	if !serviceOn(cfg, d) {
		return false
	}
	ok, _ := serviceReady(cfg, d)
	return ok
}

// usableReason explains in a short code why a service cannot be chosen.
func usableReason(cfg *config.Config, d ServiceDef) string {
	if !serviceOn(cfg, d) {
		return "off"
	}
	if ok, why := serviceReady(cfg, d); !ok {
		return why
	}
	return ""
}

// fallbackAllowed: a paid service joins a fallback chain only when the user
// said so in Settings (R1); the first element of a chain is always a choice.
func fallbackAllowed(cfg *config.Config, d ServiceDef) bool {
	return !d.Paid || cfg.PaidFallback
}

// requestChain builds the chain for one search when the user picked `chosen`
// in the top bar: the chosen service first, then the configured chain without
// it, keeping only usable services and, unless allowed, no paid fallback.
func requestChain(cfg *config.Config, kind, chosen string) ([]string, error) {
	d, ok := catalogEntry(kind, chosen)
	if !ok {
		return nil, &serviceError{kind: kind, id: chosen, reason: "unknown"}
	}
	if why := usableReason(cfg, d); why != "" {
		return nil, &serviceError{kind: kind, id: d.ID, name: d.Name, reason: why}
	}
	chain := []string{d.ID}
	for _, raw := range chainOf(cfg, kind) {
		e, ok := catalogEntry(kind, raw)
		if !ok || inList(chain, e.ID) || !serviceUsable(cfg, e) || !fallbackAllowed(cfg, e) {
			continue
		}
		chain = append(chain, e.ID)
	}
	return chain, nil
}

// defaultChain rebuilds a chain from Settings: `first` (the default), then the
// other services in `order`, then the rest of the catalog, keeping the usable
// ones and paid ones only when paid fallback is on.
func defaultChain(cfg *config.Config, kind, first string, order []string) []string {
	var chain []string
	add := func(id string, isFirst bool) {
		e, ok := catalogEntry(kind, id)
		if !ok || inList(chain, e.ID) || !serviceUsable(cfg, e) {
			return
		}
		if !isFirst && !fallbackAllowed(cfg, e) {
			return
		}
		chain = append(chain, e.ID)
	}
	if first != "" {
		add(first, true)
	}
	for _, id := range order {
		add(id, len(chain) == 0)
	}
	for _, d := range serviceCatalog {
		if d.Kind == kind {
			add(d.ID, len(chain) == 0)
		}
	}
	return chain
}

type serviceError struct {
	kind, id, name, reason string
}

func (e *serviceError) Error() string {
	switch e.reason {
	case "off":
		return e.kind + " service " + e.id + " is off in settings"
	case "missing_key":
		return e.kind + " service " + e.id + " has no api key"
	case "missing_url":
		return e.kind + " service " + e.id + " has no address"
	default:
		if e.kind == "search" {
			return "unknown search provider: " + e.id
		}
		return "unknown " + e.kind + " service: " + e.id
	}
}

// ---------- /api/services ----------

// ServiceView is one row of the bar and of Settings. Never carries a key.
type ServiceView struct {
	ServiceDef
	HasKey  bool   `json:"has_key"`
	On      bool   `json:"on"`
	Ready   bool   `json:"ready"`
	Reason  string `json:"reason,omitempty"` // why it cannot be chosen: off, missing_key, missing_url
	Status  string `json:"status"`           // ok, configured, down, unknown
	Detail  string `json:"detail,omitempty"`
	Problem string `json:"problem,omitempty"` // map R8: the doctor's finding, said in words by the page
	Version string `json:"version,omitempty"`
	Default bool   `json:"default"`
	InChain bool   `json:"in_chain"`
	URL     string `json:"url,omitempty"` // address of a local service (the browser one redacted)
}

// LLMServiceView is one model endpoint. Never carries a key.
type LLMServiceView struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	BaseURL string   `json:"base_url"`
	Model   string   `json:"model"`
	Online  bool     `json:"online"`
	Paid    bool     `json:"paid"`
	HasKey  bool     `json:"has_key"`
	KeyEnv  string   `json:"key_env,omitempty"`
	Status  string   `json:"status"` // ok, down, missing_key
	Default bool     `json:"default"`
	Models  []string `json:"models,omitempty"`
}

type ServicesResponse struct {
	ProbedAt     string           `json:"probed_at"`
	PaidFallback bool             `json:"paid_fallback"`
	Search       []ServiceView    `json:"search"`
	Reader       []ServiceView    `json:"reader"`
	LLM          []LLMServiceView `json:"llm"`
	LLMPresets   []LLMPreset      `json:"llm_presets"`
}

// Services reports every known service with its state. Probes are cheap and
// never billable: keyed services are checked for the key only, models are
// asked for their list (no chat request, unlike the effort probe).
//
// The top bar needs this list on every page, so it answers without the
// settings code too (VEDETTA_CONFIG_TOKEN); without it, the answer leaves out
// what only Settings needs: addresses, key variable names, error details and
// presets, and it cannot force a new probe.
func (h *Handlers) Services(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	full, _ := h.configCheck(r)
	write := func(resp *ServicesResponse) {
		if !full {
			resp = publicServices(resp)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
	force := full && r.URL.Query().Get("probe") == "1"
	h.capsMu.Lock()
	if !force && h.svcCache != nil && time.Since(h.svcAt) < capabilitiesCacheTTL {
		cached := h.svcCache
		h.capsMu.Unlock()
		write(cached)
		return
	}
	h.capsMu.Unlock()

	h.cfgMu.RLock()
	cfg := *h.cfg
	cfg.SearchProviders = append([]string(nil), h.cfg.SearchProviders...)
	cfg.ReaderProviders = append([]string(nil), h.cfg.ReaderProviders...)
	cfg.LLMEndpoints = append([]config.LLMEndpoint(nil), h.cfg.LLMEndpoints...)
	cfg.Services = copyServices(h.cfg.Services)
	h.cfgMu.RUnlock()

	resp := buildServices(r.Context(), &cfg)

	h.capsMu.Lock()
	h.svcCache = &resp
	h.svcAt = time.Now()
	h.capsMu.Unlock()
	write(&resp)
}

// modelsRequest asks for the model list of an endpoint that may not be saved yet.
type modelsRequest struct {
	EndpointID string `json:"endpoint_id"`
	BaseURL    string `json:"base_url"`
	APIKey     string `json:"api_key"` // a key or a "${NAME}" reference
	Paid       bool   `json:"paid"`
}

type modelsResponse struct {
	Status string          `json:"status"` // ok, down, missing_key
	Models []llm.ModelInfo `json:"models,omitempty"`
}

const notOwnPage = "only Vedetta's own page can do this"

// sameOrigin: true for a request from Vedetta's own page or from outside a
// browser. A browser always names the page behind a cross-site POST, and a page
// cannot set X-Forwarded-Host: the API does not allow that header (server.go).
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host) || strings.EqualFold(u.Host, r.Header.Get("X-Forwarded-Host"))
}

// ServiceModels lists the models of an endpoint, also one the user is still adding:
// the list is what they choose from before saving. It makes no chat request.
//
// It can send a key to the address it is given. The API allows any origin
// (server.go) and the settings code is optional, so it refuses other sites: without
// this, a page the user happens to visit could have a key sent wherever it likes.
// A saved key only ever goes to its own saved address.
func (h *Handlers) ServiceModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.authorizeConfig(w, r) {
		return
	}
	if !sameOrigin(r) {
		writeJSONError(w, http.StatusForbidden, notOwnPage)
		return
	}
	var req modelsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	base := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	if u, err := url.Parse(base); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid address")
		return
	}
	key, env := config.ResolveKeyRef(req.APIKey)
	if env != "" {
		h.cfgMu.RLock()
		allowed := keyRefAllowed(env, base, h.cfg.LLMEndpoints)
		h.cfgMu.RUnlock()
		if !allowed {
			writeJSONError(w, http.StatusBadRequest, errKeyRef(env).Error())
			return
		}
	}
	if strings.TrimSpace(req.APIKey) == "" && req.EndpointID != "" {
		h.cfgMu.RLock()
		for _, ep := range h.cfg.LLMEndpoints {
			if ep.ID == req.EndpointID && strings.TrimRight(strings.TrimSpace(ep.BaseURL), "/") == base {
				key, env = ep.APIKey, ep.KeyEnv
				break
			}
		}
		h.cfgMu.RUnlock()
	}
	resp := modelsResponse{Status: "down"}
	if key == "" && (env != "" || req.Paid) {
		resp.Status = "missing_key" // as in buildServices: a list without a key promises a model that cannot answer
	} else if models, err := llm.NewClient(base, key, "").ListModelInfo(r.Context()); err == nil {
		resp.Status, resp.Models = "ok", models
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// publicServices is the view for a request without the settings code.
// presetsFor points the presets of services at home to this machine: inside a
// container (our compose sets VEDETTA_PUBLISH) "localhost" is the container
// itself, and the compose maps host.docker.internal to the machine (map R8).
func presetsFor(cfg *config.Config) []LLMPreset {
	if cfg.Publish == "" {
		return llmPresets
	}
	out := make([]LLMPreset, len(llmPresets))
	for i, p := range llmPresets {
		p.BaseURL = strings.Replace(p.BaseURL, "://localhost:", "://host.docker.internal:", 1)
		out[i] = p
	}
	return out
}

func publicServices(in *ServicesResponse) *ServicesResponse {
	out := *in
	out.LLMPresets = nil
	strip := func(list []ServiceView) []ServiceView {
		res := make([]ServiceView, len(list))
		for i, s := range list {
			s.URL, s.Detail, s.Problem, s.Version = "", "", "", ""
			res[i] = s
		}
		return res
	}
	out.Search, out.Reader = strip(in.Search), strip(in.Reader)
	out.LLM = make([]LLMServiceView, len(in.LLM))
	for i, e := range in.LLM {
		e.BaseURL, e.KeyEnv = "", ""
		out.LLM[i] = e
	}
	return &out
}

func buildServices(ctx context.Context, cfg *config.Config) ServicesResponse {
	searchCfg := search.ProviderConfig{
		SearxngURL: cfg.SearxngURL, DegoogURL: cfg.DegoogURL, BraveAPIKey: cfg.BraveAPIKey, TavilyAPIKey: cfg.TavilyAPIKey,
		SerperAPIKey: cfg.SerperAPIKey, ExaAPIKey: cfg.ExaAPIKey,
	}
	readerCfg := reader.ReaderConfig{
		JinaBaseURL: cfg.JinaURL, FirecrawlAPIKey: cfg.FirecrawlAPIKey, ExaAPIKey: cfg.ExaAPIKey,
		Browser: BrowserReaderConfig(cfg),
	}
	resp := ServicesResponse{
		ProbedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		PaidFallback: cfg.PaidFallback,
		LLMPresets:   presetsFor(cfg),
	}
	readerChain := chainOf(cfg, "reader")
	for _, d := range serviceCatalog {
		ready, why := serviceReady(cfg, d)
		v := ServiceView{ServiceDef: d, On: serviceOn(cfg, d), Ready: ready, InChain: inList(chainOf(cfg, d.Kind), d.ID)}
		v.HasKey = d.Needs == "key" && why != "missing_key"
		v.Reason = usableReason(cfg, d)
		switch d.ID {
		case "searxng":
			v.URL = cfg.SearxngURL
		case "degoog":
			v.URL = cfg.DegoogURL
		case "jina":
			v.URL = cfg.JinaURL
		case "browser":
			v.URL = reader.RedactBrowserURL(cfg.BrowserCDPURL)
		}
		if d.Kind == "search" {
			v.Default = strings.EqualFold(d.ID, cfg.DefaultSearchProvider)
			ps := search.ProbeProvider(ctx, d.ID, searchCfg)
			v.Status, v.Detail, v.Problem, v.Version = ps.Status, ps.Detail, ps.Problem, ps.Version
			resp.Search = append(resp.Search, v)
		} else {
			v.Default = len(readerChain) > 0 && inList(readerChain[:1], d.ID)
			ps := reader.ProbeReader(ctx, d.ID, readerCfg)
			v.Status, v.Detail, v.Problem = ps.Status, ps.Detail, ps.Problem
			resp.Reader = append(resp.Reader, v)
		}
	}
	for _, ep := range cfg.LLMEndpoints {
		v := LLMServiceView{
			ID: ep.ID, Name: ep.Name, BaseURL: ep.BaseURL, Model: ep.Model,
			Online: !config.IsLocalAddress(ep.BaseURL), Paid: ep.Paid != nil && *ep.Paid,
			HasKey: ep.APIKey != "", KeyEnv: ep.KeyEnv, Status: "down",
			Default: ep.ID == cfg.LLMDefaultID,
		}
		if ep.APIKey == "" && (ep.KeyEnv != "" || v.Paid) {
			// Some services list their models without a key: "responds" would
			// promise a model that cannot answer.
			v.Status = "missing_key"
			resp.LLM = append(resp.LLM, v)
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		if models, err := llm.NewClient(ep.BaseURL, ep.APIKey, ep.Model).ListModels(pctx); err == nil {
			v.Status, v.Models = "ok", models
		}
		cancel()
		resp.LLM = append(resp.LLM, v)
	}
	return resp
}

// copyServices keeps nil as nil: nil means "no choice saved yet".
func copyServices(m map[string]bool) map[string]bool {
	if m == nil {
		return nil
	}
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// applyServiceChoices records the Settings switches and rebuilds both default
// chains from them: the default search service, then the reader order sent
// (its first element is the default reader). It runs after the keys of the
// same request are applied, because readiness depends on them.
func applyServiceChoices(cfg *config.Config, req *ConfigUpdateRequest) error {
	if req.Services == nil && req.PaidFallback == nil {
		return nil
	}
	if req.PaidFallback != nil {
		cfg.PaidFallback = *req.PaidFallback
	}
	if req.Services != nil {
		merged := copyServices(cfg.Services)
		if merged == nil {
			// first save: freeze what was implicitly on, so nothing changes by surprise
			merged = map[string]bool{}
			for _, d := range serviceCatalog {
				merged[serviceKey(d.Kind, d.ID)] = serviceOn(cfg, d)
			}
		}
		for k, v := range req.Services {
			k = strings.ToLower(strings.TrimSpace(k))
			kind, id, ok := strings.Cut(k, ":")
			if _, known := catalogEntry(kind, id); !ok || !known {
				return &serviceError{kind: kind, id: k, reason: "unknown"}
			}
			merged[k] = v
		}
		cfg.Services = merged
	}
	first := cfg.DefaultSearchProvider
	if req.DefaultSearchProvider != nil {
		first = strings.ToLower(strings.TrimSpace(*req.DefaultSearchProvider))
	}
	if d, ok := catalogEntry("search", first); ok {
		if why := usableReason(cfg, d); why != "" {
			return &serviceError{kind: "search", id: d.ID, name: d.Name, reason: why}
		}
	}
	searchChain := defaultChain(cfg, "search", first, cfg.SearchProviders)
	if len(searchChain) == 0 {
		return errNoChain("search")
	}
	order := cfg.ReaderProviders
	if len(req.ReaderProviders) > 0 {
		order = req.ReaderProviders
	}
	readerFirst := ""
	if len(order) > 0 {
		readerFirst = order[0]
		if d, ok := catalogEntry("reader", readerFirst); ok {
			if why := usableReason(cfg, d); why != "" {
				return &serviceError{kind: "reader", id: d.ID, name: d.Name, reason: why}
			}
		}
	}
	readerChain := defaultChain(cfg, "reader", readerFirst, order)
	if len(readerChain) == 0 {
		return errNoChain("reader")
	}
	cfg.SearchProviders, cfg.SearchProvider = searchChain, searchChain[0]
	cfg.DefaultSearchProvider = searchChain[0]
	cfg.ReaderProviders = readerChain
	return nil
}

type errNoChain string

func (e errNoChain) Error() string {
	return "at least one " + string(e) + " service must be on and ready"
}
