package reader

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

type mockReader struct {
	name       string
	docToRet   *Document
	errToRet   error
	calledURLs []string
}

func (m *mockReader) Name() string {
	return m.name
}

func (m *mockReader) Read(ctx context.Context, targetURL string) (*Document, error) {
	m.calledURLs = append(m.calledURLs, targetURL)
	if m.errToRet != nil {
		return nil, m.errToRet
	}
	return m.docToRet, nil
}

func TestDirectHTTPClient_HTMLParsing(t *testing.T) {
	htmlContent := `
<!DOCTYPE html>
<html>
<head>
    <title>Test Page Title</title>
    <meta name="description" content="A test meta description for Vedetta">
    <script>var x = 10;</script>
    <style>body { color: red; }</style>
</head>
<body>
    <header><nav><a href="/">Home</a></nav></header>
    <h1>Main Heading</h1>
    <p>This is the first paragraph with important informational content that satisfies density requirements.</p>
    <p>This is the second paragraph providing empirical evidence and extensive domain context for the search query.</p>
    <footer>Copyright 2026</footer>
</body>
</html>`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(htmlContent))
	}))
	defer ts.Close()

	client := NewDirectHTTPClient()
	// Disable SSRF for localhost httptest server during test by validating parse logic directly
	title, desc, content, err := extractHTMLText([]byte(htmlContent))
	if err != nil {
		t.Fatalf("extractHTMLText error: %v", err)
	}

	if title != "Test Page Title" {
		t.Errorf("expected title 'Test Page Title', got %q", title)
	}
	if desc != "A test meta description for Vedetta" {
		t.Errorf("expected desc 'A test meta description for Vedetta', got %q", desc)
	}
	if !strings.Contains(content, "Main Heading") {
		t.Errorf("expected content to contain 'Main Heading', got: %s", content)
	}
	if !strings.Contains(content, "first paragraph") {
		t.Errorf("expected content to contain 'first paragraph', got: %s", content)
	}
	if strings.Contains(content, "var x = 10") {
		t.Errorf("script tag was not stripped from content")
	}
	if strings.Contains(content, "color: red") {
		t.Errorf("style tag was not stripped from content")
	}
	if strings.Contains(content, "Copyright 2026") {
		t.Errorf("footer was not stripped from content")
	}

	// Verify client rejects PDF URL before making network call
	_, err = client.Read(context.Background(), "https://example.com/paper.pdf")
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("expected ErrUnsupportedFormat for PDF URL, got %v", err)
	}
}

func TestFirecrawlClient(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"success": true,
			"data": {
				"markdown": "# Firecrawl Result\n\nExtracted content from SPA.",
				"metadata": {
					"title": "SPA Title",
					"description": "SPA Description",
					"sourceURL": "https://example.com/spa"
				}
			}
		}`))
	}))
	defer ts.Close()

	// Missing API key fails
	fcMissing := NewFirecrawlClient("", ts.URL)
	_, err := fcMissing.Read(context.Background(), "https://example.com")
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("expected error for missing API key, got %v", err)
	}
}

func TestCompositeReader_SingleProviderRule6(t *testing.T) {
	// Single provider with error -> hard failure
	mock := &mockReader{name: "jina", errToRet: errors.New("timeout 504")}
	comp := NewCompositeReader(CompositeReaderConfig{
		Chain: []Reader{mock},
	})

	_, err := comp.Read(context.Background(), "https://example.com")
	if err == nil || !strings.Contains(err.Error(), "timeout 504") {
		t.Fatalf("expected hard failure for single provider, got %v", err)
	}

	used, _ := comp.FallbackStatus()
	if used {
		t.Errorf("fallback should not be marked used when no fallback occurred (single provider)")
	}
}

func TestCompositeReader_MultiProviderFallback(t *testing.T) {
	primary := &mockReader{name: "http", errToRet: errors.New("403 Forbidden")}
	secondary := &mockReader{
		name: "jina",
		docToRet: &Document{
			Title:   "Fallback Doc",
			URL:     "https://example.com",
			Content: "Content recovered via Jina reader fallback.",
		},
	}

	comp := NewCompositeReader(CompositeReaderConfig{
		Chain: []Reader{primary, secondary},
	})

	sessionID := "test-session-123"
	ctx := WithSessionID(context.Background(), sessionID)

	doc, err := comp.Read(ctx, "https://example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.Title != "Fallback Doc" {
		t.Errorf("expected 'Fallback Doc', got %q", doc.Title)
	}

	used, reason := comp.FallbackStatus(sessionID)
	if !used {
		t.Errorf("expected fallbackUsed=true")
	}
	if !strings.Contains(reason, "http") || !strings.Contains(reason, "jina") {
		t.Errorf("unexpected fallback reason: %q", reason)
	}

	// Verify per-session isolation
	otherUsed, _ := comp.FallbackStatus("different-session")
	if otherUsed {
		t.Errorf("different session should not inherit fallback status")
	}

	// Verify ResetFallback
	comp.ResetFallback(sessionID)
	resetUsed, _ := comp.FallbackStatus(sessionID)
	if resetUsed {
		t.Errorf("fallback status should be reset")
	}
}

func TestCompositeReader_SelectivePDFRouting(t *testing.T) {
	httpReader := &mockReader{name: "http", errToRet: errors.New("should not be called for PDF")}
	jinaReader := &mockReader{
		name: "jina",
		docToRet: &Document{
			Title:   "PDF Article",
			URL:     "https://example.com/doc.pdf",
			Content: "Extracted PDF content.",
		},
	}

	comp := NewCompositeReader(CompositeReaderConfig{
		Chain: []Reader{httpReader, jinaReader},
	})

	sessionID := "pdf-session"
	ctx := WithSessionID(context.Background(), sessionID)

	doc, err := comp.Read(ctx, "https://example.com/doc.pdf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.Title != "PDF Article" {
		t.Errorf("expected 'PDF Article', got %q", doc.Title)
	}
	if len(httpReader.calledURLs) > 0 {
		t.Errorf("http reader should have been bypassed for PDF, but was called: %v", httpReader.calledURLs)
	}

	used, reason := comp.FallbackStatus(sessionID)
	if !used {
		t.Errorf("expected fallback to be marked used for PDF routing")
	}
	if !strings.Contains(reason, "PDF") {
		t.Errorf("expected PDF mentioned in reason, got %q", reason)
	}
}

func TestCompositeReader_SelectiveSPAFallback(t *testing.T) {
	httpReader := &mockReader{
		name: "http",
		docToRet: &Document{
			Title:   "SPA App",
			URL:     "https://example.com/app",
			Content: "Loading...", // density < 250
		},
	}
	jinaReader := &mockReader{
		name: "jina",
		docToRet: &Document{
			Title:   "SPA App",
			URL:     "https://example.com/app",
			Content: "Full JavaScript rendered content that contains comprehensive information about the application state.",
		},
	}

	comp := NewCompositeReader(CompositeReaderConfig{
		Chain: []Reader{httpReader, jinaReader},
	})

	sessionID := "spa-session"
	ctx := WithSessionID(context.Background(), sessionID)

	doc, err := comp.Read(ctx, "https://example.com/app")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(doc.Content, "Full JavaScript") {
		t.Errorf("expected higher density content from Jina, got %q", doc.Content)
	}

	used, reason := comp.FallbackStatus(sessionID)
	if !used {
		t.Errorf("expected fallback to be marked used for low-density SPA")
	}
	if !strings.Contains(reason, "SPA") {
		t.Errorf("expected SPA mentioned in reason, got %q", reason)
	}
}

// TestCompositeReader_SPAFallbackNChain verifies that a low-density SPA probes the
// whole remaining chain, not just the immediate next reader.
func TestCompositeReader_SPAFallbackNChain(t *testing.T) {
	httpReader := &mockReader{
		name: "http",
		docToRet: &Document{
			Title:   "SPA App",
			URL:     "https://example.com/app",
			Content: "Loading...",
		},
	}
	jinaReader := &mockReader{
		name: "jina",
		docToRet: &Document{
			Title:   "SPA App",
			URL:     "https://example.com/app",
			Content: "Still low density", // also < 250
		},
	}
	firecrawlReader := &mockReader{
		name: "firecrawl",
		docToRet: &Document{
			Title:   "SPA App",
			URL:     "https://example.com/app",
			Content: strings.Repeat("Rendered headless content with full JavaScript execution. ", 10),
		},
	}

	comp := NewCompositeReader(CompositeReaderConfig{
		Chain: []Reader{httpReader, jinaReader, firecrawlReader},
	})

	sessionID := "spa-nchain"
	ctx := WithSessionID(context.Background(), sessionID)

	doc, err := comp.Read(ctx, "https://example.com/app")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(doc.Content, "Rendered headless") {
		t.Errorf("expected content from the third reader (firecrawl), got %q", doc.Content)
	}
	used, reason := comp.FallbackStatus(sessionID)
	if !used || !strings.Contains(reason, "firecrawl") {
		t.Errorf("expected fallback reason to reference firecrawl, got used=%t reason=%q", used, reason)
	}
}

// ncbiBlockPageContent mimics the ~888-char anti-bot page served by NCBI/PubMed
// when the plain HTTP reader is blocked.
func ncbiBlockPageContent() string {
	body := "Your access to this site has been blocked by the NCBI error diagnostic system. " +
		"The request was flagged as automated or unusual traffic. If you are a human, " +
		"please return to the previous page and try again. "
	for len(body) < 888 {
		body += "Please verify you are a human. "
	}
	return body[:888]
}

// TestCompositeReader_BlockPageEscalation verifies that an anti-bot/error block
// page returned by the plain HTTP reader (whose length exceeds the SPA
// threshold) is detected and escalated to the next reader in the chain.
func TestCompositeReader_BlockPageEscalation(t *testing.T) {
	httpReader := &mockReader{
		name: "http",
		docToRet: &Document{
			Title:   "NCBI - WWW Error Blocked Diagnostic",
			URL:     "https://pubmed.ncbi.nlm.nih.gov/12345/",
			Content: ncbiBlockPageContent(),
		},
	}
	jinaReader := &mockReader{
		name: "jina",
		docToRet: &Document{
			Title:   "Real PubMed Article",
			URL:     "https://pubmed.ncbi.nlm.nih.gov/12345/",
			Content: strings.Repeat("Abstract: real article content recovered by the fallback reader. ", 30),
		},
	}

	if len(strings.TrimSpace(httpReader.docToRet.Content)) <= 250 {
		t.Fatalf("test setup: block page must exceed the 250-char SPA threshold, got %d", len(httpReader.docToRet.Content))
	}

	comp := NewCompositeReader(CompositeReaderConfig{
		Chain: []Reader{httpReader, jinaReader},
	})

	sessionID := "block-page-session"
	ctx := WithSessionID(context.Background(), sessionID)

	doc, err := comp.Read(ctx, "https://pubmed.ncbi.nlm.nih.gov/12345/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc != jinaReader.docToRet {
		t.Fatalf("expected the jina document to win escalation, got %+v", doc)
	}
	if !strings.Contains(doc.Content, "real article content") {
		t.Errorf("expected real content from jina, got %q", doc.Content)
	}

	used, reason := comp.FallbackStatus(sessionID)
	if !used {
		t.Errorf("expected fallbackUsed=true for a block page")
	}
	if !strings.Contains(reason, "block page") {
		t.Errorf("expected reason to mention 'block page', got %q", reason)
	}
}

// TestCompositeReader_BlockPageNoBetterDoc documents the honest fallback policy:
// when the block page is detected but no reader in the remaining chain yields a
// better document, the original document is returned as-is (no error). Here the
// secondary reader also returns a block page of equal/smaller length.
func TestCompositeReader_BlockPageNoBetterDoc(t *testing.T) {
	httpReader := &mockReader{
		name: "http",
		docToRet: &Document{
			Title:   "NCBI - WWW Error Blocked Diagnostic",
			URL:     "https://pubmed.ncbi.nlm.nih.gov/99999/",
			Content: ncbiBlockPageContent(),
		},
	}
	jinaReader := &mockReader{
		name: "jina",
		docToRet: &Document{
			Title:   "Attention Required! | Cloudflare",
			URL:     "https://pubmed.ncbi.nlm.nih.gov/99999/",
			Content: "Attention Required! Please enable cookies to continue.",
		},
	}

	comp := NewCompositeReader(CompositeReaderConfig{
		Chain: []Reader{httpReader, jinaReader},
	})

	sessionID := "block-page-no-better"
	ctx := WithSessionID(context.Background(), sessionID)

	doc, err := comp.Read(ctx, "https://pubmed.ncbi.nlm.nih.gov/99999/")
	if err != nil {
		t.Fatalf("expected no error when no better document exists, got %v", err)
	}
	// The original document is preserved (honest behavior): a second block page
	// is not considered better, so no fallback is recorded.
	if doc != httpReader.docToRet {
		t.Fatalf("expected the original http document, got %+v", doc)
	}
	if !strings.Contains(doc.Title, "Error Blocked Diagnostic") {
		t.Errorf("expected original block page title, got %q", doc.Title)
	}
	used, _ := comp.FallbackStatus(sessionID)
	if used {
		t.Errorf("no fallback should be recorded when the original document is kept")
	}
}

func TestRegistry_NewReader(t *testing.T) {
	// Default to Jina
	rDefault, err := NewReader(ReaderConfig{
		Providers:   nil,
		JinaBaseURL: "http://localhost:3001",
	})
	if err != nil {
		t.Fatalf("failed to create default reader: %v", err)
	}
	if rDefault.Name() != "jina-reader" {
		t.Errorf("expected 'jina-reader', got %q", rDefault.Name())
	}

	// HTTP + Jina chain
	rMulti, err := NewReader(ReaderConfig{
		Providers:   []string{"http", "jina"},
		JinaBaseURL: "http://localhost:3001",
	})
	if err != nil {
		t.Fatalf("failed to create multi reader: %v", err)
	}
	if rMulti.Name() != "http" {
		t.Errorf("expected primary name 'http', got %q", rMulti.Name())
	}

	// Missing Firecrawl API key fails
	_, err = NewReader(ReaderConfig{
		Providers: []string{"firecrawl"},
	})
	if err == nil || !strings.Contains(err.Error(), "FIRECRAWL_API_KEY") {
		t.Errorf("expected error for missing Firecrawl key, got %v", err)
	}

	// Unknown provider fails
	_, err = NewReader(ReaderConfig{
		Providers: []string{"nonexistent"},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown reader provider") {
		t.Errorf("expected error for unknown provider, got %v", err)
	}
}

// M1.0: every page read through the chain logs the reader that served it.
func TestCompositeReader_LogsServedBy(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	content := strings.Repeat("Readable article text. ", 20)
	tests := []struct {
		name    string
		primary *mockReader
		want    string
	}{
		{"primary ok", &mockReader{name: "http", docToRet: &Document{Content: content}}, "served_by=http"},
		{"primary error", &mockReader{name: "http", errToRet: errors.New("status 403")}, "served_by=firecrawl"},
		{"primary too short", &mockReader{name: "http", docToRet: &Document{Content: "Loading..."}}, "served_by=firecrawl"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf.Reset()
			secondary := &mockReader{name: "firecrawl", docToRet: &Document{Content: content}}
			comp := NewCompositeReader(CompositeReaderConfig{Chain: []Reader{tt.primary, secondary}})
			ctx := WithSessionID(context.Background(), "sess-served")

			if _, err := comp.Read(ctx, "https://example.com/page"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			out := buf.String()
			if !strings.Contains(out, "session=sess-served url=https://example.com/page "+tt.want) {
				t.Errorf("want %q in log, got %q", tt.want, out)
			}
		})
	}
}

func TestDirectHTTPClient_OlderEncodings(t *testing.T) {
	body := strings.Repeat("La psicologia del S\xe9 e il perch\xe9 di una teoria. ", 5)
	utf8Body := strings.Repeat("La psicologia del Sé e il perché di una teoria. ", 5)
	cases := []struct{ name, contentType, page string }{
		{"declared by the server", "text/html; charset=iso-8859-1", "<html><head><title>\xa9 Psicologia del S\xe9</title></head><body><p>" + body + "</p></body></html>"},
		{"declared by the page", "text/html", "<html><head><meta charset=\"iso-8859-1\"><title>\xa9 Psicologia del S\xe9</title></head><body><p>" + body + "</p></body></html>"},
		{"not declared", "text/html", "<html><head><title>\xa9 Psicologia del S\xe9</title></head><body><p>" + body + "</p></body></html>"},
		{"UTF-8 wrongly declared as Latin-1", "text/html; charset=iso-8859-1", "<html><head><title>© Psicologia del Sé</title></head><body><p>" + utf8Body + "</p></body></html>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			title, _, content, err := extractHTMLText(toUTF8([]byte(c.page), c.contentType))
			if err != nil {
				t.Fatal(err)
			}
			if title != "© Psicologia del Sé" {
				t.Errorf("title = %q", title)
			}
			if !strings.Contains(content, "psicologia del Sé e il perché") {
				t.Errorf("content = %q", content)
			}
		})
	}
}
