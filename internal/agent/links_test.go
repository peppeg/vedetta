package agent

import (
	"strings"
	"testing"
)

// streamThrough feeds text to the reconciler in chunks of n bytes, then flushes.
func streamThrough(rec *Reconciler, text string, n int) string {
	var out strings.Builder
	for i := 0; i < len(text); i += n {
		out.WriteString(rec.Process(text[i:min(i+n, len(text))]))
	}
	out.WriteString(rec.Flush())
	return out.String()
}

func TestLinkResolver(t *testing.T) {
	lr := newLinkResolver([]string{
		"https://www.fiscalitadellenergia.it/2022/07/28/le-linee-guida-in-materia-di-impianti-agrivoltaici/",
		"https://www.normattiva.it/eli/id/2021/11/30/21G00214/CONSOLIDATED",
		"https://example.com/news/123",
		"https://example.com/page?id=7",
	})
	cases := []struct {
		raw, want    string
		repaired, ok bool
	}{
		// The writer's real rewrite (benchmark case agrivoltaico-normativo, run t34).
		{"https://www.fiscalitadellenergia.it/2022-07-28/le-linee-guida-in-materia-di-impianti-agrivoltaici/",
			"https://www.fiscalitadellenergia.it/2022/07/28/le-linee-guida-in-materia-di-impianti-agrivoltaici/", true, true},
		{"https://www.normattiva.it/eli/id/2021-11-30/21G00214/CONSOLIDATED",
			"https://www.normattiva.it/eli/id/2021/11/30/21G00214/CONSOLIDATED", true, true},
		// Same page: scheme, www, trailing slash, fragment, tracking query.
		{"http://normattiva.it/eli/id/2021/11/30/21G00214/CONSOLIDATED/#art1",
			"https://www.normattiva.it/eli/id/2021/11/30/21G00214/CONSOLIDATED", false, true},
		{"https://example.com/news/123?utm_source=x", "https://example.com/news/123", false, true},
		// Different digits are a different page, never a repair.
		{"https://example.com/news/124", "", false, false},
		{"https://unknown.org/whatever", "", false, false},
	}
	for _, c := range cases {
		got, repaired, ok := lr.Resolve(c.raw)
		if got != c.want || repaired != c.repaired || ok != c.ok {
			t.Errorf("Resolve(%q) = (%q, %v, %v), want (%q, %v, %v)", c.raw, got, repaired, ok, c.want, c.repaired, c.ok)
		}
	}
}

func TestLinkResolver_TypoOnLongPath(t *testing.T) {
	lr := newLinkResolver([]string{"https://site.it/guida/impianti-agrivoltaici-requisiti-minimi"})
	got, repaired, ok := lr.Resolve("https://site.it/guida/impianti-agrivoltaci-requisiti-minimi")
	if !ok || !repaired || got != "https://site.it/guida/impianti-agrivoltaici-requisiti-minimi" {
		t.Fatalf("typo not repaired: (%q, %v, %v)", got, repaired, ok)
	}
}

func newLinkTestReconciler() *Reconciler {
	urls := []string{"https://fonte.it/articolo", "https://www.fiscalitadellenergia.it/2022/07/28/linee-guida/", "https://altro.it/pagina"}
	return NewReconciler(NewFactStore()).WithSourceURLs(urls)
}

func TestReconciler_Links(t *testing.T) {
	text := "Norma [https://www.fiscalitadellenergia.it/2022-07-28/linee-guida/]. " +
		"Vedi [la fonte](https://fonte.it/articolo/) e [un blog](https://inventato.com/x). " +
		"Link nudo https://inventato.com/y, e uno buono https://altro.it/pagina. " +
		"Testo [consensus] e [https://sconosciuto.org/z] fine."
	// Without the old pipeline's previews (snippets) a bracketed address stays an
	// address, repaired to the source it means (map R1); it used to become [S1].
	want := "Norma [https://www.fiscalitadellenergia.it/2022/07/28/linee-guida/]. " +
		"Vedi [la fonte](https://fonte.it/articolo) e un blog. " +
		"Link nudo *unverified link*, e uno buono https://altro.it/pagina. " +
		"Testo [consensus] e  fine."
	for _, n := range []int{1, 2, 7, 1000} {
		rec := newLinkTestReconciler()
		got := streamThrough(rec, text, n)
		if got != want {
			t.Fatalf("chunk %d:\nwant %q\ngot  %q", n, want, got)
		}
		if rec.DroppedPhantomURLs() != 3 || rec.RepairedURLs() != 1 {
			t.Errorf("chunk %d: dropped=%d repaired=%d, want 3 and 1", n, rec.DroppedPhantomURLs(), rec.RepairedURLs())
		}
	}
}

func TestReconciler_DroppedLinkWithAddressText(t *testing.T) {
	text := "Repo: [github.com/morb-ai/komodo](https://github.com/morb-ai/komodo) · [www.inventato.com](https://www.inventato.com/) · " +
		"[Node.js](https://inventato.com/node) · [la fonte](https://fonte.it/articolo/)."
	want := "Repo: *unverified link* · *unverified link* · Node.js · [la fonte](https://fonte.it/articolo)."
	for _, n := range []int{1, 5, 1000} {
		rec := newLinkTestReconciler()
		if got := streamThrough(rec, text, n); got != want {
			t.Fatalf("chunk %d:\nwant %q\ngot  %q", n, want, got)
		}
	}
}

func TestReconciler_LinkVerifier(t *testing.T) {
	var asked []string
	verify := func(raw, text string) bool {
		asked = append(asked, raw+"|"+text)
		return strings.Contains(raw, "vero")
	}
	text := "[github.com/vero/progetto](https://github.com/vero/progetto) e [falso](https://github.com/falso/x), " +
		"di nuovo [qui](https://github.com/vero/progetto)."
	want := "[github.com/vero/progetto](https://github.com/vero/progetto) e falso, di nuovo [qui](https://github.com/vero/progetto)."
	for _, n := range []int{1, 1000} {
		asked = nil
		rec := newLinkTestReconciler().WithLinkVerifier(verify)
		if got := streamThrough(rec, text, n); got != want {
			t.Fatalf("chunk %d:\nwant %q\ngot  %q", n, want, got)
		}
		if rec.VerifiedURLs() != 1 || rec.DroppedPhantomURLs() != 1 || len(asked) != 2 {
			t.Errorf("chunk %d: verified=%d dropped=%d asked=%v", n, rec.VerifiedURLs(), rec.DroppedPhantomURLs(), asked)
		}
	}
}

func TestReconciler_LinksPlainTextUntouched(t *testing.T) {
	text := "Il protocollo http è vecchio; [nota] resta, (parentesi) pure, e [aperta senza chiusura\nriga dopo. Fine h"
	rec := newLinkTestReconciler()
	if got := streamThrough(rec, text, 3); got != text {
		t.Fatalf("plain text changed:\nwant %q\ngot  %q", text, got)
	}
}

// Case 12 (29/10/2026): an address written as code is a value to copy, not a
// citation; it used to be checked with the closing backtick and replaced,
// leaving the code span open.
func TestReconciler_AddressInCodeUntouched(t *testing.T) {
	text := "DoH → `https://cloudflare-dns.com/dns-query`. Nudo https://inventato.com/y`.\n" +
		"```\nserver = https://inventato.com/conf\n```\n" +
		"Backtick spaiato ` https://inventato.com/a\nriga dopo https://inventato.com/b fine."
	want := "DoH → `https://cloudflare-dns.com/dns-query`. Nudo *unverified link*`.\n" +
		"```\nserver = https://inventato.com/conf\n```\n" +
		"Backtick spaiato ` https://inventato.com/a\nriga dopo *unverified link* fine."
	for _, n := range []int{1, 2, 7, 1000} {
		rec := newLinkTestReconciler()
		if got := streamThrough(rec, text, n); got != want {
			t.Fatalf("chunk %d:\nwant %q\ngot  %q", n, want, got)
		}
		if rec.DroppedPhantomURLs() != 2 {
			t.Errorf("chunk %d: dropped=%d, want 2", n, rec.DroppedPhantomURLs())
		}
	}
}

// Case 03/11/2026: the Open WebUI home writes "github.com/open-webui/desktop"
// without the scheme; a link to it points to what the page names.
func TestAddressesInText(t *testing.T) {
	text := "Download the desktop app from github.com/open-webui/desktop. Docs at https://docs.openwebui.com/getting-started/ " +
		"and [repo](https://github.com/open-webui/open-webui). Built with Node.js and next.config.js, mail a@b.com/x."
	got := strings.Join(addressesInText(text), " ")
	for _, want := range []string{"https://github.com/open-webui/desktop", "https://docs.openwebui.com/getting-started/", "https://github.com/open-webui/open-webui"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %q", want, got)
		}
	}
	for _, not := range []string{"https://docs.openwebui.com/getting-started/ https://docs", "Node.js", "next.config", "https://b.com"} {
		if strings.Contains(got, not) {
			t.Errorf("unexpected %q in %q", not, got)
		}
	}
	if strings.Count(got, "openwebui.com") != 1 {
		t.Errorf("the host of a URL with the scheme taken twice: %q", got)
	}
	r := NewReconciler(NewFactStore()).WithSourceURLs(append([]string{"https://docs.openwebui.com/"}, addressesInText(text)...))
	out := r.Process("App: [github.com/open-webui/desktop](https://github.com/open-webui/desktop).") + r.Flush()
	if !strings.Contains(out, "(https://github.com/open-webui/desktop)") || r.DroppedPhantomURLs() != 0 {
		t.Errorf("link written in a page read was dropped: %q", out)
	}
}
