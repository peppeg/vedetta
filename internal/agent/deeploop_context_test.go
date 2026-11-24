package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"vedetta/internal/llm"
	"vedetta/internal/reader"
	"vedetta/internal/search"
	"vedetta/internal/storage"
)

func TestLoopContextLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		info  llm.ContextInfo
		local bool
		want  int
	}{
		{"halogen", llm.ContextInfo{Tokens: 262144, Source: "models", OutputCap: 65536}, true, 583824},
		{"halogen without max_tokens", llm.ContextInfo{Tokens: 262144, Source: "models"}, true, 583824},
		{"gpt-oss on Kilo", llm.ContextInfo{Tokens: 131072, Source: "models", OutputCap: 65536}, false, 288912},
		{"loaded at 32,768", llm.ContextInfo{Tokens: 32768, Source: "props"}, true, 67728},
		{"not stated", llm.ContextInfo{Tokens: llm.DefaultContextTokens, Source: "default"}, true, 288912},
		{"DeepSeek on Kilo: the cloud holds it", llm.ContextInfo{Tokens: 1048576, Source: "models", OutputCap: 943718}, false, 2353296},
		{"1M at home, read from the server", llm.ContextInfo{Tokens: 1048576, Source: "models"}, true, loopLocalMaxChars},
		{"1M at home, set by hand", llm.ContextInfo{Tokens: 1048576, Source: "setting"}, true, 2353296},
		{"too small for anything", llm.ContextInfo{Tokens: 2048, Source: "refusal"}, true, 0},
	} {
		if got := loopContextLimit(tc.info, tc.local); got != tc.want {
			t.Errorf("%s: limit %d, want %d", tc.name, got, tc.want)
		}
	}
}

// runLoopMeta is runLoop with a store, and returns the loop's meta.
func runLoopMeta(t *testing.T, s *scriptedLLM, sp search.Provider, rd reader.Reader) (loopRun, LoopStats) {
	t.Helper()
	return runLoopMetaWith(t, http.HandlerFunc(s.handler), sp, rd)
}

// setup runs after the defaults, to turn switches on or change the call budget.
func runLoopMetaWith(t *testing.T, h http.Handler, sp search.Provider, rd reader.Reader, setup ...func(*Engine)) (loopRun, LoopStats) {
	t.Helper()
	llm.ForgetLimits()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "loop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	engine := NewEngine(sp, rd, llm.NewClient(srv.URL, "", "m"), store)
	engine.SetDeepLoopMaxCalls(40)
	for _, f := range setup {
		f(engine)
	}
	var run loopRun
	var sessionID string
	run.err = engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "Qual è l'ultima versione di Komodo?", Mode: "deep", AnswerLanguage: "it"},
		func(ev Event) error {
			switch ev.Type {
			case EventSession:
				sessionID = ev.Data.(map[string]string)["session_id"]
			case EventToken:
				run.answer += ev.Data.(string)
			}
			run.events = append(run.events, ev)
			return nil
		})
	detail, err := store.GetSession(sessionID)
	if err != nil || detail == nil {
		t.Fatalf("no session: %v", err)
	}
	if len(detail.Messages) == 0 {
		return run, LoopStats{} // no answer, nothing saved
	}
	var meta AnswerMeta
	if err := json.Unmarshal(detail.Messages[len(detail.Messages)-1].Meta, &meta); err != nil || meta.Loop == nil {
		t.Fatalf("no loop meta: %v", err)
	}
	return run, *meta.Loop
}

func requestChars(body map[string]interface{}) int {
	n := 0
	msgs, _ := body["messages"].([]interface{})
	for _, m := range msgs {
		mm, _ := m.(map[string]interface{})
		c, _ := mm["content"].(string)
		n += len(c)
	}
	return n
}

// varied makes n bytes of text that differ line by line.
func varied(n int, tag string) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "%s, riga %d: un dettaglio diverso sulla versione %d.\n", tag, i, i%97)
	}
	return b.String()[:n]
}

// A model loaded with 32,768 tokens: the research closes for memory before the
// conversation outgrows it, and the answer is written.
func TestDeepLoop_SmallMemoryClosesInTime(t *testing.T) {
	var results []search.Result
	for i := 0; i < 10; i++ {
		results = append(results, search.Result{Title: fmt.Sprintf("Risultato %d", i), URL: fmt.Sprintf("https://real.test/r%d", i), Content: varied(1000, fmt.Sprintf("anteprima %d", i))})
	}
	rd := &mockReader{docs: map[string]reader.Document{
		"https://real.test/p1": {Title: "P1", URL: "https://real.test/p1", Content: varied(33000, "pagina uno")},
		"https://real.test/p2": {Title: "P2", URL: "https://real.test/p2", Content: varied(33000, "pagina due")},
	}}
	s := &scriptedLLM{models: `{"data":[{"id":"m","max_model_len":32768}]}`, turns: []string{
		callTurn("c1", "read", `{"url":"https://real.test/p1"}`),
		callTurn("c2", "read", `{"url":"https://real.test/p2"}`),
		callTurn("c3", "search", `{"query":"komodo release"}`),
		contentTurn("L'ultima versione si legge nella [pagina uno](https://real.test/p1). " + prose(40)),
	}}
	run, stats := runLoopMeta(t, s, &mockSearchProvider{results: results}, rd)
	if run.err != nil {
		t.Fatal(run.err)
	}
	if stats.ContextTokens != 32768 || stats.ContextSource != "models" || stats.ContextLimitChars != 67728 {
		t.Fatalf("context meta = %d (%s) limit %d, want 32768 (models) limit 67728", stats.ContextTokens, stats.ContextSource, stats.ContextLimitChars)
	}
	if stats.ClosedBy != "context" {
		t.Fatalf("closed_by = %q, want context", stats.ClosedBy)
	}
	last := s.bodies[len(s.bodies)-1]
	if _, ok := last["tools"]; ok {
		t.Error("research not closed: the last request still offers tools")
	}
	if _, msg := lastUserOrTool(last); !strings.Contains(msg, "too long to read more pages") {
		t.Errorf("closing message = %q", msg)
	}
	memoryBytes := 32768 * llm.BytesPerToken
	for i, b := range s.bodies {
		if n := requestChars(b); n > memoryBytes {
			t.Errorf("request %d is %d bytes, over the memory (%d)", i, n, memoryBytes)
		}
	}
	if stats.ContextPeakChars == 0 || stats.ContextPeakChars > memoryBytes {
		t.Errorf("peak = %d, want between 1 and %d", stats.ContextPeakChars, memoryBytes)
	}
	if !strings.Contains(run.answer, "pagina uno") {
		t.Errorf("no answer written: %q", run.answer)
	}
}

// halogen's memory gives the limit of production, and the peak is recorded.
func TestDeepLoop_HalogenLimit(t *testing.T) {
	s := &scriptedLLM{models: halogenTestModels, turns: []string{contentTurn("Ciao. " + prose(40))}}
	_, stats := runLoopMeta(t, s, &mockSearchProvider{}, &mockReader{})
	if stats.ContextLimitChars != 583824 || stats.ContextSource != "models" || stats.ContextPeakChars == 0 {
		t.Fatalf("limit %d (%s) peak %d, want 583824 from models and a peak", stats.ContextLimitChars, stats.ContextSource, stats.ContextPeakChars)
	}
}

// refusingLLM refuses, as halogen does, the requests whose messages are over
// `over` bytes, without using a scripted turn; silent answers as Kilo does.
// Each refusal moves to the next of `then`, if any: a server whose memory was
// overestimated again.
type refusingLLM struct {
	scriptedLLM
	over    int
	then    []int
	silent  bool
	refused int
}

func (r *refusingLLM) serve(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodGet {
		r.handler(w, req)
		return
	}
	body, _ := io.ReadAll(req.Body)
	var m map[string]interface{}
	_ = json.Unmarshal(body, &m)
	if requestChars(m) > r.over {
		r.mu.Lock()
		r.refused++
		if len(r.then) > 0 {
			r.over, r.then = r.then[0], r.then[1:]
		}
		r.mu.Unlock()
		if r.silent {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"other\"}]}\n\ndata: [DONE]\n\n"))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"the prompt does not fit: it is 300000 tokens and the context is 262144, 37856 tokens over.","type":"invalid_request_error"}}`))
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	r.handler(w, req)
}

func twoPages() *mockReader {
	return &mockReader{docs: map[string]reader.Document{
		"https://real.test/p1": {Title: "P1", URL: "https://real.test/p1", Content: varied(20000, "pagina uno")},
		"https://real.test/p2": {Title: "P2", URL: "https://real.test/p2", Content: varied(30000, "pagina due")},
	}}
}

func toolContents(body map[string]interface{}) []string {
	var out []string
	msgs, _ := body["messages"].([]interface{})
	for _, m := range msgs {
		mm, _ := m.(map[string]interface{})
		if mm["role"] == "tool" {
			c, _ := mm["content"].(string)
			out = append(out, c)
		}
	}
	return out
}

// The server refuses more than its memory states: the page of the last turn
// comes out, the research closes, the answer is written and the list says so.
func TestDeepLoop_RefusedTakesOutTheLastPage(t *testing.T) {
	for _, silent := range []bool{false, true} {
		t.Run(fmt.Sprintf("silent=%t", silent), func(t *testing.T) {
			r := &refusingLLM{over: 40000, silent: silent, scriptedLLM: scriptedLLM{models: halogenTestModels, turns: []string{
				callTurn("c1", "read", `{"url":"https://real.test/p1"}`),
				callTurn("c2", "read", `{"url":"https://real.test/p2"}`),
				contentTurn("Dalla [pagina uno](https://real.test/p1). " + prose(40)),
			}}}
			if silent {
				// Kilo's empty answer counts only over half the memory.
				r.models = `{"data":[{"id":"m","context_length":32768}]}`
			}
			run, stats := runLoopMetaWith(t, http.HandlerFunc(r.serve), &mockSearchProvider{}, twoPages())
			if run.err != nil {
				t.Fatal(run.err)
			}
			if r.refused != 1 || stats.ContextRefused != 1 || stats.PagesRemoved != 1 || stats.PagesRead != 1 || stats.ClosedBy != "context" {
				t.Fatalf("refused %d, meta refused %d removed %d read %d closed_by %q; want 1, 1, 1, 1, context",
					r.refused, stats.ContextRefused, stats.PagesRemoved, stats.PagesRead, stats.ClosedBy)
			}
			last := r.bodies[len(r.bodies)-1]
			tools := toolContents(last)
			if len(tools) != 2 || !strings.Contains(tools[0], "pagina uno") || tools[1] != pageRemovedLine {
				t.Fatalf("tool messages after the refusal: %d, want page one kept and page two removed", len(tools))
			}
			if _, ok := last["tools"]; ok {
				t.Error("research not closed after the refusal")
			}
			if !strings.Contains(run.answer, "pagina uno") {
				t.Errorf("no answer: %q", run.answer)
			}
			var bib string
			for _, ev := range run.events {
				if ev.Type == EventBibliography {
					bib = ev.Data.(string)
				}
			}
			if !strings.Contains(bib, "[P2](https://real.test/p2) (tolta: non entrava nella memoria del modello)") || !strings.Contains(bib, "1. [P1](https://real.test/p1)") {
				t.Errorf("the list does not say the page was taken out: %q", bib)
			}
		})
	}
}

// Kilo's empty answer on a small conversation is not taken for the memory.
func TestDeepLoop_SilentEmptyBelowHalfIsNotMemory(t *testing.T) {
	r := &refusingLLM{over: 30000, silent: true, scriptedLLM: scriptedLLM{models: halogenTestModels, turns: []string{
		callTurn("c1", "read", `{"url":"https://real.test/p2"}`),
		contentTurn("Risposta. " + prose(40)),
	}}}
	runLoopMetaWith(t, http.HandlerFunc(r.serve), &mockSearchProvider{}, twoPages())
	for _, b := range r.bodies {
		for _, c := range toolContents(b) {
			if c == pageRemovedLine {
				t.Fatal("a page was taken out after an empty answer far below the memory")
			}
		}
	}
}

// A second refusal takes out the longest page left.
func TestDeepLoop_RefusedTwiceTakesOutTheLongest(t *testing.T) {
	r := &refusingLLM{over: 40000, then: []int{15000, 1000000}, scriptedLLM: scriptedLLM{models: halogenTestModels, turns: []string{
		callTurn("c1", "read", `{"url":"https://real.test/p1"}`),
		callTurn("c2", "read", `{"url":"https://real.test/p2"}`),
		contentTurn("Scritta senza pagine. " + prose(40)),
	}}}
	run, stats := runLoopMetaWith(t, http.HandlerFunc(r.serve), &mockSearchProvider{}, twoPages())
	if r.refused != 2 || stats.PagesRemoved != 2 || stats.ContextRefused != 2 || stats.PagesRead != 0 {
		t.Fatalf("refused %d, removed %d after %d, read %d; want 2, 2, 2, 0", r.refused, stats.PagesRemoved, stats.ContextRefused, stats.PagesRead)
	}
	if !strings.Contains(run.answer, "Scritta senza pagine") {
		t.Errorf("no answer: %q", run.answer)
	}
}

// After two tries the refusal is an error as before: no loop of removals.
func TestDeepLoop_RefusedThenGivesUp(t *testing.T) {
	r := &refusingLLM{over: 40000, then: []int{15000, 1000}, scriptedLLM: scriptedLLM{models: halogenTestModels, turns: []string{
		callTurn("c1", "read", `{"url":"https://real.test/p1"}`),
		callTurn("c2", "read", `{"url":"https://real.test/p2"}`),
		contentTurn("Mai scritta. " + prose(40)),
	}}}
	run, _ := runLoopMetaWith(t, http.HandlerFunc(r.serve), &mockSearchProvider{}, twoPages())
	if r.refused > 5 {
		t.Fatalf("refused %d times: the loop kept trying", r.refused)
	}
	if strings.Contains(run.answer, "Mai scritta") {
		t.Errorf("an answer from a server that refuses everything: %q", run.answer)
	}
}

func TestLoopNote_Memory(t *testing.T) {
	s := LoopStats{ClosedBy: "context", ContextTokens: 131072, ContextSource: "default", PagesRemoved: 1, PagesRead: 3}
	it := loopNote("it", s, 2, 0)
	for _, want := range []string{
		"La conversazione ha riempito la memoria del modello (131.072 token): la ricerca si è chiusa lì. (memoria non dichiarata dal server: Vedetta ha usato un valore prudente)",
		"Il server ha rifiutato la conversazione perché troppo lunga: Vedetta ha tolto 1 pagina letta e ha chiesto la risposta con il resto.",
	} {
		if !strings.Contains(it, want) {
			t.Errorf("note misses %q:\n%s", want, it)
		}
	}
	en := loopNote("en", LoopStats{ClosedBy: "context", ContextTokens: 262144, ContextSource: "models", PagesRemoved: 2}, 0, 0)
	if !strings.Contains(en, "(262,144 tokens)") || strings.Contains(en, "prudent") || !strings.Contains(en, "took out 2 pages read") {
		t.Errorf("English note: %s", en)
	}
	if quiet := loopNote("it", LoopStats{ClosedBy: "answer", ContextTokens: 262144, ContextSource: "models"}, 1, 0); strings.Contains(quiet, "memoria") {
		t.Errorf("a research closed by the model speaks of memory: %s", quiet)
	}
}

func TestGroupThousands(t *testing.T) {
	for _, tc := range []struct {
		lang string
		n    int
		want string
	}{{"it", 131072, "131.072"}, {"en", 1048576, "1,048,576"}, {"it", 999, "999"}, {"de", 4096, "4.096"}} {
		if got := groupThousands(tc.lang, tc.n); got != tc.want {
			t.Errorf("groupThousands(%s, %d) = %q, want %q", tc.lang, tc.n, got, tc.want)
		}
	}
}
