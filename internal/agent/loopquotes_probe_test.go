package agent

import (
	"strings"
	"testing"
)

func TestQuoteProbe_RebuildsTheConversation(t *testing.T) {
	answer := "La parete del reattore è spessa venticinque centimetri secondo la scheda.\n\n" + NotesMarker + "\nNote di Vedetta: qualcosa"
	p := NewQuoteProbe("Quanto è spessa la parete del reattore?", answer, []QuoteProbePage{
		{URL: "https://a.test/x", Title: "Scheda", Text: quotePageA},
	})
	if len(p.Units) != 1 {
		t.Fatalf("units = %q", p.Units)
	}
	m := p.Messages
	last := m[len(m)-1]
	if m[0].Role != "system" || m[1].Role != "user" || last.Role != "user" || !strings.Contains(last.Content, "PAGES:") || !strings.Contains(last.Content, "SENTENCES:") {
		t.Fatalf("shape: %+v", m)
	}
	if len(m[2].ToolCalls) != 1 || m[3].Role != "tool" || !strings.Contains(m[3].Content, "forged steel") {
		t.Errorf("the page must come as a read result: %+v", m[2:4])
	}
	if got := m[len(m)-2]; got.Role != "assistant" || strings.Contains(got.Content, "Note di Vedetta") {
		t.Errorf("the answer goes without notes: %+v", got)
	}
	items, lines := p.Settle(`1 | P1 | "Its walls are 25 centimetres thick, and the whole unit was installed in 1987"`)
	if lines != 1 || items[0].State != "found" {
		t.Errorf("%+v %d", items, lines)
	}
}
