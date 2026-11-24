package agent

// The entry of scripts/quoteprobe (docs/SPEC_FASE6_CITAZIONI.md, work 6.0): the request
// of work 6.1 on an answer already saved, outside Vedetta, to see whether a model
// copies true passages. Same functions as the product, so a measure never follows a rule of its own.

import (
	"encoding/json"
	"strconv"
	"time"

	"vedetta/internal/llm"
)

// QuoteProbePage is a page the model read for the answer.
type QuoteProbePage struct {
	URL   string
	Title string
	Text  string
}

// QuoteProbe is a conversation rebuilt from a saved answer: the system prompt, the
// question, one read per page with its result, the answer, and the request.
type QuoteProbe struct {
	Messages []llm.Message // empty when there is nothing to ask
	Units    []string
	pages    []quotePage
}

// NewQuoteProbe rebuilds the conversation. The searches and the reasoning of the real
// one are not there. answer may still hold Vedetta's notes: they are cut.
func NewQuoteProbe(question, answer string, pages []QuoteProbePage) *QuoteProbe {
	lang := DetectTextLanguage(question)
	if lang == "" {
		lang = "en"
	}
	body := withoutNotes(answer)
	p := &QuoteProbe{Units: quoteUnits(body, StepMessage(lang, "deduction_label"))}
	for i, pg := range pages {
		p.pages = append(p.pages, newQuotePage("P"+itoa(i+1), pg.URL, pg.Text))
	}
	if len(p.Units) == 0 || len(p.pages) == 0 {
		p.Units = nil
		return p
	}
	var calls []llm.ToolCall
	var results []llm.Message
	for i, pg := range pages {
		args, _ := json.Marshal(map[string]string{"url": pg.URL})
		id := "call_" + itoa(i+1)
		calls = append(calls, llm.ToolCall{ID: id, Type: "function", Function: llm.ToolCallFunction{Name: "read", Arguments: string(args)}})
		results = append(results, llm.Message{Role: "tool", ToolCallID: id, Content: loopPageBlock("Page", pageTitle(pg.Title), pg.URL, pg.Text)})
	}
	p.Messages = []llm.Message{
		{Role: "system", Content: loopSystemPrompt(lang, time.Now(), false, true)},
		{Role: "user", Content: question},
		{Role: "assistant", ToolCalls: calls},
	}
	p.Messages = append(p.Messages, results...)
	p.Messages = append(p.Messages,
		llm.Message{Role: "assistant", Content: body},
		llm.Message{Role: "user", Content: quotesRequest(p.pages, p.Units)})
	return p
}

// Settle reads the model's reply and looks for each passage on the pages.
func (p *QuoteProbe) Settle(reply string) (items []QuoteItem, lines int) {
	return settleQuotes(p.Units, parseQuotes(reply, len(p.Units)), p.pages)
}

func itoa(i int) string { return strconv.Itoa(i) }

// QuoteProbeHasNumbers reports whether the sentence states a number the check counts
// (small ones too), for the line "numbers in the passage" of the probe's table.
func QuoteProbeHasNumbers(sentence string) bool {
	for _, n := range scanNumbers(sentence) {
		if !n.weak && !n.ident {
			return true
		}
	}
	return false
}
