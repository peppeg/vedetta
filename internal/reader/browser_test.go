package reader

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestBrowserVersion(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"Browser":"Chrome/151.0.7922.34","Protocol-Version":"1.3"}`))
	}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/devtools/browser/abc?token=secret"
	if v, err := BrowserVersion(context.Background(), ws); err != nil || v != "Chrome/151.0.7922.34" {
		t.Errorf("version = %q, %v", v, err)
	}
	if _, err := BrowserVersion(context.Background(), "not a url"); err == nil {
		t.Error("invalid address: want error")
	}
}

func TestBrowserDoctor(t *testing.T) {
	version := "Chrome/151.0.7922.34"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"Browser":"` + version + `"}`))
	}))
	defer ts.Close()
	ctx := context.Background()

	status, msg := BrowserDoctor(ctx, "")
	if status != "unknown" || !strings.Contains(msg, "not configured") || strings.ContainsAny(msg, "%0123456789") {
		t.Errorf("not configured: %s %q (no numbers about what is lost)", status, msg)
	}
	if status, msg = BrowserDoctor(ctx, ts.URL); status != "ok" || !strings.Contains(msg, "tested with Vedetta on 2026-10-20") {
		t.Errorf("tested: %s %q", status, msg)
	}
	version = "Chrome/160.0.1.2"
	if status, msg = BrowserDoctor(ctx, ts.URL); status != "ok" || !strings.Contains(msg, "not yet tested") {
		t.Errorf("untested: %s %q", status, msg)
	}
	if status, msg = BrowserDoctor(ctx, "ws://127.0.0.1:1/?token=secret"); status != "down" || strings.Contains(msg, "secret") {
		t.Errorf("down: %s %q", status, msg)
	}
}

func TestBrowserReader_Registry(t *testing.T) {
	if _, err := NewReader(ReaderConfig{Providers: []string{"http", "browser"}}); err == nil {
		t.Error("browser without address: want error")
	}
	r, err := NewReader(ReaderConfig{Providers: []string{"http", "browser"}, Browser: &BrowserReaderConfig{CDPURL: "ws://127.0.0.1:9222"}})
	if err != nil || r == nil {
		t.Errorf("browser with address: %v", err)
	}
	if st := ProbeReader(context.Background(), "browser", ReaderConfig{}); st.Status != "unknown" {
		t.Errorf("probe without address = %+v", st)
	}
}

func TestBrowserReader_RefusesPrivateAddress(t *testing.T) {
	b := NewBrowserReader(BrowserReaderConfig{CDPURL: "ws://127.0.0.1:1"})
	if _, err := b.Read(context.Background(), "http://192.168.1.1/"); err == nil || !strings.Contains(err.Error(), "ssrf") {
		t.Errorf("want ssrf error, got %v", err)
	}
	if _, err := b.Read(context.Background(), "https://example.com/"); err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("browser down: want not reachable, got %v", err)
	}
}

func TestRedactBrowserURL(t *testing.T) {
	for in, want := range map[string]string{
		"ws://user:pw@host:9222/devtools/browser/x?token=t": "ws://host:9222",
		"":  "",
		"%": "(invalid address)",
	} {
		if got := RedactBrowserURL(in); got != want {
			t.Errorf("RedactBrowserURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// Integration tests against a real browser: VEDETTA_TEST_CDP_URL=ws://127.0.0.1:9222
// (needs internet). Run with Chromium and with Lightpanda (SPEC_BROWSER B.1, B.2).
func testBrowser(t *testing.T) *BrowserReader {
	t.Helper()
	cdpURL := os.Getenv("VEDETTA_TEST_CDP_URL")
	if cdpURL == "" {
		t.Skip("VEDETTA_TEST_CDP_URL not set")
	}
	return NewBrowserReader(BrowserReaderConfig{CDPURL: cdpURL})
}

func TestBrowserReader_Integration_Reads(t *testing.T) {
	b := testBrowser(t)
	for i := 0; i < 2; i++ { // twice: the room of the first page must be gone
		html, final, err := b.render(context.Background(), "https://example.com/")
		if err != nil || !strings.Contains(html, "Example Domain") || !strings.HasPrefix(final, "https://example.com") {
			t.Fatalf("read %d: final %q err %v html %d bytes", i, final, err, len(html))
		}
	}
	// A document, or (if the page is under 250 characters) ErrLowDensityContent so the chain goes on.
	doc, err := b.Read(context.Background(), "https://example.com/")
	if err != nil && !errors.Is(err, ErrLowDensityContent) {
		t.Errorf("read: %v", err)
	}
	if err == nil && (!strings.Contains(doc.Title, "Example Domain") || doc.Metadata["source_type"] != "browser") {
		t.Errorf("doc = %q %v", doc.Title, doc.Metadata)
	}
}

func TestBrowserReader_Integration_BlocksRedirectToPrivate(t *testing.T) {
	b := testBrowser(t)
	_, err := b.Read(context.Background(), "https://httpbin.org/redirect-to?url=http%3A%2F%2F127.0.0.1%3A9%2Fsecret")
	if err == nil {
		t.Fatal("redirect to a private address was read")
	}
	t.Logf("refused as expected: %v", err)
}

// V25-F0.2: the doctor looks at the whole chain, not only at the browser.
func TestChainDoctor(t *testing.T) {
	ctx := context.Background()
	jina := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[Usage 1] https://r.jina.ai/YOUR_URL\n")) // Jina's home page (map R8 doctor)
	}))
	defer jina.Close()
	browser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"Browser":"HeadlessChrome/151.0.1.2"}`))
	}))
	defer browser.Close()

	name, status, msg := ChainDoctor(ctx, ReaderConfig{Providers: []string{"http", "jina-reader"}, JinaBaseURL: jina.URL})
	if name != "jina-reader" || status != "ok" || !strings.Contains(msg, "no browser in the chain") {
		t.Errorf("http,jina: %q %s %q", name, status, msg)
	}
	name, status, msg = ChainDoctor(ctx, ReaderConfig{Providers: []string{"http", "browser", "jina"}, JinaBaseURL: jina.URL,
		Browser: &BrowserReaderConfig{CDPURL: browser.URL}})
	if name != "browser" || status != "ok" || !strings.Contains(msg, "tested with Vedetta") {
		t.Errorf("http,browser,jina: %q %s %q", name, status, msg)
	}
	if name, status, _ = ChainDoctor(ctx, ReaderConfig{Providers: []string{"http", "browser"}}); name != "browser" || status != "unknown" {
		t.Errorf("browser without address: %q %s", name, status)
	}
	name, status, msg = ChainDoctor(ctx, ReaderConfig{Providers: []string{"http"}})
	if name != "" || status != "unknown" || !strings.Contains(msg, "No fallback reader configured") ||
		strings.ContainsAny(msg, "%0123456789") {
		t.Errorf("http only: %q %s %q (no numbers about what is lost)", name, status, msg)
	}
	jina.Close()
	if _, status, _ = ChainDoctor(ctx, ReaderConfig{Providers: []string{"http", "jina"}, JinaBaseURL: jina.URL}); status != "down" {
		t.Errorf("jina down: %s", status)
	}
}
