package agent

import (
	"fmt"
	"strings"

	"vedetta/internal/reader"
)

// Reading outcomes (V25-T2.12). Only readOK documents reach the extractor: a
// block page, an almost empty page or a failed read is not evidence, and its
// slot goes to the next candidate of the same node.
type readStatus string

const (
	readOK      readStatus = "ok"
	readBlocked readStatus = "blocked" // anti-bot or access-denied interstitial
	readThin    readStatus = "thin"    // fewer than minUsefulChars of text
	readFailed  readStatus = "failed"  // reader error or empty body
)

// minUsefulChars is the shortest page text worth an extraction call. Below it
// are JS shells (TripAdvisor 179 chars), stubs and search-result snippets.
const minUsefulChars = 300

// classifyRead decides whether a read document can go to the extractor.
func classifyRead(doc *reader.Document, err error) (readStatus, string) {
	if err != nil {
		return readFailed, err.Error()
	}
	if doc == nil || strings.TrimSpace(doc.Content) == "" {
		return readFailed, "empty body"
	}
	if marker := reader.BlockPageMarker(doc); marker != "" {
		return readBlocked, fmt.Sprintf("marker %q", marker)
	}
	if n := len([]rune(strings.TrimSpace(doc.Content))); n < minUsefulChars {
		return readThin, fmt.Sprintf("%d chars", n)
	}
	return readOK, ""
}
