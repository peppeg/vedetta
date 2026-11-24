package agent

import (
	"strings"
)

// Centralized language-aware UI/report labels (Vedetta v2.4, F5 — English-first).

// NotesMarker separates the answer the model wrote from the notes Go appends after
// it (method, limits, provenance, pages read). The model may use "---" rules inside
// its own answer, so the interface cuts at this marker, never at a rule. It is an
// HTML comment: invisible in any Markdown viewer.
const NotesMarker = "<!-- vedetta:notes -->"

// notesStart is what a pipeline writes, once, where its notes begin.
const notesStart = "\n\n" + NotesMarker

// withoutNotes is what the model wrote in a saved answer. A follow-up must not hand it
// Vedetta's notes back: it copied them, and one answer went out with an invented report
// of the checks next to the real one (4 conversations, 06/11/2026).
func withoutNotes(content string) string {
	if i := strings.Index(content, NotesMarker); i >= 0 {
		return strings.TrimRight(content[:i], " \n")
	}
	return content
}
