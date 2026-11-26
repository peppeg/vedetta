package agent

// The research, the "Deep a ciclo" (docs/PIANO_DEEP_A_CICLO.md): the model drives it
// with two tools, search and read, in one conversation, and writes the answer from
// the pages it read. Go keeps what makes Vedetta: the cache, the readers, the
// isolation of untrusted content, the link check and the notes written by Go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"vedetta/config"
	"vedetta/internal/cache"
	"vedetta/internal/llm"
	"vedetta/internal/reader"
	"vedetta/internal/search"
	"vedetta/internal/storage"
)

// errLoopUnsupported: the server refused a request with tools, so the research
// cannot run (no fallback since map R1).
var errLoopUnsupported = errors.New("the model server does not accept tools")

const (
	loopFirstTokenWait = 300 * time.Second // prefill of a long conversation
	loopTokenStall     = 90 * time.Second  // no token at all once the turn started
	loopMaxRepeats     = 3                 // calls repeated beyond this close the research
	loopLocalMaxChars  = 600000            // at home the conversation stops here unless set by hand: each turn rereads it (DL3, D2)
	loopMinReadChars   = 30000             // with less room than this in the conversation no page is opened
	loopCommitChars    = 300               // text without tool calls beyond this is the answer, streamed
	loopReadTimeout    = 20 * time.Second
	loopMaxLinkChecks  = 40 // links outside the sources Vedetta opens per answer
	loopLinkTimeout    = 15 * time.Second
	loopMemoryAge      = 15 * time.Minute // a page read longer ago than this comes from Vedetta's memory, and says when (map R2b)
)

var loopTools = []llm.Tool{
	{Type: "function", Function: llm.ToolFunction{
		Name:        "search",
		Description: "Search the web. Returns titles, URLs and short previews of the results.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"query":{"type":"string","description":"The search query."},` +
			`"lang":{"type":"string","description":"Optional language code of the search, e.g. en, it, de."}},` +
			`"required":["query"]}`),
	}},
	{Type: "function", Function: llm.ToolFunction{
		Name:        "read",
		Description: "Read a whole web page or PDF.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"url":{"type":"string","description":"The URL to read."},` +
			`"fresh":{"type":"boolean","description":"Optional: true reads the page again now, instead of the copy Vedetta keeps from an earlier reading. Use it when the page's data may have changed since then."}},` +
			`"required":["url"]}`),
	}},
}

// loopPage is a page read in this session: Text is what the model got, the whole
// page unless it does not fit in the conversation.
type loopPage struct {
	URL     string
	Title   string
	Text    string
	Partial bool
	ReadOn  time.Time // when the page was read, if it comes from Vedetta's memory (read more than loopMemoryAge ago); zero otherwise
}

// loopCall is one tool call, for the session record and the steps shown.
type loopCall struct {
	Tool   string `json:"tool"`
	Query  string `json:"query,omitempty"`
	Lang   string `json:"lang,omitempty"`
	URL    string `json:"url,omitempty"`
	Fresh  bool   `json:"fresh,omitempty"` // read now, not from Vedetta's memory (map R2b)
	Result string `json:"result"`          // ok, repeated, empty, unreadable, no_room, budget, error
}

// LoopStats is what the Deep a ciclo did, saved with the answer.
type LoopStats struct {
	Turns     int `json:"turns"`
	Calls     int `json:"calls"`
	Searches  int `json:"searches"`
	Reads     int `json:"reads"`
	PagesRead int `json:"pages_read"`
	// Pages too long for the conversation: the model got only their beginning.
	PagesPartial int `json:"pages_partial,omitempty"`
	PDFsPartial  int `json:"pdfs_partial,omitempty"` // PDFs the reader read only in part (time or page limit)
	FreshReads   int `json:"fresh_reads,omitempty"`  // pages the model asked to read again now (map R2b)
	// The model's memory and the conversation it allows (DL3): where the memory
	// comes from (setting, models, props, refusal, default), the limit in bytes,
	// and the most the conversation reached.
	ContextTokens     int    `json:"context_tokens,omitempty"`
	ContextSource     string `json:"context_source,omitempty"`
	ContextLimitChars int    `json:"context_limit_chars,omitempty"`
	ContextPeakChars  int    `json:"context_peak_chars,omitempty"`
	// The server refused the conversation as too long: refusals answered by
	// taking pages out, and the pages taken out (DL3.6).
	ContextRefused int    `json:"context_refused,omitempty"`
	PagesRemoved   int    `json:"pages_removed,omitempty"`
	Unreadable     int    `json:"unreadable"`
	Repeats        int    `json:"repeats"`
	ClosedBy       string `json:"closed_by"` // answer, budget, context, repeats, stall, loop, error
	PagesCited     int    `json:"pages_cited"`
	// Links of the answer to pages the model did not read: at most it saw a preview,
	// or a passage Vedetta showed it.
	LinksUnread int `json:"links_unread"`
	// Links outside the sources that Vedetta opened, and how many held.
	LinksChecked  int `json:"links_checked"`
	LinksVerified int `json:"links_verified"`
	// L4: distinctive numbers of the answer, and those not in the sources seen.
	NumbersChecked int          `json:"numbers_checked"`
	NumbersMissing []NumberMiss `json:"numbers_missing,omitempty"`
	// The sentinel's turn (loopreview.go): the draft and what was sent back.
	Reviewed    bool         `json:"reviewed,omitempty"`
	ReviewItems []ReviewItem `json:"review_items,omitempty"`
	// Details seen only in previews whose result page, opened by Vedetta, contains
	// them: not sent back, kept with the passage for the judge.
	ReviewConfirmed []ReviewItem `json:"review_confirmed,omitempty"`
	ReviewPages     int          `json:"review_pages,omitempty"` // result pages Vedetta opened for the review
	Draft           string       `json:"draft,omitempty"`
	// The draft was sent back and the rewrite did not finish: the first draft went
	// out instead (Piano dopo audit 1.6 / D3).
	ReviewUnfinished bool `json:"review_unfinished,omitempty"`
	// The first draft went out without the sentinel's turn, and why (length, error,
	// loop…): it was cut while written (Piano dopo audit 1.8 / D5).
	ReviewSkipped string `json:"review_skipped,omitempty"`
	// Pages read in the earlier turns of the session: those found again in the cache,
	// seen text for the checks and listed to the model, and those gone (Piano dopo audit 4.4).
	SessionPages     int `json:"session_pages,omitempty"`
	SessionPagesGone int `json:"session_pages_gone,omitempty"`
	// Pages the answer cites that the model read in an earlier turn and not again in
	// this one: among LinksUnread, because this turn did not read them.
	PagesCitedBefore int `json:"pages_cited_before,omitempty"`
	// Where each sentence comes from (loopquotes.go, VEDETTA_DEEP_LOOP_QUOTES): after the
	// answer, the model is asked for the passage of a page it read that says each
	// sentence, and Go looks for it on the page. Nothing is rewritten.
	Quotes       string      `json:"quotes,omitempty"` // done, format, error, skipped, no_pages, no_sentences, no_room
	QuoteItems   []QuoteItem `json:"quote_items,omitempty"`
	QuotePages   int         `json:"quote_pages,omitempty"`   // pages offered
	QuoteSeconds int         `json:"quote_seconds,omitempty"` // the cost of the request
	QuoteLines   int         `json:"quote_lines,omitempty"`   // sentences with a readable line in the reply, "none" included
	// Some searches went to the backup search service (Piano dopo audit 1.9 / R1).
	SearchFallback bool `json:"search_fallback,omitempty"`
	// The lines sent back when only those are rewritten, and what came of it (loopreview.go).
	ReviewLines    []LanguageSlip `json:"review_lines,omitempty"`
	ReviewLinesHow string         `json:"review_lines_how,omitempty"`
	// Lines of the final text with words outside its language, and their rewrite (looplanguage.go).
	LanguageSlips []LanguageSlip `json:"language_slips,omitempty"`
	// The cited pages the model did not read (loopcheck.go): every check, the sentences
	// sent back, what each link of the final answer is, and how many of each.
	CitedChecks     []CitedCheck      `json:"link_checks,omitempty"`
	LinkTroubles    []LinkTrouble     `json:"link_troubles,omitempty"`
	LinkStates      map[string]string `json:"link_states,omitempty"`
	LinksControlled int               `json:"links_controlled,omitempty"`
	LinksPartial    int               `json:"links_partial,omitempty"`
	LinksAddress    int               `json:"links_address,omitempty"`
	LinksUnchecked  int               `json:"links_unchecked,omitempty"`
	LinksRemoved    int               `json:"links_removed,omitempty"`
	LinkCheckOff    bool              `json:"link_check_off,omitempty"` // asked for, but the reader is paid
	// Lines of the final text describing the research, and their rewrite (loopclaims.go).
	WorkClaims []LanguageSlip `json:"work_claims,omitempty"`
	CheckTalk  []string       `json:"check_talk,omitempty"` // opening paragraphs that talked to the check, cut
	// The model was asked once, at its first draft, to go on with the research
	// (looppush.go, VEDETTA_DEEP_LOOP_PUSH): "kept" it made no call and the first
	// draft stood, "more" it searched or read again, "asked" no answer came.
	Push        string `json:"push,omitempty"`
	PushCalls   int    `json:"push_calls,omitempty"`   // calls made after the request
	PushPages   int    `json:"push_pages,omitempty"`   // pages read after the request
	PushDraft   string `json:"push_draft,omitempty"`   // the first draft, when a new one replaced it
	PushSeconds int    `json:"push_seconds,omitempty"` // from the request to the next draft: its cost
	// The cap was reached with the special permission on (loopgrant.go,
	// VEDETTA_DEEP_LOOP_GRANT): each look, and whether more calls were allowed.
	Grants      []LoopGrant `json:"grants,omitempty"`
	CallsDetail []loopCall  `json:"calls_detail,omitempty"`
}

// deepLoop holds the state of one session of the Deep a ciclo.
type deepLoop struct {
	e                 *Engine
	params            SearchParams
	sp                search.Provider
	cr                reader.Reader
	client            *llm.Client
	emit              func(Event) error
	defaultMaxResults int // results per search when the switch does not say (SEARXNG_MAX_RESULTS)
	maxCalls          int
	effort            llm.Effort
	pushOn            bool // "Continua a cercare", as it was when the search started (map R3)
	grantOn           bool // the special permission, as it was when the search started (loopgrant.go)
	started           time.Time
	total             time.Duration // the most the whole answer may take
	reviewLinesOn     bool          // rewrite only the lines sent back, as it was when the search started (map R3)
	cats              []string
	rep               *search.Report
	reranker          search.Reranker

	sources    []search.Result
	sourceSeen map[string]bool
	pages      map[string]*loopPage // by normalizeLinkURL
	pageOrder  []string
	done       map[string]bool
	unreadable map[string]string // why a page could not be read, by normalizeLinkURL: a repeat is told so
	stats      LoopStats
	limit      int               // the conversation the research may grow to, in bytes (loopContextLimit)
	readCalls  map[string]string // tool call id -> page key, of the pages in the conversation (makeRoom)
	removed    []*loopPage       // pages taken out after a refusal: they did not fit
	roomTries  int               // refusals answered by taking pages out
	room       int               // characters left in the conversation, updated before each call
	seen       []string          // addresses written inside pages read and previews
	seenSet    map[string]bool   // by normalizeLinkURL
	linked     int               // sources and seen addresses the link check knows
	checked    map[string]bool   // links Vedetta opened, by normalizeLinkURL: held or not
	seenTexts  []string          // pages read and previews: the evidence of the numbers check
	earlier    []sessionPage     // pages read in the earlier turns of the session, still in the cache (loopsession.go)
	before     map[string]string // addresses of every page read in the earlier turns, by normalizeLinkURL
	verified   map[string]string // text of the pages Vedetta opened and accepted, by normalizeLinkURL
	previews   []search.Result   // every search result shown to the model, for the claim anchors
	holding    bool              // the answer is held as a draft for the sentinel's turn
	rewriting  bool              // what is held is the rewrite after the review: only the language check is left
	// The check of the cited pages (loopcheck.go).
	linkCheck     bool
	citedTexts    map[string]string // pages opened for it, by normalizeLinkURL; "" did not open
	citedVerdicts map[string]string // by citedKey
	citedFinal    []CitedCheck      // the checks of the last round
	push          loopPush          // the request to go on with the research (looppush.go)
	draft         strings.Builder
	aside         string // what was written in the turns that ended in a call (heldText)
	ctx           context.Context
}

func (l *deepLoop) addSource(r search.Result) {
	key := normalizeLinkURL(r.URL)
	if key == "" || l.sourceSeen[key] {
		return
	}
	l.sourceSeen[key] = true
	l.sources = append(l.sources, r)
}

// sourceURLs are the addresses a link may lead to: the sources, the pages read in the
// earlier turns of the session, and the addresses written inside the pages and previews
// the model read.
func (l *deepLoop) sourceURLs() []string {
	urls := make([]string, 0, len(l.sources)+len(l.before)+len(l.seen))
	for _, s := range l.sources {
		urls = append(urls, s.URL)
	}
	for _, k := range slices.Sorted(maps.Keys(l.before)) {
		urls = append(urls, l.before[k])
	}
	return append(urls, l.seen...)
}

// readEver reports whether the model read the page, in this turn or in an earlier one.
func (l *deepLoop) readEver(key string) bool {
	return l.pages[key] != nil || l.before[key] != ""
}

// noteSeenURLs records the addresses written in a text the model was shown: a link
// to one of them points to something the model saw in a source.
func (l *deepLoop) noteSeenURLs(text string) {
	for _, u := range addressesInText(text) {
		k := normalizeLinkURL(u)
		if k == "" || l.seenSet[k] {
			continue
		}
		l.seenSet[k] = true
		l.seen = append(l.seen, u)
	}
}

func (l *deepLoop) step(step, msg string) {
	_ = l.emit(Event{Type: EventStep, Data: map[string]string{"step": step, "message": msg}})
}

// loopSystemPrompt is short on purpose (memory: writer-fewer-rules): the date, the
// tools, the method in one sentence, how to cite, and the half of the method that
// gets forgotten, saying what was not found. compass adds the sentinel before the
// research: what the model remembers may be out of date (case 12, 29/10/2026).
func loopSystemPrompt(lang string, now time.Time, compass, citeRead bool) string {
	memory := ""
	if compass {
		memory = loopCompass + "\n"
	}
	cite := "Cite with Markdown links to the pages you read or the search results you saw, like [title](url)."
	if citeRead {
		cite = "Cite with Markdown links to the pages you read, like [title](url). A result you did not open is not a source: give its address if it helps, but do not attribute to it what you have not read."
	}
	return hardenLoopSystemPrompt(fmt.Sprintf(`Today is %s.
You answer the user's question by researching the web with two tools: search(query, lang) and read(url).

Search and read until the pages cover what the user asks, then write the answer the way the user asked for it.
%sIf something the user asks for (a law, a patent, a person, a figure) cannot be found, say so plainly; do not replace it with namesakes or nearby topics.
%s Use only URLs that appear in your tool results.
Write the answer in %s. Do your best.`, now.Format("2 January 2006"), memory, cite, GetLanguageName(lang)))
}

const loopCompass = "What you remember stops at your training, and the world has changed since: prices, versions, laws, " +
	"who holds an office, what still exists. Before you state something that may have changed, check it in current sources, " +
	"and read the pages rather than relying on the previews."

// closingMessage asks the model to write with what it has.
func closingMessage(reason string) string {
	switch reason {
	case "budget":
		return "You have used all the searches and readings available for this question. Write the answer now with what you found, and say what you could not verify."
	case "context":
		return "The conversation is now too long to read more pages. Write the answer now with what you found, and say what you could not verify."
	case "repeats":
		return "You are repeating calls already made. Write the answer now with what you found, and say what you could not verify."
	default:
		return "The research was interrupted. Write the answer now with what you found, and say what you could not verify."
	}
}

// closingMessage says why the research closed, and what to write now.
func (l *deepLoop) closingMessage(reason string) string {
	if l.stats.ReviewLines != nil {
		return closingLinesMessage
	}
	return closingMessage(reason)
}

// executeDeepLoop runs the Deep a ciclo. It returns errLoopUnsupported, before
// emitting anything, when the server refuses tools.
func (e *Engine) executeDeepLoop(ctx context.Context, params SearchParams, emit func(Event) error,
	sp search.Provider, cr reader.Reader, client *llm.Client, cacheSess *cache.Session, history []storage.TurnMessage, trouble *llmTrouble) error {
	query := strings.TrimSpace(params.Query)
	l := &deepLoop{
		e: e, params: params, sp: sp, cr: cr, client: client, emit: emit,
		defaultMaxResults: e.maxResults, maxCalls: e.deepLoopMaxCalls, effort: e.deepEffort,
		cats: []string{"general"}, rep: search.NewReport(), reranker: e.reranker,
		sourceSeen: make(map[string]bool), pages: make(map[string]*loopPage), done: make(map[string]bool), unreadable: make(map[string]string), seenSet: make(map[string]bool),
		checked: make(map[string]bool), verified: make(map[string]string), holding: e.deepLoopReview,
		started: time.Now(), grantOn: e.deepLoopGrant.Load(),
		pushOn: e.deepLoopPush.Load(), reviewLinesOn: e.deepLoopReviewLines.Load(),
		citedTexts: make(map[string]string), citedVerdicts: make(map[string]string), readCalls: make(map[string]string),
	}
	if e.deepLoopLinkCheck && e.deepLoopReview {
		l.linkCheck = !paidReaderName(cr.Name())
		l.stats.LinkCheckOff = !l.linkCheck
	}
	if l.maxCalls <= 0 {
		l.maxCalls = 40
	}
	total := e.deepSessionTimeout + e.synthesisTimeout
	if total <= 0 {
		total = 2400 * time.Second
	}
	l.total = total
	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()
	l.ctx = ctx
	info := client.ContextInfo(ctx)
	l.limit = loopContextLimit(info, config.IsLocalAddress(client.BaseURL()))
	l.stats.ContextTokens, l.stats.ContextSource, l.stats.ContextLimitChars = info.Tokens, info.Source, l.limit
	log.Printf("[deeploop] session=%s start max_calls=%d effort=%s timeout=%v sampling=%s keep_reasoning=%t review=%t review_previews=%t compass=%t cite_read=%t link_check=%t review_lines=%t rerank=%t max_results=%d search_lang_auto=%t quote_fallback=%t read_date=%t name_words=%t push=%t grant=%t session_pages=%t quotes=%t context=%d (%s) limit_chars=%d",
		params.SessionID, l.maxCalls, l.effort, total, l.samplingText(), e.loopSampling.keepReasoning, e.deepLoopReview, e.deepLoopPreviews, e.deepLoopCompass, e.deepLoopCiteRead, l.linkCheck, l.reviewLinesOn, !e.deepLoopNoRerank, l.maxResults(), e.deepLoopSearchAuto, e.deepLoopQuoteRetry, e.cacheReadDate, nameWords.Load(), l.pushOn, l.grantOn, e.deepLoopSessionPages, e.deepLoopQuotes,
		info.Tokens, info.Source, l.limit)

	messages := []llm.Message{{Role: "system", Content: loopSystemPrompt(params.AnswerLanguage, time.Now(), e.deepLoopCompass, e.deepLoopCiteRead)}}
	start := 0
	if len(history) > 4 {
		start = len(history) - 4
	}
	for _, m := range history[start:] {
		messages = append(messages, llm.Message{Role: "user", Content: m.Query}, llm.Message{Role: "assistant", Content: withoutNotes(m.Content)})
	}
	l.before = pagesReadBefore(history)
	if e.deepLoopSessionPages {
		l.earlier, l.stats.SessionPagesGone = sessionPages(history, cacheSess)
		l.stats.SessionPages = len(l.earlier)
		log.Printf("[deeploop] session=%s earlier pages of the session: %d in the cache, %d gone", params.SessionID, l.stats.SessionPages, l.stats.SessionPagesGone)
	}
	messages = append(messages, llm.Message{Role: "user", Content: earlierPagesNote(l.earlier) + query})

	reconciler := NewReconciler(NewFactStore()).WithSourceURLs(nil).WithLinkVerifier(l.verifyLink).WithLanguage(params.AnswerLanguage)
	var fullAnswer, fullThinking strings.Builder
	reasoningLoop := newRepetitionDetector()
	contentLoop := newRepetitionDetector()
	closing := "" // why the research was closed; "" while it is open
	closingSent := false
	partial := false
	var answerMeta *AnswerMeta

	defer func() {
		if e.storage != nil && params.SessionID != "" && (fullAnswer.Len() > 0 || fullThinking.Len() > 0) {
			sourcesJSON, _ := json.Marshal(l.sources)
			_ = e.storage.AddMessage(storage.TurnMessage{
				SessionID: params.SessionID, Role: "assistant", Query: query,
				Content: fullAnswer.String(), Thinking: fullThinking.String(),
				Mode: "deep", Sources: sourcesJSON,
				Meta: marshalAnswerMeta(answerMeta),
			})
		}
	}()

	l.step("planning", StepMessage(params.UILanguage, "loop_start"))

	for turn := 0; ; turn++ {
		if turn > l.maxCalls+loopMaxRepeats+5 {
			// A safety net: every turn either calls tools, which the budget counts, or ends.
			l.stats.ClosedBy = "turns"
			partial = fullAnswer.Len() > 0
			break
		}
		var tools []llm.Tool
		if closing == "" {
			tools = loopTools
		}
		if n := conversationChars(messages); n > l.stats.ContextPeakChars {
			l.stats.ContextPeakChars = n
		}
		res := l.runTurn(ctx, messages, tools, reconciler, &fullAnswer, &fullThinking, reasoningLoop, contentLoop)
		l.stats.Turns++

		// The server refused the conversation as too long: pages come out, the
		// research closes, and the model writes with the rest (DL3.6).
		if res.err != nil && !res.streamed && l.refusedForMemory(res.err, messages) {
			if n := l.makeRoom(messages); n > 0 {
				l.stats.ContextRefused++
				log.Printf("[deeploop] session=%s the server refused the conversation (%v): %d pages taken out, %d bytes left",
					params.SessionID, res.err, n, conversationChars(messages))
				if closing == "" {
					closing = "context"
					l.stats.ClosedBy = "context"
					l.step("synthesizing", StepMessage(params.UILanguage, "loop_write", len(l.pageOrder)))
					messages = append(messages, llm.Message{Role: "user", Content: l.closingMessage(closing)})
				}
				continue
			}
		}
		if res.err != nil && turn == 0 && !res.streamed && !errors.Is(res.err, llm.ErrContextExceeded) {
			var se *llm.StatusError
			if errors.As(res.err, &se) && se.Code == http.StatusBadRequest {
				log.Printf("[deeploop] session=%s server refused tools (%v): the research cannot run", params.SessionID, res.err)
				return errLoopUnsupported
			}
		}
		if res.err != nil {
			log.Printf("[deeploop] session=%s turn %d error: %v", params.SessionID, turn, res.err)
			noteLLMTrouble(ctx, stageSynthesis, res.err)
		}

		// No answer to the request to go on (an error, the Stop, a cut or empty turn)
		// and nothing searched or read after it: as "done", the first draft stands
		// and goes to the sentinel's checks like any draft (looppush.go).
		if l.push.open && l.holding && l.stats.Calls == l.push.calls &&
			(res.err != nil || res.degenerate || res.cut || (len(res.calls) == 0 && res.text == "")) {
			log.Printf("[deeploop] session=%s push: no answer to the request, the first draft stands", params.SessionID)
			res = turnResult{text: "done", committed: true}
			l.resetHeld()
		}

		// A turn that wrote the answer, completely: done.
		if res.err == nil && !res.degenerate && len(res.calls) == 0 && res.text != "" {
			if !res.committed {
				l.commit(res.text, reconciler, &fullAnswer, contentLoop)
			}
			if !res.cut {
				if l.holding {
					// The sentinel's turn: the draft goes out as it is, or back to the model.
					l.holding = false
					draft := l.heldText()
					contentLoop = newRepetitionDetector()
					// "Continua a cercare": once, at the first draft, while the research
					// is open, before the checks (looppush.go).
					if l.pushOn && !l.rewriting {
						if l.stats.Push == "" && closing == "" {
							messages = l.askPush(messages, draft)
							continue
						}
						if l.push.open {
							draft, messages = l.answerPush(draft, messages)
						}
					}
					var items []ReviewItem
					var troubles []LinkTrouble
					if !l.rewriting {
						items = l.review(draft, query, history)
						if l.reviewLinesOn {
							items = outsideCode(draft, items)
						}
						if l.linkCheck {
							var checks []CitedCheck
							checks, troubles = l.checkCited(draft, 1)
							l.stats.CitedChecks = append(l.stats.CitedChecks, checks...)
						}
					}
					if len(items) == 0 && len(troubles) == 0 {
						if l.rewriting {
							draft = l.cutCheckTalk(ctx, l.withReviewLines(draft))
						}
						if l.linkCheck {
							draft = l.settleCited(draft)
						}
						l.commit(l.fixLanguage(ctx, l.fixWorkClaims(ctx, draft), query, history), reconciler, &fullAnswer, contentLoop)
					} else {
						l.stats.Reviewed, l.stats.ReviewItems, l.stats.LinkTroubles, l.stats.Draft = true, items, troubles, draft
						for _, t := range troubles {
							l.seenTexts = append(l.seenTexts, t.Passage) // what the model is shown
						}
						log.Printf("[deeploop] session=%s review: %d sentences sent back, %d for the page they cite", params.SessionID, len(items), len(troubles))
						l.step("deep_critic", StepMessage(params.UILanguage, "loop_review", len(items)+len(troubles)))
						if l.reviewLinesOn {
							l.stats.ReviewLines = reviewLines(draft, items, troubles)
						}
						messages = append(messages, llm.Message{Role: "assistant", Content: draft},
							llm.Message{Role: "user", Content: reviewMessage(items, troubles, params.AnswerLanguage, l.stats.Calls > 0, l.stats.ReviewLines)})
						if closing != "context" {
							closing = ""
							if l.stats.Calls > 0 {
								l.maxCalls = l.stats.Calls + loopReviewCalls // a few calls to verify, then the limits close the research again
							}
						}
						closingSent = false
						// The rewrite is held too: slips of language are born there, where
						// the review does not look (case 03/11/2026).
						l.holding, l.rewriting = true, true
						l.resetHeld()
						continue
					}
				}
				if l.stats.ClosedBy == "" {
					l.stats.ClosedBy = "answer"
				}
				break
			}
		}

		// Interrupted turns, or a turn with nothing in it: ask once to write.
		if res.err != nil || res.degenerate || res.cut || (len(res.calls) == 0 && res.text == "") {
			why := "error"
			switch {
			case res.degenerate:
				why = res.degenerateWhy
			case res.cut:
				why = "length"
			case res.err == nil:
				why = "empty"
			}
			if l.holding && l.stats.ReviewLines != nil {
				// The draft is a whole answer: the lines finished so far go in, and
				// nothing is left to continue.
				l.holding = false
				reply := l.heldText()
				reply = reply[:strings.LastIndexByte(reply, '\n')+1]
				l.commit(l.withReviewLines(reply), reconciler, &fullAnswer, newRepetitionDetector())
				l.stats.ClosedBy = why
				break
			}
			if held := l.heldText(); l.holding && held != "" {
				// An interrupted draft is not reviewed: it goes out and is continued,
				// and the note says so.
				if !l.rewriting {
					l.stats.ReviewSkipped = why
				}
				l.holding = false
				contentLoop = newRepetitionDetector()
				l.commit(held, reconciler, &fullAnswer, contentLoop)
			}
			if closingSent || ctx.Err() != nil {
				l.stats.ClosedBy = why
				partial = fullAnswer.Len() > 0
				break
			}
			closingSent = true
			if closing == "" {
				closing = why
			}
			l.stats.ClosedBy = why
			l.step("wrap_up", StepMessage(params.UILanguage, "wrap_up"))
			written := fullAnswer.String()
			if res.committed || written != "" {
				// The answer was being written: continue it, do not start over.
				messages = append(messages, llm.Message{Role: "assistant", Content: written},
					llm.Message{Role: "user", Content: "Your answer was interrupted. Continue it from where it stops, without repeating what is written."})
				if !strings.HasSuffix(written, "\n") {
					fullAnswer.WriteString("\n\n")
					_ = l.emit(Event{Type: EventToken, Data: "\n\n"})
				}
			} else {
				messages = append(messages, llm.Message{Role: "user", Content: l.closingMessage(why)})
			}
			continue
		}

		// Calls after the research was closed: answered with "write now", once.
		if closing != "" {
			if closingSent {
				l.stats.ClosedBy = closing
				partial = fullAnswer.Len() > 0
				break
			}
			closingSent = true
		}

		// Tool calls: run them, answer each one, then check the limits.
		assistant := llm.Message{Role: "assistant", Content: res.textForHistory(), ToolCalls: res.calls}
		if l.e.loopSampling.keepReasoning {
			assistant.ReasoningContent = res.reasoning
		}
		messages = append(messages, assistant)
		if res.committed && l.holding {
			l.setAside()
		}
		for _, call := range res.calls {
			l.room = l.limit - conversationChars(messages)
			out := l.runTool(ctx, call, closing != "")
			messages = append(messages, llm.Message{Role: "tool", ToolCallID: call.ID, Content: out})
		}
		if closing == "" {
			switch {
			case l.stats.Calls >= l.maxCalls && !l.grantCalls(conversationChars(messages)):
				closing = "budget"
			case l.stats.Repeats > loopMaxRepeats:
				closing = "repeats"
			case conversationChars(messages) > l.limit:
				closing = "context"
			}
			if closing != "" {
				log.Printf("[deeploop] session=%s research closed: %s (calls=%d, repeats=%d, context_chars=%d)",
					params.SessionID, closing, l.stats.Calls, l.stats.Repeats, conversationChars(messages))
				l.stats.ClosedBy = closing
				l.step("synthesizing", StepMessage(params.UILanguage, "loop_write", len(l.pageOrder)))
				messages = append(messages, llm.Message{Role: "user", Content: l.closingMessage(closing)})
			}
		}
	}

	// The request to go on had no draft after it: the model searched or read again
	// and then its writing was cut or failed.
	pushUnanswered := l.push.open
	if pushUnanswered && l.stats.Calls > l.push.calls {
		l.settlePush("more")
	}
	if l.holding && l.stats.ReviewLines != nil {
		l.holding = false
		l.commit(l.withReviewLines(l.heldText()), reconciler, &fullAnswer, newRepetitionDetector())
	}
	if held := l.heldText(); l.holding && held != "" {
		if !l.rewriting {
			l.stats.ReviewSkipped = l.stats.ClosedBy
		}
		l.holding = false
		l.commit(held, reconciler, &fullAnswer, newRepetitionDetector())
		partial = partial || l.stats.ClosedBy != "answer"
	}
	// The draft was sent back and nothing came of the rewrite (turns failed, the
	// Stop): Vedetta holds a whole answer, and whoever asked gets it, through the
	// same last steps as an accepted draft. Those ask the model and keep the text
	// as it is when it does not answer (Piano dopo audit 1.6 / D3).
	// The same when the model was asked to go on and nothing came after it: the
	// first draft goes out, without the sentinel's turn, and the note says so.
	first := l.stats.Draft
	if first == "" && pushUnanswered {
		first = l.push.draft
	}
	if fullAnswer.Len() == 0 && first != "" {
		l.holding = false
		partial = true
		if l.stats.Draft != "" {
			l.stats.ReviewUnfinished = true
			log.Printf("[deeploop] session=%s the rewrite did not finish (%s): the first draft goes out", params.SessionID, l.stats.ClosedBy)
		} else {
			l.stats.ReviewSkipped = l.stats.ClosedBy
			if l.stats.ReviewSkipped == "" {
				l.stats.ReviewSkipped = "error"
			}
			l.stats.PushDraft = "" // no new draft took its place
			log.Printf("[deeploop] session=%s nothing came after the request to go on (%s): the first draft goes out", params.SessionID, l.stats.ClosedBy)
		}
		draft := first
		if l.linkCheck {
			draft = l.settleCited(draft)
		}
		l.commit(l.fixLanguage(ctx, l.fixWorkClaims(ctx, draft), query, history), reconciler, &fullAnswer, newRepetitionDetector())
	}
	if tail := reconciler.Flush(); tail != "" {
		fullAnswer.WriteString(tail)
		_ = l.emit(Event{Type: EventToken, Data: tail})
	}
	if e.deepLoopQuotes {
		l.askQuotes(ctx, messages, fullAnswer.String(), StepMessage(params.AnswerLanguage, "deduction_label"), partial)
	}
	l.stats.PagesRead = len(l.pageOrder)
	l.stats.PagesCited, l.stats.LinksUnread = l.citedPages(fullAnswer.String())
	l.stats.PagesCitedBefore = l.citedBefore(fullAnswer.String())
	if l.linkCheck {
		l.stats.LinkStates = l.linkStates(fullAnswer.String())
	}
	l.stats.LinksChecked = len(l.checked)
	l.stats.LinksVerified = reconciler.VerifiedURLs()
	if l.stats.Calls > 0 {
		l.stats.NumbersChecked, l.stats.NumbersMissing = checkAnswerNumbers(fullAnswer.String(), l.numbersSeen(l.evidence(query, history), l.sourceAndPageURLs(true)))
	}
	anchorTexts := make(map[string]anchorSource, len(l.pages)+len(l.verified))
	for k, p := range l.pages {
		anchorTexts[k] = anchorSource{text: p.Text, basis: "page"}
	}
	for k, t := range l.verified {
		anchorTexts[k] = anchorSource{text: t, basis: "verified"}
	}
	for _, p := range l.earlier {
		if k := normalizeLinkURL(p.URL); anchorTexts[k].text == "" {
			anchorTexts[k] = anchorSource{text: p.Text, basis: "page"}
		}
	}
	pool := newClaimPool()
	for _, k := range l.pageOrder {
		pool.add(l.pages[k].URL, "page", l.pages[k].Text)
	}
	for _, r := range l.previews {
		pool.add(r.URL, "preview", r.Title+"\n"+r.Content)
	}
	verifiedKeys := make([]string, 0, len(l.verified))
	for k := range l.verified {
		verifiedKeys = append(verifiedKeys, k)
	}
	sort.Strings(verifiedKeys) // the same pool order on every run
	for _, k := range verifiedKeys {
		pool.add(k, "verified", l.verified[k])
	}
	anchors := anchorCitations(fullAnswer.String(), anchorTexts, pool, StepMessage(params.AnswerLanguage, "deduction_label"))
	searches, empty := l.rep.Searches()
	log.Printf("[telemetry] session=%s deeploop turns=%d calls=%d searches=%d empty=%d reads=%d pages_read=%d unreadable=%d repeats=%d closed_by=%s pages_cited=%d links_verified=%d/%d links_dropped=%d numbers_missing=%d/%d answer_chars=%d thinking_chars=%d pages_partial=%d context=%d (%s) limit_chars=%d peak_chars=%d",
		params.SessionID, l.stats.Turns, l.stats.Calls, searches, empty, l.stats.Reads, l.stats.PagesRead, l.stats.Unreadable,
		l.stats.Repeats, l.stats.ClosedBy, l.stats.PagesCited, l.stats.LinksVerified, l.stats.LinksChecked, reconciler.DroppedPhantomURLs(), len(l.stats.NumbersMissing), l.stats.NumbersChecked, fullAnswer.Len(), fullThinking.Len(), l.stats.PagesPartial,
		l.stats.ContextTokens, l.stats.ContextSource, l.stats.ContextLimitChars, l.stats.ContextPeakChars)
	var cacheStats *cache.Stats
	if cacheSess != nil {
		st := cacheSess.Stats()
		cacheStats = &st
		log.Printf("[telemetry] session=%s cache=%s search_hit=%d/%d page_hit=%d/%d",
			params.SessionID, st.Mode, st.SearchHits, st.SearchHits+st.SearchMisses, st.PageHits, st.PageHits+st.PageMisses)
	}

	if fullAnswer.Len() == 0 {
		errMsg := StepMessage(params.UILanguage, "error_no_answer")
		if !trouble.empty() {
			errMsg = StepMessage(params.UILanguage, "error_llm_unavailable")
		}
		_ = l.emit(Event{Type: EventError, Data: map[string]string{"error": errMsg}})
		return fmt.Errorf("%s", errMsg)
	}
	if partial {
		_ = l.emit(Event{Type: EventWarning, Data: map[string]string{
			"kind": string(WarnKindPartial), "warning": StepMessage(params.UILanguage, "warning_partial"), "error": l.stats.ClosedBy,
		}})
	}

	// Notes written by Go: what was done and what was dropped. An answer given
	// without any research (a greeting) gets only the provenance.
	notes := notesStart
	if l.stats.Calls > 0 {
		notes += loopNote(params.AnswerLanguage, l.stats, searches, reconciler.DroppedPhantomURLs()) +
			reviewNote(params.AnswerLanguage, l.stats) +
			linkReviewNote(params.AnswerLanguage, l.stats) +
			reviewUnfinishedNote(params.AnswerLanguage, l.stats) +
			reviewSkippedNote(params.AnswerLanguage, l.stats) +
			searchTroubleNote(params.AnswerLanguage, l.stats.SearchFallback, l.rep.Unresponsive()) +
			workClaimsNote(params.AnswerLanguage, l.stats.WorkClaims) +
			numbersNote(params.AnswerLanguage, l.stats.NumbersChecked, l.stats.NumbersMissing) +
			quotesNote(params.AnswerLanguage, l.stats) +
			readerFallbackNote(params.AnswerLanguage, l.cr, params.SessionID)
	}
	troubleRec := trouble.snapshot()
	if troubleRec != nil {
		log.Printf("[telemetry] session=%s llm_trouble=%s", params.SessionID, formatLLMTrouble(troubleRec))
		notes += llmTroubleNote(troubleRec, params.AnswerLanguage)
	}
	basis := l.stats.basis()
	provenance := loopProvenance(basis, partial, troubleRec != nil)
	notes += loopProvenanceNote(provenance, params.AnswerLanguage, l.stats.Calls > 0, l.linkCheck)
	if provenance == ProvenanceResearched {
		notes += basisNote(basis, params.AnswerLanguage)
	}
	fullAnswer.WriteString(notes)
	_ = l.emit(Event{Type: EventToken, Data: notes})

	if bib := l.bibliography(params.AnswerLanguage); bib != "" {
		bib += quotesList(params.AnswerLanguage, l.stats, l.pageNumbers()) // the quotes come from pages read only
		fullAnswer.WriteString(bib)
		_ = l.emit(Event{Type: EventBibliography, Data: bib})
	}

	_ = l.emit(Event{Type: EventSources, Data: BuildSourcesPayload(providerName(sp), e.ReaderProviderName(), l.sources, l.stats.SearchFallback)})
	stats := l.stats
	answerMeta = &AnswerMeta{
		Version:             answerMetaVersion,
		Provenance:          provenance,
		Partial:             partial,
		LLMTrouble:          troubleRec,
		DroppedPhantomURLs:  reconciler.DroppedPhantomURLs(),
		RepairedURLs:        reconciler.RepairedURLs(),
		Deductions:          reconciler.Deductions(),
		SourcesCount:        len(l.sources),
		SourcesRead:         len(l.pageOrder),
		SearchQueries:       searches,
		EmptySearches:       empty,
		UnresponsiveEngines: l.rep.Unresponsive(),
		Cache:               cacheStats,
		Loop:                &stats,
		Anchors:             anchors,
	}
	pagesRead := make([]string, len(l.pageOrder))
	for i, k := range l.pageOrder {
		pagesRead[i] = l.pages[k].URL
	}
	var quotes []QuoteItem // the interface marks them only from a usable reply; LoopStats keeps them all
	if l.stats.Quotes == "done" {
		quotes = l.stats.QuoteItems
	}
	return l.emit(Event{Type: EventDone, Data: map[string]interface{}{
		"session_id":           params.SessionID,
		"status":               "completed",
		"partial":              partial,
		"provenance":           string(provenance),
		"dropped_phantom_urls": reconciler.DroppedPhantomURLs(),
		"sources_count":        len(l.sources),
		"sources_read":         len(l.pageOrder),
		"pages_read":           pagesRead, // the steps name a page before reading it: only this list says it was read
		"link_states":          l.stats.LinkStates,
		"numbers_missing":      l.stats.NumbersMissing, // the interface marks these sentences (Piano dopo audit 4.2)
		"quotes":               quotes,                 // where each sentence comes from (fase 6, the interface at work 6.6)
		"loop_calls":           l.stats.Calls,
	}})
}

// turnResult is the outcome of one model turn.
type turnResult struct {
	text          string // content of the turn
	reasoning     string // reasoning of the turn, sent back with its tool calls (keepReasoning)
	committed     bool   // the content was already streamed as the answer
	streamed      bool   // the server started answering
	calls         []llm.ToolCall
	cut           bool // finish_reason "length"
	degenerate    bool
	degenerateWhy string // loop or stall
	err           error
}

// chatOptions is the request of a turn: the sampling set for the Deep a ciclo
// (P0, docs/APPUNTI_MOTORI_INFERENZA.md), defaultLoopTemperature when none is set.
func (l *deepLoop) chatOptions(tools []llm.Tool) llm.ChatOptions {
	s := l.e.loopSampling
	o := llm.ChatOptions{Effort: l.effort, Tools: tools, TopP: s.topP, TopK: s.topK, ModelSampling: s.modelDefault}
	switch {
	case s.temperature != nil:
		o.Temperature = s.temperature
	case !s.modelDefault:
		o.Temperature = llm.Float64Ptr(defaultLoopTemperature)
	}
	return o
}

// samplingText describes the sampling for the log.
func (l *deepLoop) samplingText() string {
	o := l.chatOptions(nil)
	parts := []string{"temperature=model"}
	if o.Temperature != nil {
		parts[0] = fmt.Sprintf("temperature=%g", *o.Temperature)
	}
	if o.TopP != nil {
		parts = append(parts, fmt.Sprintf("top_p=%g", *o.TopP))
	}
	if o.TopK != nil {
		parts = append(parts, fmt.Sprintf("top_k=%d", *o.TopK))
	}
	return strings.Join(parts, ",")
}

// textForHistory is the content to keep in the assistant message that carries
// tool calls: a short preamble, or the part of the answer already shown.
func (r turnResult) textForHistory() string { return r.text }

// commit sends text through the link check and out as answer tokens.
func (l *deepLoop) commit(text string, rec *Reconciler, answer *strings.Builder, contentLoop *repetitionDetector) bool {
	if urls := l.sourceURLs(); l.linked != len(urls) {
		// The sources grow with the research: the link check learns the new ones.
		rec.WithSourceURLs(urls)
		l.linked = len(urls)
	}
	if l.holding {
		// Held for the sentinel's turn: the user sees it being written as reasoning.
		l.draft.WriteString(text)
		_ = l.emit(Event{Type: EventReasoning, Data: text})
		return contentLoop.Write(text)
	}
	out := rec.Process(text)
	if out == "" {
		return false
	}
	answer.WriteString(out)
	_ = l.emit(Event{Type: EventToken, Data: out})
	return contentLoop.Write(out)
}

// setAside keeps apart what was held of a turn that ended in a call: "now I look for
// the official sources…" is not the beginning of the answer (external audit
// 24/11/2026, BUG-01).
func (l *deepLoop) setAside() {
	if l.draft.Len() == 0 {
		return
	}
	if l.aside != "" {
		l.aside += "\n\n"
	}
	l.aside += l.draft.String()
	l.draft.Reset()
}

// heldText is the text held. What was written before a call counts only when it is
// longer than what came after it: then it was the answer, with a last check in the
// middle, and what came after goes on from there.
func (l *deepLoop) heldText() string {
	d := l.draft.String()
	if len(l.aside) <= len(d) {
		return d
	}
	if d == "" {
		return l.aside
	}
	return l.aside + "\n\n" + d
}

func (l *deepLoop) resetHeld() {
	l.aside = ""
	l.draft.Reset()
}

// runTurn streams one model turn with the watchdogs on degenerations: a repetition
// loop, no tokens at all, or reasoning that grows for minutes with nothing else.
func (l *deepLoop) runTurn(ctx context.Context, messages []llm.Message, tools []llm.Tool, rec *Reconciler,
	answer, thinking *strings.Builder, reasoningLoop, contentLoop *repetitionDetector) turnResult {
	turnCtx, cancelTurn := context.WithCancel(ctx)
	defer cancelTurn()

	var res turnResult
	var mu sync.Mutex
	turnStart := time.Now()
	lastToken := time.Time{}
	lastOutput := turnStart // last content or tool call
	reasoningGrew := false
	degenerate := func(why string) {
		mu.Lock()
		if !res.degenerate {
			res.degenerate, res.degenerateWhy = true, why
		}
		mu.Unlock()
		cancelTurn()
	}

	contentStall := l.e.synthContentStall
	if contentStall <= 0 {
		contentStall = 360 * time.Second
	}
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-turnCtx.Done():
				return
			case now := <-t.C:
				mu.Lock()
				lt, lo, grew := lastToken, lastOutput, reasoningGrew
				mu.Unlock()
				switch {
				case lt.IsZero() && now.Sub(turnStart) > loopFirstTokenWait,
					!lt.IsZero() && now.Sub(lt) > loopTokenStall:
					log.Printf("[deeploop] session=%s watchdog: no tokens, turn canceled", l.params.SessionID)
					noteLLMTrouble(ctx, stageSynthesis, context.DeadlineExceeded)
					degenerate("stall")
					return
				case grew && now.Sub(lo) > contentStall:
					log.Printf("[deeploop] session=%s watchdog: reasoning for %v with no output, turn canceled", l.params.SessionID, now.Sub(lo).Round(time.Second))
					degenerate("loop")
					return
				}
			}
		}
	}()

	var text, turnReasoning strings.Builder
	err := l.client.StreamChatWithOptions(turnCtx, oneUserAtATime(messages), l.chatOptions(tools),
		func(tok llm.StreamToken) error {
			mu.Lock()
			res.streamed = true
			lastToken = time.Now()
			switch tok.Type {
			case llm.TokenReasoning:
				reasoningGrew = true
			case llm.TokenContent, llm.TokenToolCalls:
				lastOutput = lastToken
			}
			mu.Unlock()
			switch tok.Type {
			case llm.TokenReasoning:
				thinking.WriteString(tok.Text)
				if !tok.NoReturn {
					// Only the reasoning the server takes back goes back (keepReasoning).
					turnReasoning.WriteString(tok.Text)
				}
				if reasoningLoop.Write(tok.Text) {
					log.Printf("[deeploop] session=%s watchdog: reasoning repetition loop", l.params.SessionID)
					degenerate("loop")
					return nil
				}
				return l.emit(Event{Type: EventReasoning, Data: tok.Text})
			case llm.TokenContent:
				text.WriteString(tok.Text)
				if res.committed {
					if l.commit(tok.Text, rec, answer, contentLoop) {
						log.Printf("[deeploop] session=%s watchdog: answer repetition loop", l.params.SessionID)
						degenerate("loop")
					}
				} else if len(tools) == 0 || text.Len() >= loopCommitChars {
					// Without tools the text can only be the answer; with tools, a long
					// text is the answer too (a preamble to a call is short).
					res.committed = true
					if l.commit(text.String(), rec, answer, contentLoop) {
						degenerate("loop")
					}
				}
			case llm.TokenToolCalls:
				res.calls = tok.ToolCalls
			case llm.TokenTruncated:
				res.cut = true
			}
			return nil
		})
	cancelTurn()
	<-watchDone
	res.text = text.String()
	res.reasoning = turnReasoning.String()
	if err != nil && !res.degenerate {
		res.err = err
	}
	if res.degenerate {
		// Calls of a canceled turn are incomplete.
		res.calls = nil
	}
	return res
}

// oneUserAtATime joins user messages that follow each other: after a failed turn the
// request to write comes again with nothing between, and the template of some models
// (Mistral, Gemma) refuses two in a row.
func oneUserAtATime(messages []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(messages))
	for _, m := range messages {
		if n := len(out); n > 0 && m.Role == "user" && out[n-1].Role == "user" {
			if m.Content != out[n-1].Content { // the same request twice is said once
				out[n-1].Content += "\n\n" + m.Content
			}
			continue
		}
		out = append(out, m)
	}
	return out
}

// runTool runs one call and returns the text the model reads as its result.
func (l *deepLoop) runTool(ctx context.Context, call llm.ToolCall, closed bool) string {
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		args = map[string]interface{}{}
	}
	c := loopCall{Tool: call.Function.Name}
	defer func() { l.stats.CallsDetail = append(l.stats.CallsDetail, c) }()
	if closed {
		c.Result = "budget"
		if l.stats.ReviewLines != nil {
			return closingLinesMessage
		}
		return "No more searches or readings are available: write the answer with what you found."
	}
	l.stats.Calls++

	switch call.Function.Name {
	case "search":
		c.Query = strings.TrimSpace(argString(args, "query"))
		c.Lang = strings.TrimSpace(argString(args, "lang"))
		if c.Query == "" {
			c.Result = "error"
			return "Error: search needs a query."
		}
		return l.search(ctx, &c)
	case "read":
		c.URL = strings.TrimSpace(argString(args, "url"))
		c.Fresh, _ = args["fresh"].(bool)
		if !isHTTPPrefix(c.URL) {
			c.Result = "error"
			return "Error: read needs an http or https URL."
		}
		out := l.read(ctx, &c)
		if c.Result == "ok" {
			l.readCalls[call.ID] = normalizeLinkURL(c.URL)
		}
		return out
	default:
		c.Result = "error"
		return fmt.Sprintf("Error: there is no tool %q. The tools are search and read.", call.Function.Name)
	}
}

func (l *deepLoop) search(ctx context.Context, c *loopCall) string {
	lang := effectiveSubQueryLang(c.Lang, l.params.AnswerLanguage)
	if l.e.deepLoopSearchAuto {
		lang = c.Lang // none given: the search service decides (Piano dopo audit 3.3)
	}
	key := "search|" + strings.ToLower(c.Query) + "|" + lang
	if l.done[key] {
		l.stats.Repeats++
		c.Result = "repeated"
		return "You already ran this search: its results are above in the conversation."
	}
	l.done[key] = true
	l.stats.Searches++
	l.step("searching", StepMessage(l.params.UILanguage, "loop_search", c.Query))
	log.Printf("[deeploop] session=%s search lang=%s query=%q", l.params.SessionID, lang, c.Query)

	opts := search.Options{SessionID: l.params.SessionID, MaxResults: l.maxResults(), Categories: l.cats, Language: lang}
	res, err := l.sp.Search(search.WithReport(ctx, l.rep), c.Query, opts)
	ran, header := c.Query, fmt.Sprintf("Results for %q:\n\n", c.Query)
	if (err != nil || len(res) == 0) && l.e.deepLoopQuoteRetry {
		if q, h, r := l.relaxedSearch(ctx, c.Query, opts); len(r) > 0 {
			ran, header, res, err = q, h, r, nil
			c.Result = "relaxed"
		}
	}
	l.rep.AddSearch(err != nil || len(res) == 0)
	l.noteSearchFallback()
	if err != nil || len(res) == 0 {
		c.Result = "empty"
		if err != nil {
			log.Printf("[deeploop] session=%s search error: %v", l.params.SessionID, err)
		}
		return "No results for this search. Try other words or another language."
	}
	res = slices.DeleteFunc(slices.Clone(res), func(r search.Result) bool { return !isHTTPPrefix(r.URL) }) // a page, or nothing to open
	if rr := l.reranker; rr != nil && !l.e.deepLoopNoRerank {
		if ra, ok := rr.(search.RootAwareReranker); ok {
			res = ra.RerankWithRoot(ran, l.params.Query, res, 0.05)
		} else {
			res = rr.Rerank(ran, res, 0.05)
		}
	}
	if max := l.maxResults(); max > 0 && len(res) > max {
		res = res[:max]
	}
	var b strings.Builder
	b.WriteString(header)
	for _, r := range res {
		l.addSource(r)
		l.noteSeenURLs(r.Content)
		l.seenTexts = append(l.seenTexts, r.Title+"\n"+r.Content)
		l.previews = append(l.previews, r)
		b.WriteString(loopResultBlock(pageTitle(r.Title), r.URL, r.Content))
		b.WriteString("\n")
	}
	_ = l.emit(Event{Type: EventSources, Data: BuildSourcesPayload(providerName(l.sp), l.e.ReaderProviderName(), l.sources, l.stats.SearchFallback)})
	if c.Result == "" {
		c.Result = "ok"
	}
	return b.String()
}

var (
	quoteMarksRe = regexp.MustCompile(`["“”«»]`)
	siteTermRe   = regexp.MustCompile(`(?i)(^|\s)-?site:\S+`)
)

// relaxedSearch tries an empty search again without its quotes, then without
// site:, and returns the search that found something, the header that tells the
// model, and the results (Piano dopo audit 3.4 / R12).
func (l *deepLoop) relaxedSearch(ctx context.Context, query string, opts search.Options) (string, string, []search.Result) {
	quoted := quoteMarksRe.MatchString(query)
	noQuotes := strings.Join(strings.Fields(quoteMarksRe.ReplaceAllString(query, " ")), " ")
	if quoted {
		if res, err := l.sp.Search(search.WithReport(ctx, l.rep), noQuotes, opts); err == nil && len(res) > 0 {
			log.Printf("[deeploop] session=%s search relaxed: %q -> %q", l.params.SessionID, query, noQuotes)
			return noQuotes, fmt.Sprintf("No results for the exact phrase. Results without quotes for %q:\n\n", noQuotes), res
		}
	}
	if !siteTermRe.MatchString(noQuotes) {
		return "", "", nil
	}
	noSite := strings.Join(strings.Fields(siteTermRe.ReplaceAllString(noQuotes, " ")), " ")
	if noSite == "" {
		return "", "", nil
	}
	res, err := l.sp.Search(search.WithReport(ctx, l.rep), noSite, opts)
	if err != nil || len(res) == 0 {
		return "", "", nil
	}
	log.Printf("[deeploop] session=%s search relaxed: %q -> %q", l.params.SessionID, query, noSite)
	if quoted {
		return noSite, fmt.Sprintf("No results for the exact phrase within the site. Results without quotes and without the site restriction for %q:\n\n", noSite), res
	}
	return noSite, fmt.Sprintf("No results within the site. Results without the site restriction, from the whole web, for %q:\n\n", noSite), res
}

// effectiveSubQueryLang resolves the language actually used for a search: a
// normalized override wins; otherwise the session language applies.
func effectiveSubQueryLang(override, sessionLang string) string {
	if override != "" {
		return override
	}
	return NormalizeLanguage(sessionLang)
}

// maxResults is how many results a search asks for and shows: the switch, or the
// search service's setting (Piano dopo audit 3.2).
func (l *deepLoop) maxResults() int {
	if l.e.deepLoopMaxResults > 0 {
		return l.e.deepLoopMaxResults
	}
	return l.defaultMaxResults
}

// noteSearchFallback asks the search service whether it went to its backup,
// and warns once (Piano dopo audit 1.9 / R1).
func (l *deepLoop) noteSearchFallback() {
	fb, ok := l.sp.(search.FallbackReporter)
	if !ok || l.stats.SearchFallback {
		return
	}
	used, reason := fb.FallbackStatus(l.params.SessionID)
	if !used {
		return
	}
	l.stats.SearchFallback = true
	log.Printf("[deeploop] session=%s search fallback: %s", l.params.SessionID, reason)
	_ = l.emit(Event{Type: EventWarning, Data: map[string]string{
		"kind":    string(WarnKindSearchFallback),
		"warning": StepMessage(l.params.UILanguage, "warning_search_fallback"),
		"details": reason,
	}})
}

// searchTroubleNote says that searches went to the backup service and which
// engines did not answer, without counting how many searches.
func searchTroubleNote(lang string, fallback bool, silent []search.EngineFailure) string {
	note := ""
	if fallback {
		note += "\n\n*" + StepMessage(lang, "loop_note_search_fallback") + "*"
	}
	if len(silent) > 0 {
		names := make([]string, len(silent))
		for i, f := range silent {
			names[i] = f.Engine
		}
		note += "\n\n*" + StepMessage(lang, "loop_note_engines_silent", strings.Join(names, ", ")) + "*"
	}
	return note
}

func (l *deepLoop) read(ctx context.Context, c *loopCall) string {
	norm := normalizeLinkURL(c.URL)
	key := "read|" + norm
	if c.Fresh {
		key = "read-fresh|" + norm // a page from the memory may be read again now, once
	}
	if l.done[key] {
		l.stats.Repeats++
		c.Result = "repeated"
		if why, failed := l.unreadable[norm]; failed {
			// Not above in the conversation: it never opened (Piano dopo audit 1.3 / D2).
			return fmt.Sprintf("This page could not be read before (%s). Try another source.", why)
		}
		return "You already read this page: it is above in the conversation."
	}
	if l.room < loopMinReadChars {
		c.Result = "no_room"
		return "The conversation is too long to read more pages. Write the answer with what you found, and say what you could not verify."
	}
	l.stats.Reads++
	l.step("reading", StepMessage(l.params.UILanguage, "loop_read", c.URL))
	readCtx, cancel := context.WithTimeout(reader.WithSessionID(ctx, l.params.SessionID), loopReadTimeout)
	if c.Fresh {
		readCtx = cache.WithFresh(readCtx)
		l.stats.FreshReads++
	}
	doc, err := l.cr.Read(readCtx, c.URL)
	cancel()
	status, detail := classifyRead(doc, err)
	log.Printf("[deeploop] session=%s read url=%s fresh=%t status=%s %s", l.params.SessionID, c.URL, c.Fresh, status, detail)
	l.done[key] = true
	if status != readOK {
		l.stats.Unreadable++
		c.Result = "unreadable"
		l.unreadable[norm] = fmt.Sprintf("%s: %s", status, detail)
		return fmt.Sprintf("This page could not be read (%s: %s). Try another source.", status, detail)
	}
	title := oneLine(doc.Title)
	if title == "" {
		title = c.URL
	}
	page := &loopPage{URL: c.URL, Title: title, Text: doc.Content}
	head := ""
	// A PDF the reader could not finish says so, as a page cut below does
	// (Piano dopo audit 1.5 / R9).
	if doc.Metadata["truncated"] == "true" {
		page.Partial = true
		l.stats.PDFsPartial++
		log.Printf("[deeploop] session=%s read url=%s pdf partial pages=%s of %s", l.params.SessionID, c.URL, doc.Metadata["pages_read"], doc.Metadata["pages"])
		head = fmt.Sprintf("Only the first %s of the %s pages of this PDF could be read. What you need may be in the rest: look for it in another source, and say so if you could not find it.\n",
			doc.Metadata["pages_read"], doc.Metadata["pages"])
	}
	// A page from Vedetta's memory says when it was read, the same day too, and how to
	// get it as it is now (Piano dopo audit 3.6; map R2b, session a9fe25e5: yesterday's
	// prices given as today's).
	if at := doc.ExtractedAt; l.e.cacheReadDate && !at.IsZero() && time.Since(at) > loopMemoryAge {
		page.ReadOn = at
		head = "This is Vedetta's copy of the page, read on " + at.Local().Format("2 January 2006 at 15:04") +
			", not now. If its data may have changed since then, read it again with fresh: true.\n" + head
	}
	// A page is read whole. What does not fit in the conversation cannot be, and
	// half of the room stays for other sources: one law does not end the research.
	if len(doc.Content) > l.room {
		page.Text = cutAtLine(doc.Content, l.room/2)
		page.Partial = true
		l.stats.PagesPartial++
		log.Printf("[deeploop] session=%s read url=%s partial chars=%d of %d", l.params.SessionID, c.URL, len(page.Text), len(doc.Content))
		head += "This page is too long for this conversation: you have only its beginning. What you need may be in the rest: look for it in another source, and say so if you could not find it.\n"
	}
	l.noteSeenURLs(page.Text)
	l.seenTexts = append(l.seenTexts, title+"\n"+page.Text)
	l.pages[norm] = page
	l.pageOrder = append(l.pageOrder, norm)
	preview := page.Text
	if len(preview) > 300 {
		preview = preview[:300]
	}
	l.addSource(search.Result{Title: title, URL: c.URL, Content: strings.ToValidUTF8(preview, "")})
	c.Result = "ok"
	return head + loopPageBlock("Page", pageTitle(page.Title), page.URL, page.Text)
}

// cutAtLine returns the beginning of text, about size bytes, ending at a line end
// when there is one in the last tenth, never inside a UTF-8 character.
func cutAtLine(text string, size int) string {
	if len(text) <= size {
		return text
	}
	cut := size
	if nl := strings.LastIndexByte(text[size-size/10:size], '\n'); nl >= 0 {
		cut = size - size/10 + nl + 1
	}
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// loopContextLimit is how far the conversation may grow, in bytes (DL3,
// docs/SPEC_DL3_MEMORIA_DEL_MODELLO.md §3.2): the memory less the room for the
// last answer (the output cap, at most a quarter of the memory) and the
// template, at 3 bytes per token. At home a memory read from the server does
// not take the conversation past loopLocalMaxChars: memory is the engine's,
// already reserved, but each turn rereads the whole conversation, and at home
// that is time. A memory set by hand is a choice, and holds.
func loopContextLimit(info llm.ContextInfo, local bool) int {
	reserve := info.Tokens / 4
	if info.OutputCap > 0 && info.OutputCap < reserve {
		reserve = info.OutputCap
	}
	limit := (info.Tokens - reserve - llm.ContextMargin) * llm.BytesPerToken
	if local && info.Source != "setting" && limit > loopLocalMaxChars {
		limit = loopLocalMaxChars
	}
	return max(limit, 0)
}

// checkPageChars is the most of a page shown to the check of a cited link: it
// is a conversation of its own, and must fit in a small memory too.
func (l *deepLoop) checkPageChars() int {
	if l.limit > 0 && l.limit/2 < loopCheckPageChars {
		return l.limit / 2
	}
	return loopCheckPageChars
}

// conversationChars is what the model receives on each call, its reasoning included.
func conversationChars(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content) + len(m.ReasoningContent)
		for _, c := range m.ToolCalls {
			n += len(c.Function.Arguments)
		}
	}
	return n
}

func argString(args map[string]interface{}, k string) string {
	switch v := args[k].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

// An address may hold parentheses, as Wikipedia's do: the link ends at the one that
// closes its own.
var answerLinkRe = regexp.MustCompile(`\]\((https?://(?:\([^()\s]*\)|[^)\s])+)\)|\[(https?://[^\]\s]+)\]`)

// citedPages counts the distinct pages the answer links to: those the model read,
// and those it did not.
func (l *deepLoop) citedPages(answer string) (read, unread int) {
	seen := make(map[string]bool)
	for _, m := range answerLinkRe.FindAllStringSubmatch(answer, -1) {
		u := m[1]
		if u == "" {
			u = m[2]
		}
		k := normalizeLinkURL(u)
		if seen[k] {
			continue
		}
		seen[k] = true
		if l.pages[k] != nil {
			read++
		} else {
			unread++
		}
	}
	return read, unread
}

// citedBefore counts the pages the answer links to that only an earlier turn read.
func (l *deepLoop) citedBefore(answer string) int {
	seen := make(map[string]bool)
	for _, m := range answerLinkRe.FindAllStringSubmatch(answer, -1) {
		u := m[1]
		if u == "" {
			u = m[2]
		}
		if k := normalizeLinkURL(u); l.pages[k] == nil && l.before[k] != "" {
			seen[k] = true
		}
	}
	return len(seen)
}

// pageRemovedLine takes the place of a page taken out of the conversation.
const pageRemovedLine = "This page did not fit in the conversation and was removed."

// refusedForMemory reports whether err says the conversation does not fit in
// the model's memory. Kilo's silent empty answer counts only with a
// conversation over half the memory: finish_reason "other" has other causes
// too, and pages taken out for nothing are a loss.
func (l *deepLoop) refusedForMemory(err error, messages []llm.Message) bool {
	var ce *llm.ContextError
	if !errors.As(err, &ce) {
		return false
	}
	return !ce.Silent || conversationChars(messages) > l.stats.ContextTokens*llm.BytesPerToken/2
}

// makeRoom takes pages out of the conversation after a refusal, and returns
// how many. First the pages read in the last turn, which the model never got;
// when there are none, or at the second refusal, the longest page left. After
// two tries it gives up: the refusal is then an error as before.
func (l *deepLoop) makeRoom(messages []llm.Message) int {
	if l.roomTries >= 2 {
		return 0
	}
	ids := map[string]bool{}
	if l.roomTries == 0 {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == "assistant" && len(messages[i].ToolCalls) > 0 {
				for _, tc := range messages[i].ToolCalls {
					if _, ok := l.readCalls[tc.ID]; ok {
						ids[tc.ID] = true
					}
				}
				break
			}
		}
	}
	if len(ids) == 0 {
		longest, size := "", 0
		for _, m := range messages {
			if _, ok := l.readCalls[m.ToolCallID]; ok && m.Role == "tool" && len(m.Content) > size {
				longest, size = m.ToolCallID, len(m.Content)
			}
		}
		if longest != "" {
			ids[longest] = true
		}
	}
	removed := 0
	for i := range messages {
		if messages[i].Role != "tool" || !ids[messages[i].ToolCallID] {
			continue
		}
		messages[i].Content = pageRemovedLine
		l.removePage(l.readCalls[messages[i].ToolCallID])
		delete(l.readCalls, messages[i].ToolCallID)
		removed++
	}
	if removed > 0 {
		l.roomTries++
	}
	return removed
}

// removePage forgets a page as read: the model writes without it, so it is not
// a page read, nor evidence for the checks. It stays in the list, marked.
func (l *deepLoop) removePage(key string) {
	p := l.pages[key]
	if p == nil {
		return
	}
	delete(l.pages, key)
	for i, k := range l.pageOrder {
		if k == key {
			l.pageOrder = append(l.pageOrder[:i], l.pageOrder[i+1:]...)
			break
		}
	}
	for i, t := range l.seenTexts {
		if t == p.Title+"\n"+p.Text {
			l.seenTexts = append(l.seenTexts[:i], l.seenTexts[i+1:]...)
			break
		}
	}
	l.removed = append(l.removed, p)
	l.stats.PagesRemoved++
}

// bibliography lists the pages read, in reading order, and then the pages
// taken out because they did not fit.
func (l *deepLoop) bibliography(lang string) string {
	if len(l.pageOrder) == 0 && len(l.removed) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n---\n### " + StepMessage(lang, "loop_pages_read") + "\n\n")
	for i, k := range l.pageOrder {
		p := l.pages[k]
		fmt.Fprintf(&b, "%d. [%s](%s)", i+1, strings.ReplaceAll(p.Title, "]", ")"), p.URL)
		if p.Partial {
			b.WriteString(" (" + StepMessage(lang, "loop_page_partial") + ")")
		}
		if !p.ReadOn.IsZero() {
			hm := p.ReadOn.Local().Format("15:04")
			if sameDay(p.ReadOn, time.Now()) {
				b.WriteString(" (" + StepMessage(lang, "loop_page_read_today", hm) + ")")
			} else {
				b.WriteString(" (" + StepMessage(lang, "loop_page_read_on_at", localDate(lang, p.ReadOn), hm) + ")")
			}
		}
		b.WriteString("\n")
	}
	for _, p := range l.removed {
		fmt.Fprintf(&b, "- [%s](%s) (%s)\n", strings.ReplaceAll(p.Title, "]", ")"), p.URL, StepMessage(lang, "loop_page_removed"))
	}
	return b.String()
}

// pageNumbers maps each page read (normalizeLinkURL) to its number in «Pagine lette».
func (l *deepLoop) pageNumbers() map[string]int {
	out := make(map[string]int, len(l.pageOrder))
	for i, k := range l.pageOrder {
		out[normalizeLinkURL(l.pages[k].URL)] = i + 1
	}
	return out
}

// loopNote says in one line what the research did and what Go dropped.
// evidence is what the model saw: the question, today's date, the pages read and
// the previews, and the earlier questions (a follow-up may repeat what the user gave).
func (l *deepLoop) evidence(query string, history []storage.TurnMessage) []string {
	out := append(append([]string{query}, todayTexts(time.Now())...), l.seenTexts...)
	for _, m := range history {
		out = append(out, m.Query)
	}
	for _, p := range l.earlier {
		out = append(out, p.Title+"\n"+p.Text)
	}
	return out
}

// sourceAndPageURLs are the addresses the model was shown: the pages it read and, with
// previews, the sources of its searches too.
func (l *deepLoop) sourceAndPageURLs(previews bool) []string {
	var out []string
	if previews {
		for _, r := range l.sources {
			out = append(out, r.URL)
		}
	}
	for _, k := range l.pageOrder {
		out = append(out, l.pages[k].URL)
	}
	for _, p := range l.earlier {
		out = append(out, p.URL)
	}
	return out
}

// review is the sentinel's check of the draft. Without research the only source
// is the question: a greeting goes out, an answer stating names and numbers from
// memory goes back (case Eiffel, 30/10/2026).
func (l *deepLoop) review(draft, query string, history []storage.TurnMessage) []ReviewItem {
	ev := l.evidence(query, history)
	// A site cited by name: its name is in the address.
	addresses := l.sourceAndPageURLs(true)
	names := append(append([]string{}, ev...), addresses...)
	seen := strings.Join(names, "\n")
	items := reviewDraft(draft, l.params.AnswerLanguage, l.numbersSeen(ev, addresses), seen)
	if !l.e.deepLoopPreviews {
		return withSpellings(items, seen)
	}
	// The same check on the pages read alone: what it adds was seen only in previews.
	pages := append([]string{query}, todayTexts(time.Now())...)
	for _, k := range l.pageOrder {
		pages = append(pages, l.pages[k].Title+"\n"+l.pages[k].Text)
	}
	for _, m := range history {
		pages = append(pages, m.Query)
	}
	for _, p := range l.earlier { // read in an earlier turn: pages read, not previews
		pages = append(pages, p.Title+"\n"+p.Text)
	}
	pageNames := append(append([]string{}, pages...), addresses...)
	items = previewOnly(items, reviewDraft(draft, l.params.AnswerLanguage, l.numbersSeen(pages, l.sourceAndPageURLs(false)), strings.Join(pageNames, "\n")))
	return withSpellings(l.openPreviewPages(items), seen)
}

// openPreviewPages opens the result pages of the details the draft found only in
// previews (up to loopReviewPages, at once) and attaches to each sentence the
// page's passage closest to it. A sentence whose details the page contains is not
// sent back: it rests on a page Vedetta read, as if the model had read it.
func (l *deepLoop) openPreviewPages(items []ReviewItem) []ReviewItem {
	lang := l.params.AnswerLanguage
	var urls []string
	for i := range items {
		if len(items[i].Preview) == 0 || i >= loopReviewMaxItems {
			continue
		}
		r, ok := bestPreview(items[i], lang, l.previews)
		if !ok {
			continue
		}
		k := normalizeLinkURL(r.URL)
		known := false
		for _, u := range urls {
			known = known || normalizeLinkURL(u) == k
		}
		if !known && len(urls) == loopReviewPages {
			continue // no page for it: the message asks to read it or remove the detail
		}
		if !known {
			urls = append(urls, r.URL)
		}
		items[i].URL = r.URL
	}
	texts := make([]string, len(urls))
	opened := make([]bool, len(urls))
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(reader.WithSessionID(l.ctx, l.params.SessionID), loopReadTimeout)
			doc, err := l.cr.Read(ctx, u)
			cancel()
			status, detail := classifyRead(doc, err)
			log.Printf("[deeploop] session=%s review read url=%s status=%s %s", l.params.SessionID, u, status, detail)
			if status == readOK || status == readThin {
				texts[i] = doc.Title + "\n" + doc.Content
				opened[i] = status == readOK
			}
		}(i, u)
	}
	wg.Wait()
	for i, u := range urls {
		if opened[i] {
			l.citedTexts[normalizeLinkURL(u)] = texts[i] // the check of the cited pages does not open it again
		}
	}
	l.stats.ReviewPages += len(urls)
	byURL := make(map[string]string, len(urls))
	for i, u := range urls {
		byURL[u] = texts[i]
	}

	var out []ReviewItem
	for _, it := range items {
		if it.URL != "" {
			text := byURL[it.URL]
			if text == "" {
				it.Unread = true
			} else {
				it.Lacks = absentIn(it.Sentence, lang, text+"\n"+it.URL, it.Preview)
				it.Passage = selectExcerpt(text, claimText(it.Sentence), loopReviewPassageChars)
				l.seenTexts = append(l.seenTexts, it.Passage) // what the model is shown
				if len(it.Lacks) == 0 && len(it.Numbers)+len(it.Names) == 0 {
					l.stats.ReviewConfirmed = append(l.stats.ReviewConfirmed, it)
					continue
				}
			}
		}
		out = append(out, it)
	}
	return out
}

// reviewNote tells the user that the draft was sent back, and with how many sentences.
func reviewNote(lang string, s LoopStats) string {
	if !s.Reviewed || len(s.ReviewItems) == 0 {
		return ""
	}
	if len(s.ReviewItems) == 1 {
		return "\n\n*" + StepMessage(lang, "loop_review_note_one") + "*"
	}
	return "\n\n*" + StepMessage(lang, "loop_review_note", len(s.ReviewItems)) + "*"
}

// reviewUnfinishedNote tells the user that the answer is the first draft: the
// rewrite asked for did not finish.
func reviewUnfinishedNote(lang string, s LoopStats) string {
	if !s.ReviewUnfinished {
		return ""
	}
	return "\n\n*" + StepMessage(lang, "loop_review_unfinished") + "*"
}

// reviewSkippedNote tells the user that the draft went out without the sentinel's
// turn: its writing was cut.
func reviewSkippedNote(lang string, s LoopStats) string {
	if s.ReviewSkipped == "" {
		return ""
	}
	return "\n\n*" + StepMessage(lang, "loop_review_skipped") + "*"
}

// linkReviewNote tells the user that sentences went back for the page they cite.
func linkReviewNote(lang string, s LoopStats) string {
	switch len(s.LinkTroubles) {
	case 0:
		return ""
	case 1:
		return "\n\n*" + StepMessage(lang, "loop_link_review_note_one") + "*"
	}
	return "\n\n*" + StepMessage(lang, "loop_link_review_note", len(s.LinkTroubles)) + "*"
}

// loopNoteWords are the three counts of the note, one and many.
var loopNoteWords = map[string][3][2]string{
	"it": {{"1 ricerca", "%d ricerche"}, {"1 pagina letta", "%d pagine lette"}, {"1 non leggibile", "%d non leggibili"}},
	"en": {{"1 search", "%d searches"}, {"1 page read", "%d pages read"}, {"1 unreadable", "%d unreadable"}},
	"de": {{"1 Suche", "%d Suchen"}, {"1 gelesene Seite", "%d gelesene Seiten"}, {"1 nicht lesbar", "%d nicht lesbar"}},
	"fr": {{"1 recherche", "%d recherches"}, {"1 page lue", "%d pages lues"}, {"1 illisible", "%d illisibles"}},
	"es": {{"1 búsqueda", "%d búsquedas"}, {"1 página leída", "%d páginas leídas"}, {"1 ilegible", "%d ilegibles"}},
}

func countedWords(lang string, which, n int) string {
	words, ok := loopNoteWords[NormalizeLanguage(lang)]
	if !ok {
		words = loopNoteWords["en"]
	}
	if n == 1 {
		return words[which][0]
	}
	return fmt.Sprintf(words[which][1], n)
}

// groupThousands writes n with the language's thousands separator: 131.072,
// 131,072.
func groupThousands(lang string, n int) string {
	sep := "."
	switch NormalizeLanguage(lang) {
	case "en":
		sep = ","
	case "fr":
		sep = "\u202f"
	}
	digits := strconv.Itoa(n)
	var b strings.Builder
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteString(sep)
		}
		b.WriteRune(d)
	}
	return b.String()
}

func loopNote(lang string, s LoopStats, searches, dropped int) string {
	note := "\n\n---\n*" + StepMessage(lang, "loop_note", countedWords(lang, 0, searches), countedWords(lang, 1, s.PagesRead), countedWords(lang, 2, s.Unreadable)) + "*"
	if s.PagesPartial > 0 {
		note += "\n\n*" + StepMessage(lang, "loop_note_partial", s.PagesPartial) + "*"
	}
	if s.PDFsPartial > 0 {
		note += "\n\n*" + StepMessage(lang, "loop_note_pdf_partial", s.PDFsPartial) + "*"
	}
	if n := s.granted(); n > 0 {
		note += "\n\n*" + StepMessage(lang, "loop_note_grant", n*loopGrantCalls) + "*"
	}
	if s.ClosedBy == "context" && s.ContextTokens > 0 {
		line := StepMessage(lang, "loop_note_context", groupThousands(lang, s.ContextTokens))
		if s.ContextSource == "default" {
			line += " " + StepMessage(lang, "loop_note_context_default")
		}
		note += "\n\n*" + line + "*"
	}
	if s.PagesRemoved > 0 {
		note += "\n\n*" + StepMessage(lang, "loop_note_refused", countedWords(lang, 1, s.PagesRemoved)) + "*"
	}
	if total := s.PagesCited + s.LinksUnread; total > 0 {
		if s.LinkStates != nil {
			note += "\n\n*" + StepMessage(lang, "loop_note_links_checked", total, s.PagesCited, s.LinksControlled, s.LinksPartial, s.LinksUnchecked, s.LinksAddress) + "*"
		} else {
			note += "\n\n*" + StepMessage(lang, "loop_note_links", total, s.PagesCited, s.LinksUnread) + "*"
		}
	}
	if s.LinksRemoved > 0 {
		note += "\n\n*" + StepMessage(lang, "loop_note_links_removed", s.LinksRemoved) + "*"
	}
	if s.LinkCheckOff {
		note += "\n\n*" + StepMessage(lang, "loop_note_check_off") + "*"
	}
	if s.LinksVerified > 0 {
		note += "\n\n*" + StepMessage(lang, "loop_note_verified", s.LinksVerified) + "*"
	}
	if dropped > 0 {
		note += "\n\n*" + StepMessage(lang, "loop_note_dropped", dropped) + "*"
	}
	return note
}

// verifyLink is the reconciler's verifier: Vedetta opens a link the model wrote
// that is not among the pages found, and keeps it when the page exists and names
// what the link points to (the repository, or the site). A page that is only
// similar, such as a different project at an invented address, does not pass.
func (l *deepLoop) verifyLink(raw, text string) bool {
	k := normalizeLinkURL(raw)
	if held, done := l.checked[k]; done {
		return held
	}
	if len(l.checked) >= loopMaxLinkChecks || !isHTTPPrefix(raw) || l.ctx == nil {
		return false
	}
	name := linkName(raw)
	ctx, cancel := context.WithTimeout(reader.WithSessionID(l.ctx, l.params.SessionID), loopLinkTimeout)
	doc, err := l.cr.Read(ctx, raw)
	cancel()
	status, detail := classifyRead(doc, err)
	held := false
	switch {
	case status != readOK && status != readThin:
	case name == "":
		detail = "no name to look for"
	case !strings.Contains(alnumLower(doc.Title+" "+doc.Content), alnumLower(name)):
		detail = fmt.Sprintf("page does not name %q", name)
	default:
		held = true
	}
	l.checked[k] = held
	log.Printf("[deeploop] session=%s link check url=%s name=%q held=%t status=%s %s", l.params.SessionID, raw, name, held, status, detail)
	if held {
		l.verified[k] = doc.Title + "\n" + doc.Content
	}
	if held && !l.seenSet[k] {
		l.seenSet[k] = true
		l.seen = append(l.seen, raw)
	}
	return held
}

// genericSegments are path parts that do not name a project.
var genericSegments = map[string]bool{"releases": true, "release": true, "tag": true, "tags": true, "tree": true, "blob": true,
	"main": true, "master": true, "docs": true, "doc": true, "wiki": true, "latest": true, "index": true, "readme": true,
	"issues": true, "download": true, "downloads": true, "about": true, "home": true, "en": true, "it": true}

// linkName is what a page at raw should name: the last path part that is not
// generic (the repository in github.com/owner/repo), else the site's name.
func linkName(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := len(segs) - 1; i >= 0; i-- {
		seg := strings.ToLower(segs[i])
		seg = strings.TrimSuffix(seg, path.Ext(seg))
		if len(alnumLower(seg)) >= 3 && !genericSegments[seg] {
			return seg
		}
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if label, _, _ := strings.Cut(host, "."); len(label) >= 3 {
		return label
	}
	return ""
}

// alnumLower keeps letters and digits, lowercased: "Uptime Kuma" and
// "uptime-kuma" both become "uptimekuma".
func alnumLower(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// readerFallbackNote says, at the end of the answer, that some pages were read
// by a reader other than the first of the chain (transparency of the fallback,
// SPEC_UI_IMPOSTAZIONI_E_BARRA S5). It names readers, never counts losses.
func readerFallbackNote(lang string, cr reader.Reader, sessionID string) string {
	fd, ok := cr.(reader.FallbackDetailer)
	if !ok {
		return ""
	}
	primary, served := fd.FallbackReaders(sessionID)
	if len(served) == 0 {
		return ""
	}
	names := make([]string, len(served))
	for i, n := range served {
		names[i] = readerDisplayName(lang, n)
	}
	return "\n\n*" + StepMessage(lang, "loop_note_reader_fallback", strings.Join(names, ", "), readerDisplayName(lang, primary)) + "*"
}

func readerDisplayName(lang, name string) string {
	switch name {
	case "http", "direct":
		return StepMessage(lang, "reader_name_http")
	case "jina", "jina-reader":
		return "Jina Reader"
	case "browser":
		return "Chromium"
	case "firecrawl":
		return "Firecrawl"
	case "exa":
		return "Exa"
	}
	return name
}
