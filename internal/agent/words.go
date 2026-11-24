package agent

import (
	"strings"
	"unicode"

	"vedetta/internal/search"
)

// The substantive words of a sentence or a query, used by the Deep a ciclo to pick
// the passage of a page closest to a claim (loopanchors.go, excerpt.go).

var stopwords = map[string]bool{
	"per": true, "con": true, "tra": true, "fra": true, "del": true, "della": true,
	"delle": true, "dei": true, "degli": true, "sono": true, "come": true, "cosa": true,
	"quale": true, "quali": true, "che": true, "una": true, "uno": true, "gli": true,
	"nel": true, "nella": true, "nei": true, "sul": true, "sulla": true, "sulle": true,
	"the": true, "and": true, "for": true, "with": true, "from": true, "that": true,
	"alla": true, "alle": true, "agli": true, "allo": true, "dall": true,
}

var genericTokens = map[string]bool{
	"impianto": true, "impianti": true, "sistema": true, "sistemi": true,
	"norma": true, "norme": true, "legge": true, "leggi": true,
	"query": true, "queries": true, "subquery": true, "subqueries": true,
	"test": true, "mock": true, "dummy": true, "draft": true, "single": true,
	"search": true, "topic": true, "domanda": true, "ricerca": true,
	// Weak/decorative adjectives: they cause incidental topical matches (e.g. "turistico"
	// in both a Valencia and a Malaysia fact). Kept out of substantive tokens (spec A, G0).
	"turistico": true, "turistica": true, "turistici": true, "turistiche": true,
	"conosciuto": true, "conosciuta": true, "conosciuti": true, "conosciute": true,
	"famoso": true, "famosa": true, "famosi": true, "famose": true,
	"noto": true, "nota": true, "noti": true, "note": true,
	"bello": true, "bella": true, "bei": true, "belle": true,
	"popolare": true, "popolari": true,
	"tipico": true, "tipica": true, "tipici": true, "tipiche": true,
}

func extractSubstantiveTokens(s string) []string {
	tokens := search.Tokenize(strings.ToLower(s))
	var result []string
	seen := make(map[string]bool)
	for _, t := range tokens {
		if len(t) < 3 {
			// Preserve short numeric tokens (e.g. "5" in "Zen 5")
			if len(t) > 0 && unicode.IsDigit(rune(t[0])) {
				// Keep digit token
			} else {
				continue
			}
		}
		if stopwords[t] || genericTokens[t] {
			continue
		}
		if !seen[t] {
			seen[t] = true
			result = append(result, t)
		}
	}
	return result
}
