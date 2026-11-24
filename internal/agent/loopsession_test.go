package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vedetta/internal/cache"
	"vedetta/internal/llm"
	"vedetta/internal/reader"
	"vedetta/internal/search"
	"vedetta/internal/storage"
)

// Piano dopo audit 4.4 / D9: in a follow-up the pages read in the earlier turns of
// the conversation count as seen, and the model is told which they are.

const (
	earlierURL  = "https://real.test/ponte"
	earlierGone = "https://real.test/sparita"
)

// earlierTurn is a conversation whose first answer read two pages: one is still in
// the cache, the other is not.
func earlierTurn(t *testing.T) (*mockReader, *cache.Cache, []storage.TurnMessage) {
	t.Helper()
	rd := &mockReader{docs: map[string]reader.Document{
		earlierURL: {URL: earlierURL, Title: "Il ponte Alfa", Content: "Il ponte Alfa è lungo 1.234 metri ed è stato progettato da Tizio Caio."},
	}}
	store, err := cache.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := cache.New(store, cache.ModeRecord, time.Hour, time.Hour)
	if _, err := c.Session().WrapReader(rd).Read(context.Background(), earlierURL); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(AnswerMeta{Loop: &LoopStats{CallsDetail: []loopCall{
		{Tool: "search", Query: "ponte Alfa", Result: "ok"},
		{Tool: "read", URL: earlierURL, Result: "ok"},
		{Tool: "read", URL: earlierURL + "#storia", Result: "ok"}, // the same page: once
		{Tool: "read", URL: earlierGone, Result: "ok"},
		{Tool: "read", URL: "https://real.test/rotta", Result: "unreadable"},
	}}})
	history := []storage.TurnMessage{{Role: "assistant", Query: "Parlami del ponte Alfa", Content: "È un ponte ad arco.", Meta: meta}}
	return rd, c, history
}

func TestSessionPages(t *testing.T) {
	_, c, history := earlierTurn(t)
	pages, gone := sessionPages(history, c.Session())
	if len(pages) != 1 || pages[0].URL != earlierURL || pages[0].Title != "Il ponte Alfa" || !strings.Contains(pages[0].Text, "1.234 metri") {
		t.Fatalf("want the one page still in the cache, got %+v", pages)
	}
	if gone != 1 {
		t.Errorf("gone = %d, want 1: the page no longer in the cache does not count, and is counted as gone", gone)
	}
	if pages, gone := sessionPages(history, nil); len(pages) != 0 || gone != 2 {
		t.Errorf("without a cache no page comes back: %d pages, %d gone", len(pages), gone)
	}
	if pages, gone := sessionPages(nil, c.Session()); len(pages) != 0 || gone != 0 {
		t.Errorf("a first question has no earlier pages: %d pages, %d gone", len(pages), gone)
	}
}

// followUp runs one follow-up of the conversation with the switch on or off.
func followUp(t *testing.T, on, review bool, sp search.Provider, turns ...string) (*scriptedLLM, string) {
	t.Helper()
	rd, c, history := earlierTurn(t)
	s := &scriptedLLM{turns: turns, models: halogenTestModels}
	llm.ForgetLimits()
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	t.Cleanup(srv.Close)
	engine := NewEngine(sp, rd, llm.NewClient(srv.URL, "", "m"), nil)
	engine.SetDeepLoopMaxCalls(40)
	engine.SetDeepLoopReview(review)
	engine.SetDeepLoopSessionPages(on)
	answer := ""
	err := engine.executeDeepLoop(context.Background(), SearchParams{Query: "Quanto è lungo?", Mode: "deep", AnswerLanguage: "it"},
		func(ev Event) error {
			if ev.Type == EventToken {
				answer += ev.Data.(string)
			}
			return nil
		}, sp, rd, engine.LLMClient(), c.Session(), history, newLLMTrouble())
	if err != nil {
		t.Fatal(err)
	}
	return s, answer
}

func TestDeepLoop_FollowUpFromAnEarlierPageIsNotSentBack(t *testing.T) {
	draft := "Il ponte è lungo 1.234 metri ed è stato progettato da Tizio Caio. " + prose(20)
	rewrite := "Non ho una fonte per la lunghezza. " + prose(20)

	off, _ := followUp(t, false, true, &mockSearchProvider{}, contentTurn(draft), contentTurn(rewrite))
	if len(off.bodies) < 2 {
		t.Fatalf("switch off, as today: the draft goes back as from memory; requests = %d", len(off.bodies))
	}
	if _, q := lastUserOrTool(off.bodies[0]); q != "Quanto è lungo?" {
		t.Errorf("switch off: the model gets the question alone, got %q", q)
	}

	on, _ := followUp(t, true, true, &mockSearchProvider{}, contentTurn(draft), contentTurn(rewrite))
	if len(on.bodies) != 1 {
		t.Fatalf("switch on: the number and the name are on a page read in the first turn, nothing to send back; requests = %d", len(on.bodies))
	}
	_, q := lastUserOrTool(on.bodies[0])
	for _, want := range []string{earlierURL, "Il ponte Alfa", "Quanto è lungo?"} {
		if !strings.Contains(q, want) {
			t.Errorf("the follow-up lacks %q: %q", want, q)
		}
	}
	if !strings.HasSuffix(q, "Quanto è lungo?") {
		t.Errorf("the question comes last: %q", q)
	}
	for _, not := range []string{earlierGone, "real.test/rotta", "1.234"} {
		if strings.Contains(q, not) {
			t.Errorf("the list holds %q: only titles and addresses of pages still in the cache: %q", not, q)
		}
	}
}

func TestDeepLoop_FollowUpNumberOfAnEarlierPageIsNotFlagged(t *testing.T) {
	sp := &mockSearchProvider{results: []search.Result{{Title: "Ponti ad arco", URL: "https://real.test/archi", Content: "Un elenco di ponti ad arco."}}}
	turns := []string{callTurn("c1", "search", `{"query":"ponte Alfa lunghezza"}`), contentTurn("Il ponte è lungo 1.234 metri. " + prose(20))}
	const flag = "forse dalla memoria"

	_, answer := followUp(t, false, false, sp, turns...)
	if !strings.Contains(answer, flag) {
		t.Fatalf("switch off, as today: the number is flagged in the note: %q", answer)
	}
	_, answer = followUp(t, true, false, sp, turns...)
	if strings.Contains(answer, flag) {
		t.Errorf("switch on: the number is on a page read in the first turn, not flagged: %q", answer)
	}
}

// External audit 24/11/2026, BUG-02: a follow-up cites pages read in the earlier turns
// without opening them again. The links stay, also when the page does not name its own
// address, and also when it is no longer in the cache; an address never read does not.
func TestDeepLoop_FollowUpKeepsLinksToEarlierPages(t *testing.T) {
	const never = "https://real.test/mai-letta"
	draft := "Il ponte è lungo 1.234 metri ([la pagina](" + earlierURL + ")), come diceva [l'altra](" + earlierGone + "), e poi [questa](" + never + "). " +
		prose(20) + "\n\nFonti: [la pagina](" + earlierURL + "), [questa](" + never + ")"
	for _, on := range []bool{true, false} {
		_, answer := followUp(t, on, true, &mockSearchProvider{}, contentTurn(draft), contentTurn(draft))
		if !strings.Contains(answer, "]("+earlierURL+")") || !strings.Contains(answer, "]("+earlierGone+")") {
			t.Errorf("session pages %t: a link to a page read in the first turn was taken out: %q", on, answer)
		}
		if strings.Contains(answer, never) {
			t.Errorf("session pages %t: a link to a page nobody read stayed: %q", on, answer)
		}
		if strings.Contains(answer, "parametric") {
			t.Errorf("session pages %t: an answer that cites the pages read is not from memory: %q", on, answer)
		}
	}
}

func TestDeepLoop_FollowUpKeepsEarlierPagesInTheSourcesLine(t *testing.T) {
	l := &deepLoop{before: map[string]string{normalizeLinkURL(earlierURL): earlierURL}, pages: map[string]*loopPage{}}
	got := l.dropUnreadSources("Testo.\n\nFonti: [a](" + earlierURL + "), [b](https://real.test/mai-letta)")
	if !strings.Contains(got, earlierURL) || strings.Contains(got, "mai-letta") {
		t.Errorf("sources line: %q", got)
	}
}

func TestCitedBefore(t *testing.T) {
	l := &deepLoop{before: map[string]string{normalizeLinkURL(earlierURL): earlierURL, normalizeLinkURL("https://real.test/ora"): "https://real.test/ora"},
		pages: map[string]*loopPage{normalizeLinkURL("https://real.test/ora"): {}}}
	if n := l.citedBefore("[a](" + earlierURL + ") [a di nuovo](" + earlierURL + ") [b](https://real.test/ora) [c](https://real.test/mai)"); n != 1 {
		t.Errorf("citedBefore = %d, want 1: the page read again now is not among them", n)
	}
}
