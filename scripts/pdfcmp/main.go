// Command pdfcmp extracts the text of one PDF with one candidate library, for
// the offline comparison of V25-PDF.1 (docs/SPEC_PDF_LETTORE_INTERNO.md).
// It is a separate module so the candidates stay out of Vedetta's go.mod.
//
// Usage: pdfcmp -engine pdfium|ledongthuc IN.pdf OUT.txt
// Prints one JSON line: engine, pages, chars, ms (extraction only), error.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/ledongthuc/pdf"
)

type result struct {
	Engine string `json:"engine"`
	Pages  int    `json:"pages"`
	Chars  int    `json:"chars"`
	InitMS int64  `json:"init_ms"`
	MS     int64  `json:"ms"`
	Error  string `json:"error,omitempty"`
}

func main() {
	engine := flag.String("engine", "pdfium", "pdfium or ledongthuc")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: pdfcmp -engine pdfium|ledongthuc IN.pdf OUT.txt")
		os.Exit(2)
	}
	in, out := flag.Arg(0), flag.Arg(1)
	res := result{Engine: *engine}

	var text string
	var err error
	switch *engine {
	case "pdfium":
		text, err = withPdfium(in, &res)
	case "ledongthuc":
		text, err = withLedongthuc(in, &res)
	default:
		err = fmt.Errorf("unknown engine %q", *engine)
	}
	if err != nil {
		res.Error = err.Error()
	}
	res.Chars = len(text)
	if werr := os.WriteFile(out, []byte(text), 0o644); werr != nil && res.Error == "" {
		res.Error = werr.Error()
	}
	_ = json.NewEncoder(os.Stdout).Encode(res)
}

func withPdfium(path string, res *result) (string, error) {
	start := time.Now()
	pool, err := webassembly.Init(webassembly.Config{MinIdle: 1, MaxIdle: 1, MaxTotal: 1})
	if err != nil {
		return "", err
	}
	defer pool.Close()
	inst, err := pool.GetInstance(30 * time.Second)
	if err != nil {
		return "", err
	}
	defer inst.Close()
	res.InitMS = time.Since(start).Milliseconds()

	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	start = time.Now()
	doc, err := inst.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		return "", err
	}
	defer inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})
	count, err := inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
	if err != nil {
		return "", err
	}
	res.Pages = count.PageCount
	var b strings.Builder
	for i := 0; i < count.PageCount; i++ {
		page, err := inst.GetPageText(&requests.GetPageText{Page: requests.Page{
			ByIndex: &requests.PageByIndex{Document: doc.Document, Index: i},
		}})
		if err != nil {
			return b.String(), fmt.Errorf("page %d: %w", i+1, err)
		}
		b.WriteString(page.Text)
		b.WriteString("\n\n")
	}
	res.MS = time.Since(start).Milliseconds()
	return b.String(), nil
}

func withLedongthuc(path string, res *result) (string, error) {
	start := time.Now()
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	res.Pages = r.NumPage()
	var b strings.Builder
	for i := 1; i <= res.Pages; i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		t, err := p.GetPlainText(nil)
		if err != nil {
			return b.String(), fmt.Errorf("page %d: %w", i, err)
		}
		b.WriteString(t)
		b.WriteString("\n\n")
	}
	res.MS = time.Since(start).Milliseconds()
	return b.String(), nil
}
