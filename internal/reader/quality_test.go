package reader

import (
	"strings"
	"testing"
)

const anubisIT = `Controllo se sei un robot... Caricamento... Vedi questa schermata perché l'amministratore di questo sito web ha installato Anubis per proteggere il server dalla piaga delle aziende di AI generativa che estraggono, senza freno, dati dai siti web. Anubis è un compromesso. Anubis utilizza un meccanismo di proof-of-work in stile Hashcash, un meccanismo per ridurre le email di spam. Si noti che Anubis richiede l'utilizzo di caratteristiche moderne di JavaScript che alcuni plugin, come JShelter, disabilitano.`

const anubisEN = `Making sure you're not a bot! Loading... You are seeing this because the administrator of this website has set up Anubis to protect the server against the scourge of AI companies aggressively scraping websites. Anubis uses a Proof-of-Work scheme in the vein of Hashcash, a proposed proof-of-work scheme for reducing email spam. Please note that Anubis requires the use of modern JavaScript features that plugins like JShelter will disable.`

func TestBlockPageMarker(t *testing.T) {
	long := strings.Repeat("Il CAPTCHA è un test per distinguere persone e programmi. ", 80)
	cases := []struct {
		name  string
		doc   Document
		block bool
	}{
		{"pubmed cookie interstitial", Document{Title: "pubmed.ncbi.nlm.nih.gov", Content: "Cookies must be enabled Enable cookies for pubmed.ncbi.nlm.nih.gov and reload this page to continue."}, true},
		{"short access denied", Document{Title: "", Content: "Access Denied. You don't have permission to access this server."}, true},
		{"cloudflare title on long page", Document{Title: "Just a moment...", Content: long}, true},
		{"long article about captcha", Document{Title: "Come funziona il CAPTCHA", Content: long}, false},
		{"long article mentioning access denied", Document{Title: "Guida ai permessi", Content: "Se vedi access denied controlla i permessi. " + long}, false},
		{"short normal page", Document{Title: "Meteo Roma", Content: "Domani sole, massima 24 gradi."}, false},
		// Anubis, translated: the Italian page the plain reader got from docs.fedoraproject.org (17/11/2026), and the English one.
		{"anubis challenge, italian", Document{Title: "Controllo se sei un robot...", Content: anubisIT}, true},
		{"anubis challenge, english", Document{Title: "Making sure you're not a bot!", Content: anubisEN}, true},
		{"anubis error page, from jina", Document{Title: "Oh noes!", Content: "Protected by [Anubis](https://github.com/TecharoHQ/anubis) From [Techaro](https://techaro.lol/). Made with ❤️ in 🇨🇦."}, true},
		{"short page about the god anubis", Document{Title: "Anubi", Content: "Anubis è il dio egizio dei morti, con la testa di sciacallo; guidava le anime e pesava il cuore."}, false},
		{"long article about the anubis software", Document{Title: "Anubis contro gli scraper", Content: "Anubis usa una proof-of-work in stile Hashcash. " + long}, false},
	}
	for _, c := range cases {
		if got := IsBlockPage(&c.doc); got != c.block {
			t.Errorf("%s: IsBlockPage = %v, want %v (marker %q)", c.name, got, c.block, BlockPageMarker(&c.doc))
		}
	}
}
