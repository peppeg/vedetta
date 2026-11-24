// Command htmltext turns a page into the text Vedetta would extract from it,
// for the reader comparison of R7 (docs/SPEC_R7_LETTORE_DI_RISERVA.md):
//
//	htmltext -html FILE OUT.txt   rendered HTML (scripts/browsercmp) through Vedetta's extractor
//	htmltext -http URL OUT.txt    Vedetta's http reader
//	htmltext -jina URL OUT.txt    Vedetta's Jina reader (JINA_READER_URL, default localhost:3001)
//	htmltext -browser URL OUT.txt Vedetta's browser reader (BROWSER_CDP_URL, SPEC_BROWSER B.5)
//
// Prints one JSON line: title, chars, block (anti-bot marker, "" if none), ms, error.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"vedetta/internal/reader"
)

type result struct {
	Title string `json:"title"`
	Chars int    `json:"chars"`
	Block string `json:"block,omitempty"`
	MS    int64  `json:"ms"`
	Error string `json:"error,omitempty"`
}

func main() {
	html := flag.Bool("html", false, "input is a rendered HTML file")
	viaHTTP := flag.Bool("http", false, "read the URL with the http reader")
	viaJina := flag.Bool("jina", false, "read the URL with the Jina reader")
	viaBrowser := flag.Bool("browser", false, "read the URL with the browser reader (BROWSER_CDP_URL)")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: htmltext -html FILE|-http URL|-jina URL|-browser URL OUT.txt")
		os.Exit(2)
	}
	in, out := flag.Arg(0), flag.Arg(1)
	var res result
	var doc *reader.Document
	var err error
	start := time.Now()
	switch {
	case *html:
		var raw []byte
		if raw, err = os.ReadFile(in); err == nil {
			doc = &reader.Document{URL: in}
			doc.Title, _, doc.Content, err = reader.ExtractHTMLText(raw)
		}
	case *viaHTTP:
		doc, err = reader.NewDirectHTTPClient().Read(context.Background(), in)
	case *viaJina:
		base := os.Getenv("JINA_READER_URL")
		if base == "" {
			base = "http://localhost:3001"
		}
		doc, err = reader.NewJinaReader(base).Read(context.Background(), in)
	case *viaBrowser:
		doc, err = reader.NewBrowserReader(reader.BrowserReaderConfig{CDPURL: os.Getenv("BROWSER_CDP_URL")}).Read(context.Background(), in)
	default:
		err = fmt.Errorf("pick -html, -http, -jina or -browser")
	}
	res.MS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = err.Error()
	}
	if doc != nil {
		res.Title = doc.Title
		res.Chars = len(strings.TrimSpace(doc.Content))
		res.Block = reader.BlockPageMarker(doc)
		if werr := os.WriteFile(out, []byte(doc.Content), 0o644); werr != nil && res.Error == "" {
			res.Error = werr.Error()
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(res)
}
