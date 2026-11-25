package agent

import (
	"context"
	"fmt"
	"log"
	"time"

	"vedetta/internal/storage"
)

// Deep Thought (Settings → Experimental): every question gets the answer, after seven and
// a half seconds in place of seven and a half million years. No search, no page, no model,
// and the note under the answer says so: whoever did not turn it on must understand.
var deepThoughtTime = 7500 * time.Millisecond

const deepThoughtAnswer = "**42**"

// SetDeepThought turns Deep Thought on or off; a search takes the value it finds at its start.
func (e *Engine) SetDeepThought(enabled bool) { e.deepThought.Store(enabled) }

// DeepThought reports whether Deep Thought is on.
func (e *Engine) DeepThought() bool { return e.deepThought.Load() }

func (e *Engine) answerDeepThought(ctx context.Context, params SearchParams, emit func(Event) error) error {
	log.Printf("[deepthought] session=%s thinking", params.SessionID)
	_ = emit(Event{Type: EventStep, Data: map[string]string{"step": "deep_thought", "message": StepMessage(params.UILanguage, "deep_thought_step")}})

	wait := time.NewTimer(deepThoughtTime)
	defer wait.Stop()
	select {
	case <-wait.C:
	case <-ctx.Done():
		// The Stop, before the answer: nothing to save.
		errMsg := StepMessage(params.UILanguage, "error_no_answer")
		_ = emit(Event{Type: EventError, Data: map[string]string{"error": errMsg}})
		return fmt.Errorf("%s", errMsg)
	}

	answer := deepThoughtAnswer + notesStart + "\n\n---\n*" + StepMessage(params.AnswerLanguage, "deep_thought_note") + "*"
	if e.storage != nil && params.SessionID != "" {
		_ = e.storage.AddMessage(storage.TurnMessage{
			SessionID: params.SessionID, Role: "assistant", Query: params.Query, Content: answer, Mode: params.Mode,
		})
	}
	_ = emit(Event{Type: EventToken, Data: answer})
	return emit(Event{Type: EventDone, Data: map[string]interface{}{
		"session_id": params.SessionID, "status": "completed", "partial": false,
		"provenance": "", // none of the words for a research fits
		"loop_calls": 0,
	}})
}
