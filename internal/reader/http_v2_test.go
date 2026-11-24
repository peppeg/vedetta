package reader

import (
	"strings"
	"testing"
)

// The audit's page (Part 4, R8): a link to a decree, the date in the article's
// header, a table with an empty cell, and a page header outside the article.
const r8Page = `<!doctype html><html><head><title>Bilancio 2024 del Comune</title></head><body>
<header><nav>Home · Notizie · Contatti</nav><p>Portale del Comune</p></header>
<main><article>
<header><h1>Bilancio 2024</h1><p>Pubblicato il 3 marzo 2025 da Ufficio Stampa</p></header>
<p>Il bilancio segue il <a href="/atti/decreto-12.pdf">decreto n. 12</a> e la <a href="https://www.gazzetta.example/legge/45">legge 45</a>; vedi anche <a href="#note">le note</a> e <a href="mailto:ufficio@example.gov">l'ufficio</a>.</p>
<table>
<tr><th>Voce</th><th>2023</th><th>2024</th></tr>
<tr><td>Entrate</td><td>1.000</td><td>1.200</td></tr>
<tr><td>Uscite</td><td></td><td>1.100</td></tr>
</table>
</article></main>
<footer>Copyright 2026</footer>
</body></html>`

// The direct reader keeps links, the article's header and table cells (Piano dopo
// audit 3.5 / R8), with VEDETTA_HTTP_READER=2.
func TestExtractHTMLTextV2(t *testing.T) {
	title, _, text, err := extractHTMLTextFrom([]byte(r8Page), "https://www.comune.example/notizie/bilancio.html", true)
	if err != nil {
		t.Fatal(err)
	}
	if title != "Bilancio 2024 del Comune" {
		t.Errorf("title = %q", title)
	}
	for _, want := range []string{
		"[decreto n. 12](https://www.comune.example/atti/decreto-12.pdf)",
		"[legge 45](https://www.gazzetta.example/legge/45)",
		"Pubblicato il 3 marzo 2025",
		"Uscite |  | 1.100",
		"Entrate | 1.000 | 1.200",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text misses %q:\n%s", want, text)
		}
	}
	for _, not := range []string{"Bilancio 2024 del Comune", "Portale del Comune", "Home · Notizie", "Copyright", "#note", "mailto:"} {
		if strings.Contains(text, not) {
			t.Errorf("text has %q:\n%s", not, text)
		}
	}
	if !strings.Contains(text, "le note") || !strings.Contains(text, "l'ufficio") {
		t.Errorf("the words of links that are not web addresses are lost:\n%s", text)
	}

	// Without the page's address, relative links stay words.
	_, _, text, _ = extractHTMLTextFrom([]byte(r8Page), "", true)
	if strings.Contains(text, "decreto-12.pdf") || !strings.Contains(text, "decreto n. 12") || !strings.Contains(text, "[legge 45](https://www.gazzetta.example/legge/45)") {
		t.Errorf("without the page address:\n%s", text)
	}
}

// Version 1 stays as today, for the trial's other arm.
func TestExtractHTMLTextV1Unchanged(t *testing.T) {
	_, _, v1, _ := extractHTMLText([]byte(r8Page))
	_, _, same, _ := extractHTMLTextFrom([]byte(r8Page), "https://www.comune.example/notizie/bilancio.html", false)
	if v1 != same {
		t.Errorf("version 1 changed:\n%s\n---\n%s", v1, same)
	}
	if strings.Contains(v1, "Pubblicato il 3 marzo") || strings.Contains(v1, "](") || !strings.Contains(v1, "Bilancio 2024 del Comune") {
		t.Errorf("version 1 is not today's:\n%s", v1)
	}
}

// <base href> decides where relative links point.
func TestExtractHTMLTextV2BaseHref(t *testing.T) {
	page := `<html><head><base href="https://docs.example/v2/"></head><body><p>Vedi <a href="guida.html">la guida</a>.</p></body></html>`
	_, _, text, _ := extractHTMLTextFrom([]byte(page), "https://www.example/start", true)
	if !strings.Contains(text, "[la guida](https://docs.example/v2/guida.html)") {
		t.Errorf("text = %q", text)
	}
}
