package reader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// makePDF builds a valid PDF with one page per string (Helvetica text lines)
// and the given document title.
func makePDF(title string, pages []string) []byte {
	var objs []string
	n := len(pages)
	kids := make([]string, n)
	for i := range pages {
		kids[i] = fmt.Sprintf("%d 0 R", 4+2*i)
	}
	objs = append(objs, "<< /Type /Catalog /Pages 2 0 R >>")
	objs = append(objs, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	objs = append(objs, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i, text := range pages {
		var stream strings.Builder
		stream.WriteString("BT /F1 10 Tf 14 TL 40 760 Td\n")
		for _, line := range strings.Split(text, "\n") {
			stream.WriteString("(" + line + ") Tj T*\n")
		}
		stream.WriteString("ET")
		objs = append(objs, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", 5+2*i))
		objs = append(objs, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", stream.Len(), stream.String()))
	}
	objs = append(objs, fmt.Sprintf("<< /Title (%s) >>", title))

	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R /Info %d 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, len(objs), xref)
	return b.Bytes()
}

func lines(prefix string, n int) string {
	var out []string
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf("%s line %d of the regulation on agrivoltaic plants.", prefix, i))
	}
	return strings.Join(out, "\n")
}

// One reader for the whole file: starting PDFium takes a few seconds.
var (
	testPDFOnce   sync.Once
	testPDFReader *PDFReader
)

func sharedPDFReader(t *testing.T) *PDFReader {
	t.Helper()
	testPDFOnce.Do(func() {
		testPDFReader = NewPDFReader(PDFReaderConfig{MaxBytes: 1 << 20, Idle: time.Minute})
	})
	return testPDFReader
}

func TestPDFReader_ExtractAllPages(t *testing.T) {
	p := sharedPDFReader(t)
	data := makePDF("Regole operative", []string{lines("First page", 5), lines("Second page", 5), lines("Third page", 5)})

	title, text, pages, read, err := p.extract(context.Background(), data)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if title != "Regole operative" {
		t.Errorf("title = %q", title)
	}
	if pages != 3 || read != 3 {
		t.Errorf("pages %d read %d, want 3 and 3", pages, read)
	}
	for _, want := range []string{"First page line 1", "Second page line 3", "Third page line 5 of the regulation"} {
		if !strings.Contains(text, want) {
			t.Errorf("text misses %q", want)
		}
	}
}

func TestPDFReader_MaxPages(t *testing.T) {
	// The shared reader with a page limit, so the engine is not started twice.
	shared := sharedPDFReader(t)
	shared.cfg.MaxPages = 2
	defer func() { shared.cfg.MaxPages = 0 }()

	data := makePDF("t", []string{lines("A", 3), lines("B", 3), lines("C", 3)})
	_, text, pages, read, err := shared.extract(context.Background(), data)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if pages != 3 || read != 2 || strings.Contains(text, "C line 1") {
		t.Errorf("pages %d read %d, third page present %t", pages, read, strings.Contains(text, "C line 1"))
	}
}

func TestPDFReader_Download(t *testing.T) {
	pdf := makePDF("t", []string{lines("A", 3)})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/doc":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(pdf)
		case "/big":
			_, _ = w.Write(append([]byte("%PDF-1.4\n"), make([]byte, 2<<20)...))
		case "/html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html><body>not a pdf</body></html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	p := NewPDFReader(PDFReaderConfig{MaxBytes: 1 << 20})
	// The test server is local: this reader goes without the connection guard,
	// which TestReaders_RefusePrivateAddressAtConnection covers.
	p.client.Transport = http.DefaultTransport

	if data, err := p.download(context.Background(), ts.URL+"/doc"); err != nil || !bytes.Equal(data, pdf) {
		t.Errorf("doc: err %v, same bytes %t", err, bytes.Equal(data, pdf))
	}
	if _, err := p.download(context.Background(), ts.URL+"/big"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("big: want size limit error, got %v", err)
	}
	if _, err := p.download(context.Background(), ts.URL+"/html"); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("html: want ErrUnsupportedFormat, got %v", err)
	}
	if _, err := p.download(context.Background(), ts.URL+"/missing"); err == nil {
		t.Errorf("missing: want status error")
	}
}

func TestPDFReader_RefusesPrivateAddress(t *testing.T) {
	p := NewPDFReader(PDFReaderConfig{})
	if _, err := p.Read(context.Background(), "http://127.0.0.1:9/doc.pdf"); err == nil || !strings.Contains(err.Error(), "ssrf") {
		t.Errorf("want ssrf error, got %v", err)
	}
}

func TestCleanPDFText(t *testing.T) {
	if got := cleanPDFText("anti\x02utopia\r\nnext"); got != "anti-utopia\nnext" {
		t.Errorf("got %q", got)
	}
}

func TestTitleFromURL(t *testing.T) {
	cases := map[string]string{
		"https://x.it/a/Regole%20Operative%20DM.pdf":                  "Regole Operative DM",
		"https://x.it/portale/documents/d/guest/roagr-allegato-1-pdf": "roagr-allegato-1-pdf",
		"https://x.it/": "No Title",
	}
	for in, want := range cases {
		if got := titleFromURL(in); got != want {
			t.Errorf("titleFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// V25-PDF.3: PDFs go to the built-in reader before the chain.
func TestCompositeReader_RoutesPDFsToBuiltInReader(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	content := strings.Repeat("Readable text from the document. ", 20)
	pdfDoc := &mockReader{name: "pdf", docToRet: &Document{Content: content}}
	pdfFails := &mockReader{name: "pdf", errToRet: fmt.Errorf("%w: scanned", ErrLowDensityContent)}
	httpPDF := &mockReader{name: "http", errToRet: fmt.Errorf("%w: %w (content-type application/pdf)", ErrUnsupportedFormat, ErrPDFContent)}
	httpOK := &mockReader{name: "http", docToRet: &Document{Content: content}}
	jina := &mockReader{name: "jina", docToRet: &Document{Content: content}}

	tests := []struct {
		name         string
		pdf          *mockReader
		chain        []Reader
		url          string
		wantServed   string
		wantFallback bool
	}{
		{"pdf url", pdfDoc, []Reader{httpOK, jina}, "https://x.it/a.pdf", "served_by=pdf", false},
		{"pdf by content-type", pdfDoc, []Reader{httpPDF, jina}, "https://x.it/doc-pdf", "served_by=pdf", false},
		{"scanned pdf goes on to the chain", pdfFails, []Reader{httpOK, jina}, "https://x.it/a.pdf", "served_by=jina", true},
		{"html page untouched", pdfDoc, []Reader{httpOK, jina}, "https://x.it/page", "served_by=http", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf.Reset()
			tt.pdf.calledURLs = nil
			comp := NewCompositeReader(CompositeReaderConfig{Chain: tt.chain, PDF: tt.pdf})
			ctx := WithSessionID(context.Background(), "s-pdf")
			if _, err := comp.Read(ctx, tt.url); err != nil {
				t.Fatalf("read: %v", err)
			}
			if !strings.Contains(buf.String(), tt.wantServed) {
				t.Errorf("want %q in log, got %q", tt.wantServed, buf.String())
			}
			if used, _ := comp.FallbackStatus("s-pdf"); used != tt.wantFallback {
				t.Errorf("fallback = %t, want %t", used, tt.wantFallback)
			}
			if tt.name == "html page untouched" && len(tt.pdf.calledURLs) != 0 {
				t.Errorf("pdf reader called for an html page")
			}
		})
	}
}

// Out of time before the first page: a timeout, not "a PDF without text"
// (Piano dopo audit 1.5 / R9). The cache stores ErrLowDensityContent as a fault
// of the page; a timeout must not be stored.
func TestPDFReader_ExtractOutOfTime(t *testing.T) {
	p := sharedPDFReader(t)
	data := makePDF("t", []string{lines("A", 3), lines("B", 3)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, read, err := p.extract(ctx, data)
	if err == nil {
		t.Fatalf("no error out of time (pages read %d)", read)
	}
	if errors.Is(err, ErrLowDensityContent) || !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the context's error and not ErrLowDensityContent", err)
	}
}
