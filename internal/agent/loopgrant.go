package agent

import (
	"log"
	"time"
)

// The special permission (docs/SPEC_PERMESSO_SPECIALE.md, 24/11/2026): a model that
// reaches the cap is still asking for tools, so it has not finished. When its last
// calls were still opening new pages, Vedetta lets it go on instead of closing the
// research. Nothing is said to the model; the user is told.
const (
	loopGrantCalls  = 40 // searches and readings allowed each time
	loopGrantMax    = 2  // times, at most: 40 → 80 → 120
	loopGrantWindow = 20 // the last calls looked at
	loopGrantPages  = 4  // new pages read in them, at least: spinning researches had 0 to 2
)

// LoopGrant is one look at the cap: what the research was doing, and whether more
// calls were allowed.
type LoopGrant struct {
	Calls        int    `json:"calls"` // calls made when the cap was reached
	Pages        int    `json:"pages"` // new pages read in the last loopGrantWindow calls
	ContextChars int    `json:"context_chars"`
	Seconds      int    `json:"seconds"`       // since the start
	Why          string `json:"why,omitempty"` // why not, "" when allowed: pages, repeats, context, time, max
}

// lastNewPages looks at the last calls: the pages read there for the first time, and
// the calls already made before.
func lastNewPages(calls []loopCall, window int) (pages, repeats int) {
	from := max(0, len(calls)-window)
	read := map[string]bool{}
	for i, c := range calls {
		if i >= from && c.Result == "repeated" {
			repeats++
		}
		if c.Tool != "read" || c.Result != "ok" {
			continue
		}
		k := normalizeLinkURL(c.URL)
		if i >= from && !read[k] {
			pages++
		}
		read[k] = true
	}
	return pages, repeats
}

// grantCalls decides, at the cap, whether the research goes on. The review's few calls
// and a research reopened by "Continua a cercare" keep their limit: there the model has
// already written its answer.
func (l *deepLoop) grantCalls(contextChars int) bool {
	if !l.grantOn || l.rewriting || l.stats.Push != "" {
		return false
	}
	pages, repeats := lastNewPages(l.stats.CallsDetail, loopGrantWindow)
	elapsed := time.Since(l.started)
	g := LoopGrant{Calls: l.stats.Calls, Pages: pages, ContextChars: contextChars, Seconds: int(elapsed.Seconds())}
	switch {
	case l.stats.granted() >= loopGrantMax:
		g.Why = "max"
	case repeats > 0:
		g.Why = "repeats"
	case pages < loopGrantPages:
		g.Why = "pages"
	case contextChars >= l.limit/2:
		g.Why = "context"
	case elapsed >= l.total/4: // the writing and the checks need the rest
		g.Why = "time"
	}
	l.stats.Grants = append(l.stats.Grants, g)
	log.Printf("[deeploop] session=%s cap reached: calls=%d new_pages=%d of the last %d calls, repeats=%d, context_chars=%d of %d, seconds=%d: more calls %t %s",
		l.params.SessionID, g.Calls, pages, loopGrantWindow, repeats, contextChars, l.limit, g.Seconds, g.Why == "", g.Why)
	if g.Why != "" {
		return false
	}
	l.maxCalls = l.stats.Calls + loopGrantCalls
	l.step("planning", StepMessage(l.params.UILanguage, "loop_grant", loopGrantCalls))
	return true
}

// granted counts the times more calls were allowed.
func (s LoopStats) granted() int {
	n := 0
	for _, g := range s.Grants {
		if g.Why == "" {
			n++
		}
	}
	return n
}
