package reader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// ErrPDFContent marks a response that is a PDF document although its URL does
// not say so (Content-Type or %PDF- signature), so the composite reader can
// hand it to the PDF reader (V25-PDF.3).
var ErrPDFContent = errors.New("response is a PDF document")

// pdfMagic is the signature every PDF file starts with.
var pdfMagic = []byte("%PDF-")

// PDFReaderConfig configures the built-in PDF reader (V25-PDF.4).
type PDFReaderConfig struct {
	MaxBytes int64         // download limit; larger files are refused
	MaxPages int           // 0 = read every page (default: relevant PDFs are read in full)
	Timeout  time.Duration // download plus extraction
	Idle     time.Duration // the PDFium engine is shut down after this long without PDFs
	CacheDir string        // wazero compilation cache, so a restart skips the WASM compile; "" = none
}

// PDFReader downloads PDF documents and extracts their text with PDFium, run as
// WebAssembly inside the binary: no CGO and no external service (spec
// docs/SPEC_PDF_LETTORE_INTERNO.md). The engine costs ~300 MB while it is up,
// so it starts at the first PDF and stops after cfg.Idle without PDFs.
type PDFReader struct {
	cfg    PDFReaderConfig
	client *http.Client

	turn     chan struct{} // one extraction at a time: a PDFium instance is not thread safe
	pool     pdfium.Pool
	inst     pdfium.Pdfium
	idleStop *time.Timer
	lastUse  time.Time
}

// NewPDFReader creates the PDF reader; the PDFium engine is not started yet.
func NewPDFReader(cfg PDFReaderConfig) *PDFReader {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 50 << 20
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.Idle <= 0 {
		cfg.Idle = 5 * time.Minute
	}
	return &PDFReader{
		cfg:  cfg,
		turn: make(chan struct{}, 1),
		client: &http.Client{
			Transport: safeTransport(),
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("stopped after 5 redirects")
				}
				return ValidateSafeURL(req.URL.String())
			},
		},
	}
}

var (
	sharedPDFMu sync.Mutex
	sharedPDFs  = map[PDFReaderConfig]*PDFReader{}
)

// pdfReaderFor is the one PDF reader of a configuration: the chain of every search,
// and the one built for a reader picked in the bar, start one engine and not one each.
func pdfReaderFor(cfg PDFReaderConfig) *PDFReader {
	sharedPDFMu.Lock()
	defer sharedPDFMu.Unlock()
	if sharedPDFs[cfg] == nil {
		sharedPDFs[cfg] = NewPDFReader(cfg)
	}
	return sharedPDFs[cfg]
}

// lock waits for the turn, or for the caller to stop waiting.
func (p *PDFReader) lock(ctx context.Context) error {
	select {
	case p.turn <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("pdf reader: out of time waiting for another PDF: %w", ctx.Err())
	}
}

func (p *PDFReader) unlock() { <-p.turn }

func (p *PDFReader) Name() string {
	return "pdf"
}

// Read downloads targetURL and returns the text of the PDF. A response that is
// not a PDF, or a PDF without extractable text (e.g. a scan), is an error, so
// the composite reader goes on with the rest of the chain.
func (p *PDFReader) Read(ctx context.Context, targetURL string) (*Document, error) {
	if err := ValidateSafeURL(targetURL); err != nil {
		return nil, fmt.Errorf("ssrf validation failed: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	data, err := p.download(ctx, targetURL)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	title, text, pages, read, err := p.extract(ctx, data)
	if err != nil {
		return nil, err
	}
	content := strings.TrimSpace(text)
	truncated := read < pages
	log.Printf("[pdf] url=%s pages=%d/%d chars=%d ms=%d truncated=%t",
		targetURL, read, pages, len(content), time.Since(start).Milliseconds(), truncated)
	if len(content) < 250 && truncated && ctx.Err() != nil {
		// Out of time, which says nothing about the PDF: the cache would keep "scanned".
		return nil, fmt.Errorf("pdf reader: out of time after %d of %d pages: %w", read, pages, ctx.Err())
	}
	if len(content) < 250 {
		return nil, fmt.Errorf("%w: pdf has %d characters of text in %d pages (scanned?)", ErrLowDensityContent, len(content), read)
	}
	if title == "" {
		title = titleFromURL(targetURL)
	}
	return &Document{
		Title:       title,
		URL:         targetURL,
		Content:     content,
		ExtractedAt: time.Now(),
		Metadata: map[string]string{
			"source_type": "pdf",
			"pages":       strconv.Itoa(pages),
			"pages_read":  strconv.Itoa(read),
			"truncated":   strconv.FormatBool(truncated),
		},
	}, nil
}

func (p *PDFReader) download(ctx context.Context, targetURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create pdf request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36 VedettaBot/1.0")
	req.Header.Set("Accept", "application/pdf,*/*;q=0.8")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pdf reader get failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pdf reader returned status %d", resp.StatusCode)
	}
	if resp.ContentLength > p.cfg.MaxBytes {
		return nil, fmt.Errorf("pdf is %d bytes, over the %d byte limit", resp.ContentLength, p.cfg.MaxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, p.cfg.MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read pdf body: %w", err)
	}
	if int64(len(data)) > p.cfg.MaxBytes {
		return nil, fmt.Errorf("pdf is over the %d byte limit", p.cfg.MaxBytes)
	}
	if !bytes.HasPrefix(data, pdfMagic) {
		return nil, fmt.Errorf("%w: response is not a PDF (content-type %q)", ErrUnsupportedFormat, resp.Header.Get("Content-Type"))
	}
	return data, nil
}

// extract returns the document title (metadata), the text of the pages read,
// the page count and how many pages were read.
func (p *PDFReader) extract(ctx context.Context, data []byte) (title, text string, pages, read int, err error) {
	if err := p.lock(ctx); err != nil {
		return "", "", 0, 0, err
	}
	defer p.unlock()
	inst, err := p.instanceLocked()
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("pdf engine: %w", err)
	}
	defer func() {
		p.lastUse = time.Now()
		p.idleStop.Reset(p.cfg.Idle)
	}()

	doc, err := inst.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("pdf open failed: %w", err)
	}
	defer inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})

	if meta, err := inst.FPDF_GetMetaText(&requests.FPDF_GetMetaText{Document: doc.Document, Tag: "Title"}); err == nil {
		title = strings.TrimSpace(meta.Value)
	}
	count, err := inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("pdf page count failed: %w", err)
	}
	pages = count.PageCount
	if err := ctx.Err(); err != nil {
		// Out of time before the first page (waiting for the engine): a timeout,
		// not a PDF without text, which the cache would store (Piano dopo audit 1.5).
		return "", "", pages, 0, fmt.Errorf("pdf reader: out of time before reading a page: %w", err)
	}
	limit := pages
	if p.cfg.MaxPages > 0 && limit > p.cfg.MaxPages {
		limit = p.cfg.MaxPages
	}
	var b strings.Builder
	for i := 0; i < limit; i++ {
		if ctx.Err() != nil {
			// Out of time: keep the pages read so far.
			break
		}
		page, err := inst.GetPageText(&requests.GetPageText{Page: requests.Page{
			ByIndex: &requests.PageByIndex{Document: doc.Document, Index: i},
		}})
		if err != nil {
			return "", "", 0, 0, fmt.Errorf("pdf page %d: %w", i+1, err)
		}
		b.WriteString(cleanPDFText(page.Text))
		b.WriteString("\n\n")
		read++
	}
	return title, b.String(), pages, read, nil
}

// instanceLocked starts the PDFium engine on first use. Must hold the turn.
func (p *PDFReader) instanceLocked() (pdfium.Pdfium, error) {
	if p.inst != nil {
		return p.inst, nil
	}
	start := time.Now()
	// Same features go-pdfium enables by default: its PDFium build needs exception handling.
	rc := wazero.NewRuntimeConfig().WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling)
	if p.cfg.CacheDir != "" {
		if err := os.MkdirAll(p.cfg.CacheDir, 0o700); err == nil {
			if cache, err := wazero.NewCompilationCacheWithDir(p.cfg.CacheDir); err == nil {
				rc = rc.WithCompilationCache(cache)
			}
		}
	}
	pool, err := webassembly.Init(webassembly.Config{MinIdle: 1, MaxIdle: 1, MaxTotal: 1, RuntimeConfig: rc})
	if err != nil {
		return nil, err
	}
	inst, err := pool.GetInstance(30 * time.Second)
	if err != nil {
		pool.Close()
		return nil, err
	}
	p.pool, p.inst = pool, inst
	if p.idleStop == nil {
		p.idleStop = time.AfterFunc(p.cfg.Idle, p.stopIdle)
	}
	log.Printf("[pdf] engine started in %d ms", time.Since(start).Milliseconds())
	return inst, nil
}

// stopIdle shuts the engine down after cfg.Idle without PDFs, to give back its memory.
func (p *PDFReader) stopIdle() {
	p.turn <- struct{}{}
	defer p.unlock()
	// The timer may fire while an extraction holds the lock; that one re-armed it.
	if wait := p.cfg.Idle - time.Since(p.lastUse); wait > 0 {
		p.idleStop.Reset(wait)
		return
	}
	p.closeLocked()
}

// Close shuts the PDFium engine down, if it is running.
func (p *PDFReader) Close() {
	p.turn <- struct{}{}
	defer p.unlock()
	if p.idleStop != nil {
		p.idleStop.Stop()
	}
	p.closeLocked()
}

func (p *PDFReader) closeLocked() {
	if p.inst == nil {
		return
	}
	_ = p.inst.Close()
	_ = p.pool.Close()
	p.inst, p.pool = nil, nil
	// The engine's memory is free now, but Go would keep it for a while:
	// hand it back to the system, which is the point of stopping.
	debug.FreeOSMemory()
	log.Printf("[pdf] engine stopped")
}

// cleanPDFText turns PDFium's text into plain text: U+0002 marks a hyphen PDFium
// found at a line break, and Windows line ends become \n.
func cleanPDFText(s string) string {
	s = strings.ReplaceAll(s, "\x02", "-")
	return strings.ReplaceAll(s, "\r\n", "\n")
}

// titleFromURL is the file name of the URL without extension, as a last resort.
func titleFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "No Title"
	}
	name := strings.TrimSuffix(path.Base(u.Path), path.Ext(u.Path))
	if unescaped, err := url.PathUnescape(name); err == nil {
		name = unescaped
	}
	if name == "" || name == "." || name == "/" {
		return "No Title"
	}
	return name
}
