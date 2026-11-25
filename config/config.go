package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LLMEndpoint describes one OpenAI-compatible LLM backend. APIKey is a secret:
// it is persisted in the local config file but never returned to API clients.
type LLMEndpoint struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	APIKey  string `json:"api_key,omitempty"`
	// Paid marks a service that consumes credit (shown with "$" in the UI).
	// nil means "not decided yet": Normalize fills it from the address and key.
	Paid *bool `json:"paid,omitempty"`
	// KeyEnv is the name of the environment variable the key comes from when the
	// stored key is a "${NAME}" reference; the reference, never the value, is saved.
	KeyEnv string `json:"-"`
	// ContextTokens is the model's memory set by hand; 0 reads it from the
	// server (DL3, docs/SPEC_DL3_MEMORIA_DEL_MODELLO.md).
	ContextTokens int `json:"context_tokens,omitempty"`
}

// ResolveKeyRef turns a "${NAME}" key reference into the variable's value.
// It returns the value and the variable name; a plain key comes back as is.
func ResolveKeyRef(raw string) (value, env string) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "${") && strings.HasSuffix(raw, "}") && len(raw) > 3 {
		env = strings.TrimSpace(raw[2 : len(raw)-1])
		return strings.TrimSpace(os.Getenv(env)), env
	}
	return raw, ""
}

// storedKey is what goes to disk for an endpoint key: the reference when there is one.
func (ep LLMEndpoint) storedKey() string {
	if ep.KeyEnv != "" {
		return "${" + ep.KeyEnv + "}"
	}
	return ep.APIKey
}

// IsLocalAddress reports whether a base URL points at this machine or a private
// network (LAN, Tailscale): such services are not paid by default.
func IsLocalAddress(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Hostname() == "" {
		return true
	}
	h := u.Hostname()
	if h == "localhost" || strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".lan") || strings.HasSuffix(h, ".ts.net") || !strings.Contains(h, ".") {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return cgnat.Contains(ip)
}

type Config struct {
	Version               string
	ContactEmail          string
	DefaultSearchProvider string
	Port                  string
	// Map R6: where Vedetta listens (127.0.0.1: this machine only), the access code
	// asked to every device when it listens farther, the other sites allowed to call
	// the API, and the host names accepted besides IPs and home names.
	Bind       string
	AccessCode string
	// Map R8: in a container Vedetta listens on 0.0.0.0 and cannot see where the
	// port is published; the compose tells it here (VEDETTA_PUBLISH, the same value
	// it publishes the port on). Empty outside a container: the bind address counts.
	Publish string
	// Map R8: the first run in the browser is over (or skipped). A config.local
	// from before R8 counts as done; VEDETTA_SETUP=done skips it for scripts.
	SetupDone bool
	// Map R6b: the codes set in Settings, as salted hashes (config.local); a code in
	// .env (AccessCode, ConfigToken) wins over them.
	AccessCodeHash  string
	ConfigTokenHash string
	CORSOrigins     []string
	AllowedHosts    []string

	SearchProviders []string
	ReaderProviders []string
	// Services holds the on/off switch chosen in Settings, keyed "search:brave",
	// "reader:exa". A missing entry means the catalog default (free on, paid off).
	// nil means the file predates the switch: what the chains list counts as on.
	Services map[string]bool
	// PaidFallback lets paid services that are on act as automatic fallback.
	// Off by default: a paid service is used only when chosen.
	PaidFallback           bool
	LLMProviders           []string
	SearchFallbackOnEmpty  bool
	SearchProvider         string
	SearchFallbackProvider string
	BraveAPIKey            string
	TavilyAPIKey           string
	SerperAPIKey           string
	SearxngURL             string
	JinaURL                string
	FirecrawlAPIKey        string
	FirecrawlBaseURL       string
	ExaAPIKey              string
	ExaSearchType          string // "auto" or "lite" (docs/SPEC_EXA_PROFILO.md)
	ExaUserLocation        string // ISO country code for Exa search, "" = none
	BrowserCDPURL          string // running headless browser for the "browser" reader (docs/SPEC_BROWSER_CDP.md), "" = none
	BrowserMaxPages        int
	BrowserPageTimeoutSecs int
	LLMBaseURL             string
	LLMModel               string
	LLMAPIKey              string
	LLMEndpoints           []LLMEndpoint
	LLMDefaultID           string
	LLMEffortDeep          string
	ConfigToken            string
	ConfigPath             string
	EmbeddingsURL          string
	EmbeddingsAPIKey       string
	SearxngMaxResults      int
	// Built-in PDF reader (V25-PDF.4): relevant PDFs are read in full by
	// default (PDFMaxPages 0); bytes and time are safety limits only.
	PDFReader              bool
	PDFMaxBytes            int
	PDFMaxPages            int
	PDFTimeoutSecs         int
	PDFIdleSecs            int
	PDFCacheDir            string
	LLMRetries             int // V25-T7.1: retries before the response on 429/5xx and dropped connections, 0 = off
	LLMRetryMaxWaitSecs    int // longest single wait; a server asking more ends the retries at once
	LLMMaxTokens           int // max_tokens per request: 0 = the model's output cap from /models, > 0 = this, < 0 = never sent
	LLMContextTokens       int // the default service's memory set by hand, 0 = read from the server; an endpoint's context_tokens wins
	DeepSessionTimeoutSecs int
	SynthesisTimeoutSecs   int
	SynthContentStallSecs  int
	IntentRerank           bool
	AuthorityBoost         float64
	ProjectPenalty         float64
	MunicipalPenalty       float64
	PresetIT               bool
	DeepLoopMaxCalls       int // Deep a ciclo: searches and readings before the invitation to write
	// Deep a ciclo sampling (P0, docs/APPUNTI_MOTORI_INFERENZA.md): temperature "" keeps the
	// writer's, "model" lets the server apply the model's recommended sampling, a number sets it;
	// top_p/top_k are sent only when > 0. KeepReasoning sends the model's reasoning back (P0b).
	DeepLoopTemperature    string
	DeepLoopTopP           float64
	DeepLoopTopK           int
	DeepLoopKeepReasoning  bool
	DeepLoopReview         bool          // Deep a ciclo: the sentinel sends back details not found in the sources
	DeepLoopCompass        bool          // Deep a ciclo: the system prompt warns that the model's memory may be out of date
	DeepLoopReviewPreviews bool          // Deep a ciclo: the sentinel opens the pages of details seen only in search previews
	DeepLoopCiteRead       bool          // Deep a ciclo: the instruction stops calling unopened results sources
	DeepLoopReviewLines    bool          // Deep a ciclo: after the sentinel's review the model rewrites only the lines sent back
	DeepLoopLinkCheck      bool          // Deep a ciclo: the sentinel opens the cited pages the model did not read and compares them with the sentences
	DeepLoopRerank         bool          // Deep a ciclo: reorder (and filter) the search results before the model sees them; off = the search service's order (Piano dopo audit 3.1)
	DeepLoopMaxResults     int           // Deep a ciclo: results per search asked and shown; 0 = SEARXNG_MAX_RESULTS, as the rest (Piano dopo audit 3.2)
	DeepLoopSearchLang     string        // Deep a ciclo: language of a search the model gave none: "answer" (the answer's) or "auto" (the search service decides) (Piano dopo audit 3.3)
	DeepLoopSessionPages   bool          // Deep a ciclo: in a follow-up the pages read in the earlier turns count as seen and are listed to the model (Piano dopo audit 4.4)
	DeepLoopQuotes         bool          // Deep a ciclo: after the answer, the model is asked for the passage of a page that says each sentence, and Go looks for it on the page (Piano dopo audit, fase 6)
	DeepLoopQuoteFallback  bool          // Deep a ciclo: an empty search with quotes or site: is tried again without them, and the model is told (Piano dopo audit 3.4)
	DeepLoopPush           bool          // Deep a ciclo: at the first draft the model is asked once whether there is more to look for, before the sentinel's checks (Piano dopo audit 7.13)
	DeepLoopGrant          bool          // Deep a ciclo: at the cap, a research still opening new pages gets more calls, twice at most (docs/SPEC_PERMESSO_SPECIALE.md)
	HTTPReaderVersion      int           // how the http and browser readers turn HTML into text: 1 (today) or 2 (links, article header, table cells) (Piano dopo audit 3.5)
	CacheReadDate          bool          // Deep a ciclo: a page read on another day (from the cache) says when, to the model and in "Pagine lette" (Piano dopo audit 3.6)
	DeepLoopNameWords      bool          // Deep a ciclo: the sentinel looks for names as whole words, not inside longer ones (Piano dopo audit 4.1)
	LangDetect             int           // how the answer's language is read from the question: 2 (default: the asking words weigh more; titles and quotes less) or 1 (the old way) (Piano dopo audit 4.3)
	CacheMode              string        // V25-R1: off | record | replay
	CacheDir               string        // V25-R1: one JSON file per cached search or page
	CacheSearchTTL         time.Duration // V25-R1: search expiry in record mode
	CachePageTTL           time.Duration // V25-R1: page expiry in record mode
	CachePrune             bool          // map R2: expired entries are deleted every hour in record mode (the bench turns it off: a live round's folder is replayed later, expired entries included)

	// Sperimentale (map R3): DeepLoopReviewLines, DeepLoopPush and DeepLoopGrant can be
	// chosen in Settings. Chosen means saved in config.local, where the choice wins over .env;
	// not chosen, the .env value holds and nothing is saved.
	DeepLoopReviewLinesChosen bool
	DeepLoopPushChosen        bool
	DeepLoopGrantChosen       bool

	// Map R7: "Registro con meno dettagli": the log drops searches and addresses.
	// From VEDETTA_QUIET_LOG, or chosen in Settings (saved, stronger than .env).
	QuietLog       bool
	QuietLogChosen bool
	// DeepThought (Sperimentale) is set from Settings only: deepthought.go.
	DeepThought bool
}

func Load() *Config {
	loadDotEnv(".env")

	searchProviders := parseListEnv("SEARCH_PROVIDERS")
	searchProvLegacy := getEnv("SEARCH_PROVIDER", "searxng")
	searchFallbackLegacy := getEnv("SEARCH_FALLBACK_PROVIDER", "none")

	if len(searchProviders) == 0 {
		searchProviders = append(searchProviders, searchProvLegacy)
		if searchFallbackLegacy != "" && searchFallbackLegacy != "none" && searchFallbackLegacy != searchProvLegacy {
			searchProviders = append(searchProviders, searchFallbackLegacy)
		}
	}

	readerProviders := parseListEnv("READER_PROVIDERS")
	if len(readerProviders) == 0 {
		readerProviders = []string{"http", "jina"}
	}

	llmProviders := parseListEnv("LLM_PROVIDERS")
	if len(llmProviders) == 0 {
		llmProviders = []string{"deepseek"}
	}

	primarySearchProv := searchProviders[0]

	cfg := &Config{
		ContactEmail:           getEnv("VEDETTA_CONTACT_EMAIL", ""),
		Port:                   getEnv("VEDETTA_PORT", "3030"),
		Bind:                   getEnv("VEDETTA_BIND", "127.0.0.1"),
		Publish:                strings.TrimSpace(getEnv("VEDETTA_PUBLISH", "")),
		SetupDone:              strings.EqualFold(strings.TrimSpace(getEnv("VEDETTA_SETUP", "")), "done"),
		AccessCode:             strings.TrimSpace(getEnv("VEDETTA_ACCESS_CODE", "")),
		CORSOrigins:            parseRawListEnv("VEDETTA_CORS_ORIGINS"),
		AllowedHosts:           parseListEnv("VEDETTA_ALLOWED_HOSTS"),
		SearchProviders:        searchProviders,
		ReaderProviders:        readerProviders,
		LLMProviders:           llmProviders,
		SearchFallbackOnEmpty:  getBoolEnv("SEARCH_FALLBACK_ON_EMPTY", false),
		SearchProvider:         primarySearchProv,
		SearchFallbackProvider: searchFallbackLegacy,
		BraveAPIKey:            getEnv("BRAVE_API_KEY", ""),
		TavilyAPIKey:           getEnv("TAVILY_API_KEY", ""),
		SerperAPIKey:           getEnv("SERP_API_KEY", ""),
		SearxngURL:             getEnv("SEARXNG_API_URL", "http://localhost:8888"),
		JinaURL:                getEnv("JINA_READER_URL", "http://localhost:3001"),
		FirecrawlAPIKey:        getEnv("FIRECRAWL_API_KEY", ""),
		FirecrawlBaseURL:       getEnv("FIRECRAWL_BASE_URL", "https://api.firecrawl.dev/v1"),
		ExaAPIKey:              getEnv("EXA_API_KEY", ""),
		ExaSearchType:          getEnv("EXA_SEARCH_TYPE", "auto"),
		ExaUserLocation:        getEnv("EXA_USER_LOCATION", ""),
		BrowserCDPURL:          getEnv("BROWSER_CDP_URL", ""),
		BrowserMaxPages:        getIntEnv("BROWSER_MAX_PAGES", 2),
		BrowserPageTimeoutSecs: getIntEnv("BROWSER_PAGE_TIMEOUT_SECS", 20),
		LLMBaseURL:             getEnv("LLM_BASE_URL", "http://localhost:8000/v1"),
		LLMModel:               getEnv("LLM_MODEL", "deepseek-chat"),
		LLMAPIKey:              getEnv("LLM_API_KEY", ""),
		LLMEffortDeep:          getEnv("LLM_EFFORT_DEEP", "medium"),
		ConfigToken:            getEnv("VEDETTA_CONFIG_TOKEN", ""),
		ConfigPath:             getEnv("VEDETTA_CONFIG_PATH", "data/config.local"),
		EmbeddingsURL:          getEnv("EMBEDDINGS_URL", ""),
		EmbeddingsAPIKey:       getEnv("EMBEDDINGS_API_KEY", ""),
		SearxngMaxResults:      getIntEnv("SEARXNG_MAX_RESULTS", 10),
		PDFReader:              getBoolEnv("VEDETTA_PDF_READER", true),
		PDFMaxBytes:            getIntEnv("VEDETTA_PDF_MAX_BYTES", 50<<20),
		PDFMaxPages:            getIntEnv("VEDETTA_PDF_MAX_PAGES", 0),
		PDFTimeoutSecs:         getIntEnv("VEDETTA_PDF_TIMEOUT_SECS", 60),
		PDFIdleSecs:            getIntEnv("VEDETTA_PDF_IDLE_SECS", 300),
		PDFCacheDir:            getEnv("VEDETTA_PDF_CACHE_DIR", "data/pdfium-cache"),
		LLMRetries:             getNonNegIntEnv("VEDETTA_LLM_RETRIES", 4),
		LLMRetryMaxWaitSecs:    getIntEnv("VEDETTA_LLM_RETRY_MAX_WAIT_SECS", 30),
		LLMMaxTokens:           getIntEnv("VEDETTA_LLM_MAX_TOKENS", 0),
		LLMContextTokens:       getNonNegIntEnv("VEDETTA_LLM_CONTEXT_TOKENS", 0),
		DeepSessionTimeoutSecs: getIntEnv("DEEP_SESSION_TIMEOUT_SECS", 1200),
		SynthesisTimeoutSecs:   getIntEnv("SYNTHESIS_TIMEOUT_SECS", 1200),
		SynthContentStallSecs:  getIntEnv("VEDETTA_SYNTH_CONTENT_STALL_SECS", 360),
		IntentRerank:           getBoolEnv("VEDETTA_INTENT_RERANK", true),
		AuthorityBoost:         getFloatEnv("VEDETTA_AUTHORITY_BOOST", 0.30),
		ProjectPenalty:         getFloatEnv("VEDETTA_PROJECT_PENALTY", 0.50),
		MunicipalPenalty:       getFloatEnv("VEDETTA_MUNICIPAL_PENALTY", 0.80),
		PresetIT:               getBoolEnv("VEDETTA_PRESET_IT", false),
		DeepLoopMaxCalls:       getIntEnv("VEDETTA_DEEP_LOOP_MAX_CALLS", 40),
		DeepLoopTemperature:    getEnv("VEDETTA_DEEP_LOOP_TEMPERATURE", "1.0"),
		DeepLoopTopP:           getFloatEnv("VEDETTA_DEEP_LOOP_TOP_P", 0.95),
		DeepLoopTopK:           getIntEnv("VEDETTA_DEEP_LOOP_TOP_K", 0),
		DeepLoopKeepReasoning:  getBoolEnv("VEDETTA_DEEP_LOOP_KEEP_REASONING", true),
		DeepLoopReview:         getBoolEnv("VEDETTA_DEEP_LOOP_REVIEW", true),
		DeepLoopCompass:        getBoolEnv("VEDETTA_DEEP_LOOP_COMPASS", false),
		DeepLoopReviewPreviews: getBoolEnv("VEDETTA_DEEP_LOOP_REVIEW_PREVIEWS", true),
		DeepLoopCiteRead:       getBoolEnv("VEDETTA_DEEP_LOOP_CITE_READ", true),
		DeepLoopLinkCheck:      getBoolEnv("VEDETTA_DEEP_LOOP_LINK_CHECK", true),
		DeepLoopRerank:         getBoolEnv("VEDETTA_DEEP_LOOP_RERANK", true),
		DeepLoopMaxResults:     getIntEnv("VEDETTA_DEEP_LOOP_MAX_RESULTS", 0),
		DeepLoopSearchLang:     getEnv("VEDETTA_DEEP_LOOP_SEARCH_LANG", "answer"),
		DeepLoopQuoteFallback:  getBoolEnv("VEDETTA_DEEP_LOOP_QUOTE_FALLBACK", true),
		DeepLoopSessionPages:   getBoolEnv("VEDETTA_DEEP_LOOP_SESSION_PAGES", true),
		DeepLoopQuotes:         getBoolEnv("VEDETTA_DEEP_LOOP_QUOTES", true),
		DeepLoopPush:           getBoolEnv("VEDETTA_DEEP_LOOP_PUSH", false),
		DeepLoopGrant:          getBoolEnv("VEDETTA_DEEP_LOOP_GRANT", false),
		HTTPReaderVersion:      getIntEnv("VEDETTA_HTTP_READER", 1),
		CacheReadDate:          getBoolEnv("VEDETTA_CACHE_READ_DATE", true),
		DeepLoopNameWords:      getBoolEnv("VEDETTA_DEEP_LOOP_NAME_WORDS", false),
		LangDetect:             getIntEnv("VEDETTA_LANG_DETECT", 2),
		DeepLoopReviewLines:    getBoolEnv("VEDETTA_DEEP_LOOP_REVIEW_LINES", false),
		CacheMode:              parseCacheMode(getEnv("VEDETTA_CACHE_MODE", "record")),
		CacheDir:               getEnv("VEDETTA_CACHE_DIR", "data/cache"),
		CacheSearchTTL:         getDurationEnv("VEDETTA_CACHE_SEARCH_TTL", 24*time.Hour),
		CachePageTTL:           getDurationEnv("VEDETTA_CACHE_PAGE_TTL", 3*time.Hour), // 18/11: a day-old page misleads on prices (a9fe25e5); 51 of 58 reuses across searches in production were within 3 hours
		CachePrune:             getBoolEnv("VEDETTA_CACHE_PRUNE", true),
		QuietLog:               getBoolEnv("VEDETTA_QUIET_LOG", false),
	}

	if err := loadLocalConfig(cfg.ConfigPath, cfg); err != nil {
		log.Fatalf("[config] %v", err)
	}
	Normalize(cfg)
	return cfg
}

// Normalize fills derived/default fields after load or update.
func Normalize(cfg *Config) {
	if cfg == nil {
		return
	}
	if cfg.DefaultSearchProvider == "" {
		if len(cfg.SearchProviders) > 0 {
			cfg.DefaultSearchProvider = cfg.SearchProviders[0]
		} else {
			cfg.DefaultSearchProvider = "searxng"
		}
	}

	// Backward compatibility: legacy single-LLM configs become a one-element
	// registry whose endpoint carries the legacy values.
	if len(cfg.LLMEndpoints) == 0 {
		cfg.LLMEndpoints = []LLMEndpoint{{
			ID:      "default",
			Name:    "default",
			BaseURL: cfg.LLMBaseURL,
			Model:   cfg.LLMModel,
			APIKey:  cfg.LLMAPIKey,
		}}
	}
	// Resolve an empty or dangling default id to the first endpoint.
	if _, ok := EndpointByID(cfg.LLMEndpoints, cfg.LLMDefaultID); !ok {
		cfg.LLMDefaultID = cfg.LLMEndpoints[0].ID
	}
	// An endpoint is paid by default when it is online and needs a key.
	for i := range cfg.LLMEndpoints {
		if cfg.LLMEndpoints[i].Paid == nil {
			paid := cfg.LLMEndpoints[i].APIKey != "" && !IsLocalAddress(cfg.LLMEndpoints[i].BaseURL)
			cfg.LLMEndpoints[i].Paid = &paid
		}
	}
	// Always mirror the default endpoint onto the legacy fields so existing
	// code paths (main.go, llm.NewClient, legacy handlers) keep working.
	if def, ok := EndpointByID(cfg.LLMEndpoints, cfg.LLMDefaultID); ok {
		cfg.LLMBaseURL = def.BaseURL
		cfg.LLMModel = def.Model
		cfg.LLMAPIKey = def.APIKey
	}
}

// EndpointByID returns the endpoint with the given id.
func EndpointByID(endpoints []LLMEndpoint, id string) (LLMEndpoint, bool) {
	for _, ep := range endpoints {
		if ep.ID == id {
			return ep, true
		}
	}
	return LLMEndpoint{}, false
}

// ContextTokensFor is the memory set by hand for a service: its own
// context_tokens, or VEDETTA_LLM_CONTEXT_TOKENS for the default service.
// 0 means read it from the server.
func ContextTokensFor(cfg *Config, ep LLMEndpoint) int {
	if ep.ContextTokens > 0 {
		return ep.ContextTokens
	}
	if cfg == nil {
		return 0
	}
	if def, ok := DefaultEndpoint(cfg); ok && def.ID != ep.ID {
		return 0
	}
	return cfg.LLMContextTokens
}

// MainContextTokens is ContextTokensFor the default service.
func MainContextTokens(cfg *Config) int {
	def, _ := DefaultEndpoint(cfg)
	return ContextTokensFor(cfg, def)
}

// DefaultEndpoint returns the effective default endpoint. It honours
// LLMDefaultID and falls back to the first configured endpoint when the id is
// empty or missing.
func DefaultEndpoint(cfg *Config) (LLMEndpoint, bool) {
	if cfg == nil || len(cfg.LLMEndpoints) == 0 {
		return LLMEndpoint{}, false
	}
	if ep, ok := EndpointByID(cfg.LLMEndpoints, cfg.LLMDefaultID); ok {
		return ep, true
	}
	return cfg.LLMEndpoints[0], true
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getIntEnv(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil && i > 0 {
			return i
		}
	}
	return defaultVal
}

// getNonNegIntEnv is getIntEnv that also accepts 0.
func getNonNegIntEnv(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil && i >= 0 {
			return i
		}
	}
	return defaultVal
}

func getBoolEnv(key string, defaultVal bool) bool {
	if val := os.Getenv(key); val != "" {
		lower := strings.ToLower(strings.TrimSpace(val))
		if lower == "true" || lower == "1" || lower == "yes" {
			return true
		}
		if lower == "false" || lower == "0" || lower == "no" {
			return false
		}
	}
	return defaultVal
}

func getFloatEnv(key string, defaultVal float64) float64 {
	if val := os.Getenv(key); val != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
			return f
		}
	}
	return defaultVal
}

func getDurationEnv(key string, defaultVal time.Duration) time.Duration {
	if val := os.Getenv(key); val != "" {
		if d, err := time.ParseDuration(strings.TrimSpace(val)); err == nil && d > 0 {
			return d
		}
	}
	return defaultVal
}

// parseCacheMode accepts off, record and replay; anything else turns the cache
// off, so a typo never changes what Vedetta reads from the network.
func parseCacheMode(val string) string {
	mode := strings.ToLower(strings.TrimSpace(val))
	switch mode {
	case "off", "record", "replay":
		return mode
	}
	log.Printf("[config] VEDETTA_CACHE_MODE=%q is not off, record or replay: cache disabled", val)
	return "off"
}

// loadDotEnv parses a simple KEY=VALUE file without external dependencies
func loadDotEnv(filepath string) {
	file, err := os.Open(filepath)
	if err != nil {
		return // File does not exist, ignore silently
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			k := strings.TrimSpace(parts[0])
			v := strings.Trim(strings.TrimSpace(parts[1]), "\"'")
			if os.Getenv(k) == "" {
				_ = os.Setenv(k, v)
			}
		}
	}
}

// parseRawListEnv splits a comma list keeping each item as written (trimmed): web
// origins keep their scheme and port.
func parseRawListEnv(key string) []string {
	var out []string
	for _, p := range strings.Split(os.Getenv(key), ",") {
		if p = strings.TrimRight(strings.TrimSpace(p), "/"); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ReachAddr is the address that says who can reach Vedetta: where a container
// publishes the port when the compose says so (map R8), else where Vedetta listens.
func (c *Config) ReachAddr() string {
	if c.Publish != "" {
		return c.Publish
	}
	return c.Bind
}

// ListensBeyondMachine reports whether Vedetta is reachable from other devices:
// any bind address but a loopback one (map R6).
func ListensBeyondMachine(bind string) bool {
	b := strings.Trim(strings.TrimSpace(bind), "[]")
	if b == "localhost" {
		return false
	}
	ip := net.ParseIP(b)
	return ip == nil || !ip.IsLoopback()
}

func parseListEnv(key string) []string {
	val := os.Getenv(key)
	if strings.TrimSpace(val) == "" {
		return nil
	}
	parts := strings.Split(val, ",")
	var result []string
	seen := make(map[string]bool)
	for _, p := range parts {
		clean := strings.ToLower(strings.TrimSpace(p))
		if clean != "" && clean != "none" && !seen[clean] {
			seen[clean] = true
			result = append(result, clean)
		}
	}
	return result
}

// LocalConfigFile models the JSON structure stored in data/config.local with 0600 permissions.
type LocalConfigFile struct {
	SearchProviders       []string        `json:"search_providers,omitempty"`
	ReaderProviders       []string        `json:"reader_providers,omitempty"`
	LLMProviders          []string        `json:"llm_providers,omitempty"`
	SearchFallbackOnEmpty *bool           `json:"search_fallback_on_empty,omitempty"`
	BraveAPIKey           string          `json:"brave_api_key,omitempty"`
	TavilyAPIKey          string          `json:"tavily_api_key,omitempty"`
	SerperAPIKey          string          `json:"serper_api_key,omitempty"`
	FirecrawlAPIKey       string          `json:"firecrawl_api_key,omitempty"`
	ExaAPIKey             string          `json:"exa_api_key,omitempty"`
	ExaSearchType         string          `json:"exa_search_type,omitempty"`
	ExaUserLocation       string          `json:"exa_user_location,omitempty"`
	BrowserCDPURL         string          `json:"browser_cdp_url,omitempty"`
	SearxngURL            string          `json:"searxng_url,omitempty"`
	JinaURL               string          `json:"jina_url,omitempty"`
	LLMEndpoints          []LLMEndpoint   `json:"llm_endpoints,omitempty"`
	LLMDefaultID          string          `json:"llm_default_id,omitempty"`
	LLMBaseURL            string          `json:"llm_base_url,omitempty"`
	LLMModel              string          `json:"llm_model,omitempty"`
	LLMAPIKey             string          `json:"llm_api_key,omitempty"`
	ContactEmail          string          `json:"contact_email,omitempty"`
	DefaultSearchProvider string          `json:"default_search_provider,omitempty"`
	Services              map[string]bool `json:"services,omitempty"`
	PaidFallback          *bool           `json:"paid_fallback,omitempty"`
	// Sperimentale (map R3): present only when chosen in Settings.
	DeepLoopReviewLines *bool `json:"deep_loop_review_lines,omitempty"`
	DeepLoopPush        *bool `json:"deep_loop_push,omitempty"`
	DeepLoopGrant       *bool `json:"deep_loop_grant,omitempty"`
	// Map R7: present only when chosen in Settings.
	QuietLog    *bool `json:"quiet_log,omitempty"`
	DeepThought bool  `json:"deep_thought,omitempty"`
	// Map R6b: codes set in Settings, as salted hashes, never the codes.
	AccessCodeHash  string `json:"access_code_hash,omitempty"`
	ConfigTokenHash string `json:"settings_code_hash,omitempty"`
	// Map R8: absent in files written before R8, which count as done.
	SetupDone *bool `json:"setup_done,omitempty"`
}

// SaveLocalConfig safely writes runtime overrides to data/config.local with 0600 permissions.
func SaveLocalConfig(path string, cfg *Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// Mirror the effective default endpoint onto the legacy fields so older
	// builds keep loading these files.
	legacyBaseURL, legacyModel, legacyAPIKey := cfg.LLMBaseURL, cfg.LLMModel, cfg.LLMAPIKey
	if def, ok := DefaultEndpoint(cfg); ok {
		legacyBaseURL, legacyModel, legacyAPIKey = def.BaseURL, def.Model, def.storedKey()
	}
	// Keys read from the environment are saved as their "${NAME}" reference.
	endpoints := make([]LLMEndpoint, len(cfg.LLMEndpoints))
	for i, ep := range cfg.LLMEndpoints {
		ep.APIKey = ep.storedKey()
		endpoints[i] = ep
	}
	paidFallback := cfg.PaidFallback
	f := LocalConfigFile{
		SearchProviders:       cfg.SearchProviders,
		ReaderProviders:       cfg.ReaderProviders,
		LLMProviders:          cfg.LLMProviders,
		SearchFallbackOnEmpty: &cfg.SearchFallbackOnEmpty,
		BraveAPIKey:           cfg.BraveAPIKey,
		TavilyAPIKey:          cfg.TavilyAPIKey,
		SerperAPIKey:          cfg.SerperAPIKey,
		FirecrawlAPIKey:       cfg.FirecrawlAPIKey,
		ExaAPIKey:             cfg.ExaAPIKey,
		ExaSearchType:         cfg.ExaSearchType,
		ExaUserLocation:       cfg.ExaUserLocation,
		BrowserCDPURL:         cfg.BrowserCDPURL,
		SearxngURL:            cfg.SearxngURL,
		JinaURL:               cfg.JinaURL,
		LLMEndpoints:          endpoints,
		LLMDefaultID:          cfg.LLMDefaultID,
		LLMBaseURL:            legacyBaseURL,
		LLMModel:              legacyModel,
		LLMAPIKey:             legacyAPIKey,
		ContactEmail:          cfg.ContactEmail,
		DefaultSearchProvider: cfg.DefaultSearchProvider,
		Services:              cfg.Services,
		PaidFallback:          &paidFallback,
	}
	if cfg.DeepLoopReviewLinesChosen {
		v := cfg.DeepLoopReviewLines
		f.DeepLoopReviewLines = &v
	}
	if cfg.DeepLoopPushChosen {
		v := cfg.DeepLoopPush
		f.DeepLoopPush = &v
	}
	if cfg.DeepLoopGrantChosen {
		v := cfg.DeepLoopGrant
		f.DeepLoopGrant = &v
	}
	if cfg.QuietLogChosen {
		v := cfg.QuietLog
		f.QuietLog = &v
	}
	f.DeepThought = cfg.DeepThought
	f.AccessCodeHash, f.ConfigTokenHash = cfg.AccessCodeHash, cfg.ConfigTokenHash
	setupDone := cfg.SetupDone
	f.SetupDone = &setupDone
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileWhole(path, data)
}

var saveMu sync.Mutex

// writeFileWhole replaces a file in one step, 0600: a power cut leaves the old one or
// the new one, never a half.
func writeFileWhole(path string, data []byte) error {
	saveMu.Lock()
	defer saveMu.Unlock()
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// loadLocalConfig reads the choices saved in Settings. A file that is there and cannot
// be read is an error: going on without it would drop the access codes and leave
// Vedetta open to the network, without a word (external audit 24/11/2026, SEC-05).
func loadLocalConfig(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	var f LocalConfigFile
	if err == nil {
		err = json.Unmarshal(data, &f)
	}
	if err != nil {
		return fmt.Errorf("%s cannot be read (%v). Vedetta does not start without it, because the access codes are saved there: "+
			"put back a good copy, or delete it to start over with the settings and the codes to choose again", path, err)
	}
	if len(f.SearchProviders) > 0 {
		cfg.SearchProviders = f.SearchProviders
		cfg.SearchProvider = f.SearchProviders[0]
	}
	if len(f.ReaderProviders) > 0 {
		cfg.ReaderProviders = f.ReaderProviders
	}
	if len(f.LLMProviders) > 0 {
		cfg.LLMProviders = f.LLMProviders
	}
	if f.SearchFallbackOnEmpty != nil {
		cfg.SearchFallbackOnEmpty = *f.SearchFallbackOnEmpty
	}
	if f.BraveAPIKey != "" {
		cfg.BraveAPIKey = f.BraveAPIKey
	}
	if f.TavilyAPIKey != "" {
		cfg.TavilyAPIKey = f.TavilyAPIKey
	}
	if f.SerperAPIKey != "" {
		cfg.SerperAPIKey = f.SerperAPIKey
	}
	if f.FirecrawlAPIKey != "" {
		cfg.FirecrawlAPIKey = f.FirecrawlAPIKey
	}
	if f.ExaAPIKey != "" {
		cfg.ExaAPIKey = f.ExaAPIKey
	}
	if f.ExaSearchType != "" {
		cfg.ExaSearchType = f.ExaSearchType
	}
	if f.ExaUserLocation != "" {
		cfg.ExaUserLocation = f.ExaUserLocation
	}
	if f.BrowserCDPURL != "" {
		cfg.BrowserCDPURL = f.BrowserCDPURL
	}
	if f.SearxngURL != "" {
		cfg.SearxngURL = f.SearxngURL
	}
	if f.JinaURL != "" {
		cfg.JinaURL = f.JinaURL
	}
	if len(f.LLMEndpoints) > 0 {
		cfg.LLMEndpoints = f.LLMEndpoints
		for i := range cfg.LLMEndpoints {
			cfg.LLMEndpoints[i].APIKey, cfg.LLMEndpoints[i].KeyEnv = ResolveKeyRef(cfg.LLMEndpoints[i].APIKey)
		}
	}
	if f.LLMDefaultID != "" {
		cfg.LLMDefaultID = f.LLMDefaultID
	}
	if f.LLMBaseURL != "" {
		cfg.LLMBaseURL = f.LLMBaseURL
	}
	if f.LLMModel != "" {
		cfg.LLMModel = f.LLMModel
	}
	if f.LLMAPIKey != "" {
		cfg.LLMAPIKey, _ = ResolveKeyRef(f.LLMAPIKey)
	}
	if f.ContactEmail != "" {
		cfg.ContactEmail = f.ContactEmail
	}
	if f.DefaultSearchProvider != "" {
		cfg.DefaultSearchProvider = f.DefaultSearchProvider
	}
	if f.Services != nil {
		cfg.Services = f.Services
	}
	if f.PaidFallback != nil {
		cfg.PaidFallback = *f.PaidFallback
	}
	if f.DeepLoopReviewLines != nil {
		cfg.DeepLoopReviewLines, cfg.DeepLoopReviewLinesChosen = *f.DeepLoopReviewLines, true
	}
	if f.DeepLoopPush != nil {
		cfg.DeepLoopPush, cfg.DeepLoopPushChosen = *f.DeepLoopPush, true
	}
	if f.DeepLoopGrant != nil {
		cfg.DeepLoopGrant, cfg.DeepLoopGrantChosen = *f.DeepLoopGrant, true
	}
	cfg.DeepThought = f.DeepThought
	if f.QuietLog != nil {
		cfg.QuietLog, cfg.QuietLogChosen = *f.QuietLog, true
	}
	cfg.AccessCodeHash, cfg.ConfigTokenHash = f.AccessCodeHash, f.ConfigTokenHash
	// map R8: VEDETTA_SETUP=done wins; a file without the field is an installation
	// from before the first run existed
	cfg.SetupDone = cfg.SetupDone || f.SetupDone == nil || *f.SetupDone
	return nil
}
