package agent

import (
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vedetta/internal/cache"
	"vedetta/internal/llm"
	"vedetta/internal/reader"
	"vedetta/internal/search"
	"vedetta/internal/storage"
)

type EventType string

const (
	EventStep         EventType = "step"
	EventSession      EventType = "session"
	EventSources      EventType = "sources"
	EventSubqueries   EventType = "subqueries"
	EventFact         EventType = "fact"
	EventNode         EventType = "node"
	EventTree         EventType = "tree"
	EventReasoning    EventType = "reasoning"
	EventToken        EventType = "token"
	EventBibliography EventType = "bibliography"
	EventDone         EventType = "done"
	EventError        EventType = "error"
	EventWarning      EventType = "warning"
)

type Event struct {
	Type EventType   `json:"type"`
	Data interface{} `json:"data"`
}

type Emitter func(event Event) error

type SearchParams struct {
	SessionID      string `json:"session_id,omitempty"`
	Query          string `json:"query"`
	Mode           string `json:"mode,omitempty"`            // "standard", "deep", "followup"
	UILanguage     string `json:"ui_language,omitempty"`     // interface locale (labels, steps, warnings)
	AnswerLanguage string `json:"answer_language,omitempty"` // response language + search base language
	Resume         bool   `json:"resume,omitempty"`

	// Per-search overrides (F3.6-5). When set they apply only to this call and
	// never mutate the engine-global providers. Excluded from JSON: they are
	// runtime handles resolved by the HTTP layer.
	SearchProviderOverride search.Provider `json:"-"`
	LLMClientOverride      *llm.Client     `json:"-"`
	ContentReaderOverride  reader.Reader   `json:"-"`
}

// resolveAnswerLanguage resolves the response language for this turn now that the
// session is known: explicit override > detected query language > sticky session > UI locale.
func (e *Engine) resolveAnswerLanguage(params *SearchParams) {
	params.AnswerLanguage = ResolveAnswerLanguage(params.SessionID, params.Query, params.UILanguage, params.AnswerLanguage)
}

func GetLanguageName(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "it", "italian":
		return "Italian"
	case "en", "english":
		return "English"
	case "de", "german":
		return "German"
	case "fr", "french":
		return "French"
	case "es", "spanish":
		return "Spanish"
	default:
		return "English"
	}
}

type Engine struct {
	provMu               sync.RWMutex
	searchProvider       search.Provider
	contentReader        reader.Reader
	llmClient            *llm.Client
	deepEffort           llm.Effort
	storage              *storage.Storage
	reranker             search.Reranker
	maxResults           int
	deepSessionTimeout   time.Duration
	synthesisTimeout     time.Duration
	synthContentStall    time.Duration
	cache                *cache.Cache // V25-R1: nil when VEDETTA_CACHE_MODE=off
	deepLoopMaxCalls     int
	loopSampling         loopSampling
	deepLoopReview       bool        // the sentinel's turn (loopreview.go), VEDETTA_DEEP_LOOP_REVIEW
	deepLoopCompass      bool        // the sentinel before: memory may be out of date, VEDETTA_DEEP_LOOP_COMPASS
	deepLoopPreviews     bool        // the sentinel opens the pages of details seen only in previews, VEDETTA_DEEP_LOOP_REVIEW_PREVIEWS
	deepLoopCiteRead     bool        // the instruction stops calling unopened results sources, VEDETTA_DEEP_LOOP_CITE_READ
	deepLoopLinkCheck    bool        // the sentinel checks the cited pages the model did not read, VEDETTA_DEEP_LOOP_LINK_CHECK
	deepLoopNoRerank     bool        // the Deep loop shows the results in the search service's order, VEDETTA_DEEP_LOOP_RERANK=false
	deepLoopMaxResults   int         // results per Deep loop search, 0 = the budget's, VEDETTA_DEEP_LOOP_MAX_RESULTS
	deepLoopSearchAuto   bool        // a Deep loop search without a language goes out without one, VEDETTA_DEEP_LOOP_SEARCH_LANG=auto
	deepLoopQuoteRetry   bool        // an empty Deep loop search with quotes or site: is retried without, VEDETTA_DEEP_LOOP_QUOTE_FALLBACK
	deepLoopQuotes       bool        // after the answer, ask the model where each sentence comes from and look for it on the pages, VEDETTA_DEEP_LOOP_QUOTES
	deepLoopSessionPages bool        // a follow-up counts the pages of the earlier turns as seen and lists them to the model, VEDETTA_DEEP_LOOP_SESSION_PAGES
	deepLoopPush         atomic.Bool // at the first draft the model is asked once to go on with the research, VEDETTA_DEEP_LOOP_PUSH or Settings (map R3)
	deepLoopGrant        atomic.Bool // more calls at the cap, when the research is still opening new pages, VEDETTA_DEEP_LOOP_GRANT or Settings
	cacheReadDate        bool        // a page read on another day says when, VEDETTA_CACHE_READ_DATE
	deepLoopReviewLines  atomic.Bool // after the review the model rewrites only the lines sent back, VEDETTA_DEEP_LOOP_REVIEW_LINES or Settings (map R3)
}

func NewEngine(sp search.Provider, cr reader.Reader, client *llm.Client, store *storage.Storage) *Engine {
	return &Engine{
		searchProvider:     sp,
		contentReader:      cr,
		llmClient:          client,
		deepEffort:         llm.EffortHigh,
		storage:            store,
		reranker:           search.NewBM25Reranker(),
		maxResults:         10,
		deepSessionTimeout: 1200 * time.Second,
		synthesisTimeout:   1200 * time.Second,
		synthContentStall:  360 * time.Second,
	}
}

func (e *Engine) SetSynthContentStall(d time.Duration) {
	if d > 0 {
		e.synthContentStall = d
	}
}

// SetDeepLoopReview holds the answer of the Deep a ciclo as a draft and sends back
// to the model the numbers and names not found in what it saw (loopreview.go).
func (e *Engine) SetDeepLoopReview(enabled bool) { e.deepLoopReview = enabled }

// SetDeepLoopCompass adds to the Deep a ciclo's system prompt that what the model
// remembers may have changed since its training (loopSystemPrompt).
func (e *Engine) SetDeepLoopCompass(enabled bool) { e.deepLoopCompass = enabled }

// SetDeepLoopReviewPreviews makes the sentinel open the result pages of the details
// the draft found only in search previews, and send them back with the page's
// passage (loopreview.go, PIANO_DEEP_A_CICLO §13 step 2). Needs the review.
func (e *Engine) SetDeepLoopReviewPreviews(enabled bool) { e.deepLoopPreviews = enabled }

// SetDeepLoopCiteRead changes the instruction: cite the pages read, a result not
// opened is not a source (docs/SPEC_LINK_CONTROLLATI.md 3.1).
func (e *Engine) SetDeepLoopCiteRead(enabled bool) { e.deepLoopCiteRead = enabled }

// SetDeepLoopLinkCheck makes the sentinel open the cited pages the model did not read
// and compare them with the sentences (loopcheck.go). It needs the sentinel's turn.
func (e *Engine) SetDeepLoopLinkCheck(enabled bool) { e.deepLoopLinkCheck = enabled }

// SetDeepLoopRerank turns the reorder of the search results in the Deep loop on (the
// default) or off: off, the model sees them in the search service's order, none
// removed (Piano dopo audit 3.1 / D8).
func (e *Engine) SetDeepLoopRerank(enabled bool) { e.deepLoopNoRerank = !enabled }

// SetDeepLoopMaxResults sets how many results a Deep loop search asks for and shows;
// 0 keeps the budget's (Piano dopo audit 3.2 / R6).
func (e *Engine) SetDeepLoopMaxResults(n int) { e.deepLoopMaxResults = n }

// SetDeepLoopSearchLang sets the language of a Deep loop search the model gave none:
// "auto" sends none, so the search service decides; anything else, the answer's
// language, as today (Piano dopo audit 3.3 / D7).
func (e *Engine) SetDeepLoopSearchLang(mode string) {
	e.deepLoopSearchAuto = strings.EqualFold(strings.TrimSpace(mode), "auto")
}

// SetDeepLoopQuotes makes the Deep a ciclo ask, after the answer is out, for the passage
// of a page that says each sentence, and look for it on the page (loopquotes.go).
func (e *Engine) SetDeepLoopQuotes(enabled bool) { e.deepLoopQuotes = enabled }

// SetDeepLoopSessionPages makes a follow-up of the Deep a ciclo count the pages read
// in the earlier turns of the session as seen text, and list them to the model
// (Piano dopo audit 4.4).
func (e *Engine) SetDeepLoopSessionPages(enabled bool) { e.deepLoopSessionPages = enabled }

// SetDeepLoopQuoteFallback makes an empty Deep loop search with quotes or site: try
// once more without the quotes, then without site:, telling the model (Piano dopo
// audit 3.4 / R12). Off by default.
func (e *Engine) SetDeepLoopQuoteFallback(enabled bool) { e.deepLoopQuoteRetry = enabled }

// SetDeepLoopPush makes the Deep loop ask the model once, at its first draft and
// before the sentinel's checks, whether there is more to look for; the model
// chooses (Piano dopo audit 7.13). It works only with the review on, which holds
// the draft. Off by default. Settings can change it while Vedetta runs: a search
// takes the value it finds at its start (map R3).
func (e *Engine) SetDeepLoopPush(enabled bool) { e.deepLoopPush.Store(enabled) }

// DeepLoopPush reports whether "Continua a cercare" is on.
func (e *Engine) DeepLoopPush() bool { return e.deepLoopPush.Load() }

// SetDeepLoopGrant lets a Deep loop research that reaches the cap while it is still
// opening new pages go on for more calls (loopgrant.go). Off by default; like
// SetDeepLoopPush, a search takes the value it finds at its start.
func (e *Engine) SetDeepLoopGrant(enabled bool) { e.deepLoopGrant.Store(enabled) }

// DeepLoopGrant reports whether the special permission is on.
func (e *Engine) DeepLoopGrant() bool { return e.deepLoopGrant.Load() }

// SetCacheReadDate makes a Deep loop page read on another day, which comes from the
// cache, say when it was read: to the model and in "Pagine lette" (Piano dopo audit
// 3.6 / R11). Off by default.
func (e *Engine) SetCacheReadDate(enabled bool) { e.cacheReadDate = enabled }

// SetDeepLoopReviewLines makes the sentinel ask for the lines it sent back instead of
// the whole answer again. Off by default. Settings can change it while Vedetta runs:
// a search takes the value it finds at its start (map R3).
func (e *Engine) SetDeepLoopReviewLines(enabled bool) { e.deepLoopReviewLines.Store(enabled) }

// DeepLoopReviewLines reports whether only the lines sent back are rewritten.
func (e *Engine) DeepLoopReviewLines() bool { return e.deepLoopReviewLines.Load() }

// loopSampling is how the Deep a ciclo samples the model (SetDeepLoopSampling).
type loopSampling struct {
	modelDefault  bool     // send no temperature: the server applies the model's
	temperature   *float64 // nil: defaultLoopTemperature
	topP          *float64
	topK          *int
	keepReasoning bool
}

// defaultLoopTemperature is the Deep a ciclo's temperature when none is set: the one
// Qwen and DeepSeek recommend, as config's default (map R2).
const defaultLoopTemperature = 1.0

// SetDeepLoopSampling sets the sampling of the Deep a ciclo: temperature "" keeps
// defaultLoopTemperature, "model" sends none (the server's recommended sampling for the
// model), a number sets it; topP and topK are sent when > 0; keepReasoning sends the
// model's reasoning back with its tool calls.
func (e *Engine) SetDeepLoopSampling(temperature string, topP float64, topK int, keepReasoning bool) {
	s := loopSampling{keepReasoning: keepReasoning}
	switch t := strings.TrimSpace(strings.ToLower(temperature)); t {
	case "":
	case "model":
		s.modelDefault = true
	default:
		if v, err := strconv.ParseFloat(t, 64); err == nil && v >= 0 {
			s.temperature = &v
		} else {
			log.Printf("[config] VEDETTA_DEEP_LOOP_TEMPERATURE=%q is not a number or \"model\": using %g", temperature, defaultLoopTemperature)
		}
	}
	if topP > 0 {
		s.topP = &topP
	}
	if topK > 0 {
		s.topK = &topK
	}
	e.loopSampling = s
}

func (e *Engine) SetReranker(r search.Reranker) {
	if r != nil {
		e.reranker = r
	}
}

// SetCache puts the search and page cache in front of every search session.
// nil turns it off.
func (e *Engine) SetCache(c *cache.Cache) {
	e.cache = c
}

// Cache returns the search and page cache, nil when it is off.
func (e *Engine) Cache() *cache.Cache { return e.cache }

// SearchProvider returns the active search provider in a thread-safe manner.
func (e *Engine) SearchProvider() search.Provider {
	e.provMu.RLock()
	defer e.provMu.RUnlock()
	return e.searchProvider
}

// SetSearchProvider atomically updates the search provider for subsequent queries.
func (e *Engine) SetSearchProvider(sp search.Provider) {
	e.provMu.Lock()
	defer e.provMu.Unlock()
	e.searchProvider = sp
}

// ContentReader returns the active content reader in a thread-safe manner.
func (e *Engine) ContentReader() reader.Reader {
	e.provMu.RLock()
	defer e.provMu.RUnlock()
	return e.contentReader
}

// SetContentReader atomically updates the content reader for subsequent queries.
func (e *Engine) SetContentReader(cr reader.Reader) {
	e.provMu.Lock()
	defer e.provMu.Unlock()
	e.contentReader = cr
}

// LLMClient returns the active LLM client in a thread-safe manner.
func (e *Engine) LLMClient() *llm.Client {
	e.provMu.RLock()
	defer e.provMu.RUnlock()
	return e.llmClient
}

// SetLLMClient atomically updates the LLM client for subsequent queries.
func (e *Engine) SetLLMClient(client *llm.Client) {
	e.provMu.Lock()
	defer e.provMu.Unlock()
	e.llmClient = client
}

// SearchProviderName returns the name of the active search provider, or "unknown".
func (e *Engine) SearchProviderName() string {
	e.provMu.RLock()
	defer e.provMu.RUnlock()
	if e.searchProvider != nil {
		return e.searchProvider.Name()
	}
	return "unknown"
}

// providerName reports the name of a search provider snapshot, or "unknown"
// when nil. It is used for per-search telemetry so that optional overrides are
// reported honestly without touching engine-global state.
func providerName(p search.Provider) string {
	if p == nil {
		return "unknown"
	}
	return p.Name()
}

// ReaderProviderName returns the name of the active content reader, or "unknown".
func (e *Engine) ReaderProviderName() string {
	e.provMu.RLock()
	defer e.provMu.RUnlock()
	if e.contentReader != nil {
		return e.contentReader.Name()
	}
	return "unknown"
}

// SetDeepLoopMaxCalls caps the tool calls of a research (VEDETTA_DEEP_LOOP_MAX_CALLS).
func (e *Engine) SetDeepLoopMaxCalls(maxCalls int) { e.deepLoopMaxCalls = maxCalls }

// SetDeepEffort sets the model's reasoning effort for the research.
func (e *Engine) SetDeepEffort(effort llm.Effort) { e.deepEffort = effort }

// SetMaxResults sets how many results a search asks for (SEARXNG_MAX_RESULTS);
// VEDETTA_DEEP_LOOP_MAX_RESULTS, when set, wins.
func (e *Engine) SetMaxResults(n int) {
	if n > 0 {
		e.maxResults = n
	}
}

// SetDeepSessionTimeout sets the research part of a research's time limit; the
// writing part is SetSynthesisTimeout, and the limit is their sum.
func (e *Engine) SetDeepSessionTimeout(timeoutSecs int) {
	if timeoutSecs <= 0 {
		return
	}
	if timeoutSecs < 30 {
		timeoutSecs = 30
	} else if timeoutSecs > 1800 {
		timeoutSecs = 1800
	}
	e.deepSessionTimeout = time.Duration(timeoutSecs) * time.Second
}

func (e *Engine) SetSynthesisTimeout(secs int) {
	if secs > 0 {
		e.synthesisTimeout = time.Duration(secs) * time.Second
	}
}

func (e *Engine) Storage() *storage.Storage {
	return e.storage
}
