package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// BrowserReaderConfig configures the browser reader (docs/SPEC_BROWSER_CDP.md).
type BrowserReaderConfig struct {
	CDPURL      string        // address of a running headless browser, e.g. ws://127.0.0.1:9222
	MaxPages    int           // pages open at the same time
	PageTimeout time.Duration // navigation plus scripts, per page
	Settle      time.Duration // wait after the load event, for scripts
}

// BrowserReader reads pages with a headless browser that runs outside Vedetta
// and speaks CDP (Chromium, Lightpanda, ...). Vedetta only gets its address:
// it does not ship, start or update the browser (SPEC_BROWSER §0).
//
// Every page gets a room of its own, closed after it: a new browser context
// where the browser supports more than one, otherwise a CDP connection of its
// own (Lightpanda allows one browser context per connection). So cookies do not
// pass from one page to the next and the browser's memory goes back down.
type BrowserReader struct {
	cfg BrowserReaderConfig
	sem chan struct{}
	// singleContext is set once the browser refused a second browser context.
	singleContext atomic.Bool
}

// NewBrowserReader creates the browser reader; it does not connect yet.
func NewBrowserReader(cfg BrowserReaderConfig) *BrowserReader {
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = 2
	}
	if cfg.PageTimeout <= 0 {
		cfg.PageTimeout = 20 * time.Second
	}
	if cfg.Settle <= 0 {
		cfg.Settle = 1500 * time.Millisecond
	}
	return &BrowserReader{cfg: cfg, sem: make(chan struct{}, cfg.MaxPages)}
}

func (b *BrowserReader) Name() string {
	return "browser"
}

// Read renders targetURL in the browser and extracts its text like the http
// reader does. Short pages and anti-bot pages are errors, so the chain goes on.
func (b *BrowserReader) Read(ctx context.Context, targetURL string) (*Document, error) {
	if err := ValidateSafeURL(targetURL); err != nil {
		return nil, fmt.Errorf("ssrf validation failed: %w", err)
	}
	select {
	case b.sem <- struct{}{}:
		defer func() { <-b.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	start := time.Now()
	html, finalURL, err := b.render(ctx, targetURL)
	if err != nil {
		return nil, err
	}
	// Redirects of the main document are checked while they happen (blockPrivate);
	// the final address is checked once more in case the browser did not report one.
	if finalURL != "" && finalURL != targetURL {
		if err := ValidateSafeURL(finalURL); err != nil {
			return nil, fmt.Errorf("ssrf validation failed after redirect to %s: %w", finalURL, err)
		}
	}
	pageURL := targetURL
	if finalURL != "" {
		pageURL = finalURL
	}
	title, description, content, err := extractHTMLTextFrom([]byte(html), pageURL, httpReaderV2.Load())
	if err != nil {
		return nil, fmt.Errorf("browser page extraction failed: %w", err)
	}
	content = strings.TrimSpace(content)
	if title == "" {
		title = "No Title"
	}
	doc := &Document{
		Title:       title,
		URL:         targetURL,
		Content:     content,
		ExtractedAt: time.Now(),
		Metadata: map[string]string{
			"description": description,
			"source_type": "browser",
		},
	}
	log.Printf("[browser] session=%s url=%s chars=%d ms=%d", SessionIDFromContext(ctx), targetURL, len(content), time.Since(start).Milliseconds())
	if marker := BlockPageMarker(doc); marker != "" {
		return nil, fmt.Errorf("%w: browser got a block page (%q)", ErrLowDensityContent, marker)
	}
	if len(content) < 250 {
		return nil, fmt.Errorf("%w: browser page has %d characters", ErrLowDensityContent, len(content))
	}
	return doc, nil
}

// render opens targetURL in a room of its own and returns the rendered HTML
// and the final address.
func (b *BrowserReader) render(ctx context.Context, targetURL string) (html, finalURL string, err error) {
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), b.cfg.CDPURL)
	defer cancelAlloc()
	connCtx, cancelConn := chromedp.NewContext(allocCtx)
	defer cancelConn()
	// Connect (and create the first tab) within the page time limit.
	if err := runWithin(ctx, connCtx, b.cfg.PageTimeout); err != nil {
		return "", "", fmt.Errorf("browser not reachable at %s: %w", b.cfg.CDPURL, err)
	}
	defer func() { _ = chromedp.Cancel(connCtx) }()

	pageCtx := connCtx
	if !b.singleContext.Load() {
		roomCtx, cancelRoom := chromedp.NewContext(connCtx, chromedp.WithNewBrowserContext())
		defer cancelRoom()
		switch err := runWithin(ctx, roomCtx, b.cfg.PageTimeout); {
		case err == nil:
			pageCtx = roomCtx
		case isSingleContextError(err):
			// One browser context per connection (Lightpanda): this connection is the room.
			b.singleContext.Store(true)
		default:
			return "", "", fmt.Errorf("browser context: %w", err)
		}
	}

	runCtx, cancel := context.WithTimeout(pageCtx, b.cfg.PageTimeout)
	defer cancel()
	stopWatch := context.AfterFunc(ctx, cancel) // the caller's cancellation stops the page too
	defer stopWatch()
	blockPrivate(runCtx)
	err = chromedp.Run(runCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			// WebSockets do not pass through Fetch: no page needs one to be read. A
			// browser without this command (Lightpanda) goes on.
			_ = network.Enable().Do(ctx)
			_ = network.SetBlockedURLs([]string{"ws://*", "wss://*"}).Do(ctx)
			return nil
		}),
		fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*"}}),
		emulation.SetUserAgentOverride(directUserAgent),
		chromedp.Navigate(targetURL),
		chromedp.Sleep(b.cfg.Settle),
		chromedp.Evaluate(`document.documentElement.outerHTML`, &html),
		chromedp.Location(&finalURL),
	)
	if err != nil {
		return "", "", fmt.Errorf("browser page failed: %w", err)
	}
	return html, finalURL, nil
}

// blockPrivate makes every request of the page (the page itself, its redirects, its
// frames, its scripts and what they ask for) pass the SSRF check: the browser follows
// redirects on its own, and a script could bring into the page what it finds in the
// home network (SPEC_BROWSER B.2; external audit 24/11/2026).
func blockPrivate(ctx context.Context) {
	var mu sync.Mutex
	hosts := map[string]error{} // one check per host of the page
	check := func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		switch strings.ToLower(u.Scheme) {
		case "data", "blob", "about":
			return nil
		}
		key := strings.ToLower(u.Scheme + "://" + u.Hostname())
		mu.Lock()
		err, done := hosts[key]
		mu.Unlock()
		if !done {
			err = ValidateSafeURL(raw)
			mu.Lock()
			hosts[key] = err
			mu.Unlock()
		}
		return err
	}
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		e, ok := ev.(*fetch.EventRequestPaused)
		if !ok {
			return
		}
		go func() {
			c := chromedp.FromContext(ctx)
			if c == nil || c.Target == nil {
				return
			}
			execCtx := cdp.WithExecutor(ctx, c.Target)
			if err := check(e.Request.URL); err != nil {
				log.Printf("[browser] blocked request to %s: %v", e.Request.URL, err)
				_ = fetch.FailRequest(e.RequestID, network.ErrorReasonBlockedByClient).Do(execCtx)
				return
			}
			_ = fetch.ContinueRequest(e.RequestID).Do(execCtx)
		}()
	})
}

func runWithin(parent, cdpCtx context.Context, limit time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- chromedp.Run(cdpCtx) }()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-parent.Done():
		return parent.Err()
	case <-timer.C:
		return errors.New("timed out")
	}
}

func isSingleContextError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "more than one browser context")
}

// BrowserVersion asks the browser at cdpURL who it is (GET /json/version),
// for the doctor (SPEC_BROWSER §3). It returns the "Browser" field, e.g.
// "Chrome/151.0.7922.34" or "Lightpanda/1.0".
func BrowserVersion(ctx context.Context, cdpURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(cdpURL))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid browser address %q", cdpURL)
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	}
	u.Path, u.RawQuery = "/json/version", ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("browser returned status %d", resp.StatusCode)
	}
	var v struct {
		Browser string `json:"Browser"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", fmt.Errorf("browser version: %w", err)
	}
	return v.Browser, nil
}

// testedBrowsers are the browsers the reader bench ran against (SPEC_BROWSER
// §0.3, "tested with", not "guaranteed with"): "Browser" prefix and date.
var testedBrowsers = []struct{ Prefix, Date string }{
	{"Chrome/151.", "2026-10-20"},
	{"HeadlessChrome/151.", "2026-10-20"},
	{"Lightpanda/1.0", "2026-10-20"},
}

// BrowserDoctor checks the configured browser and returns its state ("ok",
// "down", "unknown") and a message for the user. The messages say what Vedetta
// cannot do without the browser, never how much is lost (SPEC_BROWSER §0.4).
func BrowserDoctor(ctx context.Context, cdpURL string) (status, message string) {
	if strings.TrimSpace(cdpURL) == "" {
		return "unknown", "Fallback browser not configured. Some sites refuse direct reading: without a fallback browser " +
			"Vedetta cannot read those pages, and answers rest only on the sources it could read. " +
			"Vedetta needs this tool to work at its best (set BROWSER_CDP_URL, see guide/settings.md)."
	}
	version, err := BrowserVersion(ctx, cdpURL)
	if err != nil {
		return "down", fmt.Sprintf("Fallback browser configured but not reachable at %s (%v): pages that refuse direct "+
			"reading will not be read until it is back.", redactURL(cdpURL), err)
	}
	for _, t := range testedBrowsers {
		if strings.HasPrefix(version, t.Prefix) {
			return "ok", fmt.Sprintf("Fallback browser %s, tested with Vedetta on %s.", version, t.Date)
		}
	}
	var tested []string
	for _, t := range testedBrowsers {
		tested = append(tested, strings.TrimSuffix(t.Prefix, "."))
	}
	return "ok", fmt.Sprintf("Fallback browser %s: not yet tested with Vedetta (tested: %s). It may work; if something "+
		"goes wrong, report it with this version.", version, strings.Join(tested, ", "))
}

// ChainDoctor checks the fallback readers of the configured chain (V25-F0.2).
// With "browser" in the chain it is BrowserDoctor; with another fallback reader
// (jina, firecrawl, exa) it probes the first one; with only "http" it says what
// Vedetta cannot do without a fallback reader. It returns which reader it
// checked ("browser", the fallback name, or "" when there is none).
func ChainDoctor(ctx context.Context, cfg ReaderConfig) (reader, status, message string) {
	fallback := ""
	for _, p := range cfg.Providers {
		switch name := strings.ToLower(strings.TrimSpace(p)); name {
		case "browser":
			cdpURL := ""
			if cfg.Browser != nil {
				cdpURL = cfg.Browser.CDPURL
			}
			status, message = BrowserDoctor(ctx, cdpURL)
			return "browser", status, message
		case "jina", "jina-reader", "firecrawl", "exa":
			if fallback == "" {
				fallback = name
			}
		}
	}
	if fallback == "" {
		return "", "unknown", "No fallback reader configured. Some sites refuse direct reading: without a fallback reader " +
			"Vedetta cannot read those pages, and answers rest only on the sources it could read. " +
			"Vedetta needs this tool to work at its best (add jina, browser, firecrawl or exa to READER_PROVIDERS, see guide/settings.md)."
	}
	p := ProbeReader(ctx, fallback, cfg)
	message = fmt.Sprintf("Fallback reader %s: %s (no browser in the chain)", fallback, p.Status)
	if p.Detail != "" {
		message += ": " + p.Detail
	}
	return fallback, p.Status, message
}

// redactURL keeps scheme and host of an address: a CDP address may carry a token.
func redactURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "(invalid address)"
	}
	return u.Scheme + "://" + u.Host
}

// RedactBrowserURL is redactURL for the settings API.
func RedactBrowserURL(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	return redactURL(raw)
}
