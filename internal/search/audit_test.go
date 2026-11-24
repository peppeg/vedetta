package search

import "testing"

// External audit 24/11/2026: a name inside another name, and the region of a query
// that names two.
func TestAuthoritativeHost_WholeName(t *testing.T) {
	in := Intent{NormativeScore: 2}
	for u, want := range map[string]bool{
		"https://www.linkedin.de/x":      false,
		"https://www.din.de/x":           true,
		"https://din.de:8443/x":          true,
		"https://example.com/?r=iso.org": false,
		"https://www.normattiva.it/uri":  true,
		"https://nonmit.gov.it.ladro.io": false,
	} {
		if got := isAuthoritativeFor(in, u); got != want {
			t.Errorf("%s: %t, want %t", u, got, want)
		}
	}
	if !isAuthoritativeFor(in, "https://who.int/x", IntentRerankerOptions{CustomAuthoritativeDomains: []string{" WHO.int "}}) {
		t.Error("a domain named by the model")
	}
}

func TestClassifyIntent_FirstRegionWritten(t *testing.T) {
	for i := 0; i < 50; i++ {
		if got := ClassifyIntent("normativa in italia per toscana e sicilia sul fotovoltaico"); got.RegionToponym != "toscana" || got.QueryLevel != "national_region" {
			t.Fatalf("region %q level %q", got.RegionToponym, got.QueryLevel)
		}
	}
	if got := ClassifyIntent("normativa italia fotovoltaico"); got.RegionToponym != "" || got.QueryLevel != "national_region" {
		t.Errorf("region %q level %q", got.RegionToponym, got.QueryLevel)
	}
}
