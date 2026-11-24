package agent

// Follow-ups of the Deep a ciclo (Piano dopo audit 4.4 / D9). A follow-up gets the
// earlier questions and answers without their pages, so a number Vedetta itself
// verified one turn before was flagged as "maybe from memory" (real case 20,
// 09/11/2026). With VEDETTA_DEEP_LOOP_SESSION_PAGES the pages read in the earlier
// turns of the session are seen text for the sentinel's checks again, and the model
// is told which they are and that it may open them again.
//
// Their addresses are in the calls saved with each answer; their text comes from
// the cache, where reading them again costs nothing. A page no longer in the cache
// does not count. The text is the page as the cache holds it: a page that was cut
// in its turn because it did not fit counts whole here.

import (
	"encoding/json"
	"strings"

	"vedetta/internal/cache"
	"vedetta/internal/storage"
)

// sessionPage is a page read in an earlier turn of the session.
type sessionPage struct {
	URL, Title, Text string
}

// sessionPages returns the pages the earlier answers read that are still in the
// cache, oldest first and each once, and how many are gone.
func sessionPages(history []storage.TurnMessage, cacheSess *cache.Session) (pages []sessionPage, gone int) {
	seen := make(map[string]bool)
	for _, m := range history {
		var meta AnswerMeta
		if len(m.Meta) == 0 || json.Unmarshal(m.Meta, &meta) != nil || meta.Loop == nil {
			continue
		}
		for _, c := range meta.Loop.CallsDetail {
			key := normalizeLinkURL(c.URL)
			if c.Tool != "read" || c.Result != "ok" || key == "" || seen[key] {
				continue
			}
			seen[key] = true
			doc, ok := cacheSess.Page(c.URL)
			if status, _ := classifyRead(doc, nil); !ok || status != readOK {
				gone++
				continue
			}
			title := oneLine(doc.Title)
			if title == "" {
				title = c.URL
			}
			pages = append(pages, sessionPage{URL: c.URL, Title: title, Text: doc.Content})
		}
	}
	return pages, gone
}

// pagesReadBefore are the addresses of the pages the earlier answers read, by
// normalizeLinkURL, in the cache or not: a link to one of them leads to a page this
// conversation read. Without it 13 such links were taken out of 11 follow-ups of 80
// (external audit 24/11/2026, BUG-02): the page opened, but did not name its address.
func pagesReadBefore(history []storage.TurnMessage) map[string]string {
	out := make(map[string]string)
	for _, m := range history {
		var meta AnswerMeta
		if len(m.Meta) == 0 || json.Unmarshal(m.Meta, &meta) != nil || meta.Loop == nil {
			continue
		}
		for _, c := range meta.Loop.CallsDetail {
			if key := normalizeLinkURL(c.URL); c.Tool == "read" && c.Result == "ok" && key != "" && out[key] == "" {
				out[key] = c.URL
			}
		}
	}
	return out
}

// earlierPagesNote goes before the question of a follow-up: titles and addresses
// only. It states what is there, it is not a rule for the writer.
func earlierPagesNote(pages []sessionPage) string {
	if len(pages) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Pages read earlier in this conversation. Their text is not above: open one again with read if you need it.\n")
	for _, p := range pages {
		b.WriteString("- " + pageTitle(p.Title) + ": " + p.URL + "\n")
	}
	b.WriteString("\n")
	return b.String()
}
