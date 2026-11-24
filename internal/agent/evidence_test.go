package agent

import (
	"strings"
	"testing"
)

var evidenceDoc = `# La Torre Eiffel

La torre fu costruita tra il 1887 e il 1889 per l'Esposizione Universale di Parigi.
Il progetto strutturale si deve agli ingegneri **Maurice Koechlin** ed Émile Nouguier.

## Altezza

Con la nuova antenna DAB+ installata il 15 marzo 2022 la torre raggiunge 330 metri.
In precedenza l'altezza ufficiale era di 324 metri.

## Visite

Ogni anno la torre accoglie circa 6 milioni di visitatori, in gran parte stranieri.
Il biglietto per la cima costa 29,40 euro per un adulto.

` + strings.Repeat("Testo di riempimento che non c'entra nulla con la torre. ", 30) + `

Curiosità: la torre resta aperta anche a Natale.`

func TestLocateEvidence(t *testing.T) {
	cases := []struct {
		name    string
		snippet string
		ok      bool
		inSpan  string
	}{
		{"exact", "la torre raggiunge 330 metri", true, "330 metri"},
		{"markdown in document", "ingegneri Maurice Koechlin ed Émile Nouguier", true, "Koechlin"},
		{"one word changed out of many", "Con la nuova antenna DAB+ montata il 15 marzo 2022 la torre raggiunge 330 metri", true, "15 marzo 2022"},
		{"first four words right, rest invented", "la torre fu costruita interamente in acciaio inox dagli operai italiani", false, ""},
		{"words scattered across the document", "torre antenna visitatori biglietto ingegneri stranieri", false, ""},
		{"number elsewhere in the document", "l'altezza ufficiale era di 330 metri", false, ""},
		{"collage of close parts", "nuova antenna DAB+ ... raggiunge 330 metri", true, "330"},
		{"collage of far parts", "costruita tra il 1887 e il 1889 ... la torre resta aperta anche a Natale", false, ""},
		{"thousands separator normalized", "circa 6 milioni di visitatori", true, "visitatori"},
	}
	for _, c := range cases {
		spans, score, ok := locateEvidence(evidenceDoc, c.snippet)
		if ok != c.ok {
			t.Errorf("%s: ok=%v (score %.2f); want %v", c.name, ok, score, c.ok)
			continue
		}
		if !ok {
			continue
		}
		start, end := spanBounds(spans)
		if !strings.Contains(evidenceDoc[start:end], c.inSpan) {
			t.Errorf("%s: span %q does not contain %q", c.name, evidenceDoc[start:end], c.inSpan)
		}
	}
}

func TestPassageText(t *testing.T) {
	spans, _, ok := locateEvidence(evidenceDoc, "la torre raggiunge 330 metri")
	if !ok {
		t.Fatal("evidence not found")
	}
	start, end := spanBounds(spans)
	p := passageText(evidenceDoc, start, end, 1200)
	if !strings.Contains(p, "330 metri") || !strings.Contains(p, "324 metri") {
		t.Errorf("passage misses its paragraph: %q", p)
	}
	if len(p) > 1200 {
		t.Errorf("passage too long: %d", len(p))
	}
	// A small budget keeps only the evidence paragraph, cut on word boundaries.
	small := passageText(evidenceDoc, start, end, 60)
	if len(small) > 60 || !strings.Contains(small, "330") {
		t.Errorf("small passage = %q", small)
	}
	if strings.HasPrefix(small, "ntenna") || strings.HasSuffix(small, "metr") {
		t.Errorf("small passage cuts a word: %q", small)
	}
	if ev := evidenceText(evidenceDoc, spans); !strings.Contains(ev, "330 metri") || len(ev) > evidenceMaxChars {
		t.Errorf("evidence = %q", ev)
	}
	// A collage keeps each part's own text, joined with an ellipsis.
	cspans, _, ok := locateEvidence(evidenceDoc, "nuova antenna DAB+ ... In precedenza l'altezza ufficiale era di 324 metri")
	if !ok || len(cspans) != 2 {
		t.Fatalf("collage not aligned: ok=%v spans=%v", ok, cspans)
	}
	if ev := evidenceText(evidenceDoc, cspans); !strings.Contains(ev, " … ") || !strings.Contains(ev, "324 metri") {
		t.Errorf("collage evidence = %q", ev)
	}
}

// locateEvidence is locateEvidenceIn on a whole page (the production wrapper went
// with the old pipeline; the quotes split each page once, loopquotes.go).
func locateEvidence(doc, snippet string) (spans []evidenceSpan, score float64, ok bool) {
	return locateEvidenceIn(alignTokens(doc), snippet)
}
