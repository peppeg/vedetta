package agent

import (
	"strings"
	"testing"
)

func TestDetectRepetition_Positive(t *testing.T) {
	// The exact phrase repeated in the ds4 degeneration loop (29/09/2026).
	phrase := `Forse "Il museo della ceramica" è una chicca. `
	s := strings.Repeat(phrase, 8)
	if !detectRepetition(s, 10, 200, 4) {
		t.Fatalf("expected repetition to be detected for repeated phrase")
	}
}

func TestDetectRepetition_StreamingDetector(t *testing.T) {
	d := newRepetitionDetector()
	phrase := "Forse il museo della ceramica è una chicca. "
	detected := false
	// Feed in token-sized chunks until detection.
	for i := 0; i < 60 && !detected; i++ {
		detected = d.Write(phrase)
	}
	if !detected {
		t.Fatalf("streaming detector did not detect the loop")
	}
}

func TestDetectRepetition_Negative(t *testing.T) {
	// A normal paragraph must not be flagged.
	s := `Valencia è una destinazione adatta alle famiglie con una bambina di 9 anni. ` +
		`La Ciudad de las Artes y las Ciencias è il complesso futuristico di Calatrava. ` +
		`L'Oceanogràfic è il più grande acquario d'Europa. Il Bioparc è uno zoo di immersione. ` +
		`L'Albufera si raggiunge in autobus e le risaie sono l'origine della paella. ` +
		`Nel centro storico troviamo la Lonja e le Torres de Serranos.`
	if detectRepetition(s, 10, 200, 4) {
		t.Fatalf("false positive on normal prose")
	}
}

func TestDetectRepetition_ShortInputSafe(t *testing.T) {
	if detectRepetition("ciao", 10, 200, 4) {
		t.Fatalf("short input must never be flagged")
	}
}

func TestDetectRepetition_LongPeriod(t *testing.T) {
	// The 522-byte cycle of the Deep writer's reasoning in case 6 (28/10/2026,
	// session 6c72b4ad): ten lines repeated 77 times, unseen with maxPeriod 200.
	var b strings.Builder
	for _, r := range []string{"GitHub Trending Digest", "Best Homelab Apps", "The 2026 Homelab Stack",
		"My homelab stack", "Fedora podman guide", "Skeptomenos homelab-setup", "homelab-setup-guide",
		"Starting-A-Homelab-in-2026", "awesome-selfhosted", "GitTrend"} {
		b.WriteString("Need maybe include \"" + r + "\" resource.\n\n")
	}
	cycle := b.String()
	d := newRepetitionDetector()
	d.Write("We need to write the answer in Italian, with links to the sources the user asked for.\n\n")
	written := 0
	for i := 0; i < 20; i++ {
		for _, tok := range strings.SplitAfter(cycle, " ") {
			written += len(tok)
			if d.Write(tok) {
				if written > 6*len(cycle) {
					t.Fatalf("loop seen only after %d bytes (cycle %d)", written, len(cycle))
				}
				return
			}
		}
	}
	t.Fatalf("streaming detector did not detect the %d-byte cycle", len(cycle))
}
