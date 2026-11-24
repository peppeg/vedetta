package agent

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Evidence alignment (V25-T3.1).
//
// The extractor returns a short "verbatim_snippet" for each fact. The legacy check
// (verifyGrounding) accepted a snippet when its first 4 words appeared anywhere, or
// when 70% of its words appeared anywhere in the document, even far apart. That let
// collages and half-invented snippets through on long documents (audit A2).
//
// locateEvidence instead finds the snippet as a contiguous window of the document:
// at least evidenceMinOverlap of the snippet tokens, in order, inside a window at most
// evidenceWindowSlack times the snippet length, with every snippet number inside that
// window. It returns byte offsets into the original text, so the fact can carry what
// the document really says (Evidence) and the surrounding passage (Passage) that the
// writer will use instead of a 15-word fragment (audit A7).

const (
	evidenceMinOverlap  = 0.85
	evidenceWindowSlack = 1.3
	evidenceMaxChars    = 300
	evidenceMaxAnchors  = 4000
	// Parts of a "..." collage must sit close together (the same paragraph or the
	// next one) to count as one evidence; on real pages most collages glue parts that
	// are thousands of characters apart and are rejected.
	evidenceCollageSpan = 1200
)

var (
	passageMaxChars = 1200
)

// alignToken is a normalized word of the document with its byte span in the original.
type alignToken struct {
	norm       string
	start, end int
}

// alignWordRegex matches words and numbers, keeping decimal/thousand separators inside
// numbers ("1.200", "12,5") so they stay one token. Markdown syntax (**, #, [](), |)
// is never part of a token, which makes the alignment insensitive to it.
var alignWordRegex = regexp.MustCompile(`[\p{L}\p{N}]+(?:[.,'’][\p{L}\p{N}]+)*`)

var thousandsNumberRegex = regexp.MustCompile(`^\d{1,3}([.,]\d{3})+$`)

// collageSplitRegex splits a snippet on ellipses used by the model to glue far parts.
var collageSplitRegex = regexp.MustCompile(`\s*(?:\.\.\.|…)\s*`)

func alignTokens(text string) []alignToken {
	locs := alignWordRegex.FindAllStringIndex(text, -1)
	out := make([]alignToken, 0, len(locs))
	for _, l := range locs {
		out = append(out, alignToken{norm: normalizeAlignWord(text[l[0]:l[1]]), start: l[0], end: l[1]})
	}
	return out
}

// normalizeAlignWord lowercases, unifies apostrophes and canonicalizes numbers so that
// "1.200", "1,200" and "1200" compare equal.
func normalizeAlignWord(w string) string {
	w = strings.ToLower(strings.ReplaceAll(w, "’", "'"))
	if n := canonicalNumber(w); n != "" {
		return n
	}
	return w
}

// canonicalNumber returns the digits of a pure number with thousand separators removed
// ("1.200" -> "1200") and decimal commas turned into dots ("12,5" -> "12.5"), or "".
func canonicalNumber(w string) string {
	for _, r := range w {
		if !unicode.IsDigit(r) && r != '.' && r != ',' {
			return ""
		}
	}
	if !unicode.IsDigit(rune(w[0])) {
		return ""
	}
	if thousandsNumberRegex.MatchString(w) {
		return strings.NewReplacer(".", "", ",", "").Replace(w)
	}
	return strings.ReplaceAll(w, ",", ".")
}

func isNumberToken(t string) bool { return t != "" && unicode.IsDigit(rune(t[0])) }

// evidenceSpan is the byte range of one aligned snippet part in the document.
type evidenceSpan struct{ start, end int }

// locateEvidenceIn is locateEvidence on a document already split into words, for a
// caller that looks for many snippets on the same page.
func locateEvidenceIn(docToks []alignToken, snippet string) (spans []evidenceSpan, score float64, ok bool) {
	parts := collageSplitRegex.Split(strings.TrimSpace(snippet), -1)
	score = 1
	for _, p := range parts {
		if len(alignTokens(p)) == 0 {
			continue
		}
		s, e, sc, pok := locatePart(docToks, p)
		if !pok {
			return nil, sc, false
		}
		spans = append(spans, evidenceSpan{s, e})
		score = math.Min(score, sc)
	}
	if len(spans) == 0 {
		return nil, 0, false
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	if spans[len(spans)-1].end-spans[0].start > evidenceCollageSpan {
		return nil, 0, false
	}
	return spans, score, true
}

// spanBounds returns the overall byte range covered by spans.
func spanBounds(spans []evidenceSpan) (start, end int) {
	return spans[0].start, spans[len(spans)-1].end
}

// locatePart aligns one contiguous snippet part against the document tokens.
func locatePart(docToks []alignToken, part string) (start, end int, score float64, ok bool) {
	snip := alignTokens(part)
	n := len(snip)
	if n == 0 || len(docToks) == 0 {
		return 0, 0, 0, false
	}
	want := make([]string, n)
	var nums []string
	for i, t := range snip {
		want[i] = t.norm
		if isNumberToken(t.norm) {
			nums = append(nums, t.norm)
		}
	}

	// Fast path: exact contiguous match.
	for i := 0; i+n <= len(docToks); i++ {
		if docToks[i].norm != want[0] {
			continue
		}
		j := 1
		for j < n && docToks[i+j].norm == want[j] {
			j++
		}
		if j == n {
			return docToks[i].start, docToks[i+n-1].end, 1, true
		}
	}
	if n < 3 {
		return 0, 0, 0, false // too short for a fuzzy match to mean anything
	}

	// Anchors: document positions equal to one of the first three snippet tokens, so
	// the window starts near where the snippet starts even if the model skipped a word.
	heads := map[string]bool{want[0]: true, want[1]: true, want[2]: true}
	width := int(math.Ceil(float64(n) * evidenceWindowSlack))
	bestScore, bestFrom, bestTo := 0.0, -1, -1
	anchors := 0
	for i := 0; i < len(docToks) && anchors < evidenceMaxAnchors; i++ {
		if !heads[docToks[i].norm] {
			continue
		}
		anchors++
		hi := i + width
		if hi > len(docToks) {
			hi = len(docToks)
		}
		lcs, first, last := lcsSpan(want, docToks[i:hi])
		sc := float64(lcs) / float64(n)
		if sc <= bestScore {
			continue
		}
		window := docToks[i+first : i+last+1]
		if !numbersInWindow(nums, window) {
			continue
		}
		bestScore, bestFrom, bestTo = sc, i+first, i+last
	}
	if bestFrom < 0 || bestScore < evidenceMinOverlap {
		return 0, 0, bestScore, false
	}
	return docToks[bestFrom].start, docToks[bestTo].end, bestScore, true
}

// lcsSpan returns the length of the longest common subsequence between want and the
// window tokens, plus the window indices of the first and last matched tokens.
func lcsSpan(want []string, window []alignToken) (length, first, last int) {
	n, m := len(want), len(window)
	if n == 0 || m == 0 {
		return 0, 0, 0
	}
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if want[i-1] == window[j-1].norm {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}
	// Backtrack to find the matched window positions.
	first, last = -1, -1
	for i, j := n, m; i > 0 && j > 0; {
		switch {
		case want[i-1] == window[j-1].norm && dp[i][j] == dp[i-1][j-1]+1:
			if last < 0 {
				last = j - 1
			}
			first = j - 1
			i--
			j--
		case dp[i-1][j] >= dp[i][j-1]:
			i--
		default:
			j--
		}
	}
	if first < 0 {
		return 0, 0, 0
	}
	return dp[n][m], first, last
}

func numbersInWindow(nums []string, window []alignToken) bool {
	if len(nums) == 0 {
		return true
	}
	have := make(map[string]bool, len(window))
	for _, t := range window {
		have[t.norm] = true
	}
	for _, n := range nums {
		if !have[n] {
			return false
		}
	}
	return true
}

// evidenceText returns the original document text of the aligned windows, collapsed to
// single spaces; the parts of a collage are joined with " … ". The result is capped at
// evidenceMaxChars (each part gets an equal share).
func evidenceText(doc string, spans []evidenceSpan) string {
	if len(spans) == 0 {
		return ""
	}
	share := evidenceMaxChars / len(spans)
	parts := make([]string, 0, len(spans))
	for _, sp := range spans {
		parts = append(parts, cleanSpan(clampAround(doc, sp.start, sp.end, share)))
	}
	return strings.Join(parts, " … ")
}

// passageText returns the paragraph(s) of doc around [start,end): the paragraph that
// contains the evidence, extended with neighbouring paragraphs while the total stays
// within maxChars. A paragraph longer than maxChars is cut around the evidence.
func passageText(doc string, start, end, maxChars int) string {
	if maxChars <= 0 || start < 0 || end > len(doc) || start >= end {
		return ""
	}
	pStart := strings.LastIndex(doc[:start], "\n\n")
	if pStart < 0 {
		pStart = 0
	} else {
		pStart += 2
	}
	pEnd := strings.Index(doc[end:], "\n\n")
	if pEnd < 0 {
		pEnd = len(doc)
	} else {
		pEnd += end
	}
	if pEnd-pStart > maxChars {
		return cleanSpan(clampAround(doc, start, end, maxChars))
	}
	// Grow by whole paragraphs, alternating after and before, within budget.
	for grew := true; grew; {
		grew = false
		if pEnd < len(doc) {
			next := strings.Index(doc[pEnd+2:], "\n\n")
			nEnd := len(doc)
			if next >= 0 {
				nEnd = pEnd + 2 + next
			}
			if nEnd-pStart <= maxChars {
				pEnd, grew = nEnd, true
			}
		}
		if pStart > 0 {
			prev := strings.LastIndex(doc[:pStart-2], "\n\n")
			nStart := 0
			if prev >= 0 {
				nStart = prev + 2
			}
			if pEnd-nStart <= maxChars {
				pStart, grew = nStart, true
			}
		}
	}
	return strings.TrimSpace(doc[pStart:pEnd])
}

// clampAround returns at most maxChars bytes of doc centered on [start,end), cut on
// word boundaries and valid UTF-8.
func clampAround(doc string, start, end, maxChars int) string {
	if end-start >= maxChars {
		return safeSlice(doc, start, start+maxChars)
	}
	pad := (maxChars - (end - start)) / 2
	from, to := start-pad, end+pad
	if from < 0 {
		to -= from
		from = 0
	}
	if to > len(doc) {
		from -= to - len(doc)
		to = len(doc)
		if from < 0 {
			from = 0
		}
	}
	s := safeSlice(doc, from, to)
	// Drop partial words at the cut edges.
	if from > 0 {
		if i := strings.IndexAny(s, " \n"); i >= 0 && i < len(s)/4 {
			s = s[i+1:]
		}
	}
	if to < len(doc) {
		if i := strings.LastIndexAny(s, " \n"); i > len(s)*3/4 {
			s = s[:i]
		}
	}
	return s
}

func safeSlice(s string, from, to int) string {
	if from < 0 {
		from = 0
	}
	if to > len(s) {
		to = len(s)
	}
	for from < to && !utf8.RuneStart(s[from]) {
		from++
	}
	for to < len(s) && to > from && !utf8.RuneStart(s[to]) {
		to--
	}
	return s[from:to]
}

func cleanSpan(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// markdownQuoteSafe removes characters that would break the italic quote in the
// bibliography when the evidence comes straight from a markdown document.
var markdownQuoteReplacer = strings.NewReplacer("*", "", "`", "", "\n", " ")
