package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"vedetta/internal/cache"
	"vedetta/internal/reader"
	"vedetta/internal/search"
	"vedetta/internal/storage"
)

// ExecuteDeepPath runs a research: the Deep a ciclo (deeploop.go), where the model
// drives the search with tools and Vedetta checks the draft.
func (e *Engine) ExecuteDeepPath(ctx context.Context, params SearchParams, emit Emitter) error {
	query := strings.TrimSpace(params.Query)
	if query == "" {
		return fmt.Errorf("empty query")
	}
	// "Riprendi" went with the old pipeline; it comes back rebuilt for the Deep a ciclo.
	if params.Resume {
		return errors.New(StepMessage(params.UILanguage, "error_resume_unavailable"))
	}
	// V25-T7.1b: stages that ran without the model, for the answer's limits note.
	trouble := newLLMTrouble()
	ctx = withLLMTrouble(ctx, trouble)

	sp := e.SearchProvider()
	cr := e.ContentReader()
	llmClient := e.LLMClient()

	// Per-search overrides (F3.6-5): apply to this call only, never globally.
	if params.SearchProviderOverride != nil {
		sp = params.SearchProviderOverride
	}
	if params.LLMClientOverride != nil {
		llmClient = params.LLMClientOverride
	}
	if params.ContentReaderOverride != nil {
		cr = params.ContentReaderOverride
	}
	// V25-R1: the cache wraps whatever provider and reader this search uses,
	// including per-search overrides and providers rebuilt from Settings.
	var cacheSess *cache.Session
	if e.cache != nil {
		cacheSess = e.cache.Session()
		sp = cacheSess.WrapSearch(sp)
		cr = cacheSess.WrapReader(cr)
	}

	// One way only: whatever mode an older client sends, the research is the Deep.
	params.Mode = "deep"

	var emitMu sync.Mutex
	safeEmit := func(ev Event) error {
		emitMu.Lock()
		defer emitMu.Unlock()
		if err := emit(ev); err != nil {
			log.Printf("[deep] emit failed (client disconnected?): %v", err)
			return err
		}
		return nil
	}

	// The session: a new one, or the earlier turns of this one for a follow-up.
	var history []storage.TurnMessage
	if e.storage != nil {
		if params.SessionID == "" {
			sess, sErr := e.storage.CreateSession(query, params.Mode)
			if sErr == nil && sess != nil {
				params.SessionID = sess.ID
				_ = safeEmit(Event{
					Type: EventSession,
					Data: map[string]string{"session_id": sess.ID, "title": sess.Title},
				})
			}
		} else {
			detail, sErr := e.storage.GetSession(params.SessionID)
			if sErr == nil && detail != nil {
				history = detail.Messages
				_ = safeEmit(Event{
					Type: EventSession,
					Data: map[string]string{"session_id": detail.ID, "title": detail.Title},
				})
			}
		}
	}

	// Once the session has a name: without one, what other searches had noted went too.
	if fb, ok := sp.(search.FallbackResetter); ok && params.SessionID != "" {
		fb.ResetFallback(params.SessionID)
	}
	if fb, ok := cr.(reader.FallbackResetter); ok && params.SessionID != "" {
		fb.ResetFallback(params.SessionID)
	}

	// Resolve the answer language now that the session is known (UI locale != answer language).
	e.resolveAnswerLanguage(&params)

	if e.deepThought.Load() {
		return e.answerDeepThought(ctx, params, safeEmit)
	}
	err := e.executeDeepLoop(ctx, params, safeEmit, sp, cr, llmClient, cacheSess, history, trouble)
	if errors.Is(err, errLoopUnsupported) {
		// A server that refuses tools cannot run the research: said in words, no fallback.
		return errors.New(StepMessage(params.UILanguage, "error_tools_required"))
	}
	return err
}

// errAnswerCut: the server stopped the answer at its length limit
// (finish_reason "length"), so the text ends mid-way.
var errAnswerCut = errors.New("answer cut by the server's length limit")
