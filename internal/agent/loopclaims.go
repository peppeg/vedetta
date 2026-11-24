package agent

// What the answer says it did (case Zorbatek, 04/11/2026): "nelle banche dati che ho
// consultato, Normattiva, USPTO, EPO", with none of them opened; "ho incrociato le
// opere di Kohut", having read pages about him. The conclusion may be right, but a
// reader who does not know the field trusts it for the work it describes. Go finds
// the lines that describe the research and gives the model what was really done;
// the model rewrites only those lines, and leaves them alone if they are true.

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
)

const workClaimMaxQueries = 30

// workClaimRe: the first person of reading, consulting, checking and searching, and
// the phrases that say what the answer rests on. The first group marks a negation:
// "non ho letto" is what a sentinel wants to hear.
var workClaimRe = regexp.MustCompile(`(?i)` +
	`\b(non\s+)?(?:l['’]|li\s+|le\s+|lo\s+|la\s+)?(?:ho|abbiamo)\s+(?:[\p{L}'’]+\s+){0,2}?(?:consultat|interrogat|lett[oaie]\b|verificat|(?:ri)?controllat|incrociat|esaminat|analizzat|cercat|confrontat|passat\p{L}*\s+in\s+rassegna|(?:effettuat|eseguit|svolt|fatt|compiut|condott)\p{L}*\s+(?:[\p{L}'’]+\s+){0,2}?(?:ricerc|verific|controll))` +
	`|\b(?:pagine|fonti|banche dati|documenti|siti|testi)\s+(?:\p{L}+\s+){0,2}?(?:consultat|esaminat)` +
	`|\bverific[ah]e?\s+(?:effettuat|su\s+fonti|sulle\s+fonti)` +
	`|\b(?:interrogando|consultando)\s+(?:le|i|gli|il|la|l['’])` +
	`|\b(?:ricerch|interrogazion)\p{L}*\s+(?:\p{L}+\s+){0,3}?(?:banche dati|database|registri|archivi)` +
	`|\b(?:nelle|sulle)\s+(?:\p{L}+\s+)?(?:banche dati|database)\b[^.]{0,160}\bnon\s+(?:ho|abbiamo)\s+trovat` +
	`|\b(?:risposta|ricostruzione|sintesi|quanto segue|analisi)\s+(?:\p{L}+\s+)?basat\p{L}+\s+su` +
	`|\bI(?:'ve|\s+have)?\s+(?:\p{L}+\s+)?(?:consulted|queried|read|checked|verified|reviewed|searched|cross-?referenced|examined|compared)\b` +
	`|\b(?:this (?:answer|summary|report|reconstruction)|everything below|the following)\s+(?:\p{L}+\s+)?(?:is\s+|are\s+)?based on` +
	`|\b(?:pages|sources|databases)\s+(?:I\s+)?(?:consulted|reviewed|checked)\b` +
	`|\bich\s+habe\s+(?:\p{L}+\s+){0,4}?(?:gelesen|geprüft|überprüft|konsultiert|recherchiert|durchsucht|verglichen)` +
	`|\bj['’]ai\s+(?:\p{L}+\s+)?(?:consulté|lu\b|vérifié|examiné|recherché|comparé|analysé|croisé)` +
	`|\bhe\s+(?:consultado|leído|verificado|comprobado|revisado|buscado|comparado|analizado|cotejado)`)

// workClaimLines returns the lines of answer that describe the research.
func workClaimLines(answer string) []LanguageSlip {
	var out []LanguageSlip
	fence := false
	seen := map[string]bool{}
	for _, line := range strings.Split(answer, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fence = !fence
			continue
		}
		if fence || seen[line] {
			continue
		}
		text := mdLinkRe.ReplaceAllStringFunc(inlineCodeRe.ReplaceAllString(line, " "), func(m string) string {
			return m[:strings.Index(m, "](")+1] // the words of a link count, its address does not
		})
		var words []string
		for _, m := range workClaimRe.FindAllStringSubmatch(text, -1) {
			if m[1] == "" {
				words = append(words, m[0])
			}
		}
		if len(words) > 0 {
			seen[line] = true
			out = append(out, LanguageSlip{Line: line, Words: words})
		}
	}
	return out
}

// workDone says what the research really did: the searches and the pages opened.
func (l *deepLoop) workDone() string {
	var b strings.Builder
	var queries []string
	for _, c := range l.stats.CallsDetail {
		if c.Tool == "search" && c.Query != "" {
			queries = append(queries, c.Query)
		}
	}
	fmt.Fprintf(&b, "Web searches made through a search engine, which return only titles and short previews: %d.\n", len(queries))
	for i, q := range queries {
		if i == workClaimMaxQueries {
			fmt.Fprintf(&b, "- and %d more.\n", len(queries)-i)
			break
		}
		fmt.Fprintf(&b, "- %s\n", q)
	}
	opened := map[string]bool{}
	var pages []string
	add := func(title, url string) {
		if k := normalizeLinkURL(url); !opened[k] {
			opened[k] = true
			pages = append(pages, fmt.Sprintf("- %s (%s)", strings.Join(strings.Fields(title), " "), url))
		}
	}
	for _, k := range l.pageOrder {
		add(l.pages[k].Title, l.pages[k].URL)
	}
	for _, it := range append(append([]ReviewItem{}, l.stats.ReviewItems...), l.stats.ReviewConfirmed...) {
		if it.URL != "" && !it.Unread && it.Passage != "" {
			add("passage shown", it.URL)
		}
	}
	for _, t := range l.stats.LinkTroubles {
		add("passage shown", t.URL)
	}
	if len(pages) == 0 {
		b.WriteString("Pages opened and read: none.\n")
	} else {
		b.WriteString("Pages opened and read, and no others:\n" + strings.Join(pages, "\n") + "\n")
	}
	return b.String()
}

// workClaimsMessage asks the model to make the lines say only what was done. A
// true line comes first and is copied: "rewrite" next to "change nothing else"
// left smaller models reasoning for minutes without a reply (DL5.2).
func workClaimsMessage(claims []LanguageSlip, done, lang string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Below are lines from an answer written in %s. Each line says something about the research behind the answer, "+
		"and it may claim more than was done.\n\n"+
		"What was really done:\n%s\n"+
		"Only the pages in this list were opened and read. A site seen only in search results was not consulted. "+
		"A page about an author is not that author's work.\n\n"+
		"Check each line against this:\n"+
		"- if it is true, copy it unchanged;\n"+
		"- if it claims a consultation, reading or check that did not happen, replace only that claim with what did happen "+
		"(for instance \"a search preview says\", or \"searches found no trace of it\"), in few words and without listing the searches. "+
		"Leave the rest of the line as it is: what the pages support, links, numbers and Markdown.\n\n"+
		"Reply only with the lines, one per line, each starting with its number and a colon, like \"1: …\".\n\n",
		GetLanguageName(lang), done)
	for i, c := range claims {
		fmt.Fprintf(&b, "%d: %s\n", i+1, c.Line)
	}
	return b.String()
}

// workClaimFits: a rewrite may drop a list of sources or take a few words more to
// say what was done, but a reply that leaves almost nothing of the line is no rewrite.
func workClaimFits(orig, f string) bool {
	return len(f)*4 >= len(orig) && len(f) <= len(orig)*2+200
}

// fixWorkClaims checks a held answer and, when it describes its own research, asks
// the model to match those lines to what was done. On any trouble the answer goes
// out as it was.
func (l *deepLoop) fixWorkClaims(ctx context.Context, text string) string {
	if l.client == nil {
		return text
	}
	claims := workClaimLines(text)
	if len(claims) == 0 {
		return text
	}
	if len(claims) > languageFixMaxLines {
		claims = claims[:languageFixMaxLines]
	}
	log.Printf("[deeploop] session=%s work claims: %d lines sent back", l.params.SessionID, len(claims))
	l.step("deep_critic", StepMessage(l.params.UILanguage, "loop_claims", len(claims)))
	reply, err := l.askLines(ctx, workClaimsMessage(claims, l.workDone(), l.params.AnswerLanguage))
	if err != nil {
		log.Printf("[deeploop] session=%s work claims: no reply (%v), answer kept as it was", l.params.SessionID, err)
		l.stats.WorkClaims = claims
		return text
	}
	out, claims := applyLineFixes(text, claims, reply, workClaimFits)
	l.stats.WorkClaims = claims
	return out
}

// workClaimsNote tells the reader, only when a line really changed.
func workClaimsNote(lang string, claims []LanguageSlip) string {
	n := 0
	for _, c := range claims {
		if c.Fixed != "" && c.Fixed != c.Line {
			n++
		}
	}
	switch n {
	case 0:
		return ""
	case 1:
		return "\n\n*" + StepMessage(lang, "loop_claims_note_one") + "*"
	}
	return "\n\n*" + StepMessage(lang, "loop_claims_note", n) + "*"
}
