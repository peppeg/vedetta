package agent

// A citation to a search preview must show the reader what the sentence says.
// Counted on the Standard answers of 21-30/10/2026: 13% of the sentences citing a
// preview were rejected by the judge (4% of those citing a fact), and in half of
// them the detail was in a page the session had read. The writer uses the passages
// it is given, then attaches a preview on the same subject. The sentinel fixes it
// here, instead of one more rule in the prompt: when the sentence names numbers,
// code or names the preview does not contain, and the passage of a fact has them
// and covers clearly more of the sentence's words, the citation goes to that fact.
//
// The details alone are not enough: on those same answers they moved 80 sentences
// the preview already supported, and 55 of them ended on a passage that did not.

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"vedetta/internal/search"
)

// retargetMinShare is how many of the details missing from the preview the fact
// must contain. Not all: a sentence can join a detail read with one of its own.
const retargetMinShare = 0.7

// retargetMinGain is how much more of the sentence's words the fact must cover
// than the preview. Chosen on the answers of 21-30/10/2026 (69 moved sentences:
// supported 4 -> 52, rejected 52 -> 6) and checked on those of 11-20/10.
const retargetMinGain = 0.2

var (
	renderedCitationRe = regexp.MustCompile(`\s*\[S?\d+\]`)
	sentenceWordRe     = regexp.MustCompile(`\p{L}{5,}`)
)

// sentenceDetails are the things a reader would look for in the source: numbers
// that say something, inline code, names.
func sentenceDetails(sentence, lang string) (numbers [][]string, words []string) {
	for _, n := range scanNumbers(sentence) {
		if !n.weak && !n.ident && distinctive(n) {
			numbers = append(numbers, n.alts)
		}
	}
	for _, m := range inlineCodeRe.FindAllString(sentence, -1) {
		if c := strings.Trim(m, "` "); c != "" {
			words = append(words, c)
		}
	}
	return numbers, append(words, sentenceNames(sentence, lang)...)
}

// countIn returns how many details are in the text, whose numbers are have.
func countIn(numbers [][]string, words []string, lower string, have map[string]bool) int {
	found := 0
	for _, alts := range numbers {
		for _, alt := range alts {
			if have[alt] {
				found++
				break
			}
		}
	}
	for _, w := range words {
		if strings.Contains(lower, strings.ToLower(w)) {
			found++
		}
	}
	return found
}

// betterFactFor returns the fact to cite instead of the preview, if the sentence
// states details the preview lacks and a fact's passage has.
func betterFactFor(sentence string, snip search.Snippet, cards []FactCard, lang string) (FactCard, bool) {
	sentence = renderedCitationRe.ReplaceAllString(sentence, "")
	numbers, words := sentenceDetails(sentence, lang)
	preview := snip.Title + "\n" + snip.Content
	previewLower, previewNumbers := strings.ToLower(preview), numberSet(preview)

	var missingNumbers [][]string
	for _, alts := range numbers {
		if countIn([][]string{alts}, nil, "", previewNumbers) == 0 {
			missingNumbers = append(missingNumbers, alts)
		}
	}
	var missingWords []string
	for _, w := range words {
		if !strings.Contains(previewLower, strings.ToLower(w)) {
			missingWords = append(missingWords, w)
		}
	}
	missing := len(missingNumbers) + len(missingWords)
	if missing == 0 {
		return FactCard{}, false
	}

	sentenceWords := sentenceWordRe.FindAllString(strings.ToLower(sentence), -1)
	if len(sentenceWords) == 0 {
		return FactCard{}, false
	}
	coverage := func(lower string) float64 {
		covered := 0
		for _, w := range sentenceWords {
			if strings.Contains(lower, w) {
				covered++
			}
		}
		return float64(covered) / float64(len(sentenceWords))
	}
	var best FactCard
	bestCoverage := coverage(previewLower) + retargetMinGain
	found := false
	for _, c := range cards {
		text := c.Claim + "\n" + c.Evidence + "\n" + c.Passage
		lower := strings.ToLower(text)
		if float64(countIn(missingNumbers, missingWords, lower, numberSet(text))) < retargetMinShare*float64(missing) {
			continue
		}
		if cov := coverage(lower); cov > bestCoverage {
			best, bestCoverage, found = c, cov, true
		}
	}
	return best, found
}

// currentSentence is the sentence the stream is in, or the one it just ended: the
// citation follows the sentence it belongs to, before or after the full stop.
func currentSentence(tail string) string {
	tail = strings.TrimRight(tail, " \t")
	for i := len(tail) - 2; i >= 0; i-- {
		switch tail[i] {
		case '\n', '|':
			return strings.TrimSpace(tail[i+1:])
		case '.', '!', '?', ';':
			if tail[i+1] == ' ' && !(tail[i] == '.' && abbreviationBefore(tail[:i])) {
				return strings.TrimSpace(tail[i+1:])
			}
		}
	}
	return strings.TrimSpace(tail)
}

// rememberOutput keeps the last part of what was emitted, for currentSentence.
func (r *Reconciler) rememberOutput(text string) {
	const keep = 1200
	r.tail += text
	if len(r.tail) > 2*keep {
		cut := len(r.tail) - keep
		for cut < len(r.tail) && !utf8.RuneStart(r.tail[cut]) {
			cut++
		}
		r.tail = r.tail[cut:]
	}
}
