package agent

// The pages an answer cites (docs/SPEC_LINK_CONTROLLATI.md). Most links of the Deep a
// ciclo go to pages the model saw only as a search preview, and a sentence linking one
// is fully on that page about one time in three (75% for a page it read; audit of
// 05/11/2026). So the sentinel opens those pages, asks the model about each one apart
// from the conversation, and sends back the sentences that do not stand: the model
// corrects them against the passage. After the rewrite the same check runs once more
// without further turns: what still does not stand loses its link.
// VEDETTA_DEEP_LOOP_LINK_CHECK=false turns it off. Pages the model read are not
// checked in this version.

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"vedetta/internal/llm"
	"vedetta/internal/reader"
)

const (
	loopCheckMaxPages    = 40    // pages opened per answer: beyond this they stay unchecked
	loopCheckReaders     = 8     // pages opened at once
	loopCheckAsks        = 4     // comparisons at once
	loopCheckPageChars   = 40000 // of a page shown to the comparison, as scripts/judge_links.py
	loopCheckBatch       = 20    // sentences per comparison
	loopCheckMinClaim    = 15    // below this a piece of a sentence that has links cites the one before it
	loopCheckAskDeadline = 3 * time.Minute
)

// What a comparison says of a sentence and a page, and what stands for it.
const (
	citedSupported    = "supported"
	citedPartial      = "partial"
	citedAbsent       = "absent"
	citedContradicted = "contradicted"
	citedPointer      = "pointer"
	citedUnknown      = "unknown"  // opened, but the comparison gave no answer
	citedUnopened     = "unopened" // the page did not open, or over the limit
)

// What a link of the final answer is, for the reader (LoopStats.LinkStates).
const (
	linkChecked = "checked" // not read by the model; Vedetta opened it and it says the sentences
	linkPartial = "partial" // it says them only in part
	linkUnread  = "unread"  // not read, not checked
	linkAddress = "address" // it shows a project's address: it cites nothing
)

// citedCheckPrompt is the question of scripts/judge_links.py: the verdicts that
// motivate this check were measured with these words.
var citedCheckPrompt = hardenSystemPrompt(`You are a strict fact-checking judge.
You get ONE web page and a numbered list of SENTENCES from an answer that links this page as a source.
For each sentence, decide using only this page, with no outside knowledge:
- SUPPORTED: every factual detail of the sentence (numbers, dates, names, qualifiers) is stated in or directly implied by this page.
- PARTIAL: this page supports the main point, but the sentence adds details, numbers or qualifiers the page does not contain.
- ABSENT: this page does not say what the sentence says.
- CONTRADICTED: this page says something incompatible with the sentence.
- POINTER: the sentence states nothing to check: it only names this page, gives its address, recommends it as further reading, or introduces what follows (a line ending in a colon).
A sentence may link other pages too: judge it on this page alone. Text marked (context: ...) only helps to understand the sentence; do not judge it.
The texts may be in different languages. Answer with one line per sentence, exactly "n: VERDICT", and nothing else.`)

var (
	citedVerdictRe  = regexp.MustCompile(`(?im)^\s*\**\s*(\d+)\s*\**\s*[:.)\-]\s*\**\s*(SUPPORTED|PARTIAL|ABSENT|CONTRADICTED|POINTER)\b`)
	labelledLinkRe  = regexp.MustCompile(`\[([^\]\n]*)\]\((https?://(?:\([^()\s]*\)|[^)\s])+)\)`)
	bracketURLRe    = regexp.MustCompile(`\[https?://[^\]\s]+\]`)
	ownAddressRe    = regexp.MustCompile(`^(?:https?://\S+|(?:www\.)?[\p{L}\p{N}_-]+(?:\.[\p{L}\p{N}_-]+)+/\S*)$`)
	claimListMark   = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+`)
	claimNonWordRe  = regexp.MustCompile(`[^\p{L}\p{N}_]+`)
	sourcesLineRe   = regexp.MustCompile(`(?i)^[\s*_>-]*(?:fonti|fonte|sources?|riferimenti|references|quellen|références|fuentes|bibliografia|bibliography)\b[^:\n]{0,40}:`)
	sourcesHeadRe   = regexp.MustCompile(`(?i)^#{1,6}\s*\**\s*(?:le\s+|the\s+)?(?:fonti|fonte|sources?|riferimenti|references|quellen|références|fuentes|bibliografia|bibliography)\b`)
	leftoverPunctRe = regexp.MustCompile(`^[\s*_>,;:·|–—()-]*$`)
)

// CitedCheck is one sentence and one cited page the model did not read.
type CitedCheck struct {
	Sentence string   `json:"sentence"`
	URL      string   `json:"url"`
	Verdict  string   `json:"verdict"`
	Lacks    []string `json:"lacks,omitempty"` // names and numbers in none of the sentence's pages
	Round    int      `json:"round"`
}

// LinkTrouble is a sentence sent back because of a page it cites.
type LinkTrouble struct {
	Sentence string   `json:"sentence"`
	URL      string   `json:"url"`
	Verdict  string   `json:"verdict"`
	Lacks    []string `json:"lacks,omitempty"`
	Passage  string   `json:"passage,omitempty"` // the passage of the page closest to the sentence
}

// linkClaim is a sentence of an answer with the links it carries.
type linkClaim struct {
	Text      string // as a reader sees it, without addresses
	Context   string // the sentence before it, to understand this one
	Parts     []string
	Links     []string
	Addresses map[string]bool // links, by normalizeLinkURL, whose words are their own address
}

// splitClaimParts cuts a line at the end of its sentences, as scripts/judge.py: a
// stop followed by a capital letter or a number, perhaps after a quote or a bracket.
func splitClaimParts(line string) []string {
	var out []string
	start := 0
	for i := 0; i+1 < len(line); i++ {
		if !strings.ContainsRune(".!?;", rune(line[i])) || !isClaimSpace(line[i+1]) {
			continue
		}
		next := i + 1
		for next < len(line) && isClaimSpace(line[next]) {
			next++
		}
		r, _ := utf8.DecodeRuneInString(strings.TrimLeft(line[next:], "\"'«([*"))
		if unicode.IsUpper(r) || unicode.IsDigit(r) {
			out = append(out, strings.TrimSpace(line[start:i+1]))
			start = next
		}
	}
	if s := strings.TrimSpace(line[start:]); s != "" {
		out = append(out, s)
	}
	return out
}

func isClaimSpace(b byte) bool { return b == ' ' || b == '\t' }

// linksOf returns the links of a piece of text, code left out, with the ones whose
// words are their own address.
func linksOf(part string) (links []string, addresses map[string]bool) {
	part = inlineCodeRe.ReplaceAllString(part, " ")
	addresses = map[string]bool{}
	for _, m := range answerLinkRe.FindAllStringSubmatch(part, -1) {
		u := m[1]
		if u == "" {
			u = m[2]
		}
		links = append(links, u)
	}
	for _, m := range labelledLinkRe.FindAllStringSubmatch(part, -1) {
		if ownAddressRe.MatchString(strings.TrimSpace(m[1])) {
			addresses[normalizeLinkURL(m[2])] = true
		}
	}
	return links, addresses
}

// claimWords is a piece of a sentence as a reader sees it.
func claimWords(part string) string {
	s := bracketURLRe.ReplaceAllString(part, "")
	s = labelledLinkRe.ReplaceAllString(s, "$1")
	s = bareAddressRe.ReplaceAllString(s, "")
	s = strings.NewReplacer("**", "", "__", "").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// wordsOutsideLinks is what a piece says besides the words of its links.
func wordsOutsideLinks(part string) string {
	s := bracketURLRe.ReplaceAllString(labelledLinkRe.ReplaceAllString(part, " "), " ")
	return strings.TrimSpace(claimNonWordRe.ReplaceAllString(s, " "))
}

func (c *linkClaim) addLinks(links []string, addresses map[string]bool) {
	seen := map[string]bool{}
	for _, u := range c.Links {
		seen[normalizeLinkURL(u)] = true
	}
	for _, u := range links {
		if k := normalizeLinkURL(u); !seen[k] {
			seen[k] = true
			c.Links = append(c.Links, u)
		}
	}
	for k := range addresses {
		c.Addresses[k] = true
	}
}

// linkClaims returns the sentences of answer that carry a link.
func linkClaims(answer string) []linkClaim {
	var out []linkClaim
	for _, c := range answerClaims(answer) {
		if len(c.Links) > 0 {
			out = append(out, c)
		}
	}
	return out
}

// answerClaims returns the sentences of answer, with or without a link. A piece that holds
// little more than links, as "([docs](u))." after a full stop, cites the sentence
// before it; a piece starting with a small letter or a number was cut at an
// abbreviation ("art. 30", "n. 633") and goes on with it. A table row stays whole.
func answerClaims(answer string) []linkClaim {
	var out []linkClaim
	last := ""
	fence := false
	for _, line := range strings.Split(answer, "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "```") {
			fence = !fence
			continue
		}
		if fence || s == "" || strings.HasPrefix(s, "#") || strings.Trim(s, "-*_|: ") == "" {
			continue
		}
		parts := []string{s}
		if !strings.HasPrefix(s, "|") {
			parts = splitClaimParts(claimListMark.ReplaceAllString(s, ""))
		}
		var cur []linkClaim
		for _, part := range parts {
			links, addresses := linksOf(part)
			text := claimWords(part)
			if n := len(cur); n > 0 {
				first, _ := utf8.DecodeRuneInString(part)
				if unicode.IsLower(first) || unicode.IsDigit(first) {
					cur[n-1].Text = strings.TrimSpace(cur[n-1].Text + " " + text)
					cur[n-1].Parts = append(cur[n-1].Parts, part)
					cur[n-1].addLinks(links, addresses)
					last = cur[n-1].Text
					continue
				}
				if len(links) > 0 && utf8.RuneCountInString(wordsOutsideLinks(part)) < loopCheckMinClaim {
					cur[n-1].Parts = append(cur[n-1].Parts, part)
					cur[n-1].addLinks(links, addresses)
					continue
				}
			}
			c := linkClaim{Text: text, Context: last, Parts: []string{part}, Addresses: map[string]bool{}}
			c.addLinks(links, addresses)
			cur = append(cur, c)
			if text != "" {
				last = text
			}
		}
		out = append(out, cur...)
	}
	return out
}

// openCited opens the pages not opened yet, a few at a time, up to the limit. A page
// that does not open is remembered as empty: its sentences stay unchecked.
func (l *deepLoop) openCited(urls []string) {
	var todo []string
	planned := map[string]bool{}
	for _, u := range urls {
		k := normalizeLinkURL(u)
		if _, done := l.citedTexts[k]; done || planned[k] {
			continue
		}
		if t, ok := l.verified[k]; ok {
			l.citedTexts[k] = t
			continue
		}
		if len(l.citedTexts)+len(todo) >= loopCheckMaxPages {
			continue
		}
		planned[k] = true
		todo = append(todo, u)
	}
	if len(todo) == 0 {
		return
	}
	texts := make([]string, len(todo))
	slots := make(chan struct{}, loopCheckReaders)
	var wg sync.WaitGroup
	for i, u := range todo {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			texts[i] = l.readCited(u)
		}(i, u)
	}
	wg.Wait()
	for i, u := range todo {
		l.citedTexts[normalizeLinkURL(u)] = texts[i]
	}
}

// readCited opens one page; a page too short or blocked counts as not opened.
func (l *deepLoop) readCited(u string) string {
	ctx, cancel := context.WithTimeout(reader.WithSessionID(l.ctx, l.params.SessionID), loopReadTimeout)
	doc, err := l.cr.Read(ctx, u)
	cancel()
	status, detail := classifyRead(doc, err)
	log.Printf("[deeploop] session=%s cited read url=%s status=%s %s", l.params.SessionID, u, status, detail)
	if status != readOK {
		return ""
	}
	return doc.Title + "\n" + doc.Content
}

// citedUnit is what a comparison is asked about one sentence.
type citedUnit struct {
	Key     string
	Text    string
	Context string
}

func citedKey(sentence, normURL string) string { return sentence + "\x00" + normURL }

// citedMessage is the page, isolated as untrusted, at most maxChars of it, and
// its numbered sentences.
func citedMessage(url, page string, units []citedUnit, maxChars int) string {
	if len(page) > maxChars {
		page = strings.ToValidUTF8(page[:maxChars], "")
	}
	var b strings.Builder
	b.WriteString(isolateContent(1, "Page", url, page))
	b.WriteString("\nSENTENCES:\n")
	for i, u := range units {
		fmt.Fprintf(&b, "%d. %s", i+1, u.Text)
		if u.Context != "" {
			fmt.Fprintf(&b, " (context: %s)", u.Context)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// parseCitedVerdicts reads only "n: VERDICT" lines: whatever else the reply says,
// or a page managed to make it say, is ignored. A sentence with no line is unknown.
func parseCitedVerdicts(reply string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = citedUnknown
	}
	for _, m := range citedVerdictRe.FindAllStringSubmatch(reply, -1) {
		var i int
		if _, err := fmt.Sscanf(m[1], "%d", &i); err == nil && i >= 1 && i <= n {
			out[i-1] = strings.ToLower(m[2])
		}
	}
	return out
}

// askCited compares sentences with a page, apart from the conversation: temperature
// 0, no reasoning shown. On an error the sentences stay unknown.
func (l *deepLoop) askCited(url, page string, units []citedUnit) []string {
	ctx, cancel := context.WithTimeout(l.ctx, loopCheckAskDeadline)
	defer cancel()
	var reply strings.Builder
	err := l.client.StreamChatWithOptions(ctx, []llm.Message{
		{Role: "system", Content: citedCheckPrompt},
		{Role: "user", Content: citedMessage(url, page, units, l.checkPageChars())},
	}, llm.ChatOptions{Temperature: llm.Float64Ptr(0), Effort: llm.EffortOff}, func(tok llm.StreamToken) error {
		if tok.Type == llm.TokenContent {
			reply.WriteString(tok.Text)
		}
		return nil
	})
	if err != nil {
		log.Printf("[deeploop] session=%s cited comparison failed for %s: %v", l.params.SessionID, url, err)
	}
	return parseCitedVerdicts(reply.String(), len(units))
}

// compareCited asks about the pairs not compared yet.
func (l *deepLoop) compareCited(pending map[string][]citedUnit, urls map[string]string) {
	type job struct {
		url, page string
		units     []citedUnit
	}
	var jobs []job
	for k, units := range pending {
		for start := 0; start < len(units); start += loopCheckBatch {
			end := min(start+loopCheckBatch, len(units))
			jobs = append(jobs, job{urls[k], l.citedTexts[k], units[start:end]})
		}
	}
	slots := make(chan struct{}, loopCheckAsks)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			verdicts := l.askCited(j.url, j.page, j.units)
			mu.Lock()
			defer mu.Unlock()
			for i, u := range j.units {
				l.citedVerdicts[u.Key] = verdicts[i]
			}
		}(j)
	}
	wg.Wait()
}

// sentenceLacks returns the names and numbers of a sentence that are not in text.
func sentenceLacks(sentence, lang, text string) []string {
	var out []string
	for _, it := range reviewDraft(sentence, lang, loopNumberSet(text), text) {
		out = append(out, it.Numbers...)
		out = append(out, it.Names...)
	}
	return out
}

// citedTrouble decides on a sentence and one of its pages. A page that says it is
// enough. Otherwise a page that does not say it, or contradicts it, is a trouble, and
// so is one that says it in part when a name or a number is on none of its pages:
// that is something the model can fix, and the comparison alone is strict
// (checked against an independent judge, SPEC_LINK_CONTROLLATI §2).
func citedTrouble(verdict string, supported bool, lacks []string) bool {
	if supported {
		return false
	}
	switch verdict {
	case citedAbsent, citedContradicted:
		return true
	case citedPartial, citedUnknown:
		return len(lacks) > 0
	}
	return false
}

// checkCited checks the links of text to pages the model did not read and returns
// the checks and the sentences that do not stand. A sentence and a page are compared
// once: a sentence the rewrite left as it was costs nothing the second time. The page
// is the one the link leads to once the answer goes out: an address almost like a
// source's is repaired then (case 24g, 19/11/2026).
func (l *deepLoop) checkCited(text string, round int) ([]CitedCheck, []LinkTrouble) {
	claims := linkClaims(text)
	sources := newLinkResolver(l.sourceURLs())
	var toOpen []string
	for _, c := range claims {
		for _, u := range c.Links {
			u, address := sources.Target(u), c.Addresses[normalizeLinkURL(u)]
			if l.pages[normalizeLinkURL(u)] == nil && !address {
				toOpen = append(toOpen, u)
			}
		}
	}
	if len(toOpen) == 0 {
		return nil, nil
	}
	l.step("deep_critic", StepMessage(l.params.UILanguage, "loop_link_check"))
	l.openCited(toOpen)

	pending := map[string][]citedUnit{}
	urls := map[string]string{}
	for _, c := range claims {
		for _, u := range c.Links {
			u, address := sources.Target(u), c.Addresses[normalizeLinkURL(u)]
			k := normalizeLinkURL(u)
			key := citedKey(c.Text, k)
			if l.pages[k] != nil || address || l.citedTexts[k] == "" {
				continue
			}
			if _, done := l.citedVerdicts[key]; done {
				continue
			}
			l.citedVerdicts[key] = citedUnknown
			urls[k] = u
			pending[k] = append(pending[k], citedUnit{Key: key, Text: c.Text, Context: c.Context})
		}
	}
	l.compareCited(pending, urls)

	var checks []CitedCheck
	var troubles []LinkTrouble
	for _, c := range claims {
		union := []string{}
		supported := false
		for _, u := range c.Links {
			k := normalizeLinkURL(sources.Target(u))
			union = append(union, u)
			if p := l.pages[k]; p != nil {
				union = append(union, p.Text)
			} else if t := l.citedTexts[k]; t != "" {
				union = append(union, t)
				supported = supported || l.citedVerdicts[citedKey(c.Text, k)] == citedSupported
			}
		}
		lacks := sentenceLacks(c.Text, l.params.AnswerLanguage, strings.Join(union, "\n"))
		for _, u := range c.Links {
			k := normalizeLinkURL(sources.Target(u))
			if l.pages[k] != nil || c.Addresses[normalizeLinkURL(u)] {
				continue
			}
			page := l.citedTexts[k]
			verdict := citedUnopened
			if page != "" {
				verdict = l.citedVerdicts[citedKey(c.Text, k)]
			}
			checks = append(checks, CitedCheck{Sentence: c.Text, URL: u, Verdict: verdict, Lacks: lacks, Round: round})
			if page != "" && citedTrouble(verdict, supported, lacks) {
				troubles = append(troubles, LinkTrouble{Sentence: c.Text, URL: u, Verdict: verdict, Lacks: lacks,
					Passage: selectExcerpt(page, c.Text, loopReviewPassageChars)})
			}
		}
	}
	return checks, troubles
}

// dropLink removes a link from a piece of text: a bracket holding only that link
// goes with it, a list of links closes up, otherwise the words stay without the link.
func dropLink(part, url string) string {
	q := regexp.QuoteMeta(url)
	for _, r := range []struct{ re, to string }{
		{`\s*\(\s*\[[^\]\n]*\]\(` + q + `\)\s*\)`, ""},
		{`\[[^\]\n]*\]\(` + q + `\)\s*[,;·]\s*`, ""},
		{`\s*[,;·]\s*\[[^\]\n]*\]\(` + q + `\)`, ""},
		{`\[([^\]\n]*)\]\(` + q + `\)`, "$1"},
		{`\s*\[` + q + `\]`, ""},
	} {
		re := regexp.MustCompile(r.re)
		if re.MatchString(part) {
			return re.ReplaceAllString(part, r.to)
		}
	}
	return part
}

// settleCited is the last check, with no more turns: a link whose page still does not
// say the sentence is removed, and lists of sources keep only what the model read.
func (l *deepLoop) settleCited(text string) string {
	checks, troubles := l.checkCited(text, 2)
	l.citedFinal = checks
	l.stats.CitedChecks = append(l.stats.CitedChecks, checks...)
	claims := linkClaims(text)
	for _, t := range troubles {
		if t.Verdict != citedAbsent && t.Verdict != citedContradicted {
			continue
		}
		for _, c := range claims {
			if c.Text != t.Sentence {
				continue
			}
			for _, part := range c.Parts {
				if fixed := dropLink(part, t.URL); fixed != part {
					text = strings.Replace(text, part, fixed, 1)
					l.stats.LinksRemoved++
					break
				}
			}
			break
		}
	}
	return l.dropUnreadSources(text)
}

// dropUnreadSources keeps in the lists of sources only the pages the model read: a
// line "Fonti: a, b" and the items under a heading "Fonti". A line left without
// links goes.
func (l *deepLoop) dropUnreadSources(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	fence, section := false, false
	sources := newLinkResolver(l.sourceURLs())
	for _, line := range lines {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "```") {
			fence = !fence
		}
		if !fence && strings.HasPrefix(s, "#") {
			section = sourcesHeadRe.MatchString(s)
		}
		if fence || strings.HasPrefix(s, "#") || !(section || sourcesLineRe.MatchString(s)) {
			out = append(out, line)
			continue
		}
		links, _ := linksOf(line)
		fixed := line
		removed := 0
		for _, u := range links {
			if l.readEver(normalizeLinkURL(sources.Target(u))) {
				continue
			}
			if next := dropLink(fixed, u); next != fixed {
				fixed = next
				removed++
			}
		}
		l.stats.LinksRemoved += removed
		if removed > 0 && !sourceLineStands(fixed) {
			continue
		}
		out = append(out, fixed)
	}
	return strings.Join(out, "\n")
}

// sourceLineStands: something is left besides the label and punctuation.
func sourceLineStands(line string) bool {
	if links, _ := linksOf(line); len(links) > 0 {
		return true
	}
	rest := sourcesLineRe.ReplaceAllString(strings.TrimSpace(line), "")
	return !leftoverPunctRe.MatchString(claimListMark.ReplaceAllString(rest, ""))
}

// linkStates says what each link of the final answer is, and counts them. Pages the
// model read are not in the map.
func (l *deepLoop) linkStates(answer string) map[string]string {
	verdicts := map[string][]string{}
	sources := newLinkResolver(l.sourceURLs()) // citedFinal has the addresses as the model wrote them
	for _, c := range l.citedFinal {
		k := normalizeLinkURL(sources.Target(c.URL))
		verdicts[k] = append(verdicts[k], c.Verdict)
	}
	cites := map[string]bool{} // a link that also cites something is not just an address
	addresses := map[string]string{}
	for _, c := range linkClaims(answer) {
		for _, u := range c.Links {
			k := normalizeLinkURL(u)
			if c.Addresses[k] {
				addresses[k] = u
			} else {
				cites[k] = true
			}
		}
	}
	states := map[string]string{}
	seen := map[string]bool{}
	for _, m := range answerLinkRe.FindAllStringSubmatch(answer, -1) {
		u := m[1]
		if u == "" {
			u = m[2]
		}
		k := normalizeLinkURL(u)
		if seen[k] || l.pages[k] != nil {
			continue
		}
		seen[k] = true
		switch state := citedState(verdicts[k]); {
		case addresses[k] != "" && !cites[k]:
			states[u] = linkAddress
			l.stats.LinksAddress++
		case state == linkChecked:
			states[u] = linkChecked
			l.stats.LinksControlled++
		case state == linkPartial:
			states[u] = linkPartial
			l.stats.LinksPartial++
		default:
			states[u] = linkUnread
			l.stats.LinksUnchecked++
		}
	}
	return states
}

// citedState is the worst of a page's verdicts over the sentences citing it.
func citedState(verdicts []string) string {
	if len(verdicts) == 0 {
		return linkUnread
	}
	state := linkChecked
	for _, v := range verdicts {
		switch v {
		case citedSupported, citedPointer:
		case citedUnknown, citedUnopened:
			return linkUnread
		default:
			state = linkPartial
		}
	}
	return state
}

// paidReaderName: such a reader is paid per page, and the check opens many pages
// nobody asked for. With one first in the chain the check stays off (decision D4).
func paidReaderName(name string) bool {
	for _, w := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return !unicode.IsLetter(r) }) {
		if w == "firecrawl" || w == "exa" {
			return true
		}
	}
	return false
}
