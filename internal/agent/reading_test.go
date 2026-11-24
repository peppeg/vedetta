package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"vedetta/internal/reader"
)

func TestClassifyRead(t *testing.T) {
	long := strings.Repeat("Testo vero di un articolo. ", 20)
	cases := []struct {
		name string
		doc  *reader.Document
		err  error
		want readStatus
	}{
		{"ok", &reader.Document{Content: long}, nil, readOK},
		{"error", nil, errors.New("timeout"), readFailed},
		{"empty", &reader.Document{Content: "   "}, nil, readFailed},
		{"pubmed interstitial", &reader.Document{Title: "pubmed.ncbi.nlm.nih.gov", Content: "Cookies must be enabled. Enable cookies and reload this page to continue."}, nil, readBlocked},
		{"js shell", &reader.Document{Title: "TripAdvisor", Content: "Loading… Sign in"}, nil, readThin},
		// The Fedora release notes on 17/11/2026: an Anubis challenge, more than 300 characters, was a page "read".
		{"anubis challenge", &reader.Document{Title: "Controllo se sei un robot...", Content: "Vedi questa schermata perché l'amministratore di questo sito web ha installato Anubis per proteggere il server. " +
			"Anubis utilizza un meccanismo di proof-of-work in stile Hashcash, un meccanismo per ridurre le email di spam. " + long}, nil, readBlocked},
	}
	for _, c := range cases {
		if got, _ := classifyRead(c.doc, c.err); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

// failOnReader fails the reads of some URLs and serves the others from mockReader.
type failOnReader struct {
	mockReader
	fail map[string]bool
}

func (f *failOnReader) Read(ctx context.Context, targetURL string) (*reader.Document, error) {
	if f.fail[targetURL] {
		return nil, fmt.Errorf("read failed: %s", targetURL)
	}
	return f.mockReader.Read(ctx, targetURL)
}
