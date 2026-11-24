package agent

import (
	"context"
	"strings"
	"testing"

	"vedetta/internal/reader"
	"vedetta/internal/search"
)

type mockSearchProvider struct {
	results []search.Result
	err     error
}

func (m *mockSearchProvider) Name() string {
	return "mock-search"
}

func (m *mockSearchProvider) Search(ctx context.Context, query string, opts search.Options) ([]search.Result, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.results, nil
}

type mockReader struct {
	docs map[string]reader.Document
	err  error
}

func (m *mockReader) Name() string {
	return "mock-reader"
}

func (m *mockReader) Read(ctx context.Context, targetURL string) (*reader.Document, error) {
	if m.err != nil {
		return nil, m.err
	}
	if doc, ok := m.docs[targetURL]; ok {
		doc.Content = padTestPage(doc.Content)
		return &doc, nil
	}
	return &reader.Document{
		URL:     targetURL,
		Title:   "Sample Title",
		Content: padTestPage("Sample Content with dual-pipe decoder 4+4 architecture."),
	}, nil
}

// padTestPage lengthens a fake page past minUsefulChars, so that the short test
// fixtures are still read as real pages (V25-T2.12 drops almost empty pages).
// The filler is appended after the text, leaving evidence alignment unchanged.
func padTestPage(content string) string {
	if len(content) >= minUsefulChars || content == "" {
		return content
	}
	return content + "\n\n" + strings.Repeat("Filler paragraph for test fixtures. ", minUsefulChars/36+1)
}

func TestGetLanguageName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"it", "Italian"},
		{"italian", "Italian"},
		{"IT", "Italian"},
		{"en", "English"},
		{"english", "English"},
		{"de", "German"},
		{"german", "German"},
		{"fr", "French"},
		{"french", "French"},
		{"es", "Spanish"},
		{"spanish", "Spanish"},
		{"unknown", "English"},
		{"", "English"},
	}

	for _, tc := range tests {
		got := GetLanguageName(tc.input)
		if got != tc.expected {
			t.Errorf("GetLanguageName(%q) = %q; want %q", tc.input, got, tc.expected)
		}
	}
}
