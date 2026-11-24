package agent

// L4 of the Deep a ciclo (docs/PIANO_DEEP_A_CICLO.md): every number of the answer
// (stars, versions, prices, dates, sizes) is looked for in what the model saw:
// the pages it read, the search previews and the question. Numbers that are in
// none of them are listed in the note, with their sentence. Go only, no LLM: the
// same normalizer as the extractor's check (numbers.go), so "1.000", "1 000" and
// "mille" are one number.

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// NumberMiss is a sentence of the answer with numbers not found in the sources.
type NumberMiss struct {
	Sentence string   `json:"sentence"`
	Missing  []string `json:"missing"`
}

var (
	mdLinkTargetRe = regexp.MustCompile(`\]\((?:\([^()\s]*\)|[^)\s])*\)`)
	bareAddressRe  = regexp.MustCompile(`https?://\S+`)
	addressTextRe  = regexp.MustCompile(`\[[^\]\n]*\]`)
	listMarkerRe   = regexp.MustCompile(`^\s*(?:[-*+]\s+)?(?:#+\s*)?\d+[.)]\s+`)
	// The numbering of a heading, "### 2.1 Docker", and a reference to it, "§5.2",
	// count the answer's own sections: they are not claims (case 03/11/2026).
	headingNumberRe = regexp.MustCompile(`^(\s*#+\s*)\d{1,2}(?:\.\d{1,2})*[.)]?\s+`)
	sectionRefRe    = regexp.MustCompile(`§\s*\d+(?:\.\d+)*`)
)

// todayTexts are the ways the answer may write today's date, which the model
// knows from the system prompt.
func todayTexts(now time.Time) []string {
	return []string{now.Format("2006-01-02"), now.Format("2 January 2006"),
		fmt.Sprintf("%d %s %d", now.Day(), italianMonths[now.Month()-1], now.Year())}
}

var italianMonths = []string{"gennaio", "febbraio", "marzo", "aprile", "maggio", "giugno",
	"luglio", "agosto", "settembre", "ottobre", "novembre", "dicembre"}

// answerSentences splits the answer into sentences to check, without link
// addresses (a URL holds dates and versions that are not claims), link texts
// that are addresses, list numbering, and the numbering of sections.
func answerSentences(answer string) []string {
	text := mdLinkTargetRe.ReplaceAllString(answer, "]")
	text = bareAddressRe.ReplaceAllString(text, "")
	text = addressTextRe.ReplaceAllStringFunc(text, func(m string) string {
		if inner := strings.Trim(m, "[]"); !looksLikeURL(inner) {
			return inner
		}
		return ""
	})
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = headingNumberRe.ReplaceAllString(line, "$1")
		line = listMarkerRe.ReplaceAllString(line, "")
		line = sectionRefRe.ReplaceAllString(line, "§")
		line = strings.TrimSpace(line)
		if line == "" || strings.Trim(line, "|-: ") == "" {
			continue
		}
		start := 0
		for i := 0; i < len(line)-1; i++ {
			if (line[i] == '.' || line[i] == '!' || line[i] == '?' || line[i] == ';') && line[i+1] == ' ' && !(line[i] == '.' && abbreviationBefore(line[:i])) {
				if s := strings.TrimSpace(line[start : i+1]); s != "" {
					out = append(out, s)
				}
				start = i + 1
			}
		}
		if s := strings.TrimSpace(line[start:]); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// abbreviations do not end a sentence: "L. 7 agosto 1990, n. 241, artt. 12–15 del
// D.Lgs. 33/2013" is one (case 14, 29/10/2026). "ecc." and "etc." often do end one.
var abbreviations = map[string]bool{}

func init() {
	for _, w := range strings.Fields("n nn art artt co lett par es cfr pag pp vol cap fig sig dott dr avv on prof ing " +
		"no nos vs approx d.lgs lgs d.l d.p.r d.m s.p.a s.r.l p.a u.s e.g i.e") {
		abbreviations[w] = true
	}
}

// abbreviationBefore reports whether the word that ends s, before a period, is an
// abbreviation or a single letter (an initial: "J. R. R. Tolkien", "L. 241").
func abbreviationBefore(s string) bool {
	start := strings.LastIndexAny(s, " (\"«“'") + 1
	w := strings.ToLower(strings.TrimLeft(s[start:], "*_"))
	return utf8.RuneCountInString(w) == 1 || abbreviations[w]
}

// checkAnswerNumbers returns how many numbers the answer states and the sentences
// with numbers that are not in have.
func checkAnswerNumbers(answer string, have map[string]bool) (checked int, misses []NumberMiss) {
	for _, s := range answerSentences(answer) {
		var missing []string
		codes := codeNumbers(s, have)
		for _, n := range scanNumbers(s) {
			if n.weak || n.ident || !distinctive(n) {
				continue
			}
			checked++
			found := false
			for _, alt := range n.alts {
				if have[alt] || codes[alt] {
					found = true
					break
				}
			}
			if !found {
				missing = append(missing, n.label())
			}
		}
		if len(missing) > 0 {
			misses = append(misses, NumberMiss{Sentence: s, Missing: missing})
		}
	}
	return checked, misses
}

// MissingAnswerNumbers runs the Deep a ciclo's number check of answer against texts and
// returns the sentences with numbers found in none of them. For the bench (Piano dopo
// audit 5.3, scripts/claimnumbers -loop): the same check as the product, so a measure
// never follows a rule of its own.
func MissingAnswerNumbers(answer string, texts ...string) []NumberMiss {
	_, misses := checkAnswerNumbers(answer, loopNumberSet(texts...))
	return misses
}

// numbersSeen is the evidence of the number check: the numbers of texts, what the
// pages that did not open answered (the model is told "the site answered 502", and
// may say it), and the codes written in addresses (case 24h, 19/11/2026).
func (l *deepLoop) numbersSeen(texts, addresses []string) map[string]bool {
	all := append([]string{}, texts...)
	for _, why := range l.unreadable {
		all = append(all, bareAddressRe.ReplaceAllString(why, ""))
	}
	have := loopNumberSet(all...)
	for _, u := range addresses {
		_, path := splitLinkHostPath(normalizeLinkURL(u))
		words := strings.Fields(canonicalLinkPath(path))
		for i, w := range words {
			if r, _ := utf8.DecodeRuneInString(w); !unicode.IsLetter(r) {
				continue
			}
			for n := 2; n <= codeMaxParts && i+n <= len(words); n++ {
				have[codeKey(strings.Join(words[i:i+n], " "))] = true
			}
		}
	}
	return have
}

// A code starts with letters and joins letters and numbers: IP/26/1276,
// CVE-2026-24471. The model may know it only from an address (".../ip_26_1276/"),
// and it is looked for there whole: single numbers of an address (dates, page ids)
// would cover wrong quantities.
var answerCodeRe = regexp.MustCompile(`\p{L}[\p{L}\p{N}]*(?:[/_.-][\p{L}\p{N}]+)+`)

const codeMaxParts = 6

func codeKey(canonicalPath string) string { return "code:" + canonicalPath }

// codeNumbers returns the numbers of the codes of a sentence that are in have.
func codeNumbers(sentence string, have map[string]bool) map[string]bool {
	var out map[string]bool
	for _, code := range answerCodeRe.FindAllString(sentence, -1) {
		if !have[codeKey(canonicalLinkPath(code))] {
			continue
		}
		if out == nil {
			out = map[string]bool{}
		}
		for _, n := range scanNumbers(code) {
			for _, alt := range n.alts {
				out[alt] = true
			}
		}
	}
	return out
}

// loopNumberSet is numberSet plus the roundings an answer writes for a number it
// found: 1.331,75 € as "1.332" or "≈1.330", 1.102 $ as "1.100", 539 as "540" (case
// 13, 29/10/2026), 3.250.077 as "3,25 milioni", 19,52 as "19,5" (bench, 06/11/2026).
// Rounding goes one way: "19,52" is not found in a page that says 19,5.
func loopNumberSet(texts ...string) map[string]bool {
	have := numberSet(texts...)
	var found []float64
	for alt := range have {
		if v, err := strconv.ParseFloat(alt, 64); err == nil {
			found = append(found, v)
		}
	}
	for _, v := range found {
		for _, r := range roundings(v) {
			have[canonical(r)] = true
		}
	}
	return have
}

// roundings are the shorter ways an answer may write v. A value with only one digit
// that counts is not among them ("0,3" for 0,276): it would fit too much of a page.
func roundings(v float64) []float64 {
	var out []float64
	if v >= 100 {
		out = append(out, math.Round(v), math.Round(v/10)*10)
	}
	if v >= 1000 {
		out = append(out, math.Round(v/100)*100)
	}
	if v >= 10000 {
		out = append(out, significantDigits(v, 3), significantDigits(v, 4))
	}
	if text := strconv.FormatFloat(v, 'f', -1, 64); strings.Contains(text, ".") {
		for places := len(text) - strings.Index(text, ".") - 2; places >= 1; places-- {
			r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', places, 64), 64)
			if v >= 1 || r >= math.Pow(10, float64(1-places)) {
				out = append(out, r)
			}
		}
	}
	return out
}

// significantDigits keeps the first n digits of v, which is at least 1.
func significantDigits(v float64, n int) float64 {
	unit := math.Pow(10, float64(powerOfTen(v)+1-n))
	return math.Round(v/unit) * unit
}

// distinctive numbers say something when found: 100 or more, a decimal, a
// count in thousands. Small integers are in almost any page, so finding them
// proves nothing, and they are not counted.
func distinctive(n number) bool {
	v, err := strconv.ParseFloat(n.alts[0], 64)
	return err != nil || v >= 100 || v != math.Trunc(v)
}

const maxNumberMissesShown = 8

// numbersNote lists the numbers not found, in the answer language.
func numbersNote(lang string, checked int, misses []NumberMiss) string {
	if len(misses) == 0 {
		if checked == 0 {
			return ""
		}
		return "\n\n*" + StepMessage(lang, "loop_numbers_ok", checked) + "*"
	}
	missing := 0
	for _, m := range misses {
		missing += len(m.Missing)
	}
	var b strings.Builder
	b.WriteString("\n\n*" + StepMessage(lang, "loop_numbers_missing", missing, checked) + "*\n")
	for i, m := range misses {
		if i == maxNumberMissesShown {
			b.WriteString("- " + StepMessage(lang, "loop_numbers_more", len(misses)-i) + "\n")
			break
		}
		fmt.Fprintf(&b, "- «%s» (%s)\n", shortSentence(m.Sentence, 140), strings.Join(m.Missing, ", "))
	}
	return b.String()
}

// shortSentence cuts s to about n bytes at a word boundary, without the
// Markdown emphasis that would break the note's formatting.
func shortSentence(s string, n int) string {
	s = strings.NewReplacer("**", "", "*", "", "`", "", "«", "", "»", "").Replace(s)
	if len(s) <= n {
		return s
	}
	cut := strings.LastIndexByte(s[:n], ' ')
	if cut < n/2 {
		cut = n
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "…"
}
