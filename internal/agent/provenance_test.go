package agent

import (
	"strings"
	"testing"
)

func TestComputeProvenance(t *testing.T) {
	cases := []struct {
		cited             int
		partial, degraded bool
		want              Provenance
	}{
		{3, false, false, ProvenanceGrounded},
		{1, true, false, ProvenancePartial},
		{0, false, false, ProvenanceParametric}, // no fact cited, whether or not facts were collected
		{0, true, false, ProvenanceParametric},  // no cited fact dominates
		{2, false, true, ProvenanceDegraded},
		{0, false, true, ProvenanceDegraded},
	}
	for _, c := range cases {
		if got := ComputeProvenance(c.cited, c.partial, c.degraded); got != c.want {
			t.Errorf("ComputeProvenance(%d,%v,%v) = %q, want %q", c.cited, c.partial, c.degraded, got, c.want)
		}
	}
}

func TestProvenanceNote_ParametricIsExplicit(t *testing.T) {
	note := provenanceNote(ProvenanceParametric, "it")
	if !strings.Contains(note, "parametric") || !strings.Contains(strings.ToLower(note), "conoscenza del modello") {
		t.Errorf("parametric note must explicitly state model knowledge, got %q", note)
	}
	en := provenanceNote(ProvenanceParametric, "en")
	if !strings.Contains(en, "model's knowledge") {
		t.Errorf("english parametric note must state model knowledge, got %q", en)
	}
}

func TestProvenanceNote_Grounded(t *testing.T) {
	if note := provenanceNote(ProvenanceGrounded, "en"); !strings.Contains(note, "grounded") {
		t.Errorf("expected grounded note, got %q", note)
	}
}

func TestLoopProvenance_NeverVerified(t *testing.T) {
	cases := []struct {
		cited            int
		partial, degrade bool
		want             Provenance
	}{
		{3, false, false, ProvenanceResearched},
		{0, false, false, ProvenanceParametric},
		{3, true, false, ProvenancePartial},
		{3, false, true, ProvenanceDegraded},
	}
	for _, c := range cases {
		if got := loopProvenance(loopBasis{cited: c.cited}, c.partial, c.degrade); got != c.want {
			t.Errorf("loopProvenance(%d,%v,%v) = %s, want %s", c.cited, c.partial, c.degrade, got, c.want)
		}
	}
	for _, lang := range []string{"it", "en", "de", "fr", "es"} {
		note := loopProvenanceNote(ProvenanceResearched, lang, true, false)
		if !strings.Contains(note, "researched") {
			t.Errorf("%s: %q", lang, note)
		}
		for _, word := range []string{"verificat", "verified", "verifiziert", "vérifié"} {
			if strings.Contains(strings.ToLower(note), word) {
				t.Errorf("%s: the note says %q: %q", lang, word, note)
			}
		}
	}
}

func TestLoopProvenanceNote_NoPageCited(t *testing.T) {
	if note := loopProvenanceNote(ProvenanceParametric, "it", true, false); !strings.Contains(note, "non cita nessuna pagina letta") {
		t.Errorf("after a research: %q", note)
	}
	if loopProvenanceNote(ProvenanceParametric, "it", false, false) != provenanceNote(ProvenanceParametric, "it") {
		t.Error("without a research the usual note applies")
	}
	if loopProvenanceNote(ProvenancePartial, "it", true, false) != provenanceNote(ProvenancePartial, "it") {
		t.Error("the other notes do not change")
	}
}

func TestLoopProvenanceNote_SaysWhatIsChecked(t *testing.T) {
	for _, lang := range []string{"it", "en", "de", "fr", "es"} {
		on, off := loopProvenanceNote(ProvenanceResearched, lang, true, true), loopProvenanceNote(ProvenanceResearched, lang, true, false)
		if on == off || !strings.Contains(on, "researched") {
			t.Errorf("%s: %q", lang, on)
		}
	}
	if loopProvenanceNote(ProvenancePartial, "it", true, true) != provenanceNote(ProvenancePartial, "it") {
		t.Error("the other notes do not change")
	}
}

// Map R5: a researched answer says that Vedetta checks facts, not the model's
// judgments, in every language and with or without the check of the cited pages.
func TestLoopProvenanceNote_JudgmentsAreTheModels(t *testing.T) {
	want := map[string]string{
		"it": "giudizi, consigli e confronti sono del modello",
		"en": "judgments, advice and comparisons are the model's",
		"de": "Urteile, Ratschläge und Vergleiche stammen vom Modell",
		"fr": "les jugements, conseils et comparaisons sont ceux du modèle",
		"es": "los juicios, consejos y comparaciones son del modelo",
	}
	for lang, w := range want {
		for _, checked := range []bool{true, false} {
			if got := loopProvenanceNote(ProvenanceResearched, lang, true, checked); !strings.Contains(got, w) {
				t.Errorf("%s checked=%t: %q misses %q", lang, checked, got, w)
			}
		}
	}
}

// Session 42a545c9 (25/11/2026): no link in the answer, 11 sentences of 15 with their quote
// found on the page read, and the line said "from previews and the model's knowledge".
func TestLoopBasis(t *testing.T) {
	items := []QuoteItem{{State: quoteFound}, {State: quoteShort}, {State: quoteNotFound}, {State: quoteNone}, {State: quoteFound}}
	cases := []struct {
		name  string
		stats LoopStats
		want  Provenance
		note  string // key of the sentence added to the line
	}{
		{"nothing behind it", LoopStats{}, ProvenanceParametric, ""},
		{"a page read is linked", LoopStats{PagesCited: 1, Quotes: "done", QuoteItems: items}, ProvenanceResearched, ""},
		{"a page read before is linked", LoopStats{PagesCitedBefore: 1}, ProvenanceResearched, ""},
		{"no link, quotes found", LoopStats{Quotes: "done", QuoteItems: items}, ProvenanceResearched, "loop_basis_quotes"},
		{"no link, quotes not found or too short", LoopStats{Quotes: "done", QuoteItems: items[1:4]}, ProvenanceParametric, ""},
		{"the reply on the quotes was not usable", LoopStats{Quotes: "format", QuoteItems: items}, ProvenanceParametric, ""},
		{"linked pages opened by Vedetta", LoopStats{LinkStates: map[string]string{"https://a.test/": linkPartial, "https://b.test/": linkUnread}}, ProvenanceParametric, ""},
		{"linked pages nobody opened", LoopStats{LinkStates: map[string]string{"https://b.test/": linkUnread, "https://c.test/": linkAddress}}, ProvenanceParametric, ""},
	}
	for _, c := range cases {
		b := c.stats.basis()
		if got := loopProvenance(b, false, false); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
		for _, lang := range []string{"it", "en", "de", "fr", "es"} {
			want := ""
			if c.note != "" {
				want = " " + StepMessage(lang, c.note, StepMessage(lang, "loop_quotes_list_title"))
			}
			if got := basisNote(b, lang); got != want || strings.Contains(got, "%") {
				t.Errorf("%s (%s): %q, want %q", c.name, lang, got, want)
			}
		}
	}
}
