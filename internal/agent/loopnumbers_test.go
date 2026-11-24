package agent

import (
	"strings"
	"testing"
)

// Piano dopo audit 5.3: the bench asks the Deep a ciclo's own check whether a number
// flagged in a follow-up was on a page read in an earlier turn.
func TestMissingAnswerNumbersIsTheLoopCheck(t *testing.T) {
	page := "Nel censimento la città contava 3.250.077 abitanti."
	got := MissingAnswerNumbers("La città ha circa 3,25 milioni di abitanti.\nLe scuole sono 4.812.", page)
	if len(got) != 1 || len(got[0].Missing) != 1 || !strings.Contains(got[0].Sentence, "scuole") {
		t.Fatalf("want only the sentence with 4.812 flagged (the rounding of the page's number is found), got %+v", got)
	}
	if got := MissingAnswerNumbers("Le scuole sono 4.812.", page, "Un'altra pagina: 4812 scuole."); len(got) != 0 {
		t.Fatalf("a number on any of the texts is found, got %+v", got)
	}
}
