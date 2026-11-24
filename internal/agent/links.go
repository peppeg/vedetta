package agent

import (
	"net/url"
	"regexp"
	"strings"
)

// linkResolver maps the URLs the writer prints back to the session's real source
// URLs (V25-T3.5). The writer sometimes rewrites a true URL while copying it
// (2022/07/28 -> 2022-07-28), producing a broken link: a link is kept only when it
// matches a session source exactly or almost exactly, and is then replaced by the
// source's own spelling.
type linkResolver struct {
	exact  map[string]string   // normalized URL -> source URL
	noQry  map[string]string   // normalized URL without query string -> source URL
	byHost map[string][]string // host -> source URLs on that host
}

func newLinkResolver(urls []string) *linkResolver {
	lr := &linkResolver{
		exact:  make(map[string]string),
		noQry:  make(map[string]string),
		byHost: make(map[string][]string),
	}
	for _, u := range urls {
		lr.add(u)
	}
	return lr
}

// add makes u a known source.
func (lr *linkResolver) add(u string) {
	u = strings.TrimSpace(u)
	if u == "" {
		return
	}
	n := normalizeLinkURL(u)
	if _, dup := lr.exact[n]; dup {
		return
	}
	lr.exact[n] = u
	if q := stripLinkQuery(n); q != n {
		if _, ok := lr.noQry[q]; !ok {
			lr.noQry[q] = u
		}
	}
	host, _ := splitLinkHostPath(n)
	lr.byHost[host] = append(lr.byHost[host], u)
}

// Resolve returns the session source URL that raw points to, whether raw had to be
// repaired, and ok=false when raw matches no source.
func (lr *linkResolver) Resolve(raw string) (resolved string, repaired bool, ok bool) {
	if lr == nil {
		return raw, false, true
	}
	n := normalizeLinkURL(raw)
	if u, hit := lr.exact[n]; hit {
		return u, false, true
	}
	// A link that differs from a source only in its query string (tracking params)
	// still points to the same page.
	q := stripLinkQuery(n)
	if u, hit := lr.noQry[q]; hit {
		return u, false, true
	}
	if u, hit := lr.exact[q]; hit {
		return u, false, true
	}

	host, path := splitLinkHostPath(q)
	candidates := lr.byHost[host]
	if len(candidates) == 0 {
		return "", false, false
	}

	// Same path once separators and case are ignored: "2022-07-28" vs "2022/07/28".
	target := canonicalLinkPath(path)
	var match string
	matches := 0
	for _, c := range candidates {
		_, cp := splitLinkHostPath(stripLinkQuery(normalizeLinkURL(c)))
		if canonicalLinkPath(cp) == target {
			match = c
			matches++
		}
	}
	if matches == 1 {
		return match, true, true
	}
	if matches > 1 {
		return "", false, false
	}

	// Small typos on a long path, with exactly the same digits (so /news/123 never
	// becomes /news/124) and a single closest candidate.
	if len(target) < 25 {
		return "", false, false
	}
	digits := linkDigits(path)
	best, bestDist, ties := "", 3, 0
	for _, c := range candidates {
		_, cp := splitLinkHostPath(stripLinkQuery(normalizeLinkURL(c)))
		if linkDigits(cp) != digits {
			continue
		}
		d := levenshtein(canonicalLinkPath(cp), target)
		switch {
		case d < bestDist:
			best, bestDist, ties = c, d, 1
		case d == bestDist:
			ties++
		}
	}
	if best != "" && bestDist <= 2 && ties == 1 {
		return best, true, true
	}
	return "", false, false
}

// Target is the address of the source raw points to, or raw itself when it matches
// none: where a link of the draft will lead once the answer goes out.
func (lr *linkResolver) Target(raw string) string {
	if u, _, ok := lr.Resolve(raw); ok {
		return u
	}
	return raw
}

var (
	textURLRe = regexp.MustCompile(`https?://[^\s<>"'\x60\]\[)(]+`)
	// An address written without the scheme, with a path: "github.com/open-webui/desktop".
	// The character before it must not belong to an address, so the host of a URL
	// with the scheme is not taken twice; a bare name like Node.js has no path.
	bareTextAddressRe = regexp.MustCompile(`(?i)(?:^|[^\w./@:-])((?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}/[^\s<>"'\x60\]\[)(]*)`)
)

// addressesInText returns the addresses written in a text: URLs, and addresses
// without the scheme, which get "https://" so that a link to them works.
func addressesInText(text string) []string {
	var out []string
	for _, u := range textURLRe.FindAllString(text, -1) {
		if u = strings.TrimRight(u, ".,;:!?*_"); u != "" {
			out = append(out, u)
		}
	}
	for _, m := range bareTextAddressRe.FindAllStringSubmatch(text, -1) {
		if a := strings.TrimRight(m[1], ".,;:!?*_"); strings.Contains(a, "/") {
			out = append(out, "https://"+a)
		}
	}
	return out
}

// normalizeLinkURL lowercases scheme and host, drops "www.", the fragment and a
// trailing slash, and unescapes the path, so equal pages compare equal.
func normalizeLinkURL(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimRight(s, ".,;:!?")
	if i := strings.Index(s, "#"); i >= 0 {
		s = s[:i]
	}
	lower := strings.ToLower(s)
	switch {
	case strings.HasPrefix(lower, "https://"):
		s = s[len("https://"):]
	case strings.HasPrefix(lower, "http://"):
		s = s[len("http://"):]
	}
	host, rest := s, ""
	if i := strings.IndexAny(s, "/?"); i >= 0 {
		host, rest = s[:i], s[i:]
	}
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	if un, err := url.PathUnescape(rest); err == nil {
		rest = un
	}
	return strings.TrimRight(host+rest, "/")
}

func stripLinkQuery(n string) string {
	if i := strings.Index(n, "?"); i >= 0 {
		return strings.TrimRight(n[:i], "/")
	}
	return n
}

func splitLinkHostPath(n string) (host, path string) {
	if i := strings.Index(n, "/"); i >= 0 {
		return n[:i], n[i:]
	}
	return n, ""
}

var linkSeparators = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// canonicalLinkPath lowercases a path and collapses every run of separators.
func canonicalLinkPath(path string) string {
	return strings.Trim(linkSeparators.ReplaceAllString(strings.ToLower(path), " "), " ")
}

var linkDigitRuns = regexp.MustCompile(`\d+`)

func linkDigits(path string) string {
	return strings.Join(linkDigitRuns.FindAllString(path, -1), ",")
}

// levenshtein is the edit distance between two short strings (runes).
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
