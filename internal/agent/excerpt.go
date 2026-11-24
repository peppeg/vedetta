package agent

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// DefaultExcerptMaxChars is the default character budget used by
// selectExcerpt when the caller passes a non-positive maxChars.
const DefaultExcerptMaxChars = 10000

// excerptChunkChars bounds the size of a single scoring unit produced by
// splitExcerptChunks. Oversized blocks (e.g. a PDF body extracted by Jina
// where paragraphs are separated by single "\n" rather than "\n\n") are
// broken into chunks of at most this many bytes so the greedy selector can
// pick relevant portions inside them instead of skipping the whole block.
const excerptChunkChars = 2000

// excerptCodeMarkerRegex matches generic code/legislation markers that
// suggest a block likely contains normative content. It is intentionally
// domain-agnostic: only generic legal/administrative patterns are matched.
var excerptCodeMarkerRegex = regexp.MustCompile(`(?i)(?:^|[^[:alnum:]])(?:d\.lgs|d\.l\.|decreto|legge|art\.|articolo|comma|allegato|d\.m\.|pnrr|pas|gazzetta|normattiva)(?:[^[:alnum:]]|$)`)

// excerptNumericCodeRegex matches numeric reference codes such as "123/2021".
var excerptNumericCodeRegex = regexp.MustCompile(`\b\d{1,4}\s*/\s*\d{2,4}\b`)

// isExcerptHeading reports whether s looks like a heading/short label that
// should be attached to the following block: a markdown heading (leading '#')
// or a short line ending with ':'.
func isExcerptHeading(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	if strings.HasPrefix(t, "#") {
		return true
	}
	if len(t) < 80 && strings.HasSuffix(t, ":") {
		return true
	}
	return false
}

// splitExcerptBlocks splits content on blank lines ("\n\n") into trimmed
// blocks. Heading-like blocks are glued onto the block that follows them so
// that a section title stays together with its body text.
func splitExcerptBlocks(content string) []string {
	raw := strings.Split(content, "\n\n")
	var blocks []string
	pending := ""
	for _, r := range raw {
		b := strings.TrimSpace(r)
		if b == "" {
			continue
		}
		if pending != "" {
			b = pending + "\n\n" + b
			pending = ""
		}
		if isExcerptHeading(b) {
			pending = b
			continue
		}
		blocks = append(blocks, b)
	}
	if pending != "" {
		blocks = append(blocks, pending)
	}
	return blocks
}

// splitExcerptChunks splits oversized blocks into smaller chunks so that the
// greedy selector in selectExcerpt can pick relevant portions inside a giant
// block rather than skipping it entirely. Blocks at or below excerptChunkChars
// are returned unchanged. Larger blocks are accumulated line by line (lines
// are separated by "\n"); when a chunk would exceed the budget a new chunk is
// started, and a single line longer than the budget is hard-split on rune
// boundaries. Every returned chunk is at most excerptChunkChars bytes and the
// original order (and text, minus the dropped chunk separators) is preserved.
func splitExcerptChunks(blocks []string) []string {
	chunks := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if len(b) <= excerptChunkChars {
			chunks = append(chunks, b)
			continue
		}
		var sb strings.Builder
		for _, line := range strings.Split(b, "\n") {
			// Hard-split a single line that alone exceeds the budget.
			if len(line) > excerptChunkChars {
				if sb.Len() > 0 {
					chunks = append(chunks, sb.String())
					sb.Reset()
				}
				for len(line) > excerptChunkChars {
					p := utf8Prefix(line, excerptChunkChars)
					chunks = append(chunks, p)
					line = line[len(p):]
				}
				sb.WriteString(line)
				continue
			}
			// +1 accounts for the "\n" separator when the chunk is non-empty.
			extra := len(line)
			if sb.Len() > 0 {
				extra++
			}
			if sb.Len()+extra > excerptChunkChars {
				chunks = append(chunks, sb.String())
				sb.Reset()
			}
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(line)
		}
		if sb.Len() > 0 {
			chunks = append(chunks, sb.String())
		}
	}
	return chunks
}

// scoreExcerptBlock scores a block against the substantive query tokens and
// reports how many distinct query tokens were found. Topic tokens dominate
// generic markers: each distinct matching token adds +8, while a generic
// code/legislation marker adds +1 and a numeric reference code adds +1. The
// marker/numeric bonuses are binary per block, so a block with a single topic
// token always outranks a block full of generic normative markers. Scoring is
// case-insensitive.
func scoreExcerptBlock(block string, tokens []string) (score, topicHits int) {
	lower := strings.ToLower(block)
	for _, tok := range tokens {
		if tok == "" {
			continue
		}
		if strings.Contains(lower, tok) {
			topicHits++
		}
	}
	score = topicHits * 8
	if excerptCodeMarkerRegex.MatchString(block) {
		score++
	}
	if excerptNumericCodeRegex.MatchString(block) {
		score++
	}
	return score, topicHits
}

// clampExcerpt trims s to at most maxChars bytes while keeping the result a
// valid UTF-8 string.
func clampExcerpt(s string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	if len(s) > maxChars {
		s = s[:maxChars]
	}
	return strings.ToValidUTF8(s, "")
}

// utf8Prefix returns the first n bytes of s, backing off to the nearest valid
// UTF-8 rune boundary so the result never ends mid-rune.
func utf8Prefix(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// utf8Suffix returns the last n bytes of s, advancing to the nearest valid
// UTF-8 rune boundary so the result never starts mid-rune.
func utf8Suffix(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if n >= len(s) {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// spreadClamp returns a head + middle + tail sample of content that fits the
// maxChars budget. It is used as a fallback when no block contains a topic
// token, so that long boilerplate documents still surface their beginning, a
// centred fragment and their end instead of a mere head prefix. Segments are
// cut on rune boundaries and sanitised with strings.ToValidUTF8.
func spreadClamp(content string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	if len(content) <= maxChars {
		return strings.ToValidUTF8(content, "")
	}
	const sep = "\n...\n"
	// Not enough room for three meaningful segments plus separators: fall back
	// to a plain head clamp.
	if maxChars <= 2*len(sep)+8 {
		return clampExcerpt(content, maxChars)
	}
	avail := maxChars - 2*len(sep)
	headLen := avail * 2 / 5
	tailLen := avail * 2 / 5
	midLen := avail - headLen - tailLen

	head := utf8Prefix(content, headLen)
	tail := utf8Suffix(content, tailLen)

	midStart := (len(content) - midLen) / 2
	if midStart < headLen {
		midStart = headLen
	}
	for midStart < len(content) && !utf8.RuneStart(content[midStart]) {
		midStart++
	}
	midEnd := midStart + midLen
	if limit := len(content) - tailLen; midEnd > limit {
		midEnd = limit
	}
	for midEnd > midStart && midEnd < len(content) && !utf8.RuneStart(content[midEnd]) {
		midEnd--
	}
	mid := content[midStart:midEnd]

	out := strings.ToValidUTF8(head, "") + sep + strings.ToValidUTF8(mid, "") + sep + strings.ToValidUTF8(tail, "")
	if len(out) > maxChars {
		out = clampExcerpt(out, maxChars)
	}
	return out
}

// selectExcerpt deterministically extracts the most relevant part of a long
// document within a character budget. It returns content unchanged when it
// already fits, otherwise it scores blocks against the substantive tokens of
// targetQuery and reconstructs the selected blocks in original document order.
func selectExcerpt(content, targetQuery string, maxChars int) string {
	if maxChars <= 0 {
		maxChars = DefaultExcerptMaxChars
	}
	if len(content) <= maxChars {
		return content
	}

	blocks := splitExcerptBlocks(content)
	if len(blocks) == 0 {
		return clampExcerpt(content, maxChars)
	}

	// Oversized blocks are broken into chunks so a giant block does not get
	// skipped wholesale by the budget check below.
	chunks := splitExcerptChunks(blocks)
	if len(chunks) == 0 {
		return clampExcerpt(content, maxChars)
	}

	tokens := extractSubstantiveTokens(targetQuery)

	type scoredBlock struct {
		index int
		score int
	}
	scored := make([]scoredBlock, 0, len(chunks))
	topicHitsTotal := 0
	for i, b := range chunks {
		s, hits := scoreExcerptBlock(b, tokens)
		topicHitsTotal += hits
		scored = append(scored, scoredBlock{index: i, score: s})
	}
	if topicHitsTotal == 0 {
		// No block contains any topic token (only boilerplate/markers):
		// sample head + middle + tail instead of returning just the head.
		return spreadClamp(content, maxChars)
	}

	// Highest score first, stable to preserve original order on ties.
	sort.SliceStable(scored, func(a, b int) bool {
		return scored[a].score > scored[b].score
	})

	// Greedily pick blocks that fit the budget (accounting for separators).
	selected := make(map[int]bool)
	total := 0
	for _, sb := range scored {
		if sb.score <= 0 {
			break
		}
		b := chunks[sb.index]
		cost := len(b)
		if total > 0 {
			cost += 2 // "\n\n" separator
		}
		if total+cost > maxChars {
			continue
		}
		selected[sb.index] = true
		total += cost
	}

	if len(selected) == 0 {
		// The most relevant chunk alone exceeds the budget: return its head.
		return clampExcerpt(chunks[scored[0].index], maxChars)
	}

	// Rebuild in original document order.
	parts := make([]string, 0, len(selected))
	for i, b := range chunks {
		if selected[i] {
			parts = append(parts, b)
		}
	}
	return clampExcerpt(strings.Join(parts, "\n\n"), maxChars)
}
