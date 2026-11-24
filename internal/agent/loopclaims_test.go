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

// Sentences from real answers (04/11/2026) and ones that only look like them.
func TestWorkClaimLines(t *testing.T) {
	claims := []string{
		"Nelle banche dati e nei siti ufficiali che ho consultato — Normattiva, l'USPTO — non compare.",
		"Ho eseguito ricerche mirate su fonti istituzionali (Normattiva, EPO).",
		"Ho effettuato ricerche sul web e nelle principali banche dati.",
		"Ho incrociato le opere di Kohut con la letteratura.",
		"Ricostruzione basata sui testi di Kohut.",
		"L'ho incrociata con le liste comparative più recenti.",
		"Nelle banche dati brevettuali consultate (USPTO, WIPO) non emerge nulla.",
		"## Verifica effettuata: non esiste",
		"Le pagine consultate confermano una sola accezione.",
		"Ho interrogato le fonti primarie competenti: Normattiva, UIBM.",
		"Ho compiuto ricerche mirate sul portale Normattiva.",
		"## Verifica su fonti ufficiali: non esiste",
		"Ho ricontrollato il dato.",
		"Le ricerche sulle banche dati pubbliche degli uffici brevetti non producono nulla.",
		"Le interrogazioni alle banche dati normative non restituiscono alcun risultato.",
		"Analogamente, nelle banche dati brevettuali (Espacenet, EPO) non ho trovato alcun documento.",
		"Everything below is based on the official documentation.",
		"I checked the EPO and WIPO registers.",
		"I've read the original paper.",
		"Ich habe die Datenbanken des EPA geprüft.",
		"J'ai vérifié les registres.",
		"He consultado las bases de datos.",
	}
	for _, c := range claims {
		if got := workClaimLines(c); len(got) != 1 {
			t.Errorf("not found: %q", c)
		}
	}
	others := []string{
		"Non ho trovato un link GitHub.",
		"Non ho letto il testo originale.",
		"Le fonti verificate riportano 12 casi.",
		"Ho comprato un disco nuovo.",
		"Ho lettere da spedire.",
		"La leaderboard è basata su ELO.",
		"The estimate is based on two trials.",
		"He read the paper and I agree.",
		"Il comando `ho letto` stampa il file.",
		"Vedi [nota](https://x.test/ho-consultato).",
		"```\nho letto\n```",
	}
	for _, o := range others {
		if got := workClaimLines(o); len(got) != 0 {
			t.Errorf("found in %q: %+v", o, got)
		}
	}
	if got := workClaimLines("Il titolo [Ho letto tutto](https://x.test/a)."); len(got) != 1 {
		t.Errorf("the words of a link count: %+v", got)
	}
}

func TestWorkClaimFits(t *testing.T) {
	long := "Nelle banche dati che ho consultato — Normattiva, MIMIT/UIBM, USPTO, EPO, Companies House — non compare nulla."
	if !workClaimFits(long, "Le ricerche fatte non hanno trovato traccia.") {
		t.Error("dropping the list of sources is a rewrite")
	}
	if workClaimFits(long, "ok") {
		t.Error("a reply that leaves nothing is not a rewrite")
	}
	if !workClaimFits("Ho letto Kohut.", "Ho letto pagine su Kohut, non i suoi testi, che non ho potuto aprire.") {
		t.Error("saying what was done may take more words")
	}
}

// The line goes out alone with what was really done, and comes back corrected; the
// reader is told. A true line stays, and then the reader is told nothing.
func TestDeepLoop_WorkClaims(t *testing.T) {
	const page = "https://real.test/komodo"
	claim := "Nelle banche dati che ho consultato, Normattiva ed EPO, Komodo non compare."
	for _, tc := range []struct {
		name, reply, want string
		note              bool
	}{
		{"corrected", "1: Le ricerche fatte non hanno trovato traccia di Komodo in Normattiva o EPO.", "Le ricerche fatte non hanno trovato traccia", true},
		{"already true", "1: " + claim, claim, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := &mockSearchProvider{results: []search.Result{{Title: "Komodo", URL: page, Content: "Komodo, Normattiva, EPO"}}}
			rd := &mockReader{docs: map[string]reader.Document{page: {Title: "Pagina Komodo", URL: page, Content: "Komodo, Normattiva, EPO. " + prose(20)}}}
			s := &scriptedLLM{turns: []string{
				callTurn("c1", "search", `{"query":"komodo normativa"}`),
				callTurn("c2", "read", `{"url":"`+page+`"}`),
				contentTurn(prose(30) + " ([nota](" + page + "))\n\n" + claim),
				contentTurn(tc.reply),
			}}
			srv := httptest.NewServer(http.HandlerFunc(s.handler))
			defer srv.Close()
			engine := NewEngine(sp, rd, llm.NewClient(srv.URL, "", "m"), nil)
			engine.SetDeepLoopMaxCalls(40)
			engine.SetDeepLoopReview(true)
			var answer string
			err := engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "Komodo è normato?", Mode: "deep", AnswerLanguage: "it"},
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
				t.Fatalf("requests = %d, want 4", len(s.bodies))
			}
			_, msg := lastUserOrTool(s.bodies[3])
			for _, want := range []string{"1: " + claim, "komodo normativa", "Pagina Komodo", page} {
				if !strings.Contains(msg, want) {
					t.Errorf("message lacks %q: %q", want, msg)
				}
			}
			if !strings.Contains(answer, tc.want) {
				t.Errorf("answer = %q", answer)
			}
			if got := strings.Contains(answer, "ha fatto correggere 1 frase"); got != tc.note {
				t.Errorf("note = %v, want %v: %q", got, tc.note, answer)
			}
		})
	}
}

// DL5.2: the request says first that a true line is copied, and no longer pairs
// "rewrite" with "change nothing else".
func TestWorkClaimsMessage_TrueLineFirst(t *testing.T) {
	done := "Web searches made through a search engine, which return only titles and short previews: 1.\n- superbonus 2026\n" +
		"Pages opened and read, and no others:\n- Il Tuo Perito (https://iltuoperito.it/superbonus)\n"
	msg := workClaimsMessage([]LanguageSlip{{Line: "**Fonti consultate**"}, {Line: "Ho verificato la circolare."}}, done, "it")
	for _, want := range []string{
		"written in Italian", "may claim more than was done", "What was really done:\n" + done,
		"- if it is true, copy it unchanged;", "replace only that claim with what did happen",
		"1: **Fonti consultate**\n2: Ho verificato la circolare.\n",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message misses %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "change nothing else") || strings.Contains(msg, "Add no fact") {
		t.Errorf("message keeps the contradiction:\n%s", msg)
	}
	if strings.Index(msg, "copy it unchanged") > strings.Index(msg, "replace only that claim") {
		t.Error("the true-line rule does not come first")
	}
}
