package agent

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"vedetta/internal/search"
)

// readTurns are n turns that each read a page of its own, from number first on.
func readTurns(first, n int) []string {
	var turns []string
	for i := first; i < first+n; i++ {
		turns = append(turns, callTurn(fmt.Sprintf("r%d", i), "read", fmt.Sprintf(`{"url":"https://real.test/p%d"}`, i)))
	}
	return turns
}

// searchTurns are n turns that each search something else.
func searchTurns(first, n int) []string {
	var turns []string
	for i := first; i < first+n; i++ {
		turns = append(turns, callTurn(fmt.Sprintf("s%d", i), "search", fmt.Sprintf(`{"query":"words %d"}`, i)))
	}
	return turns
}

// grantRun runs the Deep loop with the cap of production (40) and the special
// permission as asked.
func grantRun(t *testing.T, turns []string, setup ...func(*Engine)) (loopRun, LoopStats, *scriptedLLM) {
	t.Helper()
	s := &scriptedLLM{turns: turns, models: halogenTestModels}
	sp := &mockSearchProvider{results: []search.Result{{Title: "r", URL: "https://real.test/r", Content: "x"}}}
	run, stats := runLoopMetaWith(t, http.HandlerFunc(s.handler), sp, &mockReader{}, setup...)
	if run.err != nil {
		t.Fatal(run.err)
	}
	return run, stats, s
}

func withGrant(e *Engine) { e.SetDeepLoopGrant(true) }

func join(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func steps(run loopRun, text string) int {
	n := 0
	for _, ev := range run.events {
		if ev.Type == EventStep && strings.Contains(ev.Data.(map[string]string)["message"], text) {
			n++
		}
	}
	return n
}

// The research is still opening pages at the cap: it goes on, nothing is said to the
// model, the user sees a step and a line of the note.
func TestDeepLoop_GrantWhenPagesAreStillComing(t *testing.T) {
	turns := join(searchTurns(0, 36), readTurns(0, 4), readTurns(100, 5), []string{contentTurn("Risposta completa. " + prose(30))})
	run, stats, s := grantRun(t, turns, withGrant)
	if stats.Calls != 45 || stats.ClosedBy != "answer" || stats.PagesRead != 9 {
		t.Errorf("calls=%d closed_by=%s pages=%d, want 45, answer, 9", stats.Calls, stats.ClosedBy, stats.PagesRead)
	}
	if len(stats.Grants) != 1 || stats.Grants[0].Why != "" || stats.Grants[0].Calls != 40 || stats.Grants[0].Pages != 4 {
		t.Errorf("grants = %+v", stats.Grants)
	}
	if steps(run, StepMessage("", "loop_grant", 40)) != 1 || !strings.Contains(run.answer, "Vedetta ha concesso altre 40") {
		t.Error("the user is not told of the calls allowed")
	}
	if role, _ := lastUserOrTool(s.bodies[40]); role != "tool" {
		t.Errorf("the request after the cap ends with %s: the model should be told nothing", role)
	}
	if _, ok := s.bodies[40]["tools"]; !ok {
		t.Error("no tools after the permission")
	}
}

// Twice at most: 40, 80, 120, then the research closes as always.
func TestDeepLoop_GrantStopsAtTwo(t *testing.T) {
	turns := join(readTurns(0, 120), []string{contentTurn("Risposta completa. " + prose(30))})
	run, stats, s := grantRun(t, turns, withGrant)
	if stats.Calls != 120 || stats.ClosedBy != "budget" || len(s.bodies) < 121 {
		t.Errorf("calls=%d closed_by=%s requests=%d, want 120, budget, 121", stats.Calls, stats.ClosedBy, len(s.bodies))
	}
	if stats.granted() != 2 || len(stats.Grants) != 3 || stats.Grants[2].Why != "max" {
		t.Errorf("grants = %+v", stats.Grants)
	}
	if _, msg := lastUserOrTool(s.bodies[120]); !strings.Contains(msg, "used all the searches") {
		t.Errorf("at 120 the model is not asked to write: %q", msg)
	}
	if !strings.Contains(run.answer, "Vedetta ha concesso altre 80") {
		t.Error("the note does not count both times")
	}
}

// No permission: the research closes at 40, and the reason is kept.
func TestDeepLoop_GrantRefused(t *testing.T) {
	answer := []string{contentTurn("Risposta completa. " + prose(30))}
	cases := []struct {
		name  string
		turns []string
		setup []func(*Engine)
		why   string // "-": no look at all
	}{
		{"off", join(readTurns(0, 40), answer), nil, "-"},
		{"spinning: only searches", join(readTurns(0, 20), searchTurns(0, 20), answer), []func(*Engine){withGrant}, "pages"},
		{"three pages are few", join(searchTurns(0, 37), readTurns(0, 3), answer), []func(*Engine){withGrant}, "pages"},
		{"a repeat in the last calls", join(readTurns(0, 39), readTurns(5, 1), answer), []func(*Engine){withGrant}, "repeats"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run, stats, _ := grantRun(t, c.turns, c.setup...)
			if stats.Calls != 40 || stats.ClosedBy != "budget" {
				t.Errorf("calls=%d closed_by=%s, want 40 and budget", stats.Calls, stats.ClosedBy)
			}
			if c.why == "-" {
				if stats.Grants != nil {
					t.Errorf("grants = %+v with the switch off", stats.Grants)
				}
			} else if len(stats.Grants) != 1 || stats.Grants[0].Why != c.why {
				t.Errorf("grants = %+v, want %q", stats.Grants, c.why)
			}
			if strings.Contains(run.answer, "ha concesso") {
				t.Error("the note says calls were allowed")
			}
		})
	}
}

// A repeat far back does not count: only the last calls are looked at.
func TestDeepLoop_GrantOldRepeatForgotten(t *testing.T) {
	turns := join(readTurns(0, 5), readTurns(0, 1), readTurns(10, 34), []string{contentTurn("Risposta completa. " + prose(30))})
	_, stats, _ := grantRun(t, turns, withGrant)
	if stats.granted() != 1 {
		t.Errorf("grants = %+v, want one", stats.Grants)
	}
}

// Memory, time, the review and "Continua a cercare", on the rule itself.
func TestGrantCalls_Limits(t *testing.T) {
	base := func() *deepLoop {
		l := &deepLoop{emit: func(Event) error { return nil }, grantOn: true, limit: 1000, maxCalls: 40, started: time.Now(), total: 40 * time.Minute}
		l.stats.Calls = 40
		for i := 0; i < 40; i++ {
			l.stats.CallsDetail = append(l.stats.CallsDetail, loopCall{Tool: "read", URL: fmt.Sprintf("https://real.test/%d", i), Result: "ok"})
		}
		return l
	}
	l := base()
	if !l.grantCalls(499) || l.maxCalls != 80 {
		t.Fatalf("not allowed: %+v", l.stats.Grants)
	}
	for _, c := range []struct {
		name  string
		chars int
		edit  func(*deepLoop)
		why   string
	}{
		{"half of the memory", 500, func(*deepLoop) {}, "context"},
		{"unknown memory", 0, func(l *deepLoop) { l.limit = 0 }, "context"},
		{"a quarter of the time", 0, func(l *deepLoop) { l.started = time.Now().Add(-10 * time.Minute) }, "time"},
		{"the review", 0, func(l *deepLoop) { l.rewriting = true }, "-"},
		{"after the request to go on", 0, func(l *deepLoop) { l.stats.Push = "asked" }, "-"},
	} {
		l := base()
		c.edit(l)
		if l.grantCalls(c.chars) || l.maxCalls != 40 {
			t.Errorf("%s: allowed", c.name)
		}
		if c.why == "-" && l.stats.Grants != nil || c.why != "-" && (len(l.stats.Grants) != 1 || l.stats.Grants[0].Why != c.why) {
			t.Errorf("%s: grants = %+v, want %q", c.name, l.stats.Grants, c.why)
		}
	}
}

// A page read again now (fresh) is not a new page; unreadable pages are not pages.
func TestLastNewPages(t *testing.T) {
	calls := []loopCall{
		{Tool: "read", URL: "https://a.test/1", Result: "ok"},
		{Tool: "search", Query: "x", Result: "repeated"},
		{Tool: "read", URL: "https://a.test/1", Result: "ok", Fresh: true},
		{Tool: "read", URL: "https://a.test/2", Result: "ok"},
		{Tool: "read", URL: "https://a.test/2", Result: "ok", Fresh: true},
		{Tool: "read", URL: "https://a.test/3", Result: "unreadable"},
	}
	if p, r := lastNewPages(calls, 4); p != 1 || r != 0 {
		t.Errorf("last 4: pages=%d repeats=%d, want 1 and 0", p, r)
	}
	if p, r := lastNewPages(calls, 20); p != 2 || r != 1 {
		t.Errorf("all: pages=%d repeats=%d, want 2 and 1", p, r)
	}
}

// The review after a permission keeps its few calls: the cap it reaches is its own.
func TestDeepLoop_GrantNotInReview(t *testing.T) {
	draft := "La quota è di 98765 euro secondo il documento. " + prose(30)
	turns := join(readTurns(0, 41), []string{contentTurn(draft)}, readTurns(200, 8), []string{contentTurn("Risposta completa. " + prose(30))})
	_, stats, _ := grantRun(t, turns, withGrant, func(e *Engine) { e.SetDeepLoopReview(true) })
	if !stats.Reviewed {
		t.Fatal("the draft was not sent back: the test looks at nothing")
	}
	if stats.Calls != 49 || len(stats.Grants) != 1 {
		t.Errorf("calls=%d grants=%+v, want 49 calls and one look", stats.Calls, stats.Grants)
	}
}
