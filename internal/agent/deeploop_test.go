package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"vedetta/internal/llm"
	"vedetta/internal/reader"
	"vedetta/internal/search"
	"vedetta/internal/storage"
)

// scriptedLLM answers each chat request with the next scripted stream and keeps
// the request bodies.
type scriptedLLM struct {
	mu     sync.Mutex
	turns  []string // SSE data lines per turn, joined by "\n"
	bodies []map[string]interface{}
	models string // the /models answer; "" = 404
}

func (s *scriptedLLM) handler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet { // /models, /props: the memory is not stated
		if s.models != "" && strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = w.Write([]byte(s.models))
			return
		}
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var m map[string]interface{}
	_ = json.Unmarshal(body, &m)
	s.mu.Lock()
	i := len(s.bodies)
	s.bodies = append(s.bodies, m)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	if i >= len(s.turns) {
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"(no more turns)\"}}]}\n\ndata: [DONE]\n\n"))
		return
	}
	for _, l := range strings.Split(s.turns[i], "\n") {
		_, _ = w.Write([]byte("data: " + l + "\n\n"))
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

func callTurn(id, name, args string) string {
	a, _ := json.Marshal(args)
	return fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]},"finish_reason":"tool_calls"}]}`, id, name, a)
}

func contentTurn(text string) string {
	t, _ := json.Marshal(text)
	return fmt.Sprintf(`{"choices":[{"delta":{"content":%s},"finish_reason":"stop"}]}`, t)
}

type loopRun struct {
	events []Event
	answer string
	err    error
}

// halogenTestModels is halogen's /models of 09/11/2026: runLoop's conversations
// get the limit of production, 583,824 bytes.
const halogenTestModels = `{"object":"list","data":[{"id":"m","max_model_len":262144,"context_length":262144,"meta":{"n_ctx_train":262144,"n_ctx":262144},"max_tokens_cap":65536,"max_tokens_default":8192}]}`

func runLoop(t *testing.T, s *scriptedLLM, sp search.Provider, rd reader.Reader, maxCalls int) loopRun {
	t.Helper()
	return runLoopWith(t, s, sp, rd, maxCalls, func(*Engine) {})
}

func runLoopWith(t *testing.T, s *scriptedLLM, sp search.Provider, rd reader.Reader, maxCalls int, set func(*Engine)) loopRun {
	t.Helper()
	if s.models == "" {
		s.models = halogenTestModels
	}
	llm.ForgetLimits()
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	t.Cleanup(srv.Close)
	engine := NewEngine(sp, rd, llm.NewClient(srv.URL, "", "m"), nil)
	engine.SetDeepLoopMaxCalls(maxCalls)
	set(engine)
	var run loopRun
	var mu sync.Mutex
	run.err = engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "Qual è l'ultima versione di Komodo?", Mode: "deep", AnswerLanguage: "it"},
		func(ev Event) error {
			mu.Lock()
			defer mu.Unlock()
			run.events = append(run.events, ev)
			if ev.Type == EventToken {
				run.answer += ev.Data.(string)
			}
			return nil
		})
	return run
}

// prose is varied text, which the repetition detector must not flag.
func prose(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "La frase numero %d aggiunge un dettaglio. ", (i*7+3)%97) // small: not checked
	}
	return b.String()
}

func lastUserOrTool(body map[string]interface{}) (role, content string) {
	msgs, _ := body["messages"].([]interface{})
	last, _ := msgs[len(msgs)-1].(map[string]interface{})
	role, _ = last["role"].(string)
	content, _ = last["content"].(string)
	return
}

func TestDeepLoop_SearchReadWrite(t *testing.T) {
	long := strings.Repeat("Komodo 1.19 è la versione più recente.\n", 2000) // ~78k: read whole
	sp := &mockSearchProvider{results: []search.Result{{Title: "Komodo releases", URL: "https://real.test/komodo", Content: "Komodo 1.19 released"}}}
	rd := &mockReader{docs: map[string]reader.Document{"https://real.test/komodo": {Title: "Komodo", URL: "https://real.test/komodo",
		Content: "Codice su [GitHub](https://github.com/moghtech/komodo).\n" + long}}}
	s := &scriptedLLM{turns: []string{
		callTurn("c1", "search", `{"query":"komodo release"}`),
		callTurn("c2", "read", `{"url":"https://real.test/komodo"}`),
		contentTurn("L'ultima versione è la 1.19 ([note di rilascio](https://real.test/komodo)), con 987.654 stelle. " +
			"Il repository è [github.com/moghtech/komodo](https://github.com/moghtech/komodo), non [github.com/invented/komodo](https://github.com/invented/komodo). " + prose(40)),
	}}
	run := runLoop(t, s, sp, rd, 40)
	if run.err != nil {
		t.Fatal(run.err)
	}
	if len(s.bodies) != 3 {
		t.Fatalf("requests = %d, want 3", len(s.bodies))
	}
	if _, ok := s.bodies[0]["tools"]; !ok {
		t.Error("first request without tools")
	}
	role, res := lastUserOrTool(s.bodies[1])
	if role != "tool" || !strings.Contains(res, "[UNTRUSTED SEARCH RESULT STARTS]") || !strings.Contains(res, "https://real.test/komodo") {
		t.Errorf("search result not sent back as an isolated tool message: %s %q", role, res)
	}
	role, res = lastUserOrTool(s.bodies[2])
	if role != "tool" || !strings.Contains(res, long) || !strings.Contains(res, "[UNTRUSTED PAGE STARTS]") {
		t.Errorf("the whole page was not sent back: %s %q", role, res[:min(len(res), 200)])
	}
	if !strings.Contains(run.answer, "[note di rilascio](https://real.test/komodo)") {
		t.Errorf("link to the page read was removed: %q", run.answer)
	}
	if !strings.Contains(run.answer, "[github.com/moghtech/komodo](https://github.com/moghtech/komodo)") {
		t.Errorf("link written inside the page read was removed: %q", run.answer)
	}
	if strings.Contains(run.answer, "github.com/invented") {
		t.Errorf("link outside the session sources kept: %q", run.answer)
	}
	for _, want := range []string{
		"1 ricerca, 1 pagina letta, 0 non leggibili",
		"Link nella risposta: 2. A pagine lette dal modello: 1. A pagine che non ha letto (ha visto al massimo un'anteprima o un brano): 1.",
		"**Provenienza: researched**",
		"non si sono potuti verificare: 1.",
		"forse dalla memoria del modello o suoi calcoli: 1 su 2.",
		"(987654)",
	} {
		if !strings.Contains(run.answer, want) {
			t.Errorf("Go note misses %q: %q", want, run.answer)
		}
	}
	var done map[string]interface{}
	var bib string
	for _, ev := range run.events {
		switch ev.Type {
		case EventDone:
			done = ev.Data.(map[string]interface{})
		case EventBibliography:
			bib = ev.Data.(string)
		}
	}
	if strings.Contains(run.answer, "fonti verificate") {
		t.Errorf("the answer calls its sources verified: %q", run.answer)
	}
	if done == nil || done["provenance"] != string(ProvenanceResearched) || done["loop_calls"] != 2 {
		t.Errorf("done = %v", done)
	}
	if read, _ := done["pages_read"].([]string); len(read) != 1 || read[0] != "https://real.test/komodo" {
		t.Errorf("pages_read = %v", done["pages_read"])
	}
	if !strings.Contains(bib, "1. [Komodo](https://real.test/komodo)") {
		t.Errorf("bibliography = %q", bib)
	}
}

func TestDeepLoop_BudgetClosesResearch(t *testing.T) {
	sp := &mockSearchProvider{results: []search.Result{{Title: "r", URL: "https://real.test/r", Content: "x"}}}
	s := &scriptedLLM{turns: []string{
		callTurn("c1", "search", `{"query":"one"}`),
		callTurn("c2", "search", `{"query":"two"}`),
		contentTurn("Risposta scritta con quello che ho trovato. " + prose(50)),
	}}
	run := runLoop(t, s, sp, &mockReader{}, 2)
	if run.err != nil {
		t.Fatal(run.err)
	}
	if len(s.bodies) != 3 {
		t.Fatalf("requests = %d, want 3", len(s.bodies))
	}
	if _, ok := s.bodies[2]["tools"]; ok {
		t.Error("tools still offered after the budget")
	}
	role, msg := lastUserOrTool(s.bodies[2])
	if role != "user" || !strings.Contains(msg, "used all the searches") {
		t.Errorf("no invitation to write: %s %q", role, msg)
	}
	if !strings.Contains(run.answer, "Risposta scritta") {
		t.Errorf("answer = %q", run.answer)
	}
}

func TestDeepLoop_RepeatedCalls(t *testing.T) {
	sp := &mockSearchProvider{results: []search.Result{{Title: "r", URL: "https://real.test/r", Content: "x"}}}
	var turns []string
	for i := 0; i < loopMaxRepeats+2; i++ {
		turns = append(turns, callTurn(fmt.Sprintf("c%d", i), "search", `{"query":"same"}`))
	}
	turns = append(turns, contentTurn("Fine. "+prose(50)))
	s := &scriptedLLM{turns: turns}
	run := runLoop(t, s, sp, &mockReader{}, 40)
	if run.err != nil {
		t.Fatal(run.err)
	}
	_, msg := lastUserOrTool(s.bodies[2])
	if !strings.Contains(msg, "already ran this search") {
		t.Errorf("repeat not answered as such: %q", msg)
	}
	last := s.bodies[len(s.bodies)-1]
	if _, ok := last["tools"]; ok {
		t.Error("research not closed after repeats")
	}
	if _, msg := lastUserOrTool(last); !strings.Contains(msg, "repeating calls") {
		t.Errorf("closing message = %q", msg)
	}
}

func TestDeepLoop_ServerWithoutTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"tools are not supported"}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	engine := NewEngine(&mockSearchProvider{}, &mockReader{}, llm.NewClient(srv.URL, "", "m"), nil)
	engine.SetDeepLoopMaxCalls(40)
	err := engine.executeDeepLoop(context.Background(), SearchParams{Query: "q", Mode: "deep"}, func(Event) error { return nil },
		&mockSearchProvider{}, &mockReader{}, engine.LLMClient(), nil, nil, newLLMTrouble())
	if err != errLoopUnsupported {
		t.Fatalf("err = %v, want errLoopUnsupported", err)
	}
}

// On a follow-up the model copied Vedetta's notes from the answers it was given.
func TestDeepLoop_FollowUpWithoutNotes(t *testing.T) {
	s := &scriptedLLM{turns: []string{contentTurn("Ciao.")}}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()
	engine := NewEngine(&mockSearchProvider{}, &mockReader{}, llm.NewClient(srv.URL, "", "m"), nil)
	engine.SetDeepLoopMaxCalls(40)
	history := []storage.TurnMessage{{
		Role: "assistant", Query: "Prima domanda",
		Content: "La risposta del modello.\n\n" + NotesMarker + "\n\n---\n*Ricerca guidata dal modello: 4 ricerche.*",
	}}
	err := engine.executeDeepLoop(context.Background(), SearchParams{Query: "E poi?", Mode: "deep"}, func(Event) error { return nil },
		&mockSearchProvider{}, &mockReader{}, engine.LLMClient(), nil, history, newLLMTrouble())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	msgs, _ := s.bodies[0]["messages"].([]interface{})
	for _, m := range msgs {
		msg, _ := m.(map[string]interface{})
		content, _ := msg["content"].(string)
		if strings.Contains(content, "Ricerca guidata") || strings.Contains(content, NotesMarker) {
			t.Errorf("the model was given the notes: %q", content)
		}
		found = found || (msg["role"] == "assistant" && content == "La risposta del modello.")
	}
	if !found {
		t.Errorf("the previous answer is missing: %v", msgs)
	}
}

func TestWithoutNotes(t *testing.T) {
	for in, want := range map[string]string{
		"Testo.\n\n" + NotesMarker + "\n\n---\n*Nota.*": "Testo.",
		"Testo senza note, da una versione vecchia.":    "Testo senza note, da una versione vecchia.",
		NotesMarker + "\n*Nota.*":                       "",
	} {
		if got := withoutNotes(in); got != want {
			t.Errorf("withoutNotes(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCutAtLine(t *testing.T) {
	text := strings.Repeat("È una riga di prova abbastanza lunga.\n", 2000)
	got := cutAtLine(text, 30000)
	if len(got) > 30000 || len(got) < 27000 || !strings.HasSuffix(got, "\n") {
		t.Errorf("len = %d, ends at a line: %t", len(got), strings.HasSuffix(got, "\n"))
	}
	if !strings.HasPrefix(text, got) {
		t.Error("not the beginning of the text")
	}
	if !utf8.ValidString(cutAtLine(strings.Repeat("è", 40000), 30001)) {
		t.Error("cut inside a character")
	}
	if cutAtLine("short", 30000) != "short" {
		t.Error("a short text was cut")
	}
}

// A page is read whole, however long, as long as it fits in the conversation.
func TestDeepLoop_LongPageThatFitsIsWhole(t *testing.T) {
	long := strings.Repeat("Una riga della pagina lunga.\n", 17000) // ~500k
	rd := &mockReader{docs: map[string]reader.Document{"https://real.test/long": {Title: "Lunga", URL: "https://real.test/long", Content: long}}}
	s := &scriptedLLM{turns: []string{
		callTurn("c1", "read", `{"url":"https://real.test/long"}`),
		callTurn("c2", "read", `{"url":"https://real.test/long","part":2}`),
		contentTurn("Risposta dalla [pagina](https://real.test/long). " + prose(40)),
	}}
	run := runLoop(t, s, &mockSearchProvider{}, rd, 40)
	if run.err != nil {
		t.Fatal(run.err)
	}
	if _, res := lastUserOrTool(s.bodies[1]); !strings.Contains(res, long) {
		t.Errorf("the page was not passed whole: %d characters", len(res))
	}
	if _, res := lastUserOrTool(s.bodies[2]); !strings.Contains(res, "already read") || strings.Contains(res, "Una riga") {
		t.Errorf("asking for another part of a page read whole: %q", res[:min(len(res), 200)])
	}
	if strings.Contains(run.answer, "solo in parte") {
		t.Errorf("a page read whole is declared partial: %q", run.answer)
	}
}

// What cannot fit is cut and declared, and the research goes on with other sources.
func TestDeepLoop_PageTooLongIsCutAndDeclared(t *testing.T) {
	giant := strings.Repeat("Una riga di una legge lunghissima.\n", 40000) // 1.4M
	rd := &mockReader{docs: map[string]reader.Document{
		"https://real.test/legge": {Title: "Legge", URL: "https://real.test/legge", Content: giant},
		"https://real.test/nota":  {Title: "Nota", URL: "https://real.test/nota", Content: "Una nota breve sulla legge, con il comma giusto."},
	}}
	s := &scriptedLLM{turns: []string{
		callTurn("c1", "read", `{"url":"https://real.test/legge"}`),
		callTurn("c2", "read", `{"url":"https://real.test/nota"}`),
		contentTurn("Risposta dalla [legge](https://real.test/legge) e dalla [nota](https://real.test/nota). " + prose(40)),
	}}
	run := runLoop(t, s, &mockSearchProvider{}, rd, 40)
	if run.err != nil {
		t.Fatal(run.err)
	}
	_, res := lastUserOrTool(s.bodies[1])
	if !strings.Contains(res, "too long for this conversation") {
		t.Errorf("the model is not told the page is cut: %q", res[:min(len(res), 200)])
	}
	// runLoop's server states halogen's memory.
	limit := loopContextLimit(llm.ContextInfo{Tokens: 262144, Source: "models", OutputCap: 65536}, true)
	if len(res) > limit/2+5000 || len(res) < limit/3 {
		t.Errorf("the cut page is %d characters, half of the room expected", len(res))
	}
	if _, res := lastUserOrTool(s.bodies[2]); !strings.Contains(res, "comma giusto") {
		t.Errorf("no room left for another source after the giant page: %q", res[:min(len(res), 200)])
	}
	if !strings.Contains(run.answer, "lette solo in parte: 1.") {
		t.Errorf("the note does not declare the partial page: %q", run.answer)
	}
	var bib string
	for _, ev := range run.events {
		if ev.Type == EventBibliography {
			bib = ev.Data.(string)
		}
	}
	if !strings.Contains(bib, "(https://real.test/legge) (letta solo in parte)") || strings.Contains(bib, "(https://real.test/nota) (") {
		t.Errorf("the list of pages does not say which one was cut: %q", bib)
	}
}

// With the conversation full no page is opened, and the model is told to write.
func TestDeepLoop_NoRoomForAnotherPage(t *testing.T) {
	long := strings.Repeat("Una riga della pagina lunga.\n", 20000) // ~580k
	rd := &mockReader{docs: map[string]reader.Document{
		"https://real.test/long":  {Title: "Lunga", URL: "https://real.test/long", Content: long},
		"https://real.test/altra": {Title: "Altra", URL: "https://real.test/altra", Content: "Altra pagina."},
	}}
	s := &scriptedLLM{turns: []string{
		callTurn("c1", "read", `{"url":"https://real.test/long"}`),
		callTurn("c2", "read", `{"url":"https://real.test/altra"}`),
		contentTurn("Risposta dalla [pagina](https://real.test/long). " + prose(40)),
	}}
	run := runLoop(t, s, &mockSearchProvider{}, rd, 40)
	if run.err != nil {
		t.Fatal(run.err)
	}
	if _, res := lastUserOrTool(s.bodies[2]); !strings.Contains(res, "too long to read more pages") {
		t.Errorf("no refusal with the conversation full: %q", res[:min(len(res), 200)])
	}
	if !strings.Contains(run.answer, "1 pagina letta") {
		t.Errorf("a page refused for lack of room counted as read: %q", run.answer)
	}
}

func TestConversationCharsCountsReasoning(t *testing.T) {
	got := conversationChars([]llm.Message{{Role: "assistant", Content: "ab", ReasoningContent: "cde"}})
	if got != 5 {
		t.Errorf("conversationChars = %d, want 5", got)
	}
}

func TestDeepLoop_NoResearchNoNote(t *testing.T) {
	s := &scriptedLLM{turns: []string{contentTurn("Ciao! Sto bene, grazie. Dimmi pure cosa vuoi cercare.")}}
	run := runLoop(t, s, &mockSearchProvider{}, &mockReader{}, 40)
	if run.err != nil {
		t.Fatal(run.err)
	}
	if strings.Contains(run.answer, "Ricerca guidata dal modello") {
		t.Errorf("research note on an answer without research: %q", run.answer)
	}
	if !strings.Contains(run.answer, "parametric") {
		t.Errorf("provenance missing: %q", run.answer)
	}
}

// The model may separate its own sections with "---": the interface must cut the
// answer only where Go's notes begin, at the marker, written once.
func TestDeepLoop_NotesMarkerAfterModelRules(t *testing.T) {
	sp := &mockSearchProvider{results: []search.Result{{Title: "Guida", URL: "https://real.test/guida", Content: "Open WebUI"}}}
	s := &scriptedLLM{turns: []string{
		callTurn("c1", "search", `{"query":"open webui"}`),
		contentTurn("# Guida\n\n---\n\n## 1. Cos'è\n\nOpen WebUI ([guida](https://real.test/guida)). " + prose(40) + "\n\n---\n\n## 2. Installazione\n\nDocker."),
	}}
	run := runLoop(t, s, sp, &mockReader{}, 40)
	if run.err != nil {
		t.Fatal(run.err)
	}
	if n := strings.Count(run.answer, NotesMarker); n != 1 {
		t.Fatalf("marker written %d times: %q", n, run.answer)
	}
	body, notes, _ := strings.Cut(run.answer, NotesMarker)
	if !strings.Contains(body, "## 2. Installazione") || strings.Contains(body, "Ricerca guidata dal modello") {
		t.Errorf("body cut in the wrong place: %q", body)
	}
	if !strings.Contains(notes, "Ricerca guidata dal modello") {
		t.Errorf("Go note not after the marker: %q", notes)
	}
}

func TestDeepLoop_LinksOpenedByVedetta(t *testing.T) {
	sp := &mockSearchProvider{results: []search.Result{{Title: "Homelab 2026", URL: "https://real.test/list", Content: "Uptime Kuma, Komodo"}}}
	rd := &mockReader{docs: map[string]reader.Document{
		"https://github.com/louislam/uptime-kuma": {Title: "louislam/uptime-kuma", Content: "Uptime Kuma is an easy-to-use self-hosted monitoring tool."},
		// a page that answers but is not the project's (a soft 404, a landing page)
		"https://github.com/wrong/komodo": {Title: "Page not found", Content: "This is not the web page you are looking for."},
	}}
	s := &scriptedLLM{turns: []string{
		callTurn("c1", "search", `{"query":"homelab 2026"}`),
		contentTurn("1. Uptime Kuma → [github.com/louislam/uptime-kuma](https://github.com/louislam/uptime-kuma)\n" +
			"2. Komodo → [github.com/wrong/komodo](https://github.com/wrong/komodo)\n" +
			"3. Inventato → [github.com/nobody/nothing](https://github.com/nobody/nothing)\n" +
			"4. Di nuovo Uptime Kuma: [repo](https://github.com/louislam/uptime-kuma)\n" + prose(30)),
	}}
	run := runLoop(t, s, sp, rd, 40)
	if run.err != nil {
		t.Fatal(run.err)
	}
	for _, want := range []string{
		"1. Uptime Kuma → [github.com/louislam/uptime-kuma](https://github.com/louislam/uptime-kuma)",
		"2. Komodo → *link non verificato*",
		"3. Inventato → *link non verificato*",
		"4. Di nuovo Uptime Kuma: [repo](https://github.com/louislam/uptime-kuma)",
		"aperti da Vedetta per controllare che esistano: 1.",
		"non si sono potuti verificare: 2.",
	} {
		if !strings.Contains(run.answer, want) {
			t.Errorf("answer misses %q:\n%s", want, run.answer)
		}
	}
}

func TestLinkName(t *testing.T) {
	for raw, want := range map[string]string{
		"https://github.com/louislam/uptime-kuma":             "uptime-kuma",
		"https://github.com/TriliumNext/Trilium/releases":     "trilium",
		"https://github.com/foo/bar/releases/tag/v1.2":        "bar",
		"https://homarr.dev/blog/2026/09/03/homarr-2.0/":      "homarr-2",
		"https://immich.app/":                                 "immich",
		"https://www.linkwarden.app":                          "linkwarden",
		"https://github.com/geekdojo/rasputin-control-plane/": "rasputin-control-plane",
		"https://docs.example.org/index.html":                 "docs",
	} {
		if got := linkName(raw); got != want {
			t.Errorf("linkName(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestCheckAnswerNumbers(t *testing.T) {
	have := numberSet("GitTrend: Immich 117,432 stars. Trilium v0.105 released. Proxmox VE 9.1.", "Ho un N100 con 16 GB", "29 ottobre 2026")
	answer := "## 1. Immich\n" +
		"- **Immich** ha 117k stelle ([GitHub](https://github.com/immich-app/immich/releases/tag/v2.4.1)).\n" +
		"Trilium v0.105 e Proxmox VE 9.1; con 16 GB va bene. [github.com/x/y-2.0](https://github.com/x/y-2.0)\n" +
		"| col | 2 |\n|---|---|\n" +
		"OpenClaw ha 300k stelle e costa 4,99 euro; uscito il 29 ottobre 2026."
	checked, misses := checkAnswerNumbers(answer, have)
	if checked != 6 {
		t.Errorf("checked = %d, want 6 (117k, 0.105, 9.1, 300k, 4.99, 2026)", checked)
	}
	if len(misses) != 1 || strings.Join(misses[0].Missing, ",") != "300000,4.99" {
		t.Fatalf("misses = %+v", misses)
	}
	if strings.Contains(misses[0].Sentence, "http") {
		t.Errorf("link address in the sentence: %q", misses[0].Sentence)
	}
}

// Case 24h (19/11/2026): a code the model saw in an address, and the error a page
// answered with, are numbers it saw. A code is looked for whole: the same digits
// elsewhere in an address prove nothing.
func TestNumbersSeen(t *testing.T) {
	const doc = "https://ec.test/commission/api/print/en/ip_26_1276/IP_26_1276_EN.pdf"
	l := &deepLoop{unreadable: map[string]string{
		"a.test/x": "failed: the site answered with an error status: 502 (https://a.test/x/777)",
	}}
	answer := "Le misure sono nel comunicato IP/26/1276. La pagina ha risposto con errore 502."
	if _, misses := checkAnswerNumbers(answer, l.numbersSeen(nil, []string{doc})); len(misses) != 0 {
		t.Errorf("misses = %+v", misses)
	}
	if _, misses := checkAnswerNumbers("La falla è CVE-2026-24471.", l.numbersSeen(nil, []string{"https://nvd.test/vuln/detail/CVE-2026-24471"})); len(misses) != 0 {
		t.Errorf("misses = %+v", misses)
	}
	for text, want := range map[string]string{
		"Sono 1276 casi.":            "1276",  // a quantity, not that code
		"Il comunicato IP/25/1276.":  "1276",  // another code
		"La falla è CVE-2026-24472.": "24472", // never seen
		"Costa 777 euro.":            "777",   // in the address of the page, not in its answer
	} {
		_, misses := checkAnswerNumbers(text, l.numbersSeen(nil, []string{doc, "https://nvd.test/vuln/detail/CVE-2026-24471"}))
		if len(misses) != 1 || !strings.Contains(strings.Join(misses[0].Missing, ","), want) {
			t.Errorf("%q: misses = %+v, want %s", text, misses, want)
		}
	}
}

// Case 03/11/2026: "### 2.1 Docker" and "§5.2" number the answer's sections.
func TestCheckAnswerNumbersSectionNumbering(t *testing.T) {
	answer := "## 2. Installazione\n### 2.1 Docker\n### 5.2 Dettagli che pochi conoscono\n" +
		"Già spiegato al §5.2 e al § 7.\n### 10 consigli\nL'immagine pesa 197 MB."
	checked, misses := checkAnswerNumbers(answer, numberSet("197 MB"))
	if checked != 1 || len(misses) != 0 {
		t.Errorf("checked = %d, misses = %+v; want only 197, found", checked, misses)
	}
}

// Case 19 (05/11/2026, LHCb): the wrong scale was "all present" and the right value
// written in MHz was missing. A small value in hertz stays out of the count.
func TestCheckAnswerNumbersScales(t *testing.T) {
	have := loopNumberSet("down to a hundred zeptometres, thousands of trillions of times smaller than a human hair, parts per billion",
		"I sotto-rivelatori potranno essere letti a 40 MHz")
	answer := "Arriva a dimensioni migliaia di miliardi di volte più piccole di un capello. " +
		"Sono milioni di miliardi di volte più piccole di un capello. " +
		"Legge 30-40 milioni di incroci al secondo. Funziona a 40 MHz."
	checked, misses := checkAnswerNumbers(answer, have)
	if checked != 3 {
		t.Errorf("checked = %d, want 3 (the two ranges, 40 million)", checked)
	}
	if len(misses) != 1 || strings.Join(misses[0].Missing, ",") != "migliaia di miliardi" {
		t.Fatalf("misses = %+v", misses)
	}
}

// Case 13 (29/10/2026): a price found as 1.331,75 € is written 1.332 or ≈1.330;
// a total the model adds up is still not found.
func TestCheckAnswerNumbersRounded(t *testing.T) {
	have := loopNumberSet("Kingston FURY Beast 64GB a € 1.331,75. Kit 2x32 GB: 1.102 $. ASUS RTX 5060 Ti a 539,00 €. SanDisk 202,15 €.")
	answer := "Il kit costa ≈1.332 €, cioè circa 1.330 €; negli USA 1.100 $. GPU a ~540 €, SSD a 202 €. Totale ≈2.300 €; da 1.400 a 1.500 €."
	checked, misses := checkAnswerNumbers(answer, have)
	if checked != 8 {
		t.Errorf("checked = %d, want 8", checked)
	}
	if len(misses) != 2 || strings.Join(misses[0].Missing, ",") != "2300" || strings.Join(misses[1].Missing, ",") != "1400,1500" {
		t.Fatalf("misses = %+v", misses)
	}
	if exact := numberSet("1.331,75"); exact["1332"] || exact["1330"] {
		t.Error("the pipeline's number set must stay exact")
	}
}

// Bench of 06/11/2026: right numbers sent back and listed in the note as not found.
// Rounding goes one way, and what is not a rounding of anything stays missing.
func TestCheckAnswerNumbersShorter(t *testing.T) {
	have := loopNumberSet("La FUA conta 4.981.421 abitanti, il Comune 1.365.698, la città metropolitana 3.250.077.",
		"The weighted mean difference was -19.52 (95% CI -28.13 to -7.33). Il prezzo è 0,276203 €/kWh, un altro 0,0312.")
	for _, tc := range []struct {
		answer  string
		missing string
	}{
		{"Sono 4,98 milioni, cioè 4.981.000.", ""},
		{"Il Comune ha 1,37 milioni di abitanti, l'area 3,25 milioni.", ""},
		{"The difference was about 19.5 (CI 28.1 to 7.3).", ""},
		{"Il PUN è a 0,276 €/kWh, cioè 0,28.", ""},
		{"Costa 0,3 €/kWh.", "0.3"},
		{"Costa 0,03 €/kWh.", "0.03"},
		{"Sono 5 milioni.", "5000000"},
		{"Sono 4,9 milioni.", "4900000"},
		{"Era 19,6.", "19.6"},
		{"Era 19,5234.", "19.5234"},
	} {
		_, misses := checkAnswerNumbers(tc.answer, have)
		got := ""
		for _, m := range misses {
			got += strings.Join(m.Missing, ",")
		}
		if got != tc.missing {
			t.Errorf("%q: missing %q, want %q", tc.answer, got, tc.missing)
		}
	}
	if exact := numberSet("4.981.421 e 19,52"); exact["4980000"] || exact["19.5"] {
		t.Error("the pipeline's number set must stay exact")
	}
}

// A leading zero opens no thousands group, and a dotted date is a date.
func TestNumberFormats(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"0,276 €/kWh", []string{"0.276"}},
		{"0.276", []string{"0.276"}},
		{"telefonisch unter 030 115", []string{"30", "115"}},
		{"1,000 e 1.000 e 1 000", []string{"1000", "1000", "1000"}},
		{"DM 22.12.2023 n. 436", []string{"2023", "436"}},
		{"27.6.2022", []string{"2022"}},
		{"la versione 22.12", []string{"22.12"}},
		{"versione 1.2.3", []string{"1.2", "3"}},
		{"31.13.2022", []string{"31.13", "2022"}},
	} {
		var got []string
		for _, n := range scanNumbers(tc.text) {
			got = append(got, n.alts[0])
		}
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%q: %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestAnchorCitations(t *testing.T) {
	var page strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&page, "Paragraph %d about unrelated self-hosted news and other tools.\n\n", i)
	}
	page.WriteString("DockTail exposes Docker containers as Tailscale Services using labels, with no published ports.\n\n")
	for i := 40; i < 80; i++ {
		fmt.Fprintf(&page, "Paragraph %d about unrelated self-hosted news and other tools.\n\n", i)
	}
	texts := map[string]anchorSource{normalizeLinkURL("https://selfh.st/weekly/2026-10-23/"): {text: page.String(), basis: "page"}}
	answer := "**DockTail** espone i container Docker come Tailscale Services con label ([selfh.st](https://selfh.st/weekly/2026-10-23/)). " +
		"Altro progetto ([anteprima](https://other.test/x)).\n| a | b |"
	anchors := anchorCitations(answer, texts, nil, "")
	if len(anchors) != 1 {
		t.Fatalf("anchors = %+v", anchors)
	}
	a := anchors[0]
	if a.URL != "https://selfh.st/weekly/2026-10-23/" || a.Basis != "page" || strings.Contains(a.Sentence, "https://") {
		t.Errorf("anchor = %+v", a)
	}
	if !strings.Contains(a.Passage, "DockTail exposes Docker containers as Tailscale Services") || len(a.Passage) > anchorPassageChars {
		t.Errorf("passage (%d bytes) misses the DockTail paragraph: %q", len(a.Passage), a.Passage[:min(len(a.Passage), 200)])
	}
}

func TestAnchorClaimsWithoutLinks(t *testing.T) {
	pool := newClaimPool()
	pool.add("https://selfh.st/weekly/2026-10-23/", "page",
		"Intro to this week.\n\nPatchMon is a self-hosted patch management platform for Linux fleets with outbound-only agents.\n\nOther news.")
	pool.add("https://other.test/g", "preview", "Glance\nGlance is a lightweight dashboard in a single binary")
	answer := "## Novità\n" +
		"- **PatchMon** è una piattaforma self-hosted di patch management per flotte Linux, con agenti outbound-only.\n" +
		"*Deduzione, non scritta nelle fonti:* sul tuo N100 PatchMon dovrebbe girare senza problemi di memoria.\n" +
		"Breve frase.\n" +
		"Glance è una dashboard leggera distribuita come singolo binario, comoda da aprire."
	anchors := anchorCitations(answer, nil, pool, "*Deduzione, non scritta nelle fonti:*")
	if len(anchors) != 2 {
		t.Fatalf("anchors = %+v", anchors)
	}
	if anchors[0].Linked || anchors[0].Basis != "page" || !strings.Contains(anchors[0].Passage, "PatchMon is a self-hosted") ||
		strings.HasPrefix(anchors[0].Sentence, "-") || strings.Contains(anchors[0].Sentence, "**") {
		t.Errorf("claim anchor = %+v", anchors[0])
	}
	if anchors[1].Basis != "preview" || anchors[1].URL != "https://other.test/g" {
		t.Errorf("preview anchor = %+v", anchors[1])
	}
}

func TestDeepLoop_SamplingAndKeptReasoning(t *testing.T) {
	sp := &mockSearchProvider{results: []search.Result{{Title: "r", URL: "https://real.test/r", Content: "x"}}}
	reasonThenCall := `{"choices":[{"delta":{"reasoning_content":"Plan: search first, then read."}}]}` + "\n" +
		callTurn("c1", "search", `{"query":"one"}`)
	for _, tc := range []struct {
		name          string
		temperature   string
		keep          bool
		wantTemp      interface{} // nil: not sent
		wantReasoning bool
	}{
		{"recommended", "1.0", true, 1.0, true},
		{"model", "model", false, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &scriptedLLM{turns: []string{reasonThenCall, contentTurn("Risposta. " + prose(30))}}
			srv := httptest.NewServer(http.HandlerFunc(s.handler))
			defer srv.Close()
			engine := NewEngine(sp, &mockReader{}, llm.NewClient(srv.URL, "", "m"), nil)
			engine.SetDeepLoopMaxCalls(40)
			engine.SetDeepLoopSampling(tc.temperature, 0.95, 20, tc.keep)
			if err := engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "q", Mode: "deep"}, func(Event) error { return nil }); err != nil {
				t.Fatal(err)
			}
			b := s.bodies[1]
			if got := b["temperature"]; got != tc.wantTemp {
				t.Errorf("temperature = %v, want %v", got, tc.wantTemp)
			}
			if b["top_p"] != 0.95 || b["top_k"] != 20.0 {
				t.Errorf("top_p/top_k = %v/%v", b["top_p"], b["top_k"])
			}
			var assistant map[string]interface{}
			for _, m := range b["messages"].([]interface{}) {
				if mm := m.(map[string]interface{}); mm["role"] == "assistant" {
					assistant = mm
				}
			}
			_, has := assistant["reasoning_content"]
			if has != tc.wantReasoning || (has && assistant["reasoning_content"] != "Plan: search first, then read.") {
				t.Errorf("assistant message = %v", assistant)
			}
		})
	}
}

func TestReviewDraft(t *testing.T) {
	seen := strings.ToLower("Komodo 1.19 released. Run it with Docker; data in a database. CVE-2026-1")
	draft := "## Komodo e PostgreSQL\n" +
		"Komodo 1.19 ha 987.654 stelle ed è sviluppato da Moghtech con PostgreSQL.\n" +
		"Per installarlo serve Docker, per esempio con compose.\n" +
		"Lo usano anche Immich e Next.js, con API e CVEs, dal 13 August."
	items := reviewDraft(draft, "it", numberSet(seen), seen)
	if len(items) != 2 {
		t.Fatalf("items = %+v, want 2", items)
	}
	if got := fmt.Sprint(items[0].Numbers, items[0].Names); got != "[987654] [Moghtech PostgreSQL]" {
		t.Errorf("first item = %s", got)
	}
	if got := fmt.Sprint(items[1].Names); got != "[Immich Next.js API]" {
		t.Errorf("second item names = %s", got)
	}
}

// Case 14 (29/10/2026): abbreviations do not split a sentence, and a word in
// capitals for emphasis is not a name.
func TestReviewDraftAccents(t *testing.T) {
	seen := "the federal reserve and the great depression. San Nicolas, Museo Principe Felipe. Etats-Unis."
	draft := "Selon la crise, la Réserve fédérale et la Dépression.\n" +
		"La chiesa di San Nicolás e il Museo Príncipe Felipe.\n" +
		"Aux États-Unis et chez Ségolène."
	items := reviewDraft(draft, "fr", numberSet(seen), seen)
	if len(items) != 1 || fmt.Sprint(items[0].Names) != "[Ségolène]" {
		t.Fatalf("items = %+v, want only [Ségolène]", items)
	}
}

func TestReviewDraftLetter(t *testing.T) {
	seen := strings.ToLower("Legge 7 agosto 1990, n. 241. D.Lgs. 14 marzo 2013, n. 33. ATAC gestisce il tram.")
	draft := "Riscontro ai sensi della L. 7 agosto 1990, n. 241, e degli artt. 5 e 12–15 del D.Lgs. 14 marzo 2013, n. 33.\n" +
		"Tanto premesso, CHIEDO che ATAC verifichi.\n" +
		"La sede è a Piazzale Ostiense."
	items := reviewDraft(draft, "it", loopNumberSet(seen), seen)
	if len(items) != 1 || fmt.Sprint(items[0].Names) != "[Piazzale Ostiense]" {
		t.Fatalf("items = %+v, want only [Piazzale Ostiense]", items)
	}
	if got := answerSentences(draft)[0]; !strings.HasSuffix(got, "n. 33.") {
		t.Errorf("first sentence = %q", got)
	}
}

func TestDeepLoop_ReviewSendsDraftBack(t *testing.T) {
	sp := &mockSearchProvider{results: []search.Result{{Title: "Komodo releases", URL: "https://real.test/komodo", Content: "Komodo 1.19 released"}}}
	rd := &mockReader{docs: map[string]reader.Document{"https://real.test/komodo": {Title: "Komodo", URL: "https://real.test/komodo",
		Content: "Komodo 1.19 is the latest release. " + prose(20)}}}
	draft := "L'ultima versione è la 1.19, con 987.654 stelle, mantenuta da Moghtech. " + prose(30)
	final := "L'ultima versione è la 1.19 ([note](https://real.test/komodo)). " + prose(30)
	for _, tc := range []struct {
		name   string
		review bool
	}{{"review", true}, {"off", false}} {
		t.Run(tc.name, func(t *testing.T) {
			s := &scriptedLLM{turns: []string{
				callTurn("c1", "search", `{"query":"komodo release"}`),
				callTurn("c2", "read", `{"url":"https://real.test/komodo"}`),
				contentTurn(draft),
				contentTurn(final),
			}}
			srv := httptest.NewServer(http.HandlerFunc(s.handler))
			defer srv.Close()
			engine := NewEngine(sp, rd, llm.NewClient(srv.URL, "", "m"), nil)
			engine.SetDeepLoopMaxCalls(40)
			engine.SetDeepLoopReview(tc.review)
			var answer, reasoning string
			err := engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "Qual è l'ultima versione di Komodo?", Mode: "deep", AnswerLanguage: "it"},
				func(ev Event) error {
					switch ev.Type {
					case EventToken:
						answer += ev.Data.(string)
					case EventReasoning:
						reasoning += ev.Data.(string)
					}
					return nil
				})
			if err != nil {
				t.Fatal(err)
			}
			if !tc.review {
				if len(s.bodies) != 3 || !strings.Contains(answer, "987.654 stelle") {
					t.Errorf("without review the draft is the answer: requests %d, %q", len(s.bodies), answer[:100])
				}
				return
			}
			if len(s.bodies) != 5 { // the last one asks about the opening of the rewrite
				t.Fatalf("requests = %d, want 5", len(s.bodies))
			}
			if _, ok := s.bodies[3]["tools"]; !ok {
				t.Error("no tools to verify the details sent back")
			}
			role, msg := lastUserOrTool(s.bodies[3])
			if role != "user" || !strings.Contains(msg, "987654") || !strings.Contains(msg, "Moghtech") || strings.Contains(msg, ": 1.19") {
				t.Errorf("sentinel message = %s %q", role, msg)
			}
			if strings.Contains(answer, "987.654") || !strings.Contains(answer, "[note](https://real.test/komodo)") {
				t.Errorf("the answer is not the rewritten one: %q", answer)
			}
			if !strings.Contains(reasoning, "987.654 stelle") {
				t.Error("the draft was not shown while held")
			}
			if !strings.Contains(answer, "Vedetta ha rimandato al modello la bozza: 1 frase aveva") {
				t.Errorf("note of the review missing: %q", answer)
			}
		})
	}
}

func TestPreviewOnly(t *testing.T) {
	everywhere := []ReviewItem{{Sentence: "a", Names: []string{"Moghtech"}}}
	pages := []ReviewItem{{Sentence: "a", Numbers: []string{"987654"}, Names: []string{"Moghtech"}}, {Sentence: "b", Names: []string{"Immich"}}}
	got := previewOnly(everywhere, pages)
	if len(got) != 2 || fmt.Sprint(got[0].Names, got[0].Preview) != "[Moghtech] [987654]" ||
		len(got[1].Names) != 0 || fmt.Sprint(got[1].Preview) != "[Immich]" {
		t.Errorf("previewOnly = %+v", got)
	}
}

// §13 step 2: a detail seen only in a search preview makes Vedetta open the result
// page. When the page holds it the draft goes out; when it does not, the sentence
// goes back with the page's passage.
// Case 19 (05/11/2026): only the line sent back changes. A sentence nobody flagged
// keeps its words, and the model can still read before replying.
func TestDeepLoop_ReviewLines(t *testing.T) {
	sp := &mockSearchProvider{results: []search.Result{{Title: "Komodo releases", URL: "https://real.test/komodo", Content: "Komodo 1.19 released"}}}
	rd := &mockReader{docs: map[string]reader.Document{"https://real.test/komodo": {Title: "Komodo", URL: "https://real.test/komodo",
		Content: "Komodo 1.19 is the latest release, from a team of developers. " + prose(20)}}}
	kept := "Il cosiddetto zeptoverso resta un'idea. " + prose(30)
	draft := "L'ultima versione è la 1.19, con 987.654 stelle ([note](https://real.test/komodo)).\n\n" + kept
	for _, tc := range []struct {
		name, reply, want string
		absent            []string
		how               string
	}{
		{"line", "1: L'ultima versione è la 1.19 ([note](https://real.test/komodo)).", "L'ultima versione è la 1.19 ([note](https://real.test/komodo)).\n\n" + kept,
			[]string{"987.654"}, reviewLinesApplied},
		{"removed", "1: REMOVE", kept, []string{"987.654", "REMOVE"}, reviewLinesApplied},
		{"right as it is", "1: KEEP", draft, []string{"KEEP"}, reviewLinesApplied},
		{"nothing usable", "Ho controllato tutto.", draft, nil, reviewLinesKept},
		{"whole answer anyway", "## Risposta\nLa 1.19.\n\nNuova riga uno, senza il numero.\n\n" + prose(25), "## Risposta\nLa 1.19.", []string{"987.654"}, reviewLinesRewrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &scriptedLLM{turns: []string{
				callTurn("c1", "search", `{"query":"komodo release"}`),
				callTurn("c2", "read", `{"url":"https://real.test/komodo"}`),
				contentTurn(draft),
				callTurn("c3", "search", `{"query":"komodo stars"}`), // still free to verify
				contentTurn(tc.reply),
				contentTurn("1: ANSWER"),
			}}
			srv := httptest.NewServer(http.HandlerFunc(s.handler))
			defer srv.Close()
			engine := NewEngine(sp, rd, llm.NewClient(srv.URL, "", "m"), nil)
			engine.SetDeepLoopMaxCalls(40)
			engine.SetDeepLoopReview(true)
			engine.SetDeepLoopReviewLines(true)
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
			_, msg := lastUserOrTool(s.bodies[3])
			if !strings.Contains(msg, "1: L'ultima versione è la 1.19, con 987.654 stelle ([note](https://real.test/komodo)).") ||
				strings.Contains(msg, "complete answer") || strings.Contains(msg, "2: Il cosiddetto") {
				t.Errorf("sentinel message = %q", msg)
			}
			body := strings.Split(answer, "<!-- vedetta:notes -->")[0]
			if !strings.HasPrefix(body, strings.TrimSpace(tc.want)) {
				t.Errorf("answer = %q, want it to start with %q", body, tc.want)
			}
			if tc.how != reviewLinesRewrite && !strings.Contains(body, strings.TrimSpace(kept)) {
				t.Errorf("the line nobody flagged changed: %q", body)
			}
			for _, a := range tc.absent {
				if strings.Contains(body, a) {
					t.Errorf("%q still in the answer: %q", a, body)
				}
			}
			first, _, _ := strings.Cut(draft, "\n")
			if _, _, how := applyReviewLines(draft, []LanguageSlip{{Line: first}}, tc.reply); how != tc.how {
				t.Errorf("outcome = %s, want %s", how, tc.how)
			}
		})
	}
}

// The sentences sent back are cleaned of addresses and list marks; the lines are not.
func TestReviewLines(t *testing.T) {
	draft := "# Titolo\n\n- **La 1.19** ha 987.654 stelle ([note](https://a.test/x)). Costa poco.\n\nLa pagina dice altro ([b](https://b.test/y)).\n\nRiga buona.\n\n- **La 1.19** ha 987.654 stelle ([note](https://a.test/x)). Costa poco."
	items := reviewDraft(draft, "it", numberSet("1.19"), "1.19 note b altro pagina")
	if len(items) == 0 {
		t.Fatal("no sentence flagged")
	}
	claim := linkClaims("La pagina dice altro ([b](https://b.test/y)).")[0].Text
	lines := reviewLines(draft, items, []LinkTrouble{{Sentence: claim, URL: "https://b.test/y"}})
	if len(lines) != 2 || lines[0].Line != "- **La 1.19** ha 987.654 stelle ([note](https://a.test/x)). Costa poco." ||
		lines[1].Line != "La pagina dice altro ([b](https://b.test/y))." {
		t.Errorf("lines = %+v", lines)
	}
	if got := reviewLines(draft, nil, nil); got != nil {
		t.Errorf("nothing flagged, lines = %+v", got)
	}
}

// A line of code is not sent back, and neither is a sentence that is only in code; the
// same sentence also written outside the block is.
func TestReviewLinesCode(t *testing.T) {
	onlyCode := "PRAGMA busy_timeout=987654;"
	both := "La soglia è 123.456."
	draft := "Testo con 555.555 stelle.\n\n  ```sql\n  " + onlyCode + "\n  " + both + "\n  ```\n\n" + both
	if items := reviewDraft(draft, "it", numberSet(""), ""); len(items) != 4 {
		t.Fatalf("items = %+v, want four: the check itself sees code", items)
	}
	items := []ReviewItem{{Sentence: "Testo con 555.555 stelle."}, {Sentence: onlyCode}, {Sentence: both}}
	kept := outsideCode(draft, items)
	if len(kept) != 2 || kept[0].Sentence != "Testo con 555.555 stelle." || kept[1].Sentence != both {
		t.Errorf("outside code = %+v", kept)
	}
	lines := reviewLines(draft, items, nil)
	if len(lines) != 2 || lines[0].Line != "Testo con 555.555 stelle." || lines[1].Line != both {
		t.Errorf("lines = %+v", lines)
	}
}

// With only code flagged the answer is not sent back; rewriting it whole still is.
func TestDeepLoop_ReviewLinesOnlyCode(t *testing.T) {
	sp := &mockSearchProvider{results: []search.Result{{Title: "SQLite", URL: "https://real.test/wal", Content: "SQLite WAL mode"}}}
	rd := &mockReader{docs: map[string]reader.Document{"https://real.test/wal": {Title: "WAL", URL: "https://real.test/wal",
		Content: "SQLite WAL mode lets readers and writers work together. " + prose(20)}}}
	draft := "Attiva WAL ([note](https://real.test/wal)).\n\n```sql\nPRAGMA busy_timeout=987654;\n```\n\n" + prose(30)
	for _, tc := range []struct {
		name     string
		lines    bool
		requests int
	}{{"lines", true, 3}, {"whole answer", false, 4}} {
		t.Run(tc.name, func(t *testing.T) {
			s := &scriptedLLM{turns: []string{
				callTurn("c1", "search", `{"query":"sqlite wal"}`),
				callTurn("c2", "read", `{"url":"https://real.test/wal"}`),
				contentTurn(draft),
				contentTurn(draft),
			}}
			srv := httptest.NewServer(http.HandlerFunc(s.handler))
			defer srv.Close()
			engine := NewEngine(sp, rd, llm.NewClient(srv.URL, "", "m"), nil)
			engine.SetDeepLoopMaxCalls(40)
			engine.SetDeepLoopReview(true)
			engine.SetDeepLoopReviewLines(tc.lines)
			var answer string
			err := engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "Come attivo WAL su SQLite?", Mode: "deep", AnswerLanguage: "it"},
				func(ev Event) error {
					if ev.Type == EventToken {
						answer += ev.Data.(string)
					}
					return nil
				})
			if err != nil {
				t.Fatal(err)
			}
			if len(s.bodies) != tc.requests {
				t.Errorf("requests = %d, want %d", len(s.bodies), tc.requests)
			}
			if !strings.Contains(answer, "PRAGMA busy_timeout=987654;") {
				t.Errorf("the code changed: %q", answer)
			}
		})
	}
}

func TestApplyReviewLines(t *testing.T) {
	draft := "# Titolo\n\n- Prima **riga** ([a](https://a.test/x)).\n\nSeconda riga.\n\n| A | B |\n|---|---|\n| uno | 2 |\n\nTerza riga."
	lines := func() []LanguageSlip {
		return []LanguageSlip{{Line: "- Prima **riga** ([a](https://a.test/x))."}, {Line: "| uno | 2 |"}}
	}
	for _, tc := range []struct{ name, reply, want, how string }{
		{"both, the list mark kept", "Ecco le righe:\n1: Prima riga corretta ([a](https://a.test/x)).\n2: | uno | 3 |",
			"# Titolo\n\n- Prima riga corretta ([a](https://a.test/x)).\n\nSeconda riga.\n\n| A | B |\n|---|---|\n| uno | 3 |\n\nTerza riga.", reviewLinesApplied},
		{"one missing stays", "2: | uno | 3 |",
			"# Titolo\n\n- Prima **riga** ([a](https://a.test/x)).\n\nSeconda riga.\n\n| A | B |\n|---|---|\n| uno | 3 |\n\nTerza riga.", reviewLinesApplied},
		{"removed, blank lines close up", "1: **REMOVE**", "# Titolo\n\nSeconda riga.\n\n| A | B |\n|---|---|\n| uno | 2 |\n\nTerza riga.", reviewLinesApplied},
		{"removed, with a reason", "1: REMOVE, non verificato", "# Titolo\n\nSeconda riga.\n\n| A | B |\n|---|---|\n| uno | 2 |\n\nTerza riga.", reviewLinesApplied},
		{"kept: nothing changes", "1: KEEP\n2: keep", draft, reviewLinesApplied},
		{"kept: the reason goes nowhere", "1: KEEP (arrotondamento di 19,52)\n2: | uno | 3 |",
			"# Titolo\n\n- Prima **riga** ([a](https://a.test/x)).\n\nSeconda riga.\n\n| A | B |\n|---|---|\n| uno | 3 |\n\nTerza riga.", reviewLinesApplied},
		{"a line that starts with the word is a line", "1: Keep it short.",
			"# Titolo\n\n- Keep it short.\n\nSeconda riga.\n\n| A | B |\n|---|---|\n| uno | 2 |\n\nTerza riga.", reviewLinesApplied},
		{"a table row with fewer columns stays", "2: | uno e due |", draft, reviewLinesKept},
		{"numbers out of range", "7: Altro.", draft, reviewLinesKept},
		{"empty reply", "", draft, reviewLinesKept},
		{"a list longer than the lines asked is a whole answer", "1. Prima.\n2. Seconda.\n3. Terza riga lunga quanto basta per contare.\n4. Quarta riga anche lei abbastanza lunga.",
			"1. Prima.\n2. Seconda.\n3. Terza riga lunga quanto basta per contare.\n4. Quarta riga anche lei abbastanza lunga.", reviewLinesRewrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _, how := applyReviewLines(draft, lines(), tc.reply)
			if out != tc.want || how != tc.how {
				t.Errorf("got %s %q\nwant %s %q", how, out, tc.how, tc.want)
			}
		})
	}
}

// A reply cut in the middle: the finished lines go in, the cut one does not, and the
// draft is not sent to be continued.
func TestDeepLoop_ReviewLinesReplyCut(t *testing.T) {
	sp := &mockSearchProvider{results: []search.Result{{Title: "Komodo", URL: "https://real.test/komodo", Content: "Komodo 1.19 released"}}}
	rd := &mockReader{docs: map[string]reader.Document{"https://real.test/komodo": {Title: "Komodo", URL: "https://real.test/komodo",
		Content: "Komodo 1.19 is the latest release. " + prose(20)}}}
	draft := "La 1.19 ha 987.654 stelle.\n\nÈ mantenuta da 123.456 persone.\n\n" + prose(30)
	cut := `{"choices":[{"delta":{"content":"1: La 1.19 è l'ultima.\n2: È mantenuta da"},"finish_reason":"length"}]}`
	s := &scriptedLLM{turns: []string{
		callTurn("c1", "search", `{"query":"komodo"}`),
		callTurn("c2", "read", `{"url":"https://real.test/komodo"}`),
		contentTurn(draft),
		cut,
	}}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()
	engine := NewEngine(sp, rd, llm.NewClient(srv.URL, "", "m"), nil)
	engine.SetDeepLoopMaxCalls(40)
	engine.SetDeepLoopReview(true)
	engine.SetDeepLoopReviewLines(true)
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
	if len(s.bodies) != 4 {
		t.Errorf("requests = %d, want 4: nothing to continue", len(s.bodies))
	}
	if !strings.Contains(answer, "La 1.19 è l'ultima.") || strings.Contains(answer, "987.654 stelle.\n") ||
		!strings.Contains(answer, "È mantenuta da 123.456 persone.") {
		t.Errorf("answer = %q", answer)
	}
}

func TestDeepLoop_ReviewPreviews(t *testing.T) {
	sp := &mockSearchProvider{results: []search.Result{
		{Title: "Komodo releases", URL: "https://real.test/komodo", Content: "Komodo 1.19 released"},
		{Title: "Komodo stars", URL: "https://stars.test/komodo", Content: "Komodo has 987.654 stars on GitHub"},
	}}
	draft := "L'ultima versione è la 1.19, con 987.654 stelle su GitHub. " + prose(30)
	final := "L'ultima versione è la 1.19 ([note](https://real.test/komodo)). " + prose(30)
	for _, tc := range []struct {
		name, stars string
		back        bool
	}{{"page holds it", "Komodo has 987.654 stars on GitHub and many forks.", false}, {"page lacks it", "Komodo is popular on GitHub with many forks.", true}} {
		t.Run(tc.name, func(t *testing.T) {
			rd := &mockReader{docs: map[string]reader.Document{
				"https://real.test/komodo":  {Title: "Komodo", URL: "https://real.test/komodo", Content: "Komodo 1.19 is the latest release. " + prose(20)},
				"https://stars.test/komodo": {Title: "Stars", URL: "https://stars.test/komodo", Content: tc.stars + " " + prose(20)},
			}}
			s := &scriptedLLM{turns: []string{
				callTurn("c1", "search", `{"query":"komodo release"}`),
				callTurn("c2", "read", `{"url":"https://real.test/komodo"}`),
				contentTurn(draft),
				contentTurn(final),
			}}
			srv := httptest.NewServer(http.HandlerFunc(s.handler))
			defer srv.Close()
			engine := NewEngine(sp, rd, llm.NewClient(srv.URL, "", "m"), nil)
			engine.SetDeepLoopMaxCalls(40)
			engine.SetDeepLoopReview(true)
			engine.SetDeepLoopReviewPreviews(true)
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
			if !tc.back {
				if len(s.bodies) != 3 || !strings.Contains(answer, "987.654 stelle") {
					t.Errorf("a detail the page holds is not sent back: requests %d, %q", len(s.bodies), answer[:100])
				}
				return
			}
			if len(s.bodies) != 5 { // the last one asks about the opening of the rewrite
				t.Fatalf("requests = %d, want 5", len(s.bodies))
			}
			_, msg := lastUserOrTool(s.bodies[3])
			for _, want := range []string{"only in the short previews", "987654", "https://stars.test/komodo",
				"The page does not contain: 987654", "popular on GitHub"} {
				if !strings.Contains(msg, want) {
					t.Errorf("sentinel message lacks %q: %q", want, msg)
				}
			}
			if strings.Contains(msg, "do not appear in any of them") {
				t.Errorf("a preview detail is reported as missing everywhere: %q", msg)
			}
		})
	}
}

// Case Eiffel (30/10/2026): an answer written without any research is checked
// against the question alone. A greeting goes out; names and numbers from memory
// go back with the whole research budget.
func TestDeepLoop_ReviewWithoutResearch(t *testing.T) {
	sp := &mockSearchProvider{results: []search.Result{{Title: "Tour Eiffel", URL: "https://real.test/eiffel", Content: "330 m"}}}
	rd := &mockReader{docs: map[string]reader.Document{}}
	for _, tc := range []struct {
		name, draft string
		back        bool
	}{
		{"greeting", "Ciao! Sto bene, grazie: dimmi pure cosa vuoi cercare oggi e mi metto al lavoro.", false},
		{"from memory", "La torre è alta 330 metri dal 2022 ed è stata progettata da Maurice Koechlin. " + prose(20), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &scriptedLLM{turns: []string{contentTurn(tc.draft), contentTurn("La torre è alta 330 metri. " + prose(20))}}
			srv := httptest.NewServer(http.HandlerFunc(s.handler))
			defer srv.Close()
			engine := NewEngine(sp, rd, llm.NewClient(srv.URL, "", "m"), nil)
			engine.SetDeepLoopMaxCalls(40)
			engine.SetDeepLoopReview(true)
			err := engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "Quanto è alta la Torre Eiffel?", Mode: "deep", AnswerLanguage: "it"},
				func(Event) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if !tc.back {
				if len(s.bodies) != 1 {
					t.Errorf("a greeting is not sent back: requests %d", len(s.bodies))
				}
				return
			}
			if len(s.bodies) != 3 { // the last one asks about the opening of the rewrite
				t.Fatalf("requests = %d, want 3", len(s.bodies))
			}
			if _, ok := s.bodies[1]["tools"]; !ok {
				t.Error("no tools to research after the review")
			}
			_, msg := lastUserOrTool(s.bodies[1])
			for _, want := range []string{"without searching or reading", "Koechlin", "330"} {
				if !strings.Contains(msg, want) {
					t.Errorf("sentinel message lacks %q: %q", want, msg)
				}
			}
		})
	}
}

// A page the model tried to read and could not is not a page read, and a link to it
// counts as unread, however many times the answer repeats it.
func TestCitedPages_ReadAndUnread(t *testing.T) {
	l := &deepLoop{pages: map[string]*loopPage{normalizeLinkURL("https://www.real.test/a/"): {URL: "https://real.test/a"}}}
	answer := "Vedi [a](https://real.test/a), ancora [a](https://www.real.test/a/), poi [b](https://real.test/b), [b](https://real.test/b) e [https://real.test/c]."
	if read, unread := l.citedPages(answer); read != 1 || unread != 2 {
		t.Errorf("read=%d unread=%d, want 1 and 2", read, unread)
	}
}

func TestLoopSystemPrompt_CiteRead(t *testing.T) {
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	off, on := loopSystemPrompt("it", now, false, false), loopSystemPrompt("it", now, false, true)
	if !strings.Contains(off, "the pages you read or the search results you saw") {
		t.Errorf("switch off changed the instruction: %q", off)
	}
	if strings.Contains(on, "search results you saw") || !strings.Contains(on, "A result you did not open is not a source") {
		t.Errorf("switch on: %q", on)
	}
	for _, p := range []string{off, on} {
		if !strings.Contains(p, "Use only URLs that appear in your tool results.") {
			t.Errorf("the rule against invented links is gone: %q", p)
		}
	}
}
