package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vedetta/internal/cache"
	"vedetta/internal/llm"
	"vedetta/internal/reader"
	"vedetta/internal/search"
	"vedetta/internal/storage"
)

// toolReply is the tool message the model received in request i.
func toolReply(t *testing.T, s *scriptedLLM, i int) string {
	t.Helper()
	if i >= len(s.bodies) {
		t.Fatalf("request %d not made (%d requests)", i, len(s.bodies))
	}
	role, content := lastUserOrTool(s.bodies[i])
	if role != "tool" {
		t.Fatalf("request %d ends with %s, want a tool reply: %q", i, role, content)
	}
	return content
}

// "You already read this page" only when it is true (Piano dopo audit 1.3 / D2).
func TestDeepLoop_RepeatedReadOfAnUnreadablePage(t *testing.T) {
	sp := &mockSearchProvider{results: []search.Result{{Title: "r", URL: "https://real.test/r", Content: "x"}}}
	turns := []string{
		callTurn("c1", "read", `{"url":"https://real.test/page"}`),
		callTurn("c2", "read", `{"url":"https://real.test/page"}`),
		contentTurn("Risposta senza la pagina. " + prose(50)),
	}

	t.Run("first read failed", func(t *testing.T) {
		rd := &countingReader{inner: &mockReader{err: errors.New("http reader returned status 503")}}
		s := &scriptedLLM{turns: turns}
		if run := runLoop(t, s, sp, rd, 40); run.err != nil {
			t.Fatal(run.err)
		}
		first, second := toolReply(t, s, 1), toolReply(t, s, 2)
		if !strings.Contains(first, "could not be read") {
			t.Fatalf("first reply = %q", first)
		}
		if strings.Contains(second, "already read") {
			t.Errorf("the model is told it read a page it could not open: %q", second)
		}
		if !strings.Contains(second, "could not be read before") || !strings.Contains(second, "503") {
			t.Errorf("second reply does not say the page could not be read, and why: %q", second)
		}
		if n := rd.calls.Load(); n != 1 {
			t.Errorf("the page was read %d times, want 1 (no retry)", n)
		}
	})

	t.Run("first read worked", func(t *testing.T) {
		rd := &countingReader{inner: &mockReader{}}
		s := &scriptedLLM{turns: turns}
		if run := runLoop(t, s, sp, rd, 40); run.err != nil {
			t.Fatal(run.err)
		}
		if second := toolReply(t, s, 2); !strings.Contains(second, "You already read this page: it is above in the conversation.") {
			t.Errorf("second reply = %q", second)
		}
	})
}

// A PDF read only in part says so to the model and in the note (Piano dopo
// audit 1.5 / R9): "a page read is read whole", and when it is not, it is said.
func TestDeepLoop_PartialPDFIsDeclared(t *testing.T) {
	sp := &mockSearchProvider{results: []search.Result{{Title: "r", URL: "https://real.test/r", Content: "x"}}}
	rd := &mockReader{docs: map[string]reader.Document{"https://real.test/rules.pdf": {Title: "Rules", URL: "https://real.test/rules.pdf",
		Content:  "Article 1. The first rules of the document.",
		Metadata: map[string]string{"source_type": "pdf", "pages": "10", "pages_read": "3", "truncated": "true"}}}}
	s := &scriptedLLM{turns: []string{
		callTurn("c1", "read", `{"url":"https://real.test/rules.pdf"}`),
		contentTurn("Risposta con le prime regole. " + prose(50)),
	}}
	run := runLoop(t, s, sp, rd, 40)
	if run.err != nil {
		t.Fatal(run.err)
	}
	if reply := toolReply(t, s, 1); !strings.Contains(reply, "Only the first 3 of the 10 pages of this PDF could be read") {
		t.Errorf("the model is not told the PDF was read in part: %q", reply[:min(len(reply), 300)])
	}
	if !strings.Contains(run.answer, "PDF letti solo in parte: 1.") {
		t.Errorf("the note does not say a PDF was read in part: %q", run.answer)
	}
	if strings.Contains(run.answer, "troppo lunghe") {
		t.Errorf("the note calls the PDF too long for the conversation: %q", run.answer)
	}
}

// If Vedetta holds a whole answer, whoever asked gets it, even when the step after
// fails (Piano dopo audit 1.6 / D3): the review sends the draft back, and every
// turn after that fails.
func TestDeepLoop_DraftKeptWhenTheRewriteFails(t *testing.T) {
	sp := &mockSearchProvider{results: []search.Result{{Title: "Komodo releases", URL: "https://real.test/komodo", Content: "Komodo 1.19 released"}}}
	rd := &mockReader{docs: map[string]reader.Document{"https://real.test/komodo": {Title: "Komodo", URL: "https://real.test/komodo",
		Content: "Komodo 1.19 is the latest release. " + prose(20)}}}
	draft := "L'ultima versione è la 1.19, con 987.654 stelle. " + prose(30)
	s := &scriptedLLM{turns: []string{
		callTurn("c1", "search", `{"query":"komodo release"}`),
		callTurn("c2", "read", `{"url":"https://real.test/komodo"}`),
		contentTurn(draft),
	}}
	failAfter := len(s.turns)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		n := len(s.bodies)
		s.mu.Unlock()
		if r.Method == http.MethodPost && n >= failAfter {
			s.mu.Lock()
			s.bodies = append(s.bodies, nil)
			s.mu.Unlock()
			http.Error(w, `{"error":"the server broke"}`, http.StatusBadRequest) // not retried
			return
		}
		s.handler(w, r)
	}))
	defer srv.Close()
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine := NewEngine(sp, rd, llm.NewClient(srv.URL, "", "m"), store)
	engine.SetDeepLoopMaxCalls(40)
	engine.SetDeepLoopReview(true)
	var answer, sessionID string
	var partialWarning bool
	err = engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "Qual è l'ultima versione di Komodo?", Mode: "deep", AnswerLanguage: "it"},
		func(ev Event) error {
			switch ev.Type {
			case EventToken:
				answer += ev.Data.(string)
			case EventSession:
				sessionID = ev.Data.(map[string]string)["session_id"]
			case EventWarning:
				if ev.Data.(map[string]string)["kind"] == string(WarnKindPartial) {
					partialWarning = true
				}
			}
			return nil
		})
	if err != nil {
		t.Fatalf("the search ended in error, the draft lost: %v", err)
	}
	if len(s.bodies) <= failAfter {
		t.Fatalf("the draft was not sent back (%d requests)", len(s.bodies))
	}
	if !strings.Contains(answer, "L'ultima versione è la 1.19, con 987.654 stelle.") {
		t.Errorf("the first draft is not the answer: %q", answer)
	}
	if !strings.Contains(answer, "la riscrittura chiesta non si è conclusa") {
		t.Errorf("the note does not say the rewrite did not finish: %q", answer)
	}
	if !partialWarning {
		t.Error("the answer is not marked partial")
	}
	detail, err := store.GetSession(sessionID)
	if err != nil || len(detail.Messages) != 1 || !strings.Contains(detail.Messages[0].Content, "987.654 stelle") {
		t.Fatalf("the saved answer misses the draft: err %v, %+v", err, detail)
	}
	var meta AnswerMeta
	if err := json.Unmarshal(detail.Messages[0].Meta, &meta); err != nil || !meta.Partial || meta.Loop == nil || !meta.Loop.ReviewUnfinished {
		t.Errorf("saved meta: partial %t, loop %+v, err %v", meta.Partial, meta.Loop, err)
	}
}

// A server that refuses tools cannot run the research (map R1, no fallback since
// the old pipeline went): the user gets the reason in words and no answer.
func TestDeepPath_ToolsRefusedIsSaidInWords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		http.Error(w, `{"error":"tools are not supported"}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	sp := &mockSearchProvider{results: []search.Result{{Title: "Zen 5 Specs", URL: "https://example.com/zen5", Content: "dual-pipe decoder 4+4"}}}
	engine := NewEngine(sp, &mockReader{}, llm.NewClient(srv.URL, "", "m"), nil)
	var answer string
	err := engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "Come è fatto il decoder di Zen 5?", Mode: "deep", AnswerLanguage: "it", UILanguage: "it"},
		func(ev Event) error {
			if ev.Type == EventToken {
				answer += ev.Data.(string)
			}
			return nil
		})
	if err == nil || !strings.Contains(err.Error(), "strumenti") {
		t.Fatalf("want the tools message in words, got %v", err)
	}
	if answer != "" {
		t.Errorf("no answer expected, got %q", answer)
	}
}

// An older client asking for "standard" gets the Deep a ciclo, and "Riprendi"
// is refused in words (map R1).
func TestDeepPath_OneWayOnly(t *testing.T) {
	var sawTools bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		sawTools = sawTools || strings.Contains(string(body), `"tools"`)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + contentTurn("Una risposta senza ricerche, scritta dal modello.") + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	engine := NewEngine(&mockSearchProvider{}, &mockReader{}, llm.NewClient(srv.URL, "", "m"), nil)
	if err := engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "Ciao?", Mode: "standard", AnswerLanguage: "it", UILanguage: "it"},
		func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !sawTools {
		t.Error("a standard request did not go through the Deep a ciclo")
	}
	err := engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "Ciao?", Mode: "deep", Resume: true, UILanguage: "it"},
		func(Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "Riprendere") {
		t.Errorf("want the resume message in words, got %v", err)
	}
}

// The note always says whether the answer went through the check of numbers and
// names (Piano dopo audit 1.8 / D5): a draft cut while written goes out and is
// continued, without the sentinel's turn.
func TestDeepLoop_InterruptedDraftSaysItWasNotReviewed(t *testing.T) {
	sp := &mockSearchProvider{results: []search.Result{{Title: "Komodo releases", URL: "https://real.test/komodo", Content: "Komodo 1.19 released"}}}
	rd := &mockReader{docs: map[string]reader.Document{"https://real.test/komodo": {Title: "Komodo", URL: "https://real.test/komodo",
		Content: "Komodo 1.19 is the latest release. " + prose(20)}}}
	first, _ := json.Marshal("L'ultima versione è la 1.19. " + prose(20))
	for _, tc := range []struct {
		name      string
		writeTurn string
		want      bool
	}{
		{"cut", `{"choices":[{"delta":{"content":` + string(first) + `},"finish_reason":"length"}]}`, true},
		{"whole", contentTurn("L'ultima versione è la 1.19. " + prose(20)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &scriptedLLM{turns: []string{
				callTurn("c1", "search", `{"query":"komodo release"}`),
				callTurn("c2", "read", `{"url":"https://real.test/komodo"}`),
				tc.writeTurn,
				contentTurn("Il resto della risposta. " + prose(10)),
			}}
			srv := httptest.NewServer(http.HandlerFunc(s.handler))
			defer srv.Close()
			engine := NewEngine(sp, rd, llm.NewClient(srv.URL, "", "m"), nil)
			engine.SetDeepLoopMaxCalls(40)
			engine.SetDeepLoopReview(true)
			var answer string
			err := engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "Qual è l'ultima versione di Komodo?", Mode: "deep", AnswerLanguage: "it"},
				func(ev Event) error {
					if ev.Type == EventToken {
						answer += ev.Data.(string)
					}
					return nil
				})
			if err != nil {
				t.Fatal(err)
			}
			said := strings.Contains(answer, "Vedetta non ha potuto rimandare la bozza al modello")
			if said != tc.want {
				t.Errorf("note of the skipped review present %t, want %t: %q", said, tc.want, answer)
			}
		})
	}
}

// fallbackSearch is a search service that went to its backup and has an engine
// that did not answer, as SearXNG reports them.
type fallbackSearch struct{ mockSearchProvider }

func (f *fallbackSearch) Search(ctx context.Context, q string, o search.Options) ([]search.Result, error) {
	search.ReportFromContext(ctx).AddUnresponsive("google", "timeout")
	return f.mockSearchProvider.Search(ctx, q, o)
}
func (f *fallbackSearch) FallbackStatus(sessionID ...string) (bool, string) {
	return true, "searxng returned no results; brave used"
}

// "Transparency of the fallback", in the Deep loop too (Piano dopo audit 1.9 / R1).
func TestDeepLoop_SearchFallbackIsDeclared(t *testing.T) {
	sp := &fallbackSearch{mockSearchProvider{results: []search.Result{{Title: "r", URL: "https://real.test/r", Content: "x"}}}}
	s := &scriptedLLM{turns: []string{
		callTurn("c1", "search", `{"query":"one"}`),
		callTurn("c2", "search", `{"query":"two"}`),
		contentTurn("Risposta. " + prose(50)),
	}}
	run := runLoop(t, s, sp, &mockReader{}, 40)
	if run.err != nil {
		t.Fatal(run.err)
	}
	warnings, flagged := 0, false
	for _, ev := range run.events {
		switch ev.Type {
		case EventWarning:
			if d, ok := ev.Data.(map[string]string); ok && d["kind"] == string(WarnKindSearchFallback) {
				warnings++
			}
		case EventSources:
			if p, ok := ev.Data.(SourcesPayload); ok && p.Fallback {
				flagged = true
			}
		}
	}
	if warnings != 1 {
		t.Errorf("fallback warnings = %d, want 1", warnings)
	}
	if !flagged {
		t.Error("the sources never say the fallback was used")
	}
	for _, want := range []string{"alcune ricerche sono state fatte con il servizio di riserva", "Motori di ricerca che non hanno risposto ad alcune ricerche: google."} {
		if !strings.Contains(run.answer, want) {
			t.Errorf("the note misses %q: %q", want, run.answer)
		}
	}
	if strings.Contains(run.answer, "×") {
		t.Errorf("the note counts the misses: %q", run.answer)
	}
}

// selectiveReader opens only the pages it has; the others fail.
type selectiveReader struct{ ok map[string]reader.Document }

func (r *selectiveReader) Name() string { return "selective" }
func (r *selectiveReader) Read(ctx context.Context, u string) (*reader.Document, error) {
	if d, found := r.ok[u]; found {
		d.Content = padTestPage(d.Content)
		return &d, nil
	}
	return nil, errors.New("http reader returned status 403")
}

// reverseReranker reverses the results and drops the last one.
type reverseReranker struct{}

func (reverseReranker) Rerank(q string, res []search.Result, min float64) []search.Result {
	out := make([]search.Result, 0, len(res))
	for i := len(res) - 1; i > 0; i-- {
		out = append(out, res[i])
	}
	return out
}

// With VEDETTA_DEEP_LOOP_RERANK off the Deep loop shows the results in the search
// service's order, all of them (Piano dopo audit 3.1 / D8).
func TestDeepLoop_RerankSwitch(t *testing.T) {
	results := []search.Result{
		{Title: "Primo", URL: "https://one.test/1", Content: "uno"},
		{Title: "Secondo", URL: "https://two.test/2", Content: "due"},
		{Title: "Terzo", URL: "https://three.test/3", Content: "tre"},
	}
	for _, tc := range []struct {
		name   string
		rerank bool
		order  []string
	}{
		{"off", false, []string{"one.test", "two.test", "three.test"}},
		{"on", true, []string{"three.test", "two.test"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &scriptedLLM{turns: []string{callTurn("c1", "search", `{"query":"tema"}`), contentTurn("Risposta. " + prose(50))}}
			srv := httptest.NewServer(http.HandlerFunc(s.handler))
			defer srv.Close()
			engine := NewEngine(&mockSearchProvider{results: results}, &mockReader{}, llm.NewClient(srv.URL, "", "m"), nil)
			engine.SetDeepLoopMaxCalls(40)
			engine.SetReranker(reverseReranker{})
			engine.SetDeepLoopRerank(tc.rerank)
			if err := engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "Che cosa dice il tema?", Mode: "deep", AnswerLanguage: "it"},
				func(Event) error { return nil }); err != nil {
				t.Fatal(err)
			}
			reply := toolReply(t, s, 1)
			last := -1
			for _, host := range tc.order {
				i := strings.Index(reply, host)
				if i < 0 || i < last {
					t.Fatalf("results not in the order %v: %q", tc.order, reply)
				}
				last = i
			}
			if !tc.rerank && strings.Count(reply, ".test/") != 3 {
				t.Errorf("a result was dropped with the reorder off: %q", reply)
			}
		})
	}
}

// recordingSearch keeps the options of every search and returns n results.
type recordingSearch struct {
	mu      sync.Mutex
	opts    []search.Options
	n       int
	byQuery map[string][]search.Result // when set, results by query; others empty
}

func (r *recordingSearch) Name() string { return "recording" }
func (r *recordingSearch) Search(ctx context.Context, q string, o search.Options) ([]search.Result, error) {
	r.mu.Lock()
	r.opts = append(r.opts, o)
	r.mu.Unlock()
	if r.byQuery != nil {
		return r.byQuery[q], nil
	}
	out := make([]search.Result, r.n)
	for i := range out {
		out[i] = search.Result{Title: fmt.Sprintf("Risultato %d", i+1), URL: fmt.Sprintf("https://r%d.test/p", i+1), Content: "tema"}
	}
	return out, nil
}

// VEDETTA_DEEP_LOOP_MAX_RESULTS: how many results a Deep loop search asks for and
// shows (Piano dopo audit 3.2 / R6); 0 keeps today's limit.
func TestDeepLoop_MaxResultsSwitch(t *testing.T) {
	for _, tc := range []struct {
		name string
		max  int
		want int
	}{{"today", 0, 10}, {"twenty", 20, 20}} {
		t.Run(tc.name, func(t *testing.T) {
			sp := &recordingSearch{n: 25}
			s := &scriptedLLM{turns: []string{callTurn("c1", "search", `{"query":"tema"}`), contentTurn("Risposta. " + prose(50))}}
			srv := httptest.NewServer(http.HandlerFunc(s.handler))
			defer srv.Close()
			engine := NewEngine(sp, &mockReader{}, llm.NewClient(srv.URL, "", "m"), nil)
			engine.SetDeepLoopMaxCalls(40)
			engine.SetDeepLoopRerank(false)
			engine.SetDeepLoopMaxResults(tc.max)
			if err := engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "Che cosa dice il tema?", Mode: "deep", AnswerLanguage: "it"},
				func(Event) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if len(sp.opts) != 1 || sp.opts[0].MaxResults != tc.want {
				t.Errorf("search asked for %+v, want MaxResults %d", sp.opts, tc.want)
			}
			if got := strings.Count(toolReply(t, s, 1), ".test/p"); got != tc.want {
				t.Errorf("results shown = %d, want %d", got, tc.want)
			}
		})
	}
}

// VEDETTA_DEEP_LOOP_SEARCH_LANG: the language of a search whose language the model
// did not give: the answer's (today) or none, so SearXNG uses "auto" (Piano dopo
// audit 3.3 / D7). A language the model gives is used in both.
func TestDeepLoop_SearchLangSwitch(t *testing.T) {
	for _, tc := range []struct {
		mode      string
		noLang    string
		givenLang string
	}{{"answer", "it", "en"}, {"auto", "", "en"}} {
		t.Run(tc.mode, func(t *testing.T) {
			sp := &recordingSearch{n: 2}
			s := &scriptedLLM{turns: []string{
				callTurn("c1", "search", `{"query":"tema uno"}`),
				callTurn("c2", "search", `{"query":"theme two","lang":"en"}`),
				contentTurn("Risposta. " + prose(50)),
			}}
			srv := httptest.NewServer(http.HandlerFunc(s.handler))
			defer srv.Close()
			engine := NewEngine(sp, &mockReader{}, llm.NewClient(srv.URL, "", "m"), nil)
			engine.SetDeepLoopMaxCalls(40)
			engine.SetDeepLoopSearchLang(tc.mode)
			if err := engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "Che cosa dice il tema?", Mode: "deep", AnswerLanguage: "it"},
				func(Event) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if len(sp.opts) != 2 || sp.opts[0].Language != tc.noLang || sp.opts[1].Language != tc.givenLang {
				t.Errorf("languages %q, %q; want %q, %q", sp.opts[0].Language, sp.opts[1].Language, tc.noLang, tc.givenLang)
			}
		})
	}
}

// VEDETTA_DEEP_LOOP_QUOTE_FALLBACK: a search with quotes or site: that comes back
// empty is tried once without the quotes, then without site:, and the model is
// told (Piano dopo audit 3.4 / R12). It stays one search; the outcome is "relaxed".
func TestDeepLoop_QuoteFallback(t *testing.T) {
	two := []search.Result{{Title: "A", URL: "https://a.test/1", Content: "frase esatta tema"}, {Title: "B", URL: "https://b.test/2", Content: "frase esatta tema"}}
	byQuery := map[string][]search.Result{
		"frase esatta tema":  two, // the quoted search, relaxed
		"altro tema":         two, // site: and quotes gone
		`"già trovata" tema`: two, // results at once
	}
	cases := []struct {
		name, query, wantHeader string
		on                      bool
		wantResults             int
		wantResult              string
	}{
		{"quotes", `"frase esatta" tema`, `No results for the exact phrase. Results without quotes for "frase esatta tema"`, true, 2, "relaxed"},
		{"site and quotes", `site:x.test "altro" tema`, `Results without quotes and without the site restriction for "altro tema"`, true, 2, "relaxed"},
		{"found at once", `"già trovata" tema`, `Results for "\"già trovata\" tema"`, true, 2, "ok"},
		{"off", `"frase esatta" tema`, "No results for this search.", false, 0, "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := &recordingSearch{byQuery: byQuery}
			args, _ := json.Marshal(map[string]string{"query": tc.query})
			s := &scriptedLLM{turns: []string{callTurn("c1", "search", string(args)), contentTurn("Risposta. " + prose(50))}}
			srv := httptest.NewServer(http.HandlerFunc(s.handler))
			defer srv.Close()
			store, err := storage.NewStorage(filepath.Join(t.TempDir(), "v.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			engine := NewEngine(sp, &mockReader{}, llm.NewClient(srv.URL, "", "m"), store)
			engine.SetDeepLoopMaxCalls(40)
			engine.SetDeepLoopRerank(false)
			engine.SetDeepLoopQuoteFallback(tc.on)
			var sessionID string
			if err := engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "Che cosa dice il tema?", Mode: "deep", AnswerLanguage: "it"},
				func(ev Event) error {
					if ev.Type == EventSession {
						sessionID = ev.Data.(map[string]string)["session_id"]
					}
					return nil
				}); err != nil {
				t.Fatal(err)
			}
			reply := toolReply(t, s, 1)
			if !strings.Contains(reply, tc.wantHeader) || strings.Count(reply, ".test/") != tc.wantResults {
				t.Errorf("reply = %q", reply)
			}
			detail, err := store.GetSession(sessionID)
			if err != nil || len(detail.Messages) != 1 {
				t.Fatalf("saved answer: %v", err)
			}
			var meta AnswerMeta
			_ = json.Unmarshal(detail.Messages[0].Meta, &meta)
			if meta.Loop == nil || len(meta.Loop.CallsDetail) != 1 || meta.Loop.CallsDetail[0].Result != tc.wantResult || meta.Loop.Searches != 1 {
				t.Errorf("saved calls %+v, searches %d; want one search with outcome %q", meta.Loop.CallsDetail, meta.Loop.Searches, tc.wantResult)
			}
		})
	}
}

// VEDETTA_CACHE_READ_DATE: a page from Vedetta's memory says when it was read, the
// same day too, to the model and in "Pagine lette" (Piano dopo audit 3.6 / R11; map
// R2b, session a9fe25e5).
func TestDeepLoop_CacheReadDate(t *testing.T) {
	old := time.Now().AddDate(0, 0, -3)
	earlier := time.Now().Add(-2 * time.Hour)
	if !sameDay(earlier, time.Now()) {
		earlier = time.Now().Add(-20 * time.Minute) // just after midnight
	}
	oldIt := fmt.Sprintf("letta il %d %s %d alle %s", old.Day(), italianMonths[old.Month()-1], old.Year(), old.Format("15:04"))
	for _, tc := range []struct {
		name     string
		on       bool
		at       time.Time
		top, bib string // "" = no line
	}{
		{"another day", true, old, "read on " + old.Format("2 January 2006 at 15:04") + ", not now", oldIt},
		{"earlier today", true, earlier, "read on " + earlier.Format("2 January 2006 at 15:04") + ", not now", "letta oggi alle " + earlier.Format("15:04")},
		{"read now", true, time.Now(), "", ""},
		{"no date", true, time.Time{}, "", ""},
		{"switch off", false, old, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rd := &mockReader{docs: map[string]reader.Document{"https://real.test/p": {Title: "Pagina", URL: "https://real.test/p",
				Content: "Il testo della pagina.", ExtractedAt: tc.at}}}
			s := &scriptedLLM{turns: []string{callTurn("c1", "read", `{"url":"https://real.test/p"}`), contentTurn("Risposta ([pagina](https://real.test/p)). " + prose(50))}}
			srv := httptest.NewServer(http.HandlerFunc(s.handler))
			defer srv.Close()
			engine := NewEngine(&mockSearchProvider{}, rd, llm.NewClient(srv.URL, "", "m"), nil)
			engine.SetDeepLoopMaxCalls(40)
			engine.SetCacheReadDate(tc.on)
			var bib string
			if err := engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "Che cosa dice la pagina?", Mode: "deep", AnswerLanguage: "it"},
				func(ev Event) error {
					if ev.Type == EventBibliography {
						bib += ev.Data.(string)
					}
					return nil
				}); err != nil {
				t.Fatal(err)
			}
			reply := toolReply(t, s, 1)
			head := reply[:min(len(reply), 200)]
			if tc.top == "" && strings.Contains(head, "Vedetta's copy") || tc.top != "" && !strings.Contains(head, tc.top) {
				t.Errorf("line at the top of the page: want %q, got %q", tc.top, head)
			}
			if tc.bib == "" && strings.Contains(bib, "letta ") || tc.bib != "" && !strings.Contains(bib, tc.bib) {
				t.Errorf("beside the page in the list: want %q, got %q", tc.bib, bib)
			}
		})
	}
}

// Map R2b: the model asks a page again now with fresh: true; Vedetta skips its copy
// and reads it, once; the new text replaces the old one.
func TestDeepLoop_FreshRead(t *testing.T) {
	const u = "https://real.test/prezzo"
	st, err := cache.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-26 * time.Hour)
	st.Put(cache.Entry{Key: cache.PageKey(u), Kind: cache.KindPage, ExpiresAt: time.Now().Add(time.Hour),
		Document: &reader.Document{Title: "Prezzo", URL: u, Content: "Prezzo di ieri: 100 euro. " + prose(20), ExtractedAt: old}})
	rd := &mockReader{docs: map[string]reader.Document{u: {Title: "Prezzo", URL: u, Content: "Prezzo di adesso: 120 euro. " + prose(20), ExtractedAt: time.Now()}}}
	s := &scriptedLLM{turns: []string{
		callTurn("c1", "read", `{"url":"`+u+`"}`),
		callTurn("c2", "read", `{"url":"`+u+`","fresh":true}`),
		callTurn("c3", "read", `{"url":"`+u+`","fresh":true}`),
		contentTurn("Il prezzo di adesso è 120 euro ([pagina](" + u + ")). " + prose(50)),
	}}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()
	engine := NewEngine(&mockSearchProvider{}, rd, llm.NewClient(srv.URL, "", "m"), nil)
	engine.SetDeepLoopMaxCalls(40)
	engine.SetCacheReadDate(true)
	engine.SetCache(cache.New(st, cache.ModeRecord, time.Hour, time.Hour))
	if err := engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "Quanto costa adesso?", Mode: "deep", AnswerLanguage: "it"},
		func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	first, second, third := toolReply(t, s, 1), toolReply(t, s, 2), toolReply(t, s, 3)
	if !strings.Contains(first, "Prezzo di ieri") || !strings.Contains(first, "fresh: true") {
		t.Errorf("first reading: the copy from the memory with its note, got %.160q", first)
	}
	if !strings.Contains(second, "Prezzo di adesso") || strings.Contains(second, "Vedetta's copy") {
		t.Errorf("fresh reading: the page as it is now, got %.160q", second)
	}
	if !strings.Contains(third, "already read") {
		t.Errorf("a second fresh reading is a repeat, got %.120q", third)
	}
	if e, ok := st.Get(cache.KindPage, cache.PageKey(u)); !ok || !strings.Contains(e.Document.Content, "adesso") {
		t.Errorf("the memory must hold the new reading")
	}
}

// The name check by whole words (Piano dopo audit 4.1 / D1): a name absent from
// every page goes back to the model even when it hides inside a longer word, and a
// site's name in its address still counts (Orvo in orvoconsulting.example:
// under 5 letters, when a part of the site's name starts with it).
func TestReviewDraft_NameWords(t *testing.T) {
	draft := "Il rapporto è firmato da Mario Rossi e pubblicato da Anna Conti Srl. Lo studio di Orvo Consulting cita Treccani."
	seen := "Il primario ha detto che i grossi conti dell'annata sono chiusi, Srl compresa.\n" +
		"https://www.orvoconsulting.example/chi-siamo\nhttps://www.treccani.it/enciclopedia/rapporto"
	flagged := func() map[string]bool {
		out := map[string]bool{}
		for _, it := range reviewDraft(draft, "it", map[string]bool{}, seen) {
			for _, n := range it.Names {
				out[n] = true
			}
		}
		return out
	}
	SetNameWords(true)
	defer SetNameWords(false)
	got := flagged()
	for _, n := range []string{"Mario", "Rossi", "Anna"} {
		if !got[n] {
			t.Errorf("%s is in no page but was not sent back (flagged %v)", n, got)
		}
	}
	for _, n := range []string{"Conti", "Srl", "Orvo", "Consulting", "Treccani"} {
		if got[n] {
			t.Errorf("%s is in what the model saw but was sent back", n)
		}
	}
	SetNameWords(false)
	if today := flagged(); len(today) != 0 {
		t.Errorf("with the switch off the check is today's (nothing sent back here), got %v", today)
	}
}

// The measure on the bench drafts (11/11): about one new send-back in three was a
// name on the page joined to something else. Words of the pages also split where
// the case changes, at digits and dots; a month's abbreviation counts when the
// month is written in full (Piano dopo audit 4.1).
func TestReviewDraft_NameWordsJoinedForms(t *testing.T) {
	seen := "Use enableHowdy for face unlock. Licensed AGPLv3 and GPLv2. Runs on FreeBSD. See Integration.Cryptomator.\n" +
		"Updated in February 2008. Styles are written in SCSS."
	draft := "Il blocco schermo usa Howdy con licenza AGPL e GPL, anche su BSD, con Cryptomator, aggiornato a Feb 2008, senza CSS."
	SetNameWords(true)
	defer SetNameWords(false)
	got := map[string]bool{}
	for _, it := range reviewDraft(draft, "it", map[string]bool{}, seen) {
		for _, n := range it.Names {
			got[n] = true
		}
	}
	for _, n := range []string{"Howdy", "AGPL", "GPL", "BSD", "Cryptomator", "Feb"} {
		if got[n] {
			t.Errorf("%s is on the page joined to something else, but was sent back", n)
		}
	}
	if !got["CSS"] {
		t.Errorf("CSS is not on the page (SCSS is another word), but was not sent back: %v", got)
	}
}

func TestNameParts(t *testing.T) {
	for w, want := range map[string][]string{
		"enableHowdy":             {"enable", "Howdy"},
		"AGPLv3":                  {"AGPL"},
		"GPLv2":                   {"GPL"},
		"FreeBSD":                 {"Free", "BSD"},
		"HTMLParser":              {"HTML", "Parser"},
		"Integration.Cryptomator": {"Integration", "Cryptomator"},
		"grossi":                  {"grossi"},
	} {
		parts := map[string]bool{}
		for _, p := range nameParts(w) {
			parts[p] = true
		}
		for _, p := range want {
			if !parts[p] {
				t.Errorf("nameParts(%q) = %v, misses %q", w, nameParts(w), p)
			}
		}
	}
	for _, p := range nameParts("grossi") {
		if p == "rossi" {
			t.Error("grossi split into rossi")
		}
	}
}

// The answer's language on questions that cite titles or names in another language
// (Piano dopo audit 4.3 / D6): the 20 questions approved on 11/11, with an
// interface language that is never the expected one, so the fallback cannot help.
func TestResolveAnswerLanguage_MixedQuestions(t *testing.T) {
	raw, err := os.ReadFile("../../scripts/mixed_questions.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Questions []struct {
			ID, Question string
			Answer       string `json:"answer_language"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Questions) != 20 {
		t.Fatalf("questions: %d, err %v", len(f.Questions), err)
	}
	SetLangDetectV2(true)
	defer SetLangDetectV2(false)
	right := 0
	for _, q := range f.Questions {
		ui := "it"
		if q.Answer == "it" {
			ui = "en"
		}
		if got := ResolveAnswerLanguage("mixed-v2-"+q.ID, q.Question, ui, ""); got == q.Answer {
			right++
		} else {
			t.Errorf("%s %q: got %s, want %s", q.ID, q.Question, got, q.Answer)
		}
	}
	if right != 20 {
		t.Errorf("right %d of 20", right)
	}
}

// With the switch on, the questions of the language test that already existed
// keep their language.
func TestDetectQuestionLanguage_KeepsTheOldCases(t *testing.T) {
	for in, want := range map[string]string{
		"Vorrei trascorrere 4 giorni a Valencia con mia figlia": "it",
		"How does hope work in clinical psychology?":            "en",
		"¿Dónde comer paella en Valencia?":                      "es",
		"Wie funktioniert das Schulsystem?":                     "de",
		"Comment fonctionne l'impôt sur le revenu ?":            "fr",
		"ok":              "",
		"D.Lgs. 152/2006": "",
		"":                "",
	} {
		if got := detectQuestionLanguage(in); got != want {
			t.Errorf("detectQuestionLanguage(%q) = %q; want %q", in, got, want)
		}
	}
}

// The interface marks the sentences with a number found in no page (Piano dopo
// audit 4.2 / U1): the final event carries them, as the saved meta already does.
func TestDeepLoop_DoneCarriesNumbersMissing(t *testing.T) {
	sp := &mockSearchProvider{results: []search.Result{{Title: "Komodo releases", URL: "https://real.test/komodo", Content: "Komodo 1.19 released"}}}
	rd := &mockReader{docs: map[string]reader.Document{"https://real.test/komodo": {Title: "Komodo", URL: "https://real.test/komodo",
		Content: "Komodo 1.19 is the latest release. " + prose(20)}}}
	s := &scriptedLLM{turns: []string{
		callTurn("c1", "read", `{"url":"https://real.test/komodo"}`),
		contentTurn("L'ultima versione è la 1.19 ([note](https://real.test/komodo)). Il progetto ha 987.654 stelle. " + prose(30)),
	}}
	run := runLoop(t, s, sp, rd, 40)
	if run.err != nil {
		t.Fatal(run.err)
	}
	var done map[string]interface{}
	for _, ev := range run.events {
		if ev.Type == EventDone {
			done = ev.Data.(map[string]interface{})
		}
	}
	misses, ok := done["numbers_missing"].([]NumberMiss)
	if !ok || len(misses) != 1 || !strings.Contains(misses[0].Sentence, "987.654 stelle") {
		t.Errorf("done numbers_missing = %#v", done["numbers_missing"])
	}
}

// A gateway's reasoning (Kilo's "reasoning" field) is shown and saved, and is not
// sent back to the model even when the kept reasoning is on: what the
// model receives does not change.
func TestDeepLoop_GatewayReasoningShownNotSentBack(t *testing.T) {
	sp := &mockSearchProvider{results: []search.Result{{Title: "r", URL: "https://real.test/r", Content: "x"}}}
	s := &scriptedLLM{turns: []string{
		`{"choices":[{"delta":{"reasoning":"Plan: search first, then read."}}]}` + "\n" + callTurn("c1", "search", `{"query":"one"}`),
		contentTurn("Risposta. " + prose(30)),
	}}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()
	engine := NewEngine(sp, &mockReader{}, llm.NewClient(srv.URL, "", "m"), nil)
	engine.SetDeepLoopMaxCalls(40)
	engine.SetDeepLoopSampling("1.0", 0.95, 20, true)
	shown := ""
	if err := engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "q", Mode: "deep"}, func(ev Event) error {
		if ev.Type == EventReasoning {
			shown += ev.Data.(string)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(shown, "Plan: search first, then read.") {
		t.Errorf("the reasoning was not shown: %q", shown)
	}
	for _, m := range s.bodies[1]["messages"].([]interface{}) {
		if mm := m.(map[string]interface{}); mm["role"] == "assistant" {
			if _, has := mm["reasoning_content"]; has {
				t.Errorf("the gateway's reasoning went back to the model: %v", mm)
			}
		}
	}
}

// pushRun runs the Deep loop with the sentinel's review on and the request to go
// on (VEDETTA_DEEP_LOOP_PUSH) as asked, and returns the run, the saved loop data,
// and the requests the model received. failAfter > 0: from that request on the
// server answers 400 to everything (not retried by the client).
func pushRun(t *testing.T, turns []string, push bool, maxCalls, failAfter int) (loopRun, LoopStats, *scriptedLLM) {
	t.Helper()
	s := &scriptedLLM{turns: turns}
	h := http.HandlerFunc(s.handler)
	if failAfter > 0 {
		h = func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			n := len(s.bodies)
			s.mu.Unlock()
			if r.Method == http.MethodPost && n >= failAfter {
				body, _ := io.ReadAll(r.Body)
				var m map[string]interface{}
				_ = json.Unmarshal(body, &m)
				s.mu.Lock()
				s.bodies = append(s.bodies, m)
				s.mu.Unlock()
				http.Error(w, `{"error":"the server broke"}`, http.StatusBadRequest)
				return
			}
			s.handler(w, r)
		}
	}
	sp := &mockSearchProvider{results: []search.Result{{Title: "r", URL: "https://real.test/r", Content: "x"}}}
	run, stats := runLoopMetaWith(t, h, sp, &mockReader{}, func(e *Engine) {
		e.SetDeepLoopMaxCalls(maxCalls)
		e.SetDeepLoopReview(true)
		e.SetDeepLoopPush(push)
	})
	return run, stats, s
}

// requestsEndingWith counts the requests whose last message is text.
func requestsEndingWith(s *scriptedLLM, text string) int {
	n := 0
	for _, b := range s.bodies {
		if b == nil {
			continue
		}
		if _, content := lastUserOrTool(b); content == text {
			n++
		}
	}
	return n
}

// anyRequestHolds says whether some request carries text in one of its messages.
func anyRequestHolds(s *scriptedLLM, text string) bool {
	for _, b := range s.bodies {
		msgs, _ := b["messages"].([]interface{})
		for _, m := range msgs {
			if c, _ := m.(map[string]interface{})["content"].(string); strings.Contains(c, text) {
				return true
			}
		}
	}
	return false
}

// "Continua a cercare" (Piano dopo audit 7.13): at the first draft Vedetta asks the
// model once whether there is more to look for, before the sentinel's checks.
var (
	pushDraftA = "Prima bozza della risposta. " + prose(30)
	pushDraftB = "Seconda bozza della risposta. " + prose(30)
	pushDraftC = "Terza bozza della risposta. " + prose(30)
)

// 1, 4. The model goes on: the new draft takes the place of the first, the
// request comes once, with the tools.
func TestDeepLoop_PushModelGoesOn(t *testing.T) {
	run, stats, s := pushRun(t, []string{
		callTurn("c1", "read", `{"url":"https://real.test/one"}`),
		contentTurn(pushDraftA),
		callTurn("c2", "search", `{"query":"the original document"}`),
		callTurn("c3", "read", `{"url":"https://real.test/two"}`),
		contentTurn(pushDraftB),
	}, true, 40, 0)
	if run.err != nil {
		t.Fatal(run.err)
	}
	if !strings.Contains(run.answer, "Seconda bozza della risposta.") || strings.Contains(run.answer, "Prima bozza") {
		t.Errorf("the answer is not the new draft: %q", run.answer)
	}
	if len(s.bodies) < 3 {
		t.Fatalf("only %d requests", len(s.bodies))
	}
	if role, content := lastUserOrTool(s.bodies[2]); role != "user" || content != pushMessage {
		t.Errorf("the third request ends with %s %q, want the request to go on", role, content)
	}
	if _, ok := s.bodies[2]["tools"]; !ok {
		t.Error("the request to go on does not offer the tools")
	}
	if n := requestsEndingWith(s, pushMessage); n != 1 {
		t.Errorf("the model was asked to go on %d times, want once", n)
	}
	if stats.Push != "more" || stats.PushCalls != 2 || stats.PushPages != 1 {
		t.Errorf("push %q, calls after %d, pages after %d; want more, 2, 1", stats.Push, stats.PushCalls, stats.PushPages)
	}
	if !strings.Contains(stats.PushDraft, "Prima bozza della risposta.") {
		t.Errorf("the first draft is not saved: %q", stats.PushDraft)
	}
}

// 2. Nothing more to look for: the first draft stands, "done" does not go out.
func TestDeepLoop_PushNothingMore(t *testing.T) {
	run, stats, s := pushRun(t, []string{
		callTurn("c1", "read", `{"url":"https://real.test/one"}`),
		contentTurn(pushDraftA),
		contentTurn("done"),
	}, true, 40, 0)
	if run.err != nil {
		t.Fatal(run.err)
	}
	if !strings.Contains(run.answer, "Prima bozza della risposta.") || strings.Contains(run.answer, "done") {
		t.Errorf("the answer is not the first draft: %q", run.answer)
	}
	if requestsEndingWith(s, pushMessage) != 1 {
		t.Error("the model was not asked to go on")
	}
	if stats.Push != "kept" || stats.PushDraft != "" || stats.PushCalls != 0 {
		t.Errorf("push %q, draft %q, calls after %d; want kept, empty, 0", stats.Push, stats.PushDraft, stats.PushCalls)
	}
}

// 3. A rewrite without any search or reading does not replace the first draft.
func TestDeepLoop_PushRewriteWithoutSearch(t *testing.T) {
	run, stats, _ := pushRun(t, []string{
		callTurn("c1", "read", `{"url":"https://real.test/one"}`),
		contentTurn(pushDraftA),
		contentTurn(pushDraftC),
	}, true, 40, 0)
	if run.err != nil {
		t.Fatal(run.err)
	}
	if !strings.Contains(run.answer, "Prima bozza della risposta.") || strings.Contains(run.answer, "Terza bozza") {
		t.Errorf("the answer is not the first draft: %q", run.answer)
	}
	if stats.Push != "kept" {
		t.Errorf("push %q, want kept", stats.Push)
	}
}

// 5. Off = today: no request to go on.
func TestDeepLoop_PushOff(t *testing.T) {
	run, stats, s := pushRun(t, []string{
		callTurn("c1", "read", `{"url":"https://real.test/one"}`),
		contentTurn(pushDraftA),
	}, false, 40, 0)
	if run.err != nil {
		t.Fatal(run.err)
	}
	if anyRequestHolds(s, pushMessage) {
		t.Error("the model was asked to go on with the switch off")
	}
	if !strings.Contains(run.answer, "Prima bozza della risposta.") || stats.Push != "" {
		t.Errorf("answer %q, push %q", run.answer, stats.Push)
	}
}

// 6. The first draft is not lost (promise of 1.6), and the sentinel's checks run on
// it: every turn after the request fails, which counts as "done".
func TestDeepLoop_PushDraftKeptWhenTurnsFail(t *testing.T) {
	isPartial := func(run loopRun) bool {
		for _, ev := range run.events {
			if ev.Type == EventWarning && ev.Data.(map[string]string)["kind"] == string(WarnKindPartial) {
				return true
			}
		}
		return false
	}
	t.Run("clean draft", func(t *testing.T) {
		run, stats, _ := pushRun(t, []string{
			callTurn("c1", "read", `{"url":"https://real.test/one"}`),
			contentTurn(pushDraftA),
		}, true, 40, 2)
		if run.err != nil {
			t.Fatalf("the search ended in error, the draft lost: %v", run.err)
		}
		if !strings.Contains(run.answer, "Prima bozza della risposta.") {
			t.Errorf("the first draft is not the answer: %q", run.answer)
		}
		if stats.Push != "kept" || stats.ReviewSkipped != "" || stats.ReviewUnfinished || isPartial(run) {
			t.Errorf("push %q, skipped %q, unfinished %t, partial %t: want a whole, checked answer",
				stats.Push, stats.ReviewSkipped, stats.ReviewUnfinished, isPartial(run))
		}
	})
	t.Run("draft sent back", func(t *testing.T) {
		draft := "La pagina è stata scaricata 987.654 volte. " + prose(30)
		run, stats, s := pushRun(t, []string{
			callTurn("c1", "read", `{"url":"https://real.test/one"}`),
			contentTurn(draft),
		}, true, 40, 2)
		if run.err != nil {
			t.Fatalf("the search ended in error, the draft lost: %v", run.err)
		}
		if !stats.Reviewed || !anyRequestHolds(s, "987.654") || len(s.bodies) < 4 {
			t.Errorf("the checks did not send the draft back: reviewed %t, %d requests", stats.Reviewed, len(s.bodies))
		}
		if len(s.bodies) >= 4 && anyRequestHolds(&scriptedLLM{bodies: s.bodies[3:4]}, pushMessage) {
			t.Error("the request to go on stayed in the conversation the draft was sent back in")
		}
		if !strings.Contains(run.answer, "La pagina è stata scaricata 987.654 volte.") || !stats.ReviewUnfinished || !isPartial(run) {
			t.Errorf("unfinished %t, partial %t, answer %q", stats.ReviewUnfinished, isPartial(run), run.answer)
		}
	})
}

// 6b. The model searched again after the request and then its turns failed: it
// went on, and the session counts so; the first draft goes out, partial, and the
// note says it went out without the sentinel's turn, not that a rewrite did not
// finish (nothing was sent back).
func TestDeepLoop_PushMoreWhenTheNewDraftFails(t *testing.T) {
	run, stats, _ := pushRun(t, []string{
		callTurn("c1", "read", `{"url":"https://real.test/one"}`),
		contentTurn(pushDraftA),
		callTurn("c2", "read", `{"url":"https://real.test/two"}`),
	}, true, 40, 3)
	if run.err != nil {
		t.Fatal(run.err)
	}
	if !strings.Contains(run.answer, "Prima bozza della risposta.") {
		t.Errorf("the first draft is not the answer: %q", run.answer)
	}
	if stats.Push != "more" || stats.PushCalls != 1 || stats.PushPages != 1 {
		t.Errorf("push %q, calls after %d, pages after %d; want more, 1, 1", stats.Push, stats.PushCalls, stats.PushPages)
	}
	if stats.PushDraft != "" {
		t.Errorf("push_draft is saved, but no new draft replaced the first: %q", stats.PushDraft)
	}
	if stats.ReviewUnfinished || strings.Contains(run.answer, "la riscrittura chiesta non si è conclusa") {
		t.Errorf("the note says a rewrite did not finish, but nothing was sent back: %q", run.answer)
	}
	if stats.ReviewSkipped == "" || !strings.Contains(run.answer, "Vedetta non ha potuto rimandare la bozza") {
		t.Errorf("the note does not say the draft went out without the sentinel's turn (skipped %q): %q", stats.ReviewSkipped, run.answer)
	}
	partial := false
	for _, ev := range run.events {
		if ev.Type == EventWarning && ev.Data.(map[string]string)["kind"] == string(WarnKindPartial) {
			partial = true
		}
	}
	if !partial {
		t.Error("the answer is not marked partial")
	}
}

// 7. Research already closed by the limits: no request to go on.
func TestDeepLoop_PushNotWhenResearchClosed(t *testing.T) {
	run, stats, s := pushRun(t, []string{
		callTurn("c1", "read", `{"url":"https://real.test/one"}`),
		contentTurn(pushDraftA),
	}, true, 1, 0)
	if run.err != nil {
		t.Fatal(run.err)
	}
	if anyRequestHolds(s, pushMessage) {
		t.Error("the model was asked to go on after the research was closed")
	}
	if !strings.Contains(run.answer, "Prima bozza della risposta.") || stats.Push != "" {
		t.Errorf("answer %q, push %q", run.answer, stats.Push)
	}
}

// Where each sentence comes from (Piano dopo audit 6.2, docs/SPEC_FASE6_CITAZIONI.md §6.6).

const quoteTestURL = "https://real.test/reattore"

var quoteTestFiller int // characters of filler after the page, for the test of the room

const quoteTestPage = "Il recipiente del reattore è di acciaio forgiato e pesa 310 tonnellate. Le pareti sono spesse venticinque centimetri."

func quoteTestAnswer(lang string) string {
	return "Il recipiente del reattore è di acciaio forgiato e pesa 310 tonnellate secondo la scheda.\n\n" +
		"Il reattore è stato verniciato di verde dagli operatori nei primi anni novanta del secolo scorso.\n\n" +
		StepMessage(lang, "deduction_label") + " da questi dati segue che due recipienti pesano 620 tonnellate in totale."
}

// quoteRun runs a Deep a ciclo that reads one page and answers, then the quote turns that follow.
func quoteRun(t *testing.T, on bool, extra []string, h func(s *scriptedLLM) http.Handler, setup ...func(*Engine)) (loopRun, LoopStats, *scriptedLLM) {
	t.Helper()
	sp := &mockSearchProvider{results: []search.Result{{Title: "Reattore", URL: quoteTestURL, Content: "scheda"}}}
	rd := &mockReader{docs: map[string]reader.Document{quoteTestURL: {Title: "Reattore", URL: quoteTestURL, Content: quoteTestPage + strings.Repeat(" Riga di riempimento della scheda.", quoteTestFiller/35)}}}
	turns := append([]string{
		callTurn("c1", "read", `{"url":"`+quoteTestURL+`"}`),
		contentTurn(quoteTestAnswer("it")),
	}, extra...)
	s := &scriptedLLM{turns: turns}
	var handler http.Handler = http.HandlerFunc(s.handler)
	if h != nil {
		handler = h(s)
	}
	setup = append(setup, func(e *Engine) { e.SetDeepLoopQuotes(on) })
	run, stats := runLoopMetaWith(t, handler, sp, rd, setup...)
	return run, stats, s
}

func askedQuotes(s *scriptedLLM) int {
	n := 0
	for _, b := range s.bodies {
		if b == nil {
			continue
		}
		if raw, _ := json.Marshal(b); strings.Contains(string(raw), "The answer above is final") {
			n++
		}
	}
	return n
}

// quoteLog collects the log of a run, to read the lines of the quote turn.
func quoteLog(t *testing.T) *strings.Builder {
	t.Helper()
	var mu sync.Mutex
	var b strings.Builder
	prev := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return b.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(prev) })
	return &b
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// doneData is the final event of a run.
func doneData(t *testing.T, run loopRun) map[string]interface{} {
	t.Helper()
	for _, ev := range run.events {
		if ev.Type == EventDone {
			return ev.Data.(map[string]interface{})
		}
	}
	t.Fatal("no final event")
	return nil
}

// doneQuotes is what the final event gives the interface as quotes.
func doneQuotes(t *testing.T, run loopRun) []QuoteItem {
	t.Helper()
	items, _ := doneData(t, run)["quotes"].([]QuoteItem)
	return items
}

// sameAnswerAsOff checks that the answer, without the notes, is the one of the run with
// the switch off, and that it did not go out as partial.
func sameAnswerAsOff(t *testing.T, run loopRun) {
	t.Helper()
	off, _, _ := quoteRun(t, false, nil, nil)
	if off.err != nil {
		t.Fatal(off.err)
	}
	if got, want := withoutNotes(run.answer), withoutNotes(off.answer); got != want {
		t.Errorf("the answer is not the one of the switch off:\n%q\n%q", got, want)
	}
	if partial, _ := doneData(t, run)["partial"].(bool); partial {
		t.Error("the final event says partial")
	}
}

// logLine is the line of the log that holds want, or "".
func logLine(logs, want string) string {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	return ""
}

func TestDeepLoop_QuotesAfterTheAnswer(t *testing.T) {
	reply := `1 | P1 | "Il recipiente del reattore è di acciaio forgiato e pesa 310 tonnellate"` + "\n2 | none\n"
	off, offStats, offLLM := quoteRun(t, false, nil, nil)
	logs := quoteLog(t)
	on, stats, s := quoteRun(t, true, []string{contentTurn(reply)}, nil)
	if on.err != nil || off.err != nil {
		t.Fatalf("errors: %v / %v", on.err, off.err)
	}
	// both sentences have a readable line, "none" included (Piano fase 6 chiusura, A3)
	if stats.QuoteLines != 2 {
		t.Errorf("quote_lines = %d, want 2", stats.QuoteLines)
	}
	if line := logLine(logs.String(), "quotes: done"); !strings.Contains(line, fmt.Sprintf("lines=2 asked=2 reply_chars=%d seconds=", len(reply))) {
		t.Errorf("log line: %q", line)
	}
	if items := doneQuotes(t, on); len(items) != 2 || items[0].State != "found" {
		t.Errorf("the final event's quotes: %+v", items)
	}
	if withoutNotes(on.answer) != withoutNotes(off.answer) {
		t.Errorf("the answer changed:\n%q\n%q", withoutNotes(on.answer), withoutNotes(off.answer))
	}
	if stats.Quotes != "done" || len(stats.QuoteItems) != 2 {
		t.Fatalf("quotes %q, items %+v", stats.Quotes, stats.QuoteItems)
	}
	if it := stats.QuoteItems[0]; it.State != "found" || len(it.Refs) != 1 || !strings.Contains(it.Refs[0].Evidence, "acciaio forgiato e pesa 310 tonnellate") {
		t.Errorf("first item: %+v", it)
	}
	if stats.QuoteItems[1].State != "none" {
		t.Errorf("second item: %+v", stats.QuoteItems[1])
	}
	if !strings.Contains(on.answer, "Frasi con una citazione ritrovata nelle pagine lette: 1 di 2.") {
		t.Errorf("the note is missing: %q", on.answer)
	}
	last := s.bodies[len(s.bodies)-1]
	if _, has := last["tools"]; has {
		t.Error("the quote request offers tools")
	}
	if role, content := lastUserOrTool(last); role != "user" || !strings.Contains(content, "PAGES:") || !strings.Contains(content, "SENTENCES:") {
		t.Errorf("last message: %s %q", role, content)
	}
	if offStats.Quotes != "" || askedQuotes(offLLM) != 0 {
		t.Errorf("the switch is off and quotes ran: %q", offStats.Quotes)
	}
}

func TestDeepLoop_QuoteNotFound(t *testing.T) {
	reply := `1 | P1 | "Il recipiente del reattore è di rame brunito e pesa duecento tonnellate"` + "\n2 | none\n"
	run, stats, _ := quoteRun(t, true, []string{contentTurn(reply)}, nil)
	if stats.Quotes != "done" || stats.QuoteItems[0].State != "not_found" {
		t.Fatalf("quotes %q, items %+v", stats.Quotes, stats.QuoteItems)
	}
	if !strings.Contains(run.answer, "non ritrovate: 1") {
		t.Errorf("the note does not count the passage not found: %q", run.answer)
	}
}

func TestDeepLoop_QuotesOffIsToday(t *testing.T) {
	run, stats, s := quoteRun(t, false, nil, nil)
	if askedQuotes(s) != 0 || stats.Quotes != "" || stats.QuoteItems != nil {
		t.Errorf("asked %d, quotes %q", askedQuotes(s), stats.Quotes)
	}
	if strings.Contains(run.answer, "citazione") {
		t.Errorf("a note about quotes with the switch off: %q", run.answer)
	}
}

func TestDeepLoop_QuoteTurnFailsAnswerStaysWhole(t *testing.T) {
	failing := func(s *scriptedLLM) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				body, _ := io.ReadAll(r.Body)
				if strings.Contains(string(body), "The answer above is final") {
					s.mu.Lock()
					s.bodies = append(s.bodies, nil)
					s.mu.Unlock()
					http.Error(w, `{"error":"the server broke"}`, http.StatusBadRequest) // not retried
					return
				}
				r.Body = io.NopCloser(strings.NewReader(string(body)))
			}
			s.handler(w, r)
		})
	}
	run, stats, _ := quoteRun(t, true, nil, failing)
	if run.err != nil || stats.Quotes != "error" || stats.QuoteItems != nil {
		t.Fatalf("err %v, quotes %q, items %v", run.err, stats.Quotes, stats.QuoteItems)
	}
	sameAnswerAsOff(t, run)
	for _, ev := range run.events {
		if ev.Type == EventWarning {
			t.Errorf("a warning for a failed quote turn: %+v", ev)
		}
	}
	if !strings.Contains(run.answer, "non ha avuto una risposta utilizzabile") {
		t.Errorf("the note does not say the quotes did not arrive: %q", run.answer)
	}
}

func TestDeepLoop_QuoteFormatNotFollowed(t *testing.T) {
	logs := quoteLog(t)
	reply := "Non saprei dire da dove viene ogni frase,\nma la scheda è molto completa. " + strings.Repeat("Ancora parole. ", 40)
	run, stats, _ := quoteRun(t, true, []string{contentTurn(reply)}, nil)
	if stats.Quotes != "format" {
		t.Fatalf("quotes %q", stats.Quotes)
	}
	// what the model wrote can be read in the log, on one line and cut (Piano fase 6 chiusura, A3)
	if stats.QuoteLines != 0 {
		t.Errorf("quote_lines = %d, want 0", stats.QuoteLines)
	}
	if line := logLine(logs.String(), "quotes: format found="); !strings.Contains(line, "lines=0 asked=2") {
		t.Errorf("log line: %q", line)
	}
	begins := logLine(logs.String(), "quotes: format, the reply begins:")
	if !strings.Contains(begins, "ogni frase, ma la scheda") || strings.Count(begins, "Ancora parole.") > 20 {
		t.Errorf("the reply's beginning is not in the log, on one line and cut at 300 characters: %q", begins)
	}
	// the interface gets no passages from a reply out of format; LoopStats keeps them for the measures (A4)
	if items := doneQuotes(t, run); len(items) != 0 {
		t.Errorf("the final event gives the interface quotes of a reply out of format: %+v", items)
	}
	if len(stats.QuoteItems) != 2 {
		t.Errorf("LoopStats lost the items: %+v", stats.QuoteItems)
	}
	sameAnswerAsOff(t, run)
}

func TestDeepLoop_NoQuotesWithoutResearch(t *testing.T) {
	s := &scriptedLLM{turns: []string{contentTurn("Ciao! Come posso aiutarti oggi con la tua ricerca?")}}
	_, stats := runLoopMetaWith(t, http.HandlerFunc(s.handler), &mockSearchProvider{}, &mockReader{}, func(e *Engine) { e.SetDeepLoopQuotes(true) })
	if askedQuotes(s) != 0 || stats.Quotes != "" {
		t.Errorf("asked %d, quotes %q", askedQuotes(s), stats.Quotes)
	}
}

func TestDeepLoop_QuotesNoRoom(t *testing.T) {
	// a page that leaves room to be read but not for one more turn after the answer
	quoteTestFiller = 30000
	defer func() { quoteTestFiller = 0 }()
	small := func(s *scriptedLLM) http.Handler {
		s.models = `{"object":"list","data":[{"id":"m","max_model_len":16890,"context_length":16890,"max_tokens_cap":100,"max_tokens_default":100}]}`
		return http.HandlerFunc(s.handler)
	}
	run, stats, s := quoteRun(t, true, []string{contentTurn("1 | none\n")}, small)
	if stats.Quotes != "no_room" || askedQuotes(s) != 0 {
		t.Errorf("quotes %q (limit %d, peak %d), asked %d", stats.Quotes, stats.ContextLimitChars, stats.ContextPeakChars, askedQuotes(s))
	}
	if strings.Contains(run.answer, "citazione") {
		t.Errorf("a note about quotes when there was no room: %q", run.answer)
	}
}

// TestDeepLoop_QuotesListInTheNotes: with the quote turn done, the list goes at the end of the notes,
// after «Pagine lette», in the same event, so it travels with the saved answer, the copy and the print;
// the answer without the notes is the one of the switch off (Piano fase 6 chiusura, B2).
func TestDeepLoop_QuotesListInTheNotes(t *testing.T) {
	reply := `1 | P1 | "Il recipiente del reattore è di acciaio forgiato e pesa 310 tonnellate"` + "\n2 | none\n"
	run, stats, _ := quoteRun(t, true, []string{contentTurn(reply)}, nil)
	if run.err != nil || stats.Quotes != "done" {
		t.Fatalf("err %v, quotes %q", run.err, stats.Quotes)
	}
	// the whole text as the interface builds it and the answer is saved: the tokens, then the bibliography event
	whole := func(r loopRun) string {
		out := r.answer
		for _, ev := range r.events {
			if ev.Type == EventBibliography {
				out += ev.Data.(string)
			}
		}
		return out
	}
	full := whole(run)
	read, list := strings.Index(full, "### Pagine lette"), strings.Index(full, "### Citazioni")
	if read < 0 || list < read || list < strings.Index(full, NotesMarker) {
		t.Fatalf("the list is not after «Pagine lette», in the notes:\n%s", full)
	}
	for _, want := range []string{"Per un'altra frase il modello non ha indicato una citazione.",
		"- **Il recipiente del reattore è di acciaio forgiato e pesa 310…** — «Il recipiente del reattore è di acciaio forgiato e pesa 310 tonnellate» (pagina letta 1)"} {
		if !strings.Contains(full[list:], want) {
			t.Errorf("the list lacks %q:\n%s", want, full[list:])
		}
	}
	sameAnswerAsOff(t, run)

	off, _, _ := quoteRun(t, false, nil, nil)
	if all := whole(off); !strings.Contains(all, "### Pagine lette") || strings.Contains(all, "### Citazioni") {
		t.Errorf("with the switch off, «Pagine lette» and no list:\n%s", all)
	}
	bad, badStats, _ := quoteRun(t, true, []string{contentTurn("Non saprei dire da dove viene ogni frase. " + strings.Repeat("Parole. ", 30))}, nil)
	if all := whole(bad); badStats.Quotes != "format" || strings.Contains(all, "### Citazioni") {
		t.Errorf("quotes %q, a list for a reply out of format:\n%s", badStats.Quotes, all)
	}
}
