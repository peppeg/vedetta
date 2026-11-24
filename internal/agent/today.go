package agent

import (
	"fmt"
	"time"
)

// promptNow is the clock of the prompts; tests replace it.
var promptNow = time.Now

// sameDay tells whether a and b fall on the same local day.
func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Local().Date()
	by, bm, bd := b.Local().Date()
	return ay == by && am == bm && ad == bd
}

var monthNames = map[string][12]string{
	"it": {"gennaio", "febbraio", "marzo", "aprile", "maggio", "giugno", "luglio", "agosto", "settembre", "ottobre", "novembre", "dicembre"},
	"en": {"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"},
	"de": {"Januar", "Februar", "März", "April", "Mai", "Juni", "Juli", "August", "September", "Oktober", "November", "Dezember"},
	"fr": {"janvier", "février", "mars", "avril", "mai", "juin", "juillet", "août", "septembre", "octobre", "novembre", "décembre"},
	"es": {"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre"},
}

// localDate writes a day in the answer's language: 6 novembre 2026, 6. November 2026.
func localDate(lang string, t time.Time) string {
	t = t.Local()
	names, ok := monthNames[NormalizeLanguage(lang)]
	if !ok {
		names = monthNames["en"]
	}
	if NormalizeLanguage(lang) == "de" {
		return fmt.Sprintf("%d. %s %d", t.Day(), names[t.Month()-1], t.Year())
	}
	return fmt.Sprintf("%d %s %d", t.Day(), names[t.Month()-1], t.Year())
}
