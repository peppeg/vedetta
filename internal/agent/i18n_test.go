package agent

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestI18n_NormalizeLanguage(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"it", "it"},
		{"IT", "it"},
		{"italian", "it"},
		{"italiano", "it"},
		{"en", "en"},
		{"english", "en"},
		{"de", "de"},
		{"german", "de"},
		{"deutsch", "de"},
		{"fr", "fr"},
		{"french", "fr"},
		{"es", "es"},
		{"spanish", "es"},
		{"", "en"},
		{"unknown", "en"},
	}

	for _, tc := range tests {
		got := NormalizeLanguage(tc.input)
		if got != tc.expected {
			t.Errorf("NormalizeLanguage(%q) = %q; want %q", tc.input, got, tc.expected)
		}
	}
}

func TestI18n_StepMessage(t *testing.T) {
	// A message with a number, in two languages.
	if it := StepMessage("it", "loop_note_partial", 3); !strings.Contains(it, "lette solo in parte: 3") {
		t.Errorf("unexpected IT message: %q", it)
	}
	if en := StepMessage("en", "loop_note_partial", 3); !strings.Contains(en, "read only in part: 3") {
		t.Errorf("unexpected EN message: %q", en)
	}
	// A message without placeholders.
	if de := StepMessage("de", "error_tools_required"); !strings.Contains(de, "Werkzeuge") {
		t.Errorf("unexpected DE message: %q", de)
	}
	// An unknown language falls back to English; an unknown key comes back as it is.
	if xx := StepMessage("xx", "error_tools_required"); !strings.Contains(xx, "does not accept tools") {
		t.Errorf("unknown language did not fall back to English: %q", xx)
	}
	if k := StepMessage("it", "no_such_key"); k != "no_such_key" {
		t.Errorf("unknown key: %q", k)
	}
}

// TestI18n_StepMessagesAllLanguages: every message has the five languages of the interface, with
// the same placeholders; a missing one falls back to English without a word (StepMessage).
func TestI18n_StepMessagesAllLanguages(t *testing.T) {
	verbs := regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z]`)
	keys := make([]string, 0, len(stepMessages))
	for k := range stepMessages {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		want := len(verbs.FindAllString(stepMessages[k]["it"], -1))
		for _, l := range []string{"it", "en", "de", "fr", "es"} {
			tpl := stepMessages[k][l]
			if strings.TrimSpace(tpl) == "" {
				t.Errorf("%s: no %s", k, l)
				continue
			}
			if n := len(verbs.FindAllString(tpl, -1)); n != want {
				t.Errorf("%s: %s has %d placeholders, it has %d", k, l, n, want)
			}
		}
	}
}
