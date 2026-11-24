package reader

import "strings"

// blockPageMarkers are high-precision, case-insensitive substrings that
// identify anti-bot, access-denied or error interstitial pages which a plain
// HTTP reader may fetch instead of the real content. Generic terms (e.g.
// "javascript" alone) are deliberately excluded to avoid false positives on
// legitimate pages.
var blockPageMarkers = []string{
	"error blocked diagnostic",
	"access denied",
	"access forbidden",
	"403 forbidden",
	"request blocked",
	"attention required",
	"are you a robot",
	"unusual traffic",
	"captcha",
	"enable javascript and cookies to continue",
	"please verify you are a human",
	"checking your browser before accessing",
	"cookies must be enabled", // PubMed/NCBI interstitial (October 2026)
	"verify you are human",
	"just a moment",
}

// blockPageScanLimit bounds how much of the document body is inspected.
const blockPageScanLimit = 2000

// blockPageMaxContent is the longest body that can still be an interstitial.
// Challenge pages are short; a long page that merely mentions "captcha" or
// "access denied" is an article about them (idea I1, from Odysseus).
const blockPageMaxContent = 3000

// IsBlockPage reports whether the document looks like an anti-bot, access
// denied or error interstitial rather than real content.
func IsBlockPage(doc *Document) bool {
	return BlockPageMarker(doc) != ""
}

// BlockPageMarker returns the marker that identifies doc as a block page, or "".
// A short document (at most blockPageMaxContent) is a block page when its title
// or the first ~2000 chars of its body contain a marker. A longer document is one
// only when its whole title is a typical challenge title: a long article titled
// "How CAPTCHA works" is real content. It is domain-agnostic.
func BlockPageMarker(doc *Document) string {
	if doc == nil {
		return ""
	}
	title := strings.ToLower(strings.TrimSpace(doc.Title))
	content := strings.TrimSpace(doc.Content)
	if len(content) > blockPageMaxContent {
		if isChallengeTitle(title) {
			return title
		}
		return ""
	}
	if len(content) > blockPageScanLimit {
		content = content[:blockPageScanLimit]
	}
	haystack := title + "\n" + strings.ToLower(content)
	for _, marker := range blockPageMarkers {
		if strings.Contains(haystack, marker) {
			return marker
		}
	}
	return anubisMarker(haystack)
}

// anubisMarker recognizes the challenge page of Anubis, the proof-of-work gate many
// free-software sites put in front of their pages (17/11/2026: the Fedora release
// notes came to the plain reader as "Controllo se sei un robot..." and counted as
// read; Jina got its error page, "Oh noes!"). The page is translated into the
// reader's language, but it always names Anubis together with Hashcash, JShelter or
// Techaro (its maker, in the footer); asking for both keeps out a short page about
// the Egyptian god. Called only on short pages (blockPageMaxContent).
func anubisMarker(haystack string) string {
	if !strings.Contains(haystack, "anubis") {
		return ""
	}
	for _, name := range []string{"hashcash", "jshelter", "techaro"} {
		if strings.Contains(haystack, name) {
			return "anubis " + name
		}
	}
	return ""
}

// isChallengeTitle reports whether a whole page title is a typical interstitial title.
func isChallengeTitle(title string) bool {
	t := strings.TrimRight(title, ".!… ")
	switch t {
	case "just a moment", "access denied", "403 forbidden", "attention required", "security verification",
		"verify you are human", "client challenge", "request blocked":
		return true
	}
	return strings.HasPrefix(t, "attention required! | cloudflare")
}

// isBlockPage is the package-internal alias kept for the composite reader.
func isBlockPage(doc *Document) bool { return IsBlockPage(doc) }

// isBetterDoc reports whether cand is a preferable replacement for current.
// A non-block candidate always beats a block page; otherwise the candidate
// must carry strictly more non-empty content than the current document.
func isBetterDoc(cand, current *Document) bool {
	if cand == nil || len(strings.TrimSpace(cand.Content)) == 0 {
		return false
	}
	if current == nil {
		return true
	}
	candBlocked := isBlockPage(cand)
	curBlocked := isBlockPage(current)
	if curBlocked && !candBlocked {
		return true
	}
	if !curBlocked && candBlocked {
		return false
	}
	return len(strings.TrimSpace(cand.Content)) > len(strings.TrimSpace(current.Content))
}
