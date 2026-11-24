package agent

import "testing"

func TestDetectTextLanguage(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Vorrei trascorrere 4 giorni a Valencia con mia figlia", "it"},
		{"How does hope work in clinical psychology?", "en"},
		{"¿Dónde comer paella en Valencia?", "es"},
		{"Wie funktioniert das Schulsystem?", "de"},
		{"Comment fonctionne l'impôt sur le revenu ?", "fr"},
		{"ok", ""},
		{"D.Lgs. 152/2006", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := DetectTextLanguage(c.in); got != c.want {
			t.Errorf("DetectTextLanguage(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestResolveAnswerLanguage(t *testing.T) {
	// 1. Explicit override wins over everything (even a different query language).
	if got := ResolveAnswerLanguage("lang-s1", "Vorrei un consiglio", "en", "es"); got != "es" {
		t.Errorf("override: got %q; want es", got)
	}

	// 2. Detection from the query wins over the UI locale.
	if got := ResolveAnswerLanguage("lang-s2", "Vorrei trascorrere 4 giorni a Valencia", "en", ""); got != "it" {
		t.Errorf("detection over UI: got %q; want it", got)
	}
	if got := ResolveAnswerLanguage("lang-s2b", "How does hope work in clinical psychology?", "it", ""); got != "en" {
		t.Errorf("detection over UI (en): got %q; want en", got)
	}

	// 3. Sticky: a later ambiguous query keeps the session language.
	if got := ResolveAnswerLanguage("lang-s3", "Vorrei sapere i giorni migliori", "en", ""); got != "it" {
		t.Fatalf("sticky seed: got %q; want it", got)
	}
	if got := ResolveAnswerLanguage("lang-s3", "ok", "en", ""); got != "it" {
		t.Errorf("sticky follow-up: got %q; want it", got)
	}

	// 4. Fallback to the UI locale when nothing else applies.
	if got := ResolveAnswerLanguage("lang-s4", "ok", "de", ""); got != "de" {
		t.Errorf("UI fallback: got %q; want de", got)
	}
	// Empty UI locale falls back to English-first.
	if got := ResolveAnswerLanguage("lang-s5", "ok", "", ""); got != "en" {
		t.Errorf("empty UI fallback: got %q; want en", got)
	}
}
