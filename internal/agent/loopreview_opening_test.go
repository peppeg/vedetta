package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vedetta/internal/llm"
	"vedetta/internal/reader"
	"vedetta/internal/search"
)

func TestOpeningParagraphs(t *testing.T) {
	draft := "Già nella bozza.\n\n## Titolo\n\nTesto."
	for name, c := range map[string]struct {
		text string
		want []string
	}{
		"heading first":   {"# Titolo\n\nTesto.", nil},
		"rule stops":      {"Grazie.\n\n---\n\n# Titolo", []string{"Grazie."}},
		"heading stops":   {"Uno.\n\nDue.\n\n## Titolo", []string{"Uno.", "Due."}},
		"table stops":     {"Uno.\n\n| a | b |\n|---|---|", []string{"Uno."}},
		"code stops":      {"Uno.\n\n```sh\nls\n```", []string{"Uno."}},
		"draft stops":     {"Nuovo.\n\nGià nella bozza.\n\nAltro.", []string{"Nuovo."}},
		"same as draft":   {"Già nella bozza.\n\n## Titolo\n\nTesto.", nil},
		"blank with tabs": {"Uno.\n \t\nDue.", []string{"Uno.", "Due."}},
		"at most six":     {"1\n\n2\n\n3\n\n4\n\n5\n\n6\n\n7", []string{"1", "2", "3", "4", "5", "6"}},
	} {
		got := openingParagraphs(c.text, draft)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

func TestCheckTalkCount(t *testing.T) {
	for reply, want := range map[string]int{
		"1: CHECK\n2: CHECK\n3: ANSWER": 2,
		"1: ANSWER\n2: CHECK":           0, // only from the top: a later paragraph is in the answer
		"**1**: check\n2. Check":        2,
		"1: CHECK\n3: CHECK":            1, // a gap ends the count
		"1: CHECK\n2: CHECK\n3: CHECK":  2, // no more than the paragraphs asked about
		"non saprei":                    0,
		"":                              0,
	} {
		if got := checkTalkCount(reply, 2); got != want {
			t.Errorf("%q: got %d, want %d", reply, got, want)
		}
	}
}

func TestCutOpening(t *testing.T) {
	body := strings.TrimSpace("# Titolo\n\n" + prose(40))
	if got := cutOpening("Grazie.\n\n---\n\n"+body, []string{"Grazie."}); got != body {
		t.Errorf("rule stays: %q", got)
	}
	if got := cutOpening("Uno.\n\nDue.\n\n"+body+"\n", []string{"Uno.", "Due."}); got != body {
		t.Errorf("got %q", got)
	}
	short := "Una frase lunga che qualcuno ha giudicato come parte del controllo, e invece era la risposta.\n\nFine."
	if got := cutOpening(short, []string{"Una frase lunga che qualcuno ha giudicato come parte del controllo, e invece era la risposta."}); got != short {
		t.Errorf("most of the answer went: %q", got)
	}
	if text := "Altro.\n\n" + body; cutOpening(text, []string{"Grazie."}) != text {
		t.Error("a paragraph that is not there cut something")
	}
}

// reviewedRun writes a draft that is sent back, then the rewrite, then replies to what
// follows. It returns the requests made and the answer.
func reviewedRun(t *testing.T, rewrite string, after ...string) (*scriptedLLM, string) {
	t.Helper()
	sp := &mockSearchProvider{results: []search.Result{{Title: "Komodo releases", URL: "https://real.test/komodo", Content: "Komodo 1.19 released"}}}
	rd := &mockReader{docs: map[string]reader.Document{"https://real.test/komodo": {Title: "Komodo", URL: "https://real.test/komodo",
		Content: "Komodo 1.19 is the latest release. " + prose(20)}}}
	s := &scriptedLLM{turns: append([]string{
		callTurn("c1", "search", `{"query":"komodo release"}`),
		callTurn("c2", "read", `{"url":"https://real.test/komodo"}`),
		contentTurn("L'ultima versione è la 1.19, con 987.654 stelle. " + prose(30)),
		contentTurn(rewrite),
	}, after...)}
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
	return s, answer
}

func TestDeepLoop_OpeningThatTalksToTheCheck(t *testing.T) {
	body := "# Komodo\n\nL'ultima versione è la 1.19 ([note](https://real.test/komodo)). " + prose(30)
	opening := "Grazie del riscontro. Verifico i due punti.\n\nEcco la risposta completa corretta."

	t.Run("cut", func(t *testing.T) {
		s, answer := reviewedRun(t, opening+"\n\n---\n\n"+body, contentTurn("1: CHECK\n2: CHECK"))
		if len(s.bodies) != 5 {
			t.Fatalf("requests = %d, want 5", len(s.bodies))
		}
		role, msg := lastUserOrTool(s.bodies[4])
		if role != "user" || !strings.Contains(msg, "2. Ecco la risposta completa corretta.") || strings.Contains(msg, "# Komodo") {
			t.Errorf("question = %s %q", role, msg)
		}
		if _, ok := s.bodies[4]["tools"]; ok {
			t.Error("the question offers tools")
		}
		if s.bodies[4]["temperature"] != float64(0) {
			t.Errorf("temperature = %v", s.bodies[4]["temperature"])
		}
		if strings.Contains(answer, "Grazie") || strings.Contains(answer, "corretta") {
			t.Errorf("the opening stayed: %q", answer)
		}
		if !strings.HasPrefix(strings.TrimSpace(answer), "# Komodo") {
			t.Errorf("the answer does not start at its title: %q", answer[:40])
		}
	})
	t.Run("the answer stays", func(t *testing.T) {
		text := "Ho fatto diverse ricerche e non ho trovato traccia del brevetto.\n\n" + body
		_, answer := reviewedRun(t, text, contentTurn("1: ANSWER"))
		if !strings.Contains(answer, "Ho fatto diverse ricerche") {
			t.Errorf("a paragraph of the answer went: %q", answer)
		}
	})
	t.Run("an unclear reply changes nothing", func(t *testing.T) {
		_, answer := reviewedRun(t, opening+"\n\n"+body, contentTurn("non saprei"))
		if !strings.Contains(answer, "Grazie del riscontro") {
			t.Errorf("cut without a reply: %q", answer)
		}
	})
	t.Run("no question when the rewrite starts with a heading", func(t *testing.T) {
		s, _ := reviewedRun(t, body)
		if len(s.bodies) != 4 {
			t.Errorf("requests = %d, want 4", len(s.bodies))
		}
	})
}
