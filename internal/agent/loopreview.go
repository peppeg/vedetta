package agent

// The sentinel's turn of the Deep a ciclo (docs/PIANO_DEEP_A_CICLO.md §9, point d):
// the answer is held as a draft; Go looks in what the model saw (pages read,
// search previews, the question) for the distinctive numbers and the names the
// draft states, and sends back the ones it cannot find: "verify or remove". The
// model gets one more turn, with a few tool calls, and writes the answer the user
// sees. Go only decides what is missing; the model decides what to do with it.
// VEDETTA_DEEP_LOOP_REVIEW=false turns it off.
//
// With VEDETTA_DEEP_LOOP_REVIEW_PREVIEWS (§13 step 2) the details found only in
// the short search previews, not in a page read, count too: Vedetta opens the
// result page and sends the sentence back with the page's passage closest to it,
// so the model checks against what the page says, not against its own memory.
// A sentence whose details the page does contain is not sent back.
//
// With VEDETTA_DEEP_LOOP_REVIEW_LINES the model rewrites only the lines sent back and
// Go puts them in the draft (case 19, 05/11/2026): rewriting the whole answer changed
// the sentences nobody had flagged (33% stayed identical, median of 18 sessions), and
// so lost "zeptoverso", and it costs time. The model can still search and read.

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"vedetta/internal/llm"
	"vedetta/internal/search"
)

const (
	loopReviewCalls        = 8    // searches and readings allowed to verify the details sent back
	loopReviewMaxItems     = 25   // sentences sent back at most
	loopReviewPages        = 4    // result pages Vedetta opens for details seen only in previews
	loopReviewPassageChars = 1200 // passage of such a page shown with the sentence
	reviewNameMinRunes     = 3
)

// ReviewItem is a sentence of the draft with the details not found in what was seen.
type ReviewItem struct {
	Sentence string   `json:"sentence"`
	Numbers  []string `json:"numbers,omitempty"`
	Names    []string `json:"names,omitempty"`
	// Details found only in search previews, and the result page Vedetta opened for them.
	Preview []string `json:"preview,omitempty"`
	URL     string   `json:"url,omitempty"`
	Passage string   `json:"passage,omitempty"` // the passage of that page closest to the sentence
	Lacks   []string `json:"lacks,omitempty"`   // preview details the page itself does not contain
	Unread  bool     `json:"unread,omitempty"`  // the page could not be opened
}

// previewOnly merges the two checks of the draft: everywhere holds the details
// found in nothing the model saw, pages those found in no page read (a superset).
// What pages adds is what the model saw only in previews.
func previewOnly(everywhere, pages []ReviewItem) []ReviewItem {
	missing := make(map[string]map[string]bool, len(everywhere))
	for _, it := range everywhere {
		m := map[string]bool{}
		for _, d := range append(append([]string{}, it.Numbers...), it.Names...) {
			m[d] = true
		}
		missing[it.Sentence] = m
	}
	out := make([]ReviewItem, 0, len(pages))
	for _, it := range pages {
		m := missing[it.Sentence]
		item := ReviewItem{Sentence: it.Sentence}
		for _, n := range it.Numbers {
			if m[n] {
				item.Numbers = append(item.Numbers, n)
			} else {
				item.Preview = append(item.Preview, n)
			}
		}
		for _, n := range it.Names {
			if m[n] {
				item.Names = append(item.Names, n)
			} else {
				item.Preview = append(item.Preview, n)
			}
		}
		out = append(out, item)
	}
	return out
}

// absentIn returns the details of sentence among want that text does not contain,
// with the same comparison as reviewDraft.
func absentIn(sentence, lang, text string, want []string) []string {
	left := map[string]bool{}
	for _, it := range reviewDraft(sentence, lang, loopNumberSet(text), text) {
		for _, d := range append(append([]string{}, it.Numbers...), it.Names...) {
			left[d] = true
		}
	}
	var out []string
	for _, d := range want {
		if left[d] {
			out = append(out, d)
		}
	}
	return out
}

// bestPreview is the search result whose preview holds most of the item's
// preview details; ties go to the first result shown.
func bestPreview(it ReviewItem, lang string, previews []search.Result) (search.Result, bool) {
	var best search.Result
	bestN := 0
	for _, r := range previews {
		n := len(it.Preview) - len(absentIn(it.Sentence, lang, r.Title+"\n"+r.Content+"\n"+r.URL, it.Preview))
		if n > bestN {
			best, bestN = r, n
		}
	}
	return best, bestN > 0
}

// reviewDraft returns the sentences of draft whose distinctive numbers are not in
// have or whose names are not in seen. seen is what the model saw (texts and the
// addresses of pages and results, where a site's name often is), compared without
// case, spaces, punctuation or accents: "Orvo Consulting" is in orvoconsulting.example,
// "Réserve" in "Federal Reserve".
func reviewDraft(draft, lang string, have map[string]bool, seen string) []ReviewItem {
	_, misses := checkAnswerNumbers(draft, have)
	numbers := make(map[string][]string, len(misses))
	for _, m := range misses {
		numbers[m.Sentence] = m.Missing
	}
	present := namesInText(seen)
	if nameWords.Load() {
		present = namesAsWords(seen)
	}
	var out []ReviewItem
	for _, s := range answerSentences(draft) {
		if strings.HasPrefix(s, "#") {
			continue // a heading names a topic, it does not state it
		}
		var names []string
		for _, n := range sentenceNames(s, lang) {
			k := nameKey(n)
			if !present(k) && !(isAcronymPlural(n) && present(k[:len(k)-1])) {
				names = append(names, n)
			}
		}
		if len(names) > 0 || len(numbers[s]) > 0 {
			out = append(out, ReviewItem{Sentence: s, Numbers: numbers[s], Names: names})
		}
	}
	return out
}

// nameWords is VEDETTA_DEEP_LOOP_NAME_WORDS: names are looked for as whole words of
// what the model saw, not inside longer words ("Rossi" in "grossi"), and inside the
// addresses of sources and pages read (Piano dopo audit 4.1 / D1). Set once at start.
var nameWords atomic.Bool

// SetNameWords turns the whole-word name check on or off (off: today's).
func SetNameWords(enabled bool) { nameWords.Store(enabled) }

// namesInText is today's check: a name key is present anywhere in what was seen,
// reduced to one string of letters and digits.
func namesInText(seen string) func(key string) bool {
	all := nameKey(seen)
	return func(key string) bool { return strings.Contains(all, key) }
}

// nameAddressMinRunes: a name this long is looked for anywhere inside an address; a
// shorter one only at the start of a part of the site's name ("Orvo" in
// orvoconsulting.example, not "Rossi" in grossi.it).
const nameAddressMinRunes = 5

// namesAsWords is the whole-word check. The lines of seen that are addresses (the
// sources and pages read, one per line) are addresses; the rest are words.
func namesAsWords(seen string) func(key string) bool {
	words := map[string]bool{}
	var addresses []string
	for _, line := range strings.Split(seen, "\n") {
		line = strings.TrimSpace(line)
		if (strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://")) && !strings.ContainsAny(line, " \t") {
			addresses = append(addresses, line)
			continue
		}
		for _, w := range reviewWords(line) {
			for _, part := range nameParts(w) {
				words[nameKey(part)] = true
			}
		}
	}
	return func(key string) bool {
		if key == "" {
			return false
		}
		if words[key] {
			return true
		}
		// A month's abbreviation (Feb) is there when the month is written in full.
		if utf8.RuneCountInString(key) >= 3 {
			for w := range calendarWords {
				if strings.HasPrefix(w, key) && words[w] {
					return true
				}
			}
		}
		for _, a := range addresses {
			if utf8.RuneCountInString(key) >= nameAddressMinRunes && strings.Contains(nameKey(a), key) {
				return true
			}
			if u, err := url.Parse(a); err == nil {
				for _, label := range strings.Split(u.Hostname(), ".") {
					if strings.HasPrefix(nameKey(label), key) {
						return true
					}
				}
			}
		}
		return false
	}
}

// nameParts is a word of a page and the parts a name may be joined into it by:
// dots (Integration.Cryptomator), a change of case (enableHowdy, FreeBSD,
// HTMLParser) and digits (AGPLv3). The whole word stays too.
func nameParts(w string) []string {
	out := []string{w}
	for _, piece := range strings.Split(w, ".") {
		rs := []rune(piece)
		start := 0
		for i := 1; i < len(rs); i++ {
			prev, cur := rs[i-1], rs[i]
			next, after := rune(0), rune(0)
			if i+1 < len(rs) {
				next = rs[i+1]
			}
			if i+2 < len(rs) {
				after = rs[i+2]
			}
			split := (unicode.IsLower(prev) && unicode.IsUpper(cur)) ||
				(unicode.IsLetter(prev) && unicode.IsDigit(cur)) || (unicode.IsDigit(prev) && unicode.IsLetter(cur)) ||
				(unicode.IsUpper(prev) && unicode.IsUpper(cur) && unicode.IsLower(next) && unicode.IsLower(after)) || // HTML|Parser
				(unicode.IsUpper(prev) && unicode.IsLower(cur) && i >= 2 && unicode.IsUpper(rs[i-2]) && !unicode.IsLower(next) && next != 0) // AGPL|v3
			if split {
				if start < i {
					out = append(out, string(rs[start:i]))
				}
				start = i
			}
		}
		if start > 0 || len(rs) != len([]rune(w)) {
			out = append(out, string(rs[start:]))
		}
	}
	return out
}

// nameKey is how a name is looked for: accents come apart from their letters here, and
// alnumLower drops them.
func nameKey(s string) string {
	return alnumLower(norm.NFD.String(s))
}

// sentenceNames are the words of a sentence that name something: a capital letter
// inside the word (PostgreSQL), or at its start. The first word of the sentence, of
// a list item, of a table cell or after a colon or a dash is not counted: it is
// capitalized anyway ("Cielo: sereno", "Examples:"). Neither are month and day names.
// In German every noun is capitalized: only capitals inside a word count.
func sentenceNames(s, lang string) []string {
	var out []string
	seen := map[string]bool{}
	for _, seg := range reviewSegmentRe.Split(s, -1) {
		for i, w := range reviewWords(claimText(seg)) {
			rs := []rune(w)
			if len(rs) < reviewNameMinRunes || seen[w] || strings.ContainsFunc(w, unicode.IsDigit) || calendarWords[strings.ToLower(w)] {
				continue // numbers are the numbers check's
			}
			if len(rs) >= 6 && strings.ToUpper(w) == w {
				continue // a word in capitals for emphasis ("CHIEDO"); acronyms are shorter
			}
			inner := strings.ContainsFunc(string(rs[1:]), unicode.IsUpper)
			if !inner && (i == 0 || !unicode.IsUpper(rs[0]) || lang == "de") {
				continue
			}
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// reviewWords splits a sentence into words, keeping names like Next.js whole and
// splitting compounds: in "WAL-logging" the name is WAL.
func reviewWords(s string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(s, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.')
	}) {
		if w = strings.Trim(w, "."); w != "" {
			out = append(out, w)
		}
	}
	return out
}

// isAcronymPlural: CVEs, APIs.
func isAcronymPlural(w string) bool {
	return len(w) > 2 && strings.HasSuffix(w, "s") && strings.ToUpper(w[:len(w)-1]) == w[:len(w)-1]
}

// reviewSegmentRe: where a capitalized word starts a phrase, not a name.
var reviewSegmentRe = regexp.MustCompile(`[:|–—(\"«“]|[.!?]\**\s`)

var calendarWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields("january february march april may june july august september october november december " +
		"monday tuesday wednesday thursday friday saturday sunday") {
		calendarWords[w] = true
	}
}

// Outcomes of a review that asked only for the lines sent back.
const (
	reviewLinesApplied = "lines"   // the lines of the reply are in the draft
	reviewLinesRewrite = "rewrite" // the model wrote the whole answer anyway: that is the answer
	reviewLinesKept    = "kept"    // nothing usable in the reply: the draft goes out as it was
)

// What the model can write instead of a line: it is right as it is, or it goes.
const (
	reviewLineKeep   = "KEEP"
	reviewLineRemove = "REMOVE"
)

const closingLinesMessage = "No more searches or readings are available. Reply now with the numbered lines, as asked, with what you found, and say what you could not verify."

// splitCode returns the lines of text outside code blocks and those inside them.
func splitCode(text string) (prose, code []string) {
	inside := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inside = !inside
			continue
		}
		if inside {
			code = append(code, line)
		} else {
			prose = append(prose, line)
		}
	}
	return prose, code
}

// outsideCode drops the sentences that are only inside code blocks. Asked to rewrite a
// line of code, the model answered the check in it and the code became prose (series of
// 06/11/2026), and a sentence sent back with no line to write would have nowhere to go.
// The notes still say what was not found.
func outsideCode(draft string, items []ReviewItem) []ReviewItem {
	prose, code := splitCode(draft)
	inProse, inCode := map[string]bool{}, map[string]bool{}
	for _, s := range answerSentences(strings.Join(prose, "\n")) {
		inProse[s] = true
	}
	for _, s := range answerSentences(strings.Join(code, "\n")) {
		inCode[s] = true
	}
	var out []ReviewItem
	for _, it := range items {
		if !inCode[it.Sentence] || inProse[it.Sentence] {
			out = append(out, it)
		}
	}
	return out
}

// reviewLines returns the lines of draft holding the sentences sent back, once each.
func reviewLines(draft string, items []ReviewItem, troubles []LinkTrouble) []LanguageSlip {
	flagged := map[string]bool{}
	for _, it := range items {
		flagged[it.Sentence] = true
	}
	for _, t := range troubles {
		flagged[t.Sentence] = true
	}
	var out []LanguageSlip
	seen := map[string]bool{}
	prose, _ := splitCode(draft)
	for _, line := range prose {
		if seen[line] {
			continue
		}
		var hits []string
		for _, s := range answerSentences(line) {
			if flagged[s] {
				hits = append(hits, s)
			}
		}
		for _, c := range linkClaims(line) {
			if flagged[c.Text] {
				hits = append(hits, c.Text)
			}
		}
		if len(hits) > 0 {
			seen[line] = true
			out = append(out, LanguageSlip{Line: line, Words: hits})
		}
	}
	return out
}

var reviewVerbRe = regexp.MustCompile(`^(KEEP|REMOVE)\b`)

// reviewVerb reads a word in place of a line: alone in any case, or in capitals before a
// reason, which goes nowhere: "KEEP (a rounding of 19.52)". "Keep it short." is a line.
func reviewVerb(text string) string {
	text = strings.Trim(text, " \t*_.")
	for _, v := range []string{reviewLineKeep, reviewLineRemove} {
		if strings.EqualFold(text, v) {
			return v
		}
	}
	return reviewVerbRe.FindString(text)
}

// sameTableColumns refuses a table row that changes its number of columns: it would
// break the table. The row stays as it was.
func sameTableColumns(orig, fixed string) bool {
	return !strings.HasPrefix(strings.TrimSpace(orig), "|") || strings.Count(orig, "|") == strings.Count(fixed, "|")
}

// applyReviewLines puts the lines of the model's reply in the draft: every other line
// stays as it was written. A line missing from the reply stays too, and the notes say
// what is still not found in it.
func applyReviewLines(draft string, lines []LanguageSlip, reply string) (string, []LanguageSlip, string) {
	numbered, other := 0, 0
	var written []string
	removed := map[int]bool{}
	for _, l := range strings.Split(reply, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		m := fixedLineRe.FindStringSubmatch(l)
		if m == nil {
			other++
			continue
		}
		numbered++
		if n, _ := strconv.Atoi(m[1]); n >= 1 && n <= len(lines) {
			switch reviewVerb(m[2]) {
			case reviewLineRemove:
				removed[n-1] = true
				continue
			case reviewLineKeep:
				lines[n-1].Fixed = reviewLineKeep
				continue
			}
		}
		written = append(written, l)
	}
	if len(reply)*2 >= len(draft) && (other > numbered || numbered > len(lines)) {
		return reply, lines, reviewLinesRewrite
	}
	out, lines := applyLineFixes(draft, lines, strings.Join(written, "\n"), sameTableColumns)
	if len(removed) > 0 {
		rows := strings.Split(out, "\n")
		for i := range lines {
			if !removed[i] {
				continue
			}
			for j, row := range rows {
				if row == lines[i].Line {
					rows = append(rows[:j], rows[j+1:]...)
					lines[i].Fixed = reviewLineRemove
					break
				}
			}
		}
		out = strings.TrimLeft(blankRunRe.ReplaceAllString(strings.Join(rows, "\n"), "\n\n"), "\n")
	}
	for _, l := range lines {
		if l.Fixed != "" {
			return out, lines, reviewLinesApplied
		}
	}
	return draft, lines, reviewLinesKept
}

var blankRunRe = regexp.MustCompile(`\n{3,}`)

// withReviewLines turns what the model replied into the answer. Without a review that
// asked for lines, the reply is the answer.
func (l *deepLoop) withReviewLines(reply string) string {
	if l.stats.ReviewLines == nil {
		return reply
	}
	out, lines, how := applyReviewLines(l.stats.Draft, l.stats.ReviewLines, reply)
	l.stats.ReviewLines, l.stats.ReviewLinesHow = lines, how
	rewritten, kept := 0, 0
	for _, r := range lines {
		switch r.Fixed {
		case "":
		case reviewLineKeep:
			kept++
		default:
			rewritten++
		}
	}
	log.Printf("[deeploop] session=%s review lines: %d sent back, %d rewritten, %d kept, outcome %s", l.params.SessionID, len(lines), rewritten, kept, how)
	return out
}

// reviewMessage is what the sentinel sends back to the model.
// researched is false when the draft was written without any search or reading.
// lines, when there are any, are the only part of the answer to write again.
func reviewMessage(items []ReviewItem, links []LinkTrouble, lang string, researched bool, lines []LanguageSlip) string {
	var missing, preview []ReviewItem
	for _, it := range items {
		if len(it.Numbers)+len(it.Names) > 0 {
			missing = append(missing, it)
		}
		if len(it.Preview) > 0 {
			preview = append(preview, it)
		}
	}
	var b strings.Builder
	switch {
	case !researched:
		b.WriteString("You answered without searching or reading anything. These details come from your memory, " +
			"which may be out of date or wrong:\n")
	case len(missing) > 0:
		b.WriteString("Vedetta compared your answer with the pages you read, the search results you saw and the question.\n" +
			"These details do not appear in any of them:\n")
	default:
		b.WriteString("Vedetta compared your answer with the pages you read, the search results you saw and the question.\n")
	}
	for i, it := range missing {
		if i == loopReviewMaxItems {
			fmt.Fprintf(&b, "- and %d more sentences.\n", len(missing)-i)
			break
		}
		details := append(append([]string{}, it.Numbers...), it.Names...)
		fmt.Fprintf(&b, "- «%s»: %s\n", shortSentence(it.Sentence, 240), strings.Join(details, ", "))
	}
	if len(preview) > 0 {
		b.WriteString("\nThese details appear only in the short previews of search results, not in a page you read. " +
			"Vedetta opened the result page and shows the passage closest to your sentence:\n")
	}
	for i, it := range preview {
		if i == loopReviewMaxItems {
			fmt.Fprintf(&b, "- and %d more sentences.\n", len(preview)-i)
			break
		}
		fmt.Fprintf(&b, "- «%s»: %s\n", shortSentence(it.Sentence, 240), strings.Join(it.Preview, ", "))
		switch {
		case it.URL == "":
			b.WriteString("  No page was opened for it: read it, or remove the detail.\n")
		case it.Unread:
			fmt.Fprintf(&b, "  The page %s could not be opened.\n", it.URL)
		default:
			if len(it.Lacks) > 0 {
				fmt.Fprintf(&b, "  The page does not contain: %s.\n", strings.Join(it.Lacks, ", "))
			}
			b.WriteString(loopPageBlock("Passage", "", it.URL, it.Passage))
		}
	}
	if len(preview) > 0 {
		b.WriteString("\nFor a detail with a passage: keep it if the passage says it, correct it if the passage says something else, " +
			"otherwise remove it or say plainly that it is not verified.")
	}
	if len(links) > 0 {
		b.WriteString("\n\nThese sentences link a page you did not read. Vedetta opened it, and it does not say what the sentence says. " +
			"The passage of the page closest to your sentence follows each one:\n")
		for i, t := range links {
			if i == loopReviewMaxItems {
				fmt.Fprintf(&b, "- and %d more sentences.\n", len(links)-i)
				break
			}
			fmt.Fprintf(&b, "- «%s»\n", shortSentence(t.Sentence, 240))
			if len(t.Lacks) > 0 {
				fmt.Fprintf(&b, "  The page does not contain: %s.\n", strings.Join(t.Lacks, ", "))
			}
			b.WriteString(loopPageBlock("Passage", "", t.URL, t.Passage))
		}
		b.WriteString("\nFor these: correct the sentence to what the passage says, or cite another page you read that says it, " +
			"or say plainly that it is not verified and remove the link. Do not keep a link to a page for something it does not say.")
	}
	b.WriteString("\nFor each one: check it with search or read, or remove it, or say plainly that it is not verified. " +
		"If it is only a translation or another spelling of something in the sources, keep it. " +
		"If it is your own calculation from figures in the sources (a total, a difference, a range), keep it: it needs no source. ")
	if len(lines) > 0 {
		fmt.Fprintf(&b, "\nThe rest of your answer stays as it is. When you have finished checking, reply only with these lines, in %s, "+
			"one per line, each starting with its number and a colon, like \"1: …\". Keep what is right in each line, with its links and Markdown. "+
			"To drop a whole line, write \"1: REMOVE\". If a line is right as it is, write only \"1: KEEP\", with no reasons. "+
			"The user has not seen this check: the lines do not mention it.\n\n",
			GetLanguageName(lang))
		for i, l := range lines {
			fmt.Fprintf(&b, "%d: %s\n", i+1, l.Line)
		}
		return b.String()
	}
	fmt.Fprintf(&b, "Then write the complete answer again, in %s. The user has not seen the previous one nor this check: "+
		"the new answer replaces it and does not mention the check.",
		GetLanguageName(lang))
	return b.String()
}

// After a send-back the model may answer the check instead of the reader: "Grazie del
// riscontro. Verifico i due punti", "All three flagged items check out" (7 answers of
// 72 in the series of 05/11/2026), though reviewMessage asks it not to. Telling it
// again would change nothing, and word counts cannot tell these lines from a first
// paragraph rewritten by the correction, which may be the conclusion of the answer.
// So Go finds the opening of the rewrite and the model says what each paragraph is.
const (
	checkTalkMaxParagraphs  = 6
	checkTalkParagraphChars = 1500
	checkTalkDeadline       = 2 * time.Minute
)

const checkTalkPrompt = "An assistant wrote an answer, a checker sent it back with remarks, and the assistant wrote it again. " +
	"The reader never saw the first version or the remarks.\n" +
	"Below are the first paragraphs of the new version, numbered. For each one say what it is:\n" +
	"CHECK: it talks about that check: it replies to the checker, says which items were flagged, confirmed, corrected or removed, or announces the corrected or complete version.\n" +
	"ANSWER: it is meant for the reader: an introduction, the answer itself, or what the research found or could not find, even in the first person.\n" +
	"When in doubt, ANSWER.\n" +
	"Reply only with one line per paragraph, like \"1: CHECK\" or \"2: ANSWER\".\n\n"

var (
	paragraphBreakRe = regexp.MustCompile(`\n[ \t\r]*\n`)
	ruleLineRe       = regexp.MustCompile(`^[-*_]{3,}$`)
	checkTalkLineRe  = regexp.MustCompile(`(?im)^\W*(\d+)\W+(CHECK|ANSWER)\b`)
)

// openingParagraphs returns the paragraphs at the top of a rewrite that could be
// talking to the check: it stops at what is plainly the answer, a heading, a rule, a
// table or code, and at a paragraph the draft already had, which cannot be a reply to
// remarks written after it.
func openingParagraphs(text, draft string) []string {
	had := map[string]bool{}
	for _, p := range paragraphBreakRe.Split(draft, -1) {
		had[strings.TrimSpace(p)] = true
	}
	var out []string
	for _, p := range paragraphBreakRe.Split(text, -1) {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(out) == checkTalkMaxParagraphs || had[p] || ruleLineRe.MatchString(p) ||
			strings.HasPrefix(p, "#") || strings.HasPrefix(p, "|") || strings.HasPrefix(p, "```") {
			break
		}
		out = append(out, p)
	}
	return out
}

func checkTalkMessage(paragraphs []string) string {
	var b strings.Builder
	b.WriteString(checkTalkPrompt)
	for i, p := range paragraphs {
		if len(p) > checkTalkParagraphChars {
			p = strings.ToValidUTF8(p[:checkTalkParagraphChars], "")
		}
		fmt.Fprintf(&b, "%d. %s\n\n", i+1, p)
	}
	return b.String()
}

// checkTalkCount reads the reply: how many paragraphs, from the first on without a gap,
// talk about the check. A missing or unclear line ends the count.
func checkTalkCount(reply string, n int) int {
	verdict := map[int]string{}
	for _, m := range checkTalkLineRe.FindAllStringSubmatch(reply, -1) {
		if i, err := strconv.Atoi(m[1]); err == nil {
			verdict[i] = strings.ToUpper(m[2])
		}
	}
	count := 0
	for count < n && verdict[count+1] == "CHECK" {
		count++
	}
	return count
}

// cutOpening removes the first paragraphs and a rule that follows them. Most of the
// text must remain: a reply that would leave little of the answer is not believed.
func cutOpening(text string, paragraphs []string) string {
	rest := strings.TrimSpace(text)
	for _, p := range paragraphs {
		if !strings.HasPrefix(rest, p) {
			return text
		}
		rest = strings.TrimSpace(rest[len(p):])
	}
	if first, after, found := strings.Cut(rest, "\n"); found && ruleLineRe.MatchString(strings.TrimSpace(first)) {
		rest = strings.TrimSpace(after)
	}
	if len(rest)*2 < len(strings.TrimSpace(text)) {
		return text
	}
	return rest
}

// cutCheckTalk takes from a rewrite the opening that talks about the check. On any
// trouble the answer goes out as it was.
func (l *deepLoop) cutCheckTalk(ctx context.Context, text string) string {
	if l.client == nil {
		return text
	}
	paragraphs := openingParagraphs(text, l.stats.Draft)
	if len(paragraphs) == 0 {
		return text
	}
	askCtx, cancel := context.WithTimeout(ctx, checkTalkDeadline)
	defer cancel()
	var reply strings.Builder
	err := l.client.StreamChatWithOptions(askCtx, []llm.Message{{Role: "user", Content: checkTalkMessage(paragraphs)}},
		llm.ChatOptions{Temperature: llm.Float64Ptr(0), Effort: llm.EffortOff}, func(tok llm.StreamToken) error {
			if tok.Type == llm.TokenContent {
				reply.WriteString(tok.Text)
			}
			return nil
		})
	if err != nil {
		log.Printf("[deeploop] session=%s check talk: no reply (%v), answer kept as it was", l.params.SessionID, err)
		return text
	}
	n := checkTalkCount(reply.String(), len(paragraphs))
	if n == 0 {
		return text
	}
	cut := cutOpening(text, paragraphs[:n])
	if cut == text {
		log.Printf("[deeploop] session=%s check talk: %d paragraphs not cut, too much of the answer", l.params.SessionID, n)
		return text
	}
	log.Printf("[deeploop] session=%s check talk: %d opening paragraphs cut", l.params.SessionID, n)
	l.stats.CheckTalk = paragraphs[:n]
	return cut
}
