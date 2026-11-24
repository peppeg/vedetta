package agent

import (
	"strings"
	"testing"
)

func TestLoopNotes_SingularAndPlural(t *testing.T) {
	one := loopNote("it", LoopStats{PagesRead: 1, Unreadable: 1}, 1, 0)
	if !strings.Contains(one, "1 ricerca, 1 pagina letta, 1 non leggibile.") {
		t.Errorf("singular: %q", one)
	}
	many := loopNote("it", LoopStats{PagesRead: 4}, 6, 0)
	if !strings.Contains(many, "6 ricerche, 4 pagine lette, 0 non leggibili.") {
		t.Errorf("plural: %q", many)
	}
	if r := reviewNote("it", LoopStats{Reviewed: true, ReviewItems: make([]ReviewItem, 1)}); !strings.Contains(r, "1 frase aveva") {
		t.Errorf("review: %q", r)
	}
}
