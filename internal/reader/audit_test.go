package reader

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// External audit of 24/11/2026 (docs/VERIFICA_AUDIT_ESTERNO_2026-11-24.md).

func TestIsPrivateIP_InsideIPv6(t *testing.T) {
	for addr, want := range map[string]bool{
		"64:ff9b::7f00:1":      true,  // NAT64 of 127.0.0.1
		"64:ff9b::c0a8:101":    true,  // NAT64 of 192.168.1.1
		"64:ff9b::5db8:d822":   false, // NAT64 of 93.184.216.34: how a NAT64 network reaches it
		"64:ff9b:1::a00:1":     true,
		"2002:c0a8:101::1":     true, // 6to4 of 192.168.1.1
		"2002:5db8:d822::1":    false,
		"::127.0.0.1":          true,
		"::93.184.216.34":      false,
		"2001:0:1::1":          true, // Teredo
		"2001:db8::1":          false,
		"2606:2800:220:1::1":   false,
		"fec0::1":              true,
		"240.0.0.1":            true,
		"198.18.0.1":           false, // some VPNs give it to every site
		"::ffff:10.0.0.1":      true,
		"::ffff:93.184.216.34": false,
		"93.184.216.34":        false,
	} {
		if got := isPrivateIP(net.ParseIP(addr)); got != want {
			t.Errorf("%s: %t, want %t", addr, got, want)
		}
	}
}

type countingReader struct {
	name  string
	calls atomic.Int32
	err   error
}

func (r *countingReader) Name() string { return r.name }
func (r *countingReader) Read(ctx context.Context, u string) (*Document, error) {
	r.calls.Add(1)
	if r.err != nil {
		return nil, r.err
	}
	return &Document{URL: u, Title: "t", Content: strings.Repeat("segreto ", 100)}, nil
}

// An address refused because it leads inside the home network is not handed to a
// reader that follows redirects by itself.
func TestComposite_PrivateAddressStopsTheChain(t *testing.T) {
	_, refused := NewDirectHTTPClient().Read(context.Background(), "http://127.0.0.1:9/x")
	if !errors.Is(refused, ErrPrivateAddress) {
		t.Fatalf("the direct reader: %v", refused)
	}
	dialed := safeDialControl("tcp", "10.0.0.1:80", nil)
	for name, err := range map[string]error{"address": refused, "connection": dialed} {
		http, jina := &countingReader{name: "http", err: err}, &countingReader{name: "jina-reader"}
		c := NewCompositeReader(CompositeReaderConfig{Chain: []Reader{http, jina}})
		if doc, got := c.Read(context.Background(), "https://sito.test/rimanda"); doc != nil || !errors.Is(got, ErrPrivateAddress) || jina.calls.Load() != 0 {
			t.Errorf("%s: doc %v, err %v, jina asked %d times", name, doc, got, jina.calls.Load())
		}
		// the PDF reader goes first on a PDF address, and where the first reader finds a PDF
		pdf, jina2 := &countingReader{name: "pdf", err: err}, &countingReader{name: "jina-reader"}
		c = NewCompositeReader(CompositeReaderConfig{Chain: []Reader{&countingReader{name: "http"}, jina2}, PDF: pdf})
		if doc, got := c.Read(context.Background(), "https://sito.test/legge.pdf"); doc != nil || !errors.Is(got, ErrPrivateAddress) || jina2.calls.Load() != 0 {
			t.Errorf("%s, PDF: doc %v, err %v, jina asked %d times", name, doc, got, jina2.calls.Load())
		}
		jina3 := &countingReader{name: "jina-reader"}
		c = NewCompositeReader(CompositeReaderConfig{Chain: []Reader{&countingReader{name: "http", err: ErrPDFContent}, jina3}, PDF: pdf})
		if doc, got := c.Read(context.Background(), "https://sito.test/legge"); doc != nil || !errors.Is(got, ErrPrivateAddress) || jina3.calls.Load() != 0 {
			t.Errorf("%s, PDF found: doc %v, err %v, jina asked %d times", name, doc, got, jina3.calls.Load())
		}
	}
	// any other trouble still goes on
	jina := &countingReader{name: "jina-reader"}
	c := NewCompositeReader(CompositeReaderConfig{Chain: []Reader{&countingReader{name: "http", err: errors.New("timeout")}, jina}})
	if doc, err := c.Read(context.Background(), "https://sito.test/x"); err != nil || doc == nil {
		t.Errorf("an ordinary failure: %v", err)
	}
}

func TestNewReader_OnePDFReader(t *testing.T) {
	cfg := ReaderConfig{Providers: []string{"http"}, PDF: &PDFReaderConfig{MaxBytes: 1, Idle: time.Minute}}
	a, err1 := NewReader(cfg)
	b, err2 := NewReader(cfg)
	other, err3 := NewReader(ReaderConfig{Providers: []string{"http"}, PDF: &PDFReaderConfig{MaxBytes: 2, Idle: time.Minute}})
	if err1 != nil || err2 != nil || err3 != nil {
		t.Fatal(err1, err2, err3)
	}
	pdfOf := func(r Reader) Reader { return r.(*CompositeReader).pdf }
	if pdfOf(a) != pdfOf(b) {
		t.Error("two chains with the same configuration have two PDF engines")
	}
	if pdfOf(a) == pdfOf(other) {
		t.Error("another configuration needs a reader of its own")
	}
}

func TestPDFReader_WaitingStopsWithTheCaller(t *testing.T) {
	p := NewPDFReader(PDFReaderConfig{})
	if err := p.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, _, _, err := p.extract(ctx, []byte("%PDF-")); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrLowDensityContent) {
			t.Errorf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a reading behind a stuck PDF waits for ever")
	}
	p.unlock()
}

// A script of a public page asks for something on this machine, which would answer
// anybody: nothing leaves the browser. VEDETTA_TEST_CDP_URL names the browser.
func TestBrowserReader_Integration_ScriptsStayOutOfTheHomeNetwork(t *testing.T) {
	b := testBrowser(t)
	var asked atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_, _ = w.Write([]byte("segreto di casa"))
	}))
	defer local.Close()
	try := func(guarded bool) string {
		alloc, c1 := chromedp.NewRemoteAllocator(context.Background(), b.cfg.CDPURL)
		defer c1()
		conn, c2 := chromedp.NewContext(alloc)
		defer c2()
		if err := chromedp.Run(conn); err != nil {
			t.Fatal(err)
		}
		ctx, c4 := chromedp.NewContext(conn, chromedp.WithNewBrowserContext())
		defer c4()
		if err := chromedp.Run(ctx); err != nil {
			t.Fatal(err)
		}
		ctx, c3 := context.WithTimeout(ctx, 30*time.Second)
		defer c3()
		acts := []chromedp.Action{}
		if guarded {
			blockPrivate(ctx)
			acts = append(acts, fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*"}}))
		}
		var out string
		acts = append(acts, chromedp.Navigate("https://example.com/"),
			chromedp.Evaluate(`Promise.race([fetch("`+local.URL+`").then(r => r.text(), e => "refused"), new Promise(ok => setTimeout(() => ok("no answer"), 5000))])`, &out, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
		if err := chromedp.Run(ctx, acts...); err != nil {
			t.Fatal(err)
		}
		return out
	}
	t.Logf("without the guard the script gets %q (%d requests)", try(false), asked.Load())
	asked.Store(0)
	if out := try(true); strings.Contains(out, "segreto") || asked.Load() != 0 {
		t.Errorf("the script got %q, the local service was asked %d times", out, asked.Load())
	}
}

// A page cut inside a letter by the size limit is still UTF-8.
func TestWholeRunes(t *testing.T) {
	page := strings.Repeat("a ", 800) + strings.Repeat("città è più ", 100) + "€"
	for cut := 0; cut <= 4; cut++ {
		raw := []byte(page)[:len(page)-cut]
		out := wholeRunes(raw)
		if !utf8.Valid(out) || len(raw)-len(out) > 2 {
			t.Errorf("cut of %d bytes: %d bytes dropped, %q", cut, len(raw)-len(out), out[len(out)-8:])
		}
	}
	for _, whole := range []string{"", "a", "città", "€", "citt\xe0 e poi"} {
		if out := wholeRunes([]byte(whole)); string(out) != whole {
			t.Errorf("%q became %q", whole, out)
		}
	}
	if out := string(toUTF8([]byte("citt\xe0"), "text/html; charset=iso-8859-1")); out != "città" {
		t.Errorf("a whole Latin-1 page that ends with an accent: %q", out)
	}
}

func TestDirectHTTP_PageOverTheLimit(t *testing.T) {
	page := "<html><head><title>t</title></head><body><p>" + strings.Repeat("a ", 800) + strings.Repeat("città è più grande ", 400)
	cut := strings.LastIndex(page[:8000], "è") + 1 // inside the letter
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()
	h := NewDirectHTTPClient()
	h.maxBodySize = int64(cut)
	h.client = srv.Client()
	doc, err := h.readChecked(context.Background(), srv.URL)
	if err != nil || !strings.Contains(doc.Content, "città") || strings.Contains(doc.Content, "Ã") {
		t.Errorf("err %v", err)
	}
}
