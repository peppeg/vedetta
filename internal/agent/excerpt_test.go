package agent

import (
	"fmt"
	"strings"
	"testing"
)

func TestSelectExcerpt_ShortUnchanged(t *testing.T) {
	content := "Questo è un breve documento con contenuto."
	out := selectExcerpt(content, "breve documento", 1000)
	if out != content {
		t.Fatalf("short content changed: got %q want %q", out, content)
	}
}

func TestSelectExcerpt_PicksRelevantBlock(t *testing.T) {
	preamble := strings.Repeat("Testo introduttivo generico senza riferimenti utili. ", 40)
	relevant := "D.Lgs 199/2021 disciplina l'agrivoltaico e definisce le aree idonee."
	doc := preamble + "\n\n" + relevant

	out := selectExcerpt(doc, "normativa agrivoltaico D.Lgs 199/2021", 800)
	if !strings.Contains(out, "D.Lgs 199/2021") {
		t.Fatalf("expected relevant block in excerpt, got %q", out)
	}
	if len(out) > 800 {
		t.Fatalf("excerpt exceeds budget: %d > 800", len(out))
	}
}

func TestSelectExcerpt_RespectsBudget(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&sb, "Blocco %d con contenuto di riempimento e parola chiave art. %d.\n\n", i, i)
	}
	doc := sb.String()

	const maxChars = 1500
	out := selectExcerpt(doc, "normativa art", maxChars)
	if out == "" {
		t.Fatal("expected non-empty excerpt")
	}
	if len(out) > maxChars {
		t.Fatalf("excerpt exceeds budget: %d > %d", len(out), maxChars)
	}
}

func TestSelectExcerpt_TopicDominatesMarkers(t *testing.T) {
	// A large block full of generic normative markers and numeric references
	// but WITHOUT any topic token of the query.
	markerBlock := strings.Repeat("Decreto art. 5 comma 3, riferimento 123/2021 allegato A. ", 3)
	// A smaller, distant block that actually contains a topic token, with no
	// generic markers at all.
	topicBlock := "Qui si parla di agrivoltaico in modo sintetico."
	doc := markerBlock + "\n\n" + topicBlock

	const maxChars = 200
	out := selectExcerpt(doc, "agrivoltaico aree idonee", maxChars)
	if !strings.Contains(out, "agrivoltaico") {
		t.Fatalf("topic block should win over generic markers, got %q", out)
	}
	if len(out) > maxChars {
		t.Fatalf("excerpt exceeds budget: %d > %d", len(out), maxChars)
	}
}

func TestSelectExcerpt_FallbackSpread(t *testing.T) {
	head := "INIZIO-DOCUMENTO-UNIVOCO"
	body := strings.Repeat("parola completamente irrilevante ripetuta senza riferimenti. ", 60)
	tail := "MARCATORE-FINALE-UNIVOCO"
	doc := head + " " + body + tail

	const maxChars = 500
	out := selectExcerpt(doc, "agrivoltaico d.lgs", maxChars)
	if out == "" {
		t.Fatal("expected non-empty excerpt")
	}
	if len(out) > maxChars {
		t.Fatalf("excerpt exceeds budget: %d > %d", len(out), maxChars)
	}
	if !strings.Contains(out, head) {
		t.Fatalf("spread fallback should include a fragment from the beginning, got %q", out)
	}
	if !strings.Contains(out, tail) {
		t.Fatalf("spread fallback should include the tail marker, got %q", out)
	}
	if strings.HasPrefix(doc, out) {
		t.Fatalf("fallback should not be a mere head prefix, got %q", out)
	}
}

func TestSelectExcerpt_DefaultBudget(t *testing.T) {
	doc := strings.Repeat("contenuto generico ripetuto. ", 2000)

	out := selectExcerpt(doc, "qualsiasi", 0)
	if out == "" {
		t.Fatal("expected non-empty excerpt")
	}
	if len(out) > DefaultExcerptMaxChars {
		t.Fatalf("excerpt exceeds default budget: %d > %d", len(out), DefaultExcerptMaxChars)
	}
}

func TestSelectExcerpt_ChunksGiantBlock(t *testing.T) {
	const line = "Riga di riempimento generico priva di riferimenti utili. "
	const marker = "AGO_CHUNK_UNICO riga dedicata all'agrivoltaico e alle aree idonee."
	const targetLen = 30000

	var sb strings.Builder
	inserted := false
	for sb.Len() < targetLen {
		if !inserted && sb.Len() >= targetLen/2 {
			sb.WriteString(marker)
			sb.WriteString("\n")
			inserted = true
		}
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	doc := sb.String()

	if !inserted {
		t.Fatal("fixture marker was not inserted")
	}
	if strings.Contains(doc, "\n\n") {
		t.Fatal("fixture must be a single block without blank-line separators")
	}
	if n := len(splitExcerptBlocks(doc)); n != 1 {
		t.Fatalf("fixture should collapse to a single block, got %d", n)
	}

	const maxChars = 3000
	out := selectExcerpt(doc, "agrivoltaico aree idonee", maxChars)
	if !strings.Contains(out, "AGO_CHUNK_UNICO") {
		t.Fatalf("expected the chunk holding the marker, got %q", out)
	}
	if len(out) <= 11 {
		t.Fatalf("excerpt is still a tiny fragment: %q", out)
	}
	if len(out) <= maxChars/2 {
		t.Fatalf("excerpt too small: %d, expected close to budget %d", len(out), maxChars)
	}
	if len(out) > maxChars {
		t.Fatalf("excerpt exceeds budget: %d > %d", len(out), maxChars)
	}
}

func TestSplitExcerptChunks_Bounds(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&sb, "riga numero %d con contenuto di riempimento.\n", i)
	}
	sb.WriteString(strings.Repeat("X", excerptChunkChars*2+137))
	sb.WriteString("\n")
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&sb, "coda numero %d.\n", i)
	}
	blocks := []string{sb.String(), "blocco piccolo"}

	chunks := splitExcerptChunks(blocks)
	if len(chunks) == 0 {
		t.Fatal("expected chunks")
	}
	for i, c := range chunks {
		if c == "" {
			t.Fatalf("chunk %d is empty", i)
		}
		if len(c) > excerptChunkChars {
			t.Fatalf("chunk %d exceeds bound: %d > %d", i, len(c), excerptChunkChars)
		}
	}

	strip := func(s string) string { return strings.ReplaceAll(s, "\n", "") }
	if got, want := strip(strings.Join(chunks, "\n")), strip(strings.Join(blocks, "\n")); got != want {
		t.Fatalf("chunking lost content: got len=%d want len=%d", len(got), len(want))
	}
}
