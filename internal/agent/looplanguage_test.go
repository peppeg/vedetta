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

// Case 03/11/2026: the three slips of the day, and words that are not slips.
func TestLanguageSlips(t *testing.T) {
	answer := "Riduce i chunk oltre il 90% riducendo存储/embedding cost.\n" +
		"- I requisiti indicati dai développeurs sono 16 GB.\n" +
		"Le guide consigliano diConvertire in testo pulito prima.\n" +
		"Perché la città è più grande? Su iPhone e macOS, da Pérez a Zürich, con laTeX e unRAID.\n" +
		"Il titolo [Recherche guidée](https://x.test/a) e il codice `print(\"你好\")`.\n" +
		"```\nprint(\"世界\")\n```\n" +
		"Il sito francese parla di « façade » come la fonte."
	seen := []string{"La page parle de la façade du bâtiment."}
	slips := languageSlips(answer, "it", seen)
	var got []string
	for _, s := range slips {
		got = append(got, strings.Join(s.Words, ","))
	}
	if strings.Join(got, " | ") != "存储 | développeurs | diConvertire" {
		t.Errorf("slips = %q", got)
	}
	if s := languageSlips("Le 存储 est ici.", "fr", nil); len(s) != 1 {
		t.Errorf("another script in French not found: %+v", s)
	}
	if s := languageSlips("développeurs", "fr", nil); len(s) != 0 {
		t.Errorf("French accents are not slips in French: %+v", s)
	}
}

func TestApplyLanguageFixes(t *testing.T) {
	answer := "Titolo\n- I requisiti indicati dai développeurs sono 16 GB.\nAltro.\nLe guide consigliano diConvertire prima."
	slips := languageSlips(answer, "it", nil)
	reply := "Ecco:\n1: I requisiti indicati dagli sviluppatori sono 16 GB.\n2: x"
	out, slips := applyLanguageFixes(answer, slips, reply)
	if !strings.Contains(out, "\n- I requisiti indicati dagli sviluppatori sono 16 GB.\n") {
		t.Errorf("line not replaced, or its list marker lost: %q", out)
	}
	if !strings.Contains(out, "diConvertire") || slips[1].Fixed != "" {
		t.Errorf("a reply far from the line's length must not replace it: %q", out)
	}
}

// The rewrite after the review is held and checked: a line with a slip goes back
// alone and comes out rewritten.
func TestDeepLoop_LanguageCheckOfRewrite(t *testing.T) {
	sp := &mockSearchProvider{results: []search.Result{{Title: "Komodo releases", URL: "https://real.test/komodo", Content: "Komodo 1.19 released"}}}
	rd := &mockReader{docs: map[string]reader.Document{"https://real.test/komodo": {Title: "Komodo", URL: "https://real.test/komodo",
		Content: "Komodo 1.19 is the latest release. " + prose(20)}}}
	draft := "L'ultima versione è la 1.19, mantenuta da Moghtech. " + prose(30)
	final := "L'ultima versione è la 1.19 ([note](https://real.test/komodo)). " + prose(30) + "\n\nRiduce il存储 dei dati."
	s := &scriptedLLM{turns: []string{
		callTurn("c1", "search", `{"query":"komodo release"}`),
		callTurn("c2", "read", `{"url":"https://real.test/komodo"}`),
		contentTurn(draft),
		contentTurn(final),
		contentTurn("1: ANSWER\n2: ANSWER"),
		contentTurn("1: Riduce lo spazio occupato dai dati."),
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
	if len(s.bodies) != 6 {
		t.Fatalf("requests = %d, want 6", len(s.bodies))
	}
	role, msg := lastUserOrTool(s.bodies[5])
	if role != "user" || !strings.Contains(msg, "1: Riduce il存储 dei dati.") || strings.Contains(msg, "Komodo") {
		t.Errorf("language check message = %s %q", role, msg)
	}
	if strings.Contains(answer, "存储") || !strings.Contains(answer, "Riduce lo spazio occupato dai dati.") ||
		!strings.Contains(answer, "[note](https://real.test/komodo)") {
		t.Errorf("answer = %q", answer)
	}
}
