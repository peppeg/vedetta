package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vedetta/internal/reader"
)

type namedReader struct {
	name string
	err  error
}

func (r namedReader) Name() string { return r.name }
func (r namedReader) Read(ctx context.Context, u string) (*reader.Document, error) {
	if r.err != nil {
		return nil, r.err
	}
	return &reader.Document{URL: u, Content: strings.Repeat("testo della pagina ", 30)}, nil
}

func TestReaderFallbackNote(t *testing.T) {
	cr := reader.NewCompositeReader(reader.CompositeReaderConfig{Chain: []reader.Reader{
		namedReader{name: "http", err: errors.New("blocked")}, namedReader{name: "jina-reader"},
	}})
	if got := readerFallbackNote("it", cr, "x"); got != "" {
		t.Fatalf("no reads yet, want no note, got %q", got)
	}
	_, _ = cr.Read(reader.WithSessionID(context.Background(), "x"), "https://example.org")
	got := readerFallbackNote("it", cr, "x")
	want := "Alcune pagine sono state lette con Jina Reader perché il lettore interno non ci riusciva."
	if !strings.Contains(got, want) {
		t.Fatalf("got %q", got)
	}
	if en := readerFallbackNote("en", cr, "x"); !strings.Contains(en, "Some pages were read with Jina Reader because the built-in reader could not read them.") {
		t.Fatalf("en: %q", en)
	}
}
