package agent

import (
	"log"
	"time"

	"vedetta/internal/llm"
)

// "Continua a cercare" (Piano dopo audit 7.13, docs/SPEC_CONTINUA_A_CERCARE.md):
// when the model hands in its first draft, Vedetta asks it once, with a fixed text,
// whether there is more to look for. The model chooses what to search; Go only
// asks. It comes before the sentinel's checks, which then look at the draft that
// stands, once: the new one when the model searched or read again, the first one
// otherwise.

// pushMessage is the request, the same for every question and without examples
// (rule 5). It is a turn of the conversation, not a line of the system prompt
// (rule 6): the model follows requests that come as turns.
const pushMessage = "Before this answer goes out, look at what it rests on. " +
	"If the pages you read report what another document, body or person says, and it matters for the answer, search for the original and read it. " +
	"If a part of the question had no search of its own, search it. " +
	"If you search or read anything more, write the whole answer again afterwards. " +
	"If there is nothing more to look for, reply with one word: done."

// loopPush is the state of the request while it waits for the model's answer.
type loopPush struct {
	open    bool      // asked, and no draft came after it yet
	draft   string    // the first draft, held aside
	calls   int       // calls made when it was asked
	pages   int       // pages read when it was asked
	at      int       // len(messages) before the request, to take it out again
	started time.Time // when it was asked
}

// askPush holds the first draft aside and asks the model to go on; the next draft
// is held again for the sentinel's turn. It returns the conversation with the
// request in it.
func (l *deepLoop) askPush(messages []llm.Message, draft string) []llm.Message {
	l.push = loopPush{open: true, draft: draft, calls: l.stats.Calls, pages: len(l.pageOrder), at: len(messages), started: time.Now()}
	l.stats.Push = "asked"
	log.Printf("[deeploop] session=%s push: the model is asked to go on (calls=%d, pages=%d)", l.params.SessionID, l.stats.Calls, len(l.pageOrder))
	l.step("deep_critic", StepMessage(l.params.UILanguage, "loop_push"))
	l.holding = true
	l.resetHeld()
	return append(messages, llm.Message{Role: "assistant", Content: draft}, llm.Message{Role: "user", Content: pushMessage})
}

// answerPush takes the draft written after the request. With no call since the
// request the first draft stands, whatever was written now, and the request
// leaves the conversation; otherwise the new draft takes its place.
func (l *deepLoop) answerPush(draft string, messages []llm.Message) (string, []llm.Message) {
	if l.stats.Calls == l.push.calls {
		l.settlePush("kept")
		draft, messages = l.push.draft, messages[:l.push.at]
	} else {
		l.settlePush("more")
	}
	log.Printf("[deeploop] session=%s push: %s (calls after=%d, pages after=%d, seconds=%d)",
		l.params.SessionID, l.stats.Push, l.stats.PushCalls, l.stats.PushPages, l.stats.PushSeconds)
	return draft, messages
}

// settlePush records the answer to the request: "kept" or "more".
func (l *deepLoop) settlePush(outcome string) {
	l.push.open = false
	l.stats.Push = outcome
	l.stats.PushCalls = l.stats.Calls - l.push.calls
	l.stats.PushPages = len(l.pageOrder) - l.push.pages
	l.stats.PushSeconds = int(time.Since(l.push.started).Seconds())
	if outcome == "more" {
		l.stats.PushDraft = l.push.draft
	}
}
