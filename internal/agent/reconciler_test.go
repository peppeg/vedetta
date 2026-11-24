package agent

import (
	"strings"
	"testing"
)

// V25-F0.1: the writer's "~" for "about" must not become GFM strikethrough.
func TestReconciler_TildeAbout(t *testing.T) {
	src := "https://www.normattiva.it/uri-res/N2Ls?urn:nir:stato:decreto.legislativo:2021-11-08;199~art20"
	cases := []struct{ in, want string }{
		{"~3 °C", "≈3 °C"},
		{"tra 2 e (~15%) circa", "tra 2 e (≈15%) circa"},
		{"| Sep 2022 | ~Nov 2027 |", "| Sep 2022 | ≈Nov 2027 |"},
		{"costa ~$40 e ~€30", "costa ≈$40 e ≈€30"},
		{"vedi " + src, "vedi " + src},
		{"[legge](" + src + ")", "[legge](" + src + ")"},
		{"in ~/.config/niri", "in ~/.config/niri"},
		{"~~barrato~~ voluto", "~~barrato~~ voluto"},
		{"a~b e x~3", "a~b e x~3"},
		{"finisce con ~", "finisce con ~"},
		{"~ solo", "~ solo"},
	}
	for _, c := range cases {
		for _, links := range []bool{false, true} {
			rec := NewReconciler(NewFactStore())
			if links {
				rec = rec.WithSourceURLs([]string{src})
			}
			if got := rec.Process(c.in) + rec.Flush(); got != c.want {
				t.Errorf("links=%t %q: got %q, want %q", links, c.in, got, c.want)
			}
			// one byte at a time: a tilde at the end of a chunk waits for the next one
			rec = NewReconciler(NewFactStore())
			if links {
				rec = rec.WithSourceURLs([]string{src})
			}
			var b strings.Builder
			for i := 0; i < len(c.in); i++ {
				b.WriteString(rec.Process(c.in[i : i+1]))
			}
			b.WriteString(rec.Flush())
			if got := b.String(); got != c.want {
				t.Errorf("links=%t byte by byte %q: got %q, want %q", links, c.in, got, c.want)
			}
		}
	}
}

// The tag split across stream chunks, one byte at a time.
func TestReconciler_DeductionSplitAcrossChunks(t *testing.T) {
	for _, links := range []bool{false, true} {
		rec := NewReconciler(NewFactStore()).WithLanguage("it")
		if links {
			rec = rec.WithSourceURLs(nil)
		}
		in := "Quindi: [[deduction]] serve un ponte."
		var out strings.Builder
		for i := 0; i < len(in); i++ {
			out.WriteString(rec.Process(in[i : i+1]))
		}
		out.WriteString(rec.Flush())
		if want := "Quindi: *Deduzione, non scritta nelle fonti:* serve un ponte."; out.String() != want {
			t.Errorf("links %v:\nwant %q\ngot  %q", links, want, out.String())
		}
		if rec.Deductions() != 1 {
			t.Errorf("links %v: deductions = %d, want 1", links, rec.Deductions())
		}
	}
}

// Other double brackets are not deductions and stay as they were.
func TestReconciler_NotADeduction(t *testing.T) {
	rec := NewReconciler(NewFactStore()).WithLanguage("it")
	in := "Vedi [[nota]] e [[dedicato]] qui."
	if got := rec.Process(in) + rec.Flush(); got != in {
		t.Errorf("want %q, got %q", in, got)
	}
	if rec.Deductions() != 0 {
		t.Errorf("deductions = %d, want 0", rec.Deductions())
	}
}

// An address on the reader's machine or network is something to type: it stays, as
// code, while a public address outside the sources still goes (series of 05/11/2026).
func TestReconciler_LocalAddresses(t *testing.T) {
	src := "https://src.test/a"
	label := "*link non verificato*"
	for _, c := range []struct{ in, want string }{
		{"Poi apri **http://localhost:3000** nel browser.", "Poi apri **`http://localhost:3000`** nel browser."},
		{"L'app apre su http://localhost:8080.", "L'app apre su `http://localhost:8080`."},
		{"Vedi [http://localhost:3000](http://localhost:3000) ora", "Vedi `http://localhost:3000` ora"},
		{"Apri [l'interfaccia](http://192.168.1.10:8080) ora", "Apri l'interfaccia (`http://192.168.1.10:8080`) ora"},
		{"Prova [http://ollama:11434] ora", "Prova `http://ollama:11434` ora"},
		{"Già `http://localhost:3000` in codice", "Già `http://localhost:3000` in codice"},
		{"Fonte **https://src.test/a** e basta", "Fonte **https://src.test/a** e basta"},
		{"Altro https://fuori.test/x e basta", "Altro " + label + " e basta"},
		{"Altro **https://fuori.test/x** e basta", "Altro **" + label + "** e basta"},
	} {
		rec := NewReconciler(NewFactStore()).WithSourceURLs([]string{src})
		rec.lang = "it"
		if got := rec.Process(c.in) + rec.Flush(); got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
		rec = NewReconciler(NewFactStore()).WithSourceURLs([]string{src})
		rec.lang = "it"
		got := ""
		for i := 0; i < len(c.in); i++ {
			got += rec.Process(c.in[i : i+1])
		}
		if got += rec.Flush(); got != c.want {
			t.Errorf("byte by byte %q: got %q, want %q", c.in, got, c.want)
		}
	}
	for addr, want := range map[string]bool{
		"http://localhost:3000":              true,
		"http://127.0.0.1:8080/x":            true,
		"http://[::1]:3000":                  true,
		"http://0.0.0.0:8080":                true,
		"http://10.0.0.5":                    true,
		"http://nas.local/admin":             true,
		"http://host.docker.internal:11434":  true,
		"http://open-webui:8080":             true,
		"http://8.8.8.8":                     false,
		"https://example.com/localhost":      false,
		"https://localhost.example.com":      false,
		"https://www.comune.roma.it/web/pec": false,
		"http://":                            false,
	} {
		if got := localAddress(addr); got != want {
			t.Errorf("localAddress(%q) = %t", addr, got)
		}
	}
}
