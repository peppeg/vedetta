package agent

// Where each sentence of an answer comes from (Piano dopo audit, fase 6, docs/SPEC_FASE6_CITAZIONI.md).
// After the answer is out, the model is asked for the passage of a page it read that says
// each sentence, and Go looks for that passage on the page. These are the pure
// functions of that step: nothing here calls the model.

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"vedetta/internal/llm"
)

const (
	// loopQuotesMaxUnits is the most sentences asked in one request; the others are "unasked".
	loopQuotesMaxUnits = 100
	// loopQuoteMinWords: two words are found everywhere and prove nothing.
	loopQuoteMinWords = 5
	// loopQuoteMaxWords: a longer passage is checked on its first words.
	loopQuoteMaxWords = 80
	// loopQuotesDeadline is the most the request may take.
	loopQuotesDeadline = 4 * time.Minute
	// loopQuotesLogChars is how much of a reply out of format goes in the log.
	loopQuotesLogChars = 300
)

// The states of a sentence.
const (
	quoteFound    = "found"     // a passage the model gave is on a page it read
	quoteNotFound = "not_found" // the model gave a passage and Go finds it on no page
	quoteShort    = "short"     // the model gave only passages under loopQuoteMinWords, which Go does not look for
	quoteNone     = "none"      // the model says no page says it, or gave no readable line
	quoteUnasked  = "unasked"   // over the cap
)

// quotePage is a page the model may quote: its label in the request, and its words, prepared once.
type quotePage struct {
	Label string // "P1", "P2", …
	URL   string
	Text  string // what the model got: the whole page, or its beginning when cut
	toks  []alignToken
}

func newQuotePage(label, url, text string) quotePage {
	return quotePage{Label: label, URL: url, Text: text, toks: alignTokens(text)}
}

// QuoteRef is one passage the model gave for a sentence, and what Go found.
type QuoteRef struct {
	URL      string  `json:"url,omitempty"`      // the page where the passage is; the page named, when it is on none
	Quote    string  `json:"quote"`              // as the model wrote it
	Evidence string  `json:"evidence,omitempty"` // the page's own words (evidenceText), when found
	Match    string  `json:"match,omitempty"`    // only the page's words the passage aligned with (matchText), for the list in the notes
	Score    float64 `json:"score,omitempty"`
	Found    bool    `json:"found"`
	Moved    bool    `json:"moved,omitempty"` // found on another page than the one named
	Short    bool    `json:"short,omitempty"` // under loopQuoteMinWords: not looked for, so not "not found"
}

// QuoteItem is a sentence of the answer and where it comes from.
type QuoteItem struct {
	Sentence   string     `json:"sentence"`
	State      string     `json:"state"` // found, not_found, short, none, unasked
	Refs       []QuoteRef `json:"refs,omitempty"`
	NumbersOut []string   `json:"numbers_out,omitempty"` // numbers of the sentence not in the passage found
}

// parsedQuote is a line of the model's reply: the page it names and the passage.
type parsedQuote struct {
	Label string
	Quote string
}

// quoteUnits returns the sentences to ask a passage for: the claims of the answer of
// claimMinChars or more, without the declared deductions (deduction is the label Go
// puts in front of them), the list of sources at the end, and the lines that
// describe the work done, which another check already owns.
func quoteUnits(answer, deduction string) []string {
	var lines []string
	inSources := false
	for _, line := range strings.Split(answer, "\n") {
		s := strings.TrimSpace(line)
		switch {
		case sourcesHeadRe.MatchString(s) || sourcesLineRe.MatchString(s):
			inSources = true
			continue
		case strings.HasPrefix(s, "#"):
			inSources = false
		}
		if !inSources {
			lines = append(lines, line)
		}
	}
	mark := strings.Trim(deduction, "*_ :")
	var out []string
	for _, c := range answerClaims(strings.Join(lines, "\n")) {
		text := strings.TrimSpace(c.Text)
		if len([]rune(text)) < claimMinChars {
			continue
		}
		if mark != "" && strings.HasPrefix(strings.Trim(text, "*_ "), mark) {
			continue
		}
		if len(workClaimLines(text)) > 0 {
			continue
		}
		out = append(out, text)
	}
	return out
}

// quotesMessage is the request. A fixed text, with no example from a real case.
const quotesMessage = "The answer above is final and will not change. For each numbered sentence below, taken from it, " +
	"give the passage of a page you read that says it. Copy the passage word for word from the page, in the language of " +
	"the page: do not translate it, do not shorten it inside. One or two sentences, 40 words at most.\n" +
	"Reply with one line per sentence and nothing else: the number, the page, the passage in quotes, separated by |. " +
	"If no page you read says it, because it is your own deduction, a calculation, or something you know from elsewhere, " +
	"write the number and none. A second passage for the same sentence goes on a line of its own, with the same number.\n" +
	"The lines look like this:\n1 | P2 | \"…\"\n2 | none\n"

// quotesRequest is the user message: the request, the pages by label, the sentences by number.
func quotesRequest(pages []quotePage, units []string) string {
	var b strings.Builder
	b.WriteString(quotesMessage)
	b.WriteString("\nPAGES:\n")
	for _, p := range pages {
		fmt.Fprintf(&b, "%s = %s\n", p.Label, p.URL)
	}
	b.WriteString("\nSENTENCES:\n")
	for i, u := range units {
		fmt.Fprintf(&b, "%d. %s\n", i+1, u)
	}
	return b.String()
}

var quoteLineRe = regexp.MustCompile(`(?im)^[ \t]*\**[ \t]*(\d+)[ \t]*\**[ \t]*(?:\||[:.])[ \t]*\|?[ \t]*(\S.*)$`)

// parseQuotes reads only the lines of the format, whatever else the reply says, or a
// page made it say, is ignored. A sentence the model answered "none" (also after a page:
// "3 | P2 | none") has an empty entry; one with no line has none. At most two passages
// per sentence.
func parseQuotes(reply string, n int) map[int][]parsedQuote {
	out := map[int][]parsedQuote{}
	for _, m := range quoteLineRe.FindAllStringSubmatch(reply, -1) {
		var i int
		if _, err := fmt.Sscanf(m[1], "%d", &i); err != nil || i < 1 || i > n {
			continue
		}
		rest := strings.TrimSpace(m[2])
		label, quote := rest, ""
		if k := strings.Index(rest, "|"); k >= 0 {
			label, quote = rest[:k], rest[k+1:]
		}
		label = strings.Trim(strings.TrimSpace(label), "*_ ")
		switch {
		case strings.EqualFold(strings.Trim(label, ".*_ "), "none") && quote == "":
			if _, ok := out[i]; !ok {
				out[i] = []parsedQuote{}
			}
		case quotePageLabelRe.MatchString(label) || strings.HasPrefix(strings.ToLower(label), "http"):
			quote = cleanQuote(quote)
			if strings.EqualFold(strings.Trim(quote, ".*_ "), "none") {
				// "12 | P3 | none": the model names a page and then says none, which is a none
				if _, ok := out[i]; !ok {
					out[i] = []parsedQuote{}
				}
				continue
			}
			if quotePageLabelRe.MatchString(label) {
				label = strings.ToUpper(label)
			}
			if quote == "" || len(out[i]) >= 2 {
				continue
			}
			out[i] = append(out[i], parsedQuote{Label: label, Quote: quote})
		}
	}
	return out
}

var quotePageLabelRe = regexp.MustCompile(`(?i)^P\d+$`)

// cleanQuote takes off the outer quotation marks and the backticks.
func cleanQuote(s string) string {
	s = strings.Trim(strings.TrimSpace(s), "`")
	s = strings.TrimSpace(s)
	for _, pair := range [][2]string{{`"`, `"`}, {"“", "”"}, {"«", "»"}} {
		if strings.HasPrefix(s, pair[0]) && strings.HasSuffix(s, pair[1]) && len(s) >= len(pair[0])+len(pair[1]) {
			s = strings.TrimSuffix(strings.TrimPrefix(s, pair[0]), pair[1])
			break
		}
	}
	return strings.TrimSpace(s)
}

// pageByLabel finds the page the model named: by label (P2) or by address.
func pageByLabel(pages []quotePage, label string) int {
	for i, p := range pages {
		if strings.EqualFold(p.Label, label) {
			return i
		}
	}
	if k := normalizeLinkURL(label); k != "" && strings.HasPrefix(strings.ToLower(label), "http") {
		for i, p := range pages {
			if normalizeLinkURL(p.URL) == k {
				return i
			}
		}
	}
	return -1
}

// verifyQuote looks for the passage on the page named, then on the others.
func verifyQuote(pages []quotePage, label, quote string) QuoteRef {
	ref, _, _ := verifyQuoteAt(pages, label, quote)
	return ref
}

// verifyQuoteAt is verifyQuote that also returns the page index and the byte spans of
// the passage on it (page -1 when not found).
func verifyQuoteAt(pages []quotePage, label, quote string) (QuoteRef, int, []evidenceSpan) {
	ref := QuoteRef{Quote: quote}
	named := pageByLabel(pages, label)
	if named >= 0 {
		ref.URL = pages[named].URL
	}
	toks := alignTokens(quote)
	if len(toks) < loopQuoteMinWords {
		ref.Short = true
		return ref, -1, nil
	}
	probe := quote
	if len(toks) > loopQuoteMaxWords {
		probe = quote[:toks[loopQuoteMaxWords-1].end]
	}
	order := make([]int, 0, len(pages))
	if named >= 0 {
		order = append(order, named)
	}
	for i := range pages {
		if i != named {
			order = append(order, i)
		}
	}
	for _, i := range order {
		spans, score, ok := locateEvidenceIn(pages[i].toks, probe)
		if !ok {
			continue
		}
		ref.Found, ref.Score, ref.URL, ref.Moved = true, score, pages[i].URL, i != named
		ref.Evidence = evidenceText(pages[i].Text, spans)
		ref.Match = matchText(pages[i].Text, spans)
		return ref, i, spans
	}
	return ref, -1, nil
}

// matchText returns the page's own words the passage aligned with, from the first aligned word
// to the last, collapsed to single spaces; the parts of a quote with an ellipsis are joined with
// " … ", as in evidenceText. Cut on a word at evidenceMaxChars characters.
func matchText(doc string, spans []evidenceSpan) string {
	parts := make([]string, 0, len(spans))
	for _, sp := range spans {
		parts = append(parts, cleanSpan(safeSlice(doc, sp.start, sp.end)))
	}
	return cutOnWord(strings.Join(parts, " … "), evidenceMaxChars, evidenceMaxChars)
}

// cutOnWord leaves s as it is up to max characters; a longer s is cut to keep characters (never
// more than max), back to the last space when there is one in the second half, with "…".
func cutOnWord(s string, keep, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	keep = min(keep, max)
	cut := keep
	for i := keep; i >= keep/2 && i > 0; i-- {
		if r[i] == ' ' {
			cut = i
			break
		}
	}
	return strings.TrimSpace(string(r[:cut])) + "…"
}

// settleQuotes verifies the passages given for each sentence and fixes its state:
// found if one is on a page, not_found if one long enough to be looked for is on none, short if
// all of them are too short to prove anything, none if the model gave no passage.
// lines counts the sentences with a readable line, "none" included.
func settleQuotes(units []string, parsed map[int][]parsedQuote, pages []quotePage) (items []QuoteItem, lines int) {
	for i, u := range units {
		item := QuoteItem{Sentence: u, State: quoteNone}
		given, ok := parsed[i+1]
		if ok {
			lines++
		}
		var passages []string // the passages found, all of them: a number may be in the second
		for _, q := range given {
			ref, at, spans := verifyQuoteAt(pages, q.Label, q.Quote)
			item.Refs = append(item.Refs, ref)
			if ref.Found {
				item.State = quoteFound
				start, end := spanBounds(spans)
				passages = append(passages, passageText(pages[at].Text, start, end, passageMaxChars))
			}
		}
		if item.State == quoteFound {
			item.NumbersOut = numbersOutOf(u, strings.Join(passages, "\n\n"))
		}
		if item.State != quoteFound && len(item.Refs) > 0 {
			item.State = quoteShort
			for _, r := range item.Refs {
				if !r.Short {
					item.State = quoteNotFound
				}
			}
		}
		items = append(items, item)
	}
	return items, lines
}

// numbersOutOf returns the numbers of the sentence, small ones too, that the passage does not have,
// each once and named as the sentence writes it ("1,6", not "1.6") when it is written in digits.
func numbersOutOf(sentence, passage string) []string {
	have := loopNumberSet(passage)
	written := map[string]string{}
	for _, t := range tokenizeNumbers(sentence) {
		if k := canonical(t.value); t.digits && t.text != "" && written[k] == "" {
			written[k] = t.text
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, n := range scanNumbers(sentence) {
		if n.weak || n.ident {
			continue
		}
		found := false
		for _, alt := range n.alts {
			if have[alt] {
				found = true
				break
			}
		}
		if found {
			continue
		}
		name := n.label()
		if w := written[name]; w != "" && n.vague == "" {
			name = w
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// askQuotes asks the model, after the answer is out, for the passage of a page it read
// that says each sentence, and settles them on the pages. It never changes the answer: on
// any trouble it only records why (LoopStats.Quotes). It is a call of its own, with the
// conversation but without tools, and a copy of it: messages does not change.
func (l *deepLoop) askQuotes(ctx context.Context, messages []llm.Message, answer, deduction string, partial bool) {
	body := withoutNotes(answer)
	if l.stats.Calls == 0 || strings.TrimSpace(body) == "" {
		return
	}
	if partial || ctx.Err() != nil {
		l.stats.Quotes = "skipped"
		return
	}
	var pages []quotePage
	for i, k := range l.pageOrder {
		if p := l.pages[k]; p != nil {
			pages = append(pages, newQuotePage("P"+strconv.Itoa(i+1), p.URL, p.Text))
		}
	}
	if len(pages) == 0 {
		l.stats.Quotes = "no_pages"
		return
	}
	units := quoteUnits(body, deduction)
	if len(units) == 0 {
		l.stats.Quotes = "no_sentences"
		return
	}
	asked := units
	if len(asked) > loopQuotesMaxUnits {
		asked = units[:loopQuotesMaxUnits]
	}
	request := quotesRequest(pages, asked)
	if conversationChars(messages)+2*len(body)+len(request) > l.limit {
		l.stats.Quotes = "no_room"
		return
	}
	asking := append(append([]llm.Message{}, messages...),
		llm.Message{Role: "assistant", Content: body}, llm.Message{Role: "user", Content: request})
	l.stats.QuotePages = len(pages)
	l.step("deep_critic", StepMessage(l.params.UILanguage, "loop_quotes"))
	qctx, cancel := context.WithTimeout(ctx, loopQuotesDeadline)
	defer cancel()
	var reply strings.Builder
	start := time.Now()
	err := l.client.StreamChatWithOptions(qctx, oneUserAtATime(asking), llm.ChatOptions{Temperature: llm.Float64Ptr(0), Effort: llm.EffortOff},
		func(tok llm.StreamToken) error {
			switch tok.Type {
			case llm.TokenContent:
				reply.WriteString(tok.Text)
			case llm.TokenReasoning:
				return l.emit(Event{Type: EventReasoning, Data: tok.Text})
			}
			return nil
		})
	l.stats.QuoteSeconds = int(time.Since(start).Seconds())
	if err != nil || ctx.Err() != nil || strings.TrimSpace(reply.String()) == "" {
		log.Printf("[deeploop] session=%s quotes: no reply (%v), answer kept as it was", l.params.SessionID, err)
		l.stats.Quotes = "error"
		return
	}
	items, lines := settleQuotes(asked, parseQuotes(reply.String(), len(asked)), pages)
	for _, u := range units[len(asked):] {
		items = append(items, QuoteItem{Sentence: u, State: quoteUnasked})
	}
	l.stats.QuoteItems = items
	l.stats.QuoteLines = lines
	l.stats.Quotes = "done"
	if lines*2 < len(asked) {
		l.stats.Quotes = "format"
	}
	count := map[string]int{}
	for _, it := range items {
		count[it.State]++
	}
	log.Printf("[deeploop] session=%s quotes: %s found=%d not_found=%d short=%d none=%d unasked=%d lines=%d asked=%d reply_chars=%d seconds=%d",
		l.params.SessionID, l.stats.Quotes, count[quoteFound], count[quoteNotFound], count[quoteShort], count[quoteNone], count[quoteUnasked],
		lines, len(asked), reply.Len(), l.stats.QuoteSeconds)
	if l.stats.Quotes == "format" {
		// the only trace of a reply out of format: its beginning, on one line
		begins := []rune(strings.Join(strings.Fields(reply.String()), " "))
		if len(begins) > loopQuotesLogChars {
			begins = begins[:loopQuotesLogChars]
		}
		log.Printf("[deeploop] session=%s quotes: format, the reply begins: %q", l.params.SessionID, string(begins))
	}
}

// quotesNote is the line of the notes about the quotes: the counts, or that they did not arrive.
func quotesNote(lang string, s LoopStats) string {
	switch s.Quotes {
	case "done":
		found, notFound, short, asked := 0, 0, 0, 0
		for _, it := range s.QuoteItems {
			if it.State == quoteUnasked {
				continue
			}
			asked++
			switch it.State {
			case quoteFound:
				found++
			case quoteNotFound:
				notFound++
			case quoteShort:
				short++
			}
		}
		out := "\n\n*" + StepMessage(lang, "loop_quotes_found", found, asked) + "*"
		if notFound > 0 {
			out += "\n*" + StepMessage(lang, "loop_quotes_notfound", notFound) + "*"
		}
		if short > 0 {
			out += "\n*" + StepMessage(lang, "loop_quotes_short", loopQuoteMinWords, short) + "*"
		}
		return out
	case "format", "error":
		return "\n\n*" + StepMessage(lang, "loop_quotes_unusable") + "*"
	}
	return ""
}

// quotesList is the list at the end of the notes, after «Pagine lette» (Piano fase 6 chiusura, B2):
// for each sentence with a quote found, in the answer's order, the start of the sentence (E3), the
// words found on the page (E4) and the page's number in «Pagine lette»; the numbers of the sentence
// the quote lacks (E2); a line counts the sentences the model gave no quote for. Only when the quote
// turn is done and found something: otherwise the line of quotesNote is all. pageNo maps a page
// read (normalizeLinkURL) to its number.
func quotesList(lang string, s LoopStats, pageNo map[string]int) string {
	if s.Quotes != "done" {
		return ""
	}
	var lines []string
	none := 0
	for _, it := range s.QuoteItems {
		if it.State == quoteNone {
			none++
		}
		if it.State != quoteFound {
			continue
		}
		var quotes []string
		for _, r := range it.Refs {
			if !r.Found {
				continue
			}
			words := r.Match
			if words == "" {
				words = r.Evidence
			}
			q := "«" + cutOnWord(quoteListSafe(words), evidenceMaxChars, evidenceMaxChars) + "»"
			if n := pageNo[normalizeLinkURL(r.URL)]; n > 0 {
				q += " (" + StepMessage(lang, "loop_quotes_list_page", n) + ")"
			}
			quotes = append(quotes, q)
		}
		if len(quotes) == 0 {
			continue
		}
		line := "- **" + quoteListSentence(it.Sentence) + "** — " + strings.Join(quotes, " · ")
		if len(it.NumbersOut) > 0 {
			key := "loop_quotes_list_out" // numbersOutOf looked at every quote found
			if len(quotes) > 1 {
				key += "_many"
			}
			line += " *(" + StepMessage(lang, key, strings.Join(it.NumbersOut, ", ")) + ")*"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	intro := StepMessage(lang, "loop_quotes_list_intro", StepMessage(lang, "loop_pages_read"))
	switch {
	case none == 1:
		intro += " " + StepMessage(lang, "loop_quotes_list_none_one")
	case none > 1:
		intro += " " + StepMessage(lang, "loop_quotes_list_none", none)
	}
	return "\n### " + StepMessage(lang, "loop_quotes_list_title") + "\n\n*" + intro + "*\n\n" + strings.Join(lines, "\n") + "\n"
}

// quoteListSentence is the start of a sentence for the list: a table row as its cells, without
// Markdown, cut on a word at about 60 characters.
func quoteListSentence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "|") {
		var cells []string
		for _, c := range strings.Split(strings.Trim(s, "|"), "|") {
			if c = strings.TrimSpace(c); c != "" {
				cells = append(cells, c)
			}
		}
		s = strings.Join(cells, " · ")
	}
	if s = cutOnWord(quoteListSafe(s), 60, 64); s == "" {
		return "…"
	}
	return s
}

// quoteListReplacer takes off what would break a list item of the notes: emphasis, code, the
// brackets of a link and the quotation marks the list puts around the words (markdownQuoteSafe and more).
var quoteListReplacer = strings.NewReplacer("*", "", "`", "", "[", "", "]", "", "«", "", "»", "")

// quoteListSafe makes words copied from a page or the answer safe for one list item of the notes:
// no Markdown that could open or close something, all on one line.
func quoteListSafe(s string) string {
	return strings.Join(strings.Fields(quoteListReplacer.Replace(s)), " ")
}
