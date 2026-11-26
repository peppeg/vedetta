package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vedetta/internal/llm"
	"vedetta/internal/reader"
	"vedetta/internal/search"
)

// Case 26 (26/11/2026): a name sent back that the model read spelled almost the same
// is shown to the model with the spelling of the sources. Translations are shown too
// (the model judges); words without a capital and short names are not compared.
func TestNearSpellings(t *testing.T) {
	seen := "ALLEGATO A — domande ammesse: BESIDETECH SRL, SIRINFO, GIPSTECH, Bestidotech Lab.\n" +
		"The stack runs in Brazil and in Tajikistan. Sky, Skye.\n" +
		"https://www.Besidetech.example/chi-siamo"
	got := nearSpellings([]string{"Besidotech", "Slack", "Brasile", "Tagikistan", "Sly", "Sirinfo", "Besidotech"}, seen) // a name in two sentences
	want := map[string]string{
		"Besidotech": "[BESIDETECH Bestidotech]", // one letter apart first
		"Brasile":    "[Brazil]",
		"Tagikistan": "[Tajikistan]",
	}
	if len(got) != len(want) {
		t.Errorf("nearSpellings = %v, want %v", got, want)
	}
	for n, w := range want {
		if fmt.Sprint(got[n]) != w {
			t.Errorf("%s: %v, want %s", n, got[n], w)
		}
	}
	if nearSpellings(nil, seen) != nil {
		t.Error("no names, no spellings")
	}
}

func TestEditDistance(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		limit int
		want  int
	}{
		{"besidotech", "besidetech", 2, 1},
		{"brasile", "brazil", 2, 2},
		{"notaioonline", "notaionline", 2, 1},
		{"slack", "stack", 1, 1},
		{"commissione", "commission", 2, 1},
		{"sirinfo", "gipstech", 2, 3}, // more than the limit
		{"abc", "abcdef", 2, 3},
	} {
		if got := editDistance([]rune(tc.a), []rune(tc.b), tc.limit); got != tc.want {
			t.Errorf("editDistance(%s, %s, %d) = %d, want %d", tc.a, tc.b, tc.limit, got, tc.want)
		}
	}
}

func TestReviewMessage_Spellings(t *testing.T) {
	items := []ReviewItem{{Sentence: "Tra le ammesse c'è Besidotech, con Sirinfo.", Names: []string{"Besidotech", "Sirinfu"},
		Spellings: map[string][]string{"Besidotech": {"BESIDETECH"}}}}
	msg := reviewMessage(items, nil, "it", true, nil)
	if !strings.Contains(msg, ": Besidotech (the sources write: BESIDETECH), Sirinfu\n") {
		t.Errorf("spelling not next to the name:\n%s", msg)
	}
	if !strings.Contains(msg, "If your name is a misspelling of it, write it as the sources do.") {
		t.Errorf("no word on what to do with it:\n%s", msg)
	}
	items[0].Spellings = nil
	if msg := reviewMessage(items, nil, "it", true, nil); strings.Contains(msg, "the sources write") || strings.Contains(msg, "misspelling") {
		t.Errorf("without spellings the message is as before:\n%s", msg)
	}
}

// The whole road: the page read writes BESIDETECH, the draft Besidotech; the
// sentinel's message shows the page's spelling, and only for that name.
func TestDeepLoop_ReviewShowsSpelling(t *testing.T) {
	const url = "https://real.test/decreto"
	sp := &mockSearchProvider{results: []search.Result{{Title: "Decreto STEP", URL: url, Content: "Allegato A, domande ammesse"}}}
	rd := &mockReader{docs: map[string]reader.Document{url: {Title: "Decreto", URL: url,
		Content: "Allegato A, domande ammesse: BESIDETECH SRL, SIRINFO SRL, INTELLIDIO SRL. " + prose(20)}}}
	draft := "Tra le ammesse ci sono Besidotech, Sirinfo e Intellidio, e anche Zanzibarco. " + prose(30)
	final := "Tra le ammesse ci sono Besidetech, Sirinfo e Intellidio. " + prose(30)
	s := &scriptedLLM{turns: []string{
		callTurn("c1", "search", `{"query":"decreto step ammesse"}`),
		callTurn("c2", "read", `{"url":"`+url+`"}`),
		contentTurn(draft),
		contentTurn(final),
	}}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()
	engine := NewEngine(sp, rd, llm.NewClient(srv.URL, "", "m"), nil)
	engine.SetDeepLoopMaxCalls(40)
	engine.SetDeepLoopReview(true)
	err := engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "Chi è stato ammesso al bando STEP?", Mode: "deep", AnswerLanguage: "it"},
		func(Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(s.bodies) < 4 {
		t.Fatalf("requests = %d, want the draft sent back", len(s.bodies))
	}
	role, msg := lastUserOrTool(s.bodies[3])
	if role != "user" || !strings.Contains(msg, "Besidotech (the sources write: BESIDETECH)") {
		t.Errorf("sentinel message = %s %q", role, msg)
	}
	if !strings.Contains(msg, "Zanzibarco") || strings.Contains(msg, "Zanzibarco (") {
		t.Errorf("a name with nothing close is sent back as before: %q", msg)
	}
}
