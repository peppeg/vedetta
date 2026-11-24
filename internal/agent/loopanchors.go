package agent

// Citation anchors of the Deep a ciclo (docs/PIANO_DEEP_A_CICLO.md, L4 and §4): for
// each sentence of the answer that links a page the model read, or a page Vedetta
// opened to verify a link, Go picks the passage of that page closest to the
// sentence and saves it with the answer. The judge reads it instead of the
// 300-character search preview; a reader could be shown where a sentence comes from.
//
// Every sentence that states something (claims of 40+ characters, not declared
// deductions), with or without a link, also gets the two closest passages among all
// the pages read and the previews: the judge then asks of every claim whether what
// was read supports it. A link here is often the project's own page, which the user
// asked for, not the source of the description.

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// CitationAnchor ties a sentence of the answer to the passage of a page it links.
type CitationAnchor struct {
	Sentence string `json:"sentence"`
	URL      string `json:"url"`
	Passage  string `json:"passage"`
	Basis    string `json:"basis"`  // "page": read by the model; "verified": opened by Vedetta; "preview"
	Linked   bool   `json:"linked"` // false: one of the closest passages to a claim, wherever it was read
}

// claimMinChars is the length from which a sentence without a link is a claim, as
// the judge's "uncited" sentences (scripts/judge.py).
const claimMinChars = 40

const anchorPassageChars = 1500

var answerLinkTargetRe = regexp.MustCompile(`\]\((https?://(?:\([^()\s]*\)|[^)\s])+)\)`)

// rawSentences splits the answer like answerSentences, keeping the links.
func rawSentences(answer string) []string {
	var out []string
	for _, line := range strings.Split(answer, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Trim(line, "|-: ") == "" {
			continue
		}
		start := 0
		for i := 0; i < len(line)-1; i++ {
			if (line[i] == '.' || line[i] == '!' || line[i] == '?' || line[i] == ';') && line[i+1] == ' ' {
				out = append(out, strings.TrimSpace(line[start:i+1]))
				start = i + 1
			}
		}
		if s := strings.TrimSpace(line[start:]); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// anchorCitations returns one anchor per (sentence, linked page) for the pages in
// texts (normalized URL -> page text, basis), and one per claim without a link,
// anchored to the closest block of pool. deduction is the label Go puts in front
// of a deduction the model declared: such sentences are not claims on the sources.
func anchorCitations(answer string, texts map[string]anchorSource, pool *claimPool, deduction string) []CitationAnchor {
	var out []CitationAnchor
	for _, s := range rawSentences(answer) {
		if strings.HasPrefix(s, "#") {
			continue
		}
		clean := strings.TrimSpace(answerLinkTargetRe.ReplaceAllString(s, "]"))
		claim := claimText(addressTextRe.ReplaceAllStringFunc(bareAddressRe.ReplaceAllString(clean, ""), func(m string) string {
			if inner := strings.Trim(m, "[]"); !looksLikeURL(inner) {
				return inner
			}
			return ""
		}))
		if pool != nil && len(claim) >= claimMinChars && !strings.HasPrefix(claim, strings.Trim(deduction, "*_ :")) {
			out = append(out, pool.closest(claim, 2)...)
		}
		seen := map[string]bool{}
		for _, m := range answerLinkTargetRe.FindAllStringSubmatch(s, -1) {
			k := normalizeLinkURL(m[1])
			src, ok := texts[k]
			if !ok || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, CitationAnchor{Sentence: clean, URL: m[1], Basis: src.basis, Linked: true,
				Passage: selectExcerpt(src.text, clean, anchorPassageChars)})
		}
	}
	return out
}

type anchorSource struct {
	text  string
	basis string
}

// claimText is a sentence as the judge reads it: no Markdown emphasis, list or
// table marks.
func claimText(s string) string {
	s = listMarkerRe.ReplaceAllString(s, "")
	s = strings.NewReplacer("**", "", "__", "", "|", " ", "`", "").Replace(s)
	s = strings.TrimLeft(s, "-*+> ")
	return strings.Join(strings.Fields(s), " ")
}

// claimPool holds the blocks of every text the model saw, lower-cased once.
type claimPool struct {
	chunks []poolChunk
}

type poolChunk struct {
	url, basis  string
	text, lower string
	next        string // the following block of the same page, for a short block
}

func newClaimPool() *claimPool { return &claimPool{} }

// add splits a text the model saw into blocks.
func (p *claimPool) add(url, basis, text string) {
	chunks := splitExcerptChunks(splitExcerptBlocks(text))
	for i, c := range chunks {
		pc := poolChunk{url: url, basis: basis, text: c, lower: strings.ToLower(c)}
		if i+1 < len(chunks) {
			pc.next = chunks[i+1]
		}
		p.chunks = append(p.chunks, pc)
	}
}

// claimTokens are the words of a claim to look for, with their weight: names
// (capitalized or mixed-case words) and numbers weigh 3, since they read the same
// in an Italian answer and an English page; other words weigh 1.
func claimTokens(claim string) map[string]int {
	strong := map[string]bool{}
	for _, w := range strings.FieldsFunc(claim, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '.')
	}) {
		w = strings.Trim(w, "-.")
		if w == "" {
			continue
		}
		rs := []rune(w)
		if unicode.IsUpper(rs[0]) || strings.ContainsFunc(w, unicode.IsDigit) || strings.ContainsFunc(string(rs[1:]), unicode.IsUpper) {
			strong[strings.ToLower(w)] = true
		}
	}
	out := map[string]int{}
	for _, t := range extractSubstantiveTokens(claim) {
		out[t] = 1
	}
	for w := range strong {
		if len(w) >= 2 {
			out[w] = 3
		}
	}
	return out
}

// closest returns the n blocks with the highest weighted overlap with claim, each
// with the next block of its page when it is short. Blocks sharing nothing are
// never returned: a claim with no anchor is not supported by what was read.
func (p *claimPool) closest(claim string, n int) []CitationAnchor {
	tokens := claimTokens(claim)
	type scored struct{ i, score int }
	var best []scored
	for i, c := range p.chunks {
		score := 0
		for t, w := range tokens {
			if strings.Contains(c.lower, t) {
				score += w
			}
		}
		if score == 0 {
			continue
		}
		best = append(best, scored{i, score})
	}
	sort.SliceStable(best, func(a, b int) bool { return best[a].score > best[b].score })
	var out []CitationAnchor
	for _, b := range best {
		if len(out) == n {
			break
		}
		c := p.chunks[b.i]
		passage := c.text
		if len(passage) < anchorPassageChars/2 && c.next != "" {
			passage += "\n\n" + c.next
		}
		out = append(out, CitationAnchor{Sentence: claim, URL: c.url, Basis: c.basis, Passage: clampExcerpt(passage, anchorPassageChars)})
	}
	return out
}
