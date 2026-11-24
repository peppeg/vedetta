package agent

import "testing"

// A-T5 — weak/decorative adjectives are not substantive tokens.
func TestExtractSubstantiveTokens_ExcludesWeakAdjectives(t *testing.T) {
	weak := map[string]bool{
		"turistico": true, "turistica": true, "conosciuto": true, "conosciuta": true,
		"famoso": true, "famosi": true, "noto": true, "bello": true, "bella": true,
		"popolare": true, "tipico": true,
	}
	for _, tok := range extractSubstantiveTokens("posti non molto conosciuto e turistico famoso noto bello popolare tipico") {
		if weak[tok] {
			t.Errorf("weak adjective %q must not be a substantive token", tok)
		}
	}
}
