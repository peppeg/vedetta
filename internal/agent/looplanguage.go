package agent

// The language check of the Deep a ciclo (case 03/11/2026): the rewrite after the
// sentinel's review came out with "riducendo存储/embedding cost", "développeurs"
// and "diConvertire", which were not in the draft. Go finds the lines with a word
// in another script, a foreign accent, or a word run together with the one
// before; the model rewrites only those lines, and Go puts them back in place.
// Go only finds the slips; the model decides how to write them in the language.

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"vedetta/internal/llm"
	"vedetta/internal/storage"
)

const (
	languageFixMaxLines = 12
	languageFixTimeout  = 3 * time.Minute
)

// LanguageSlip is a line of the answer with words that are not in its language.
type LanguageSlip struct {
	Line  string   `json:"line"`
	Words []string `json:"words"`
	Fixed string   `json:"fixed,omitempty"`
}

// otherScripts are the scripts that never belong in an answer written in a
// language with the Latin alphabet. Greek is left out: α, β, μ, Ω are symbols.
var otherScripts = []*unicode.RangeTable{unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul,
	unicode.Cyrillic, unicode.Arabic, unicode.Hebrew, unicode.Thai, unicode.Devanagari}

func inOtherScript(r rune) bool {
	for _, t := range otherScripts {
		if unicode.Is(t, r) {
			return true
		}
	}
	return false
}

var latinLanguages = map[string]bool{"it": true, "en": true, "de": true, "fr": true, "es": true}

// Italian writes accents only on the last letter (perché, città, più); these
// letters do not exist in Italian at all.
const (
	italianFinalAccents = "àèéìíòóùú"
	foreignLetters      = "äöüßçñãõâêîôûëïÿæœ"
)

// gluedWordRe: an Italian short word run together with a capitalized one,
// "diConvertire", "laGuida". A brand like iPhone or macOS does not start with one.
var gluedWordRe = regexp.MustCompile(`(?:^|[^\p{L}])((?:di|da|in|con|su|per|tra|fra|il|lo|la|le|gli|un|una|del|della|dei|delle|che|non|nel|nella|al|alla|sul|sulla|dal|dalla)\p{Lu}\p{Ll}{2,})`)

var (
	inlineCodeRe = regexp.MustCompile("`[^`\n]*`")
	mdLinkRe     = regexp.MustCompile(`\[[^\]\n]*\]\((?:\([^()\s]*\)|[^)\s])*\)`)
)

// languageSlips returns the lines of answer with words outside its language.
// A word that is in what the model saw (the question, the pages, the previews)
// is a quotation or a name, not a slip.
func languageSlips(answer, lang string, seen []string) []LanguageSlip {
	lang = NormalizeLanguage(lang)
	if !latinLanguages[lang] {
		return nil
	}
	seenLower := strings.ToLower(strings.Join(seen, "\n"))
	var out []LanguageSlip
	fence := false
	for _, line := range strings.Split(answer, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fence = !fence
			continue
		}
		if fence || strings.TrimSpace(line) == "" {
			continue
		}
		text := inlineCodeRe.ReplaceAllString(line, " ")
		text = mdLinkRe.ReplaceAllString(text, " ")
		text = textURLRe.ReplaceAllString(text, " ")
		var words []string
		add := func(w string) {
			if w == "" || strings.Contains(seenLower, strings.ToLower(w)) {
				return
			}
			for _, have := range words {
				if have == w {
					return
				}
			}
			words = append(words, w)
		}
		// A run of another script, even glued to a Latin word: "riducendo存储".
		var run []rune
		for _, r := range text + " " {
			if inOtherScript(r) {
				run = append(run, r)
				continue
			}
			if len(run) > 0 {
				add(string(run))
				run = run[:0]
			}
		}
		if lang == "it" {
			for _, w := range strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) }) {
				if foreignAccent(w) {
					add(w)
				}
			}
			for _, m := range gluedWordRe.FindAllStringSubmatch(text, -1) {
				add(m[1])
			}
		}
		if len(words) > 0 {
			out = append(out, LanguageSlip{Line: line, Words: words})
		}
	}
	return out
}

// foreignAccent reports a lowercase word with an accent Italian does not write:
// inside the word ("développeurs") or a letter Italian lacks ("façade").
// A capitalized word is a name (Pérez, Zürich) and is left alone.
func foreignAccent(w string) bool {
	rs := []rune(w)
	if len(rs) < 3 || !unicode.IsLower(rs[0]) {
		return false
	}
	for i, r := range rs {
		lr := unicode.ToLower(r)
		if strings.ContainsRune(foreignLetters, lr) {
			return true
		}
		if i < len(rs)-1 && strings.ContainsRune(italianFinalAccents, lr) {
			return true
		}
	}
	return false
}

// lineLeadRe: indentation and the mark that opens a list item, heading or quote.
var lineLeadRe = regexp.MustCompile(`^\s*(?:(?:[-*+]|\d+[.)]|#+|>)\s+)?`)

var fixedLineRe = regexp.MustCompile(`^\s*(\d+)\s*[:.)]\s?(.*)$`)

// languageFixMessage asks the model to rewrite only the lines with slips.
func languageFixMessage(slips []LanguageSlip, lang string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "These lines of an answer written in %s contain words in another language or script, "+
		"or words run together. Rewrite each line correctly in %s and change nothing else: keep the links, "+
		"numbers, names and Markdown exactly as they are.\n"+
		"Reply only with the lines, one per line, each starting with its number and a colon, like \"1: …\".\n\n",
		GetLanguageName(lang), GetLanguageName(lang))
	for i, s := range slips {
		fmt.Fprintf(&b, "%d: %s\n   (check: %s)\n", i+1, s.Line, strings.Join(s.Words, ", "))
	}
	return b.String()
}

// applyLanguageFixes puts the rewritten lines in place of the ones with slips. A
// line that is missing from the reply, empty, or far from the original length is
// left as it was.
func applyLanguageFixes(answer string, slips []LanguageSlip, reply string) (string, []LanguageSlip) {
	return applyLineFixes(answer, slips, reply, func(orig, f string) bool {
		return len(f)*2 >= len(orig) && len(f) <= len(orig)*2
	})
}

// applyLineFixes puts the numbered lines of reply in place of the lines sent back,
// when fits accepts them.
func applyLineFixes(answer string, slips []LanguageSlip, reply string, fits func(orig, f string) bool) (string, []LanguageSlip) {
	fixed := map[int]string{}
	for _, l := range strings.Split(reply, "\n") {
		m := fixedLineRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if n >= 1 && n <= len(slips) {
			fixed[n-1] = strings.TrimRight(m[2], " \t")
		}
	}
	lines := strings.Split(answer, "\n")
	for i := range slips {
		f, ok := fixed[i]
		orig := slips[i].Line
		if !ok || strings.TrimSpace(f) == "" || !fits(orig, f) {
			continue
		}
		// Keep the line's own indentation and list marker if the reply dropped them.
		if lead := lineLeadRe.FindString(orig); !strings.HasPrefix(f, lead) && strings.TrimSpace(lineLeadRe.FindString(f)) == "" {
			f = lead + strings.TrimLeft(f, " \t")
		}
		for j, l := range lines {
			if l == orig {
				lines[j] = f
				slips[i].Fixed = f
				break
			}
		}
	}
	return strings.Join(lines, "\n"), slips
}

// fixLanguage checks a held answer and, when it has slips, asks the model to
// rewrite those lines. On any trouble the answer goes out as it was.
func (l *deepLoop) fixLanguage(ctx context.Context, text, query string, history []storage.TurnMessage) string {
	if l.client == nil {
		return text
	}
	slips := languageSlips(text, l.params.AnswerLanguage, l.evidence(query, history))
	if len(slips) == 0 {
		return text
	}
	if len(slips) > languageFixMaxLines {
		slips = slips[:languageFixMaxLines]
	}
	log.Printf("[deeploop] session=%s language check: %d lines sent back", l.params.SessionID, len(slips))
	l.step("deep_critic", StepMessage(l.params.UILanguage, "loop_language", len(slips)))
	reply, err := l.askLines(ctx, languageFixMessage(slips, l.params.AnswerLanguage))
	if err != nil {
		log.Printf("[deeploop] session=%s language check: no reply (%v), answer kept as it was", l.params.SessionID, err)
		l.stats.LanguageSlips = slips
		return text
	}
	out, slips := applyLanguageFixes(text, slips, reply)
	l.stats.LanguageSlips = slips
	return out
}

// askLines sends one message outside the conversation and returns the reply.
func (l *deepLoop) askLines(ctx context.Context, message string) (string, error) {
	fixCtx, cancel := context.WithTimeout(ctx, languageFixTimeout)
	defer cancel()
	var reply strings.Builder
	cut := false
	err := l.client.StreamChatWithOptions(fixCtx, []llm.Message{{Role: "user", Content: message}},
		l.chatOptions(nil), func(tok llm.StreamToken) error {
			switch tok.Type {
			case llm.TokenContent:
				reply.WriteString(tok.Text)
			case llm.TokenTruncated:
				cut = true
			case llm.TokenReasoning:
				return l.emit(Event{Type: EventReasoning, Data: tok.Text})
			}
			return nil
		})
	out := reply.String()
	if cut {
		// The last line stopped half way: the line of the answer stays as it was.
		out = out[:strings.LastIndexByte(out, '\n')+1]
	}
	return out, err
}
