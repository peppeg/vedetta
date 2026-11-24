package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vedetta/internal/llm"
	"vedetta/internal/reader"
	"vedetta/internal/search"
	"vedetta/internal/storage"
)

// External audit of 24/11/2026: what a turn writes before a call, and two user
// messages in a row.

// textAndCall is a turn that writes text and then calls a tool.
func textAndCall(text, id, name, args string) string {
	c, _ := json.Marshal(text)
	return `{"choices":[{"delta":{"content":` + string(c) + `}}]}` + "\n" + callTurn(id, name, args)
}

func komodoTools() (*mockSearchProvider, *mockReader) {
	return &mockSearchProvider{results: []search.Result{{Title: "Komodo releases", URL: "https://real.test/komodo", Content: "Komodo 1.19 released"}}},
		&mockReader{docs: map[string]reader.Document{"https://real.test/komodo": {Title: "Komodo", URL: "https://real.test/komodo",
			Content: strings.Repeat("Komodo 1.19 è la versione più recente.\n", 50)}}}
}

func TestDeepLoop_PreambleOfACallIsNotTheAnswer(t *testing.T) {
	preamble := "PREAMBOLO: cerco le fonti ufficiali per verificare i punti richiesti. " + prose(8)
	if len(preamble) < loopCommitChars {
		t.Fatalf("the preamble must be long enough to be taken for the answer: %d", len(preamble))
	}
	sp, rd := komodoTools()
	s := &scriptedLLM{turns: []string{
		textAndCall(preamble, "c1", "search", `{"query":"komodo release"}`),
		textAndCall("SECONDO "+preamble, "c2", "read", `{"url":"https://real.test/komodo"}`),
		contentTurn("L'ultima versione è la 1.19 ([note](https://real.test/komodo)). " + prose(40)),
	}}
	run := runHeldLoop(t, s, sp, rd)
	if strings.Contains(run.answer, "PREAMBOLO") || !strings.HasPrefix(run.answer, "L'ultima versione") {
		t.Errorf("the preamble went out with the answer: %.200q", run.answer)
	}
	if msgs, _ := s.bodies[1]["messages"].([]interface{}); !strings.Contains(string(mustJSON(msgs)), "PREAMBOLO") {
		t.Error("the model must find in the conversation what it wrote")
	}
}

// The answer written whole, then a last check: what comes after goes on from there.
func TestDeepLoop_AnswerBeforeALastCallIsKept(t *testing.T) {
	sp, rd := komodoTools()
	long := "RISPOSTA: l'ultima versione è la 1.19 ([note](https://real.test/komodo)). " + prose(60)
	s := &scriptedLLM{turns: []string{
		callTurn("c1", "read", `{"url":"https://real.test/komodo"}`),
		textAndCall(long, "c2", "search", `{"query":"komodo release"}`),
		contentTurn("Confermato anche dalla ricerca."),
	}}
	run := runHeldLoop(t, s, sp, rd)
	if !strings.HasPrefix(run.answer, "RISPOSTA") || !strings.Contains(run.answer, "Confermato anche dalla ricerca.") {
		t.Errorf("the answer written before the call was lost: %.200q", run.answer)
	}
}

// The writing is cut after a call: what goes out and is continued has no preamble.
func TestDeepLoop_PreambleAndACutAnswer(t *testing.T) {
	sp, rd := komodoTools()
	cut := func(text string) string {
		c, _ := json.Marshal(text)
		return `{"choices":[{"delta":{"content":` + string(c) + `},"finish_reason":"length"}]}`
	}
	s := &scriptedLLM{turns: []string{
		textAndCall("PREAMBOLO "+prose(10), "c1", "read", `{"url":"https://real.test/komodo"}`),
		cut("L'ultima versione è la 1.19 ([note](https://real.test/komodo)). " + prose(40)),
		contentTurn("E qui finisce. " + prose(3)),
	}}
	run := runHeldLoop(t, s, sp, rd)
	if strings.Contains(run.answer, "PREAMBOLO") || !strings.Contains(run.answer, "E qui finisce.") {
		t.Errorf("a cut answer after a preamble: %.200q", run.answer)
	}
}

func TestHeldText(t *testing.T) {
	for _, c := range []struct{ aside, draft, want string }{
		{"", "bozza", "bozza"},
		{"corto", "una bozza lunga", "una bozza lunga"},
		{"una bozza lunga", "coda", "una bozza lunga\n\ncoda"},
		{"solo questo", "", "solo questo"},
		{"", "", ""},
	} {
		l := &deepLoop{aside: c.aside}
		l.draft.WriteString(c.draft)
		if got := l.heldText(); got != c.want {
			t.Errorf("heldText(%q, %q) = %q, want %q", c.aside, c.draft, got, c.want)
		}
	}
}

func TestOneUserAtATime(t *testing.T) {
	u := func(c string) llm.Message { return llm.Message{Role: "user", Content: c} }
	a := llm.Message{Role: "assistant", Content: "a"}
	in := []llm.Message{{Role: "system", Content: "s"}, u("1"), u("2"), a, u("3"), u("3")}
	got := ""
	for _, m := range oneUserAtATime(in) {
		got += m.Role + ":" + m.Content + "|"
	}
	if want := "system:s|user:1\n\n2|assistant:a|user:3|"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if in[1].Content != "1" || len(in) != 6 {
		t.Error("the conversation itself must not change")
	}
}

// After a failed turn the request to write comes again: one user message, not two.
func TestDeepLoop_OneUserMessageAfterAFailedTurn(t *testing.T) {
	sp, rd := komodoTools()
	s := &scriptedLLM{turns: []string{
		callTurn("c1", "read", `{"url":"https://real.test/komodo"}`),
		`{"choices":[{"delta":{}}]}`, // nothing written, after the closing message
		contentTurn("L'ultima versione è la 1.19 ([note](https://real.test/komodo)). " + prose(40)),
	}}
	run := runLoop(t, s, sp, rd, 1)
	if run.err != nil || len(s.bodies) != 3 {
		t.Fatalf("err %v, requests %d", run.err, len(s.bodies))
	}
	for i, b := range s.bodies {
		before := ""
		for _, m := range b["messages"].([]interface{}) {
			role, _ := m.(map[string]interface{})["role"].(string)
			if role == "user" && before == "user" {
				t.Errorf("request %d has two user messages in a row", i+1)
			}
			before = role
		}
	}
}

// runHeldLoop is runLoop with the sentinel's turn on, as it ships.
func runHeldLoop(t *testing.T, s *scriptedLLM, sp search.Provider, rd reader.Reader) loopRun {
	t.Helper()
	return runLoopWith(t, s, sp, rd, 40, func(e *Engine) { e.SetDeepLoopReview(true) })
}

func mustJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// A reply of lines cut at the server's limit: the line stopped half way does not count.
func TestAskLines_CutReply(t *testing.T) {
	c, _ := json.Marshal("1: la prima riga intera\n2: la seconda si ferma a me")
	s := &scriptedLLM{turns: []string{`{"choices":[{"delta":{"content":` + string(c) + `},"finish_reason":"length"}]}`}}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()
	e := NewEngine(nil, nil, nil, nil)
	l := &deepLoop{e: e, client: llm.NewClient(srv.URL, "", "m"), emit: func(Event) error { return nil }}
	got, err := l.askLines(context.Background(), "righe")
	if err != nil || got != "1: la prima riga intera\n" {
		t.Errorf("got %q, %v", got, err)
	}
}

// A new search has no name yet: what the others noted about their backup services stays
// (external audit 24/11/2026, BUG-05).
func TestDeepPath_ResetsOnlyItsOwnFallback(t *testing.T) {
	sp, rd := komodoTools()
	noted := &resettable{mockSearchProvider: sp}
	s := &scriptedLLM{models: halogenTestModels, turns: []string{contentTurn("Ciao. " + prose(10))}}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()
	store, err := storage.NewStorage(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := NewEngine(noted, rd, llm.NewClient(srv.URL, "", "m"), store)
	if err := e.ExecuteDeepPath(context.Background(), SearchParams{Query: "ciao", AnswerLanguage: "it"}, func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(noted.reset) != 1 || noted.reset[0] == "" {
		t.Errorf("reset of %q: want the new session, by its name", noted.reset)
	}
}

type resettable struct {
	*mockSearchProvider
	reset []string
}

func (r *resettable) ResetFallback(id ...string) { r.reset = append(r.reset, id...) }
