package reader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
	"golang.org/x/text/transform"
)

// DirectHTTPClient is a Tier-1 ultra-fast HTTP document reader for static HTML (<50ms).
// It reuses the SSRF guard and performs text-density extraction.
type DirectHTTPClient struct {
	client      *http.Client
	maxBodySize int64
}

// NewDirectHTTPClient creates a new DirectHTTPClient.
func NewDirectHTTPClient() *DirectHTTPClient {
	return &DirectHTTPClient{
		client: &http.Client{
			Timeout:   10 * time.Second,
			Transport: safeTransport(),
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("stopped after 5 redirects")
				}
				// Enforce SSRF validation on every redirect
				return ValidateSafeURL(req.URL.String())
			},
		},
		maxBodySize: 4 * 1024 * 1024, // 4MB
	}
}

func (h *DirectHTTPClient) Name() string {
	return "http"
}

// Read fetches the static HTML document directly and extracts its text.
func (h *DirectHTTPClient) Read(ctx context.Context, targetURL string) (*Document, error) {
	// 1. SSRF guard on target URL
	if err := ValidateSafeURL(targetURL); err != nil {
		return nil, fmt.Errorf("ssrf validation failed: %w", err)
	}
	return h.readChecked(ctx, targetURL)
}

// readChecked is Read past the check of the address.
func (h *DirectHTTPClient) readChecked(ctx context.Context, targetURL string) (*Document, error) {
	// 2. Reject obvious non-HTML URLs (e.g. PDF) early so CompositeReader can route to Jina/Firecrawl
	uLower := strings.ToLower(targetURL)
	if strings.HasSuffix(uLower, ".pdf") || strings.Contains(uLower, ".pdf?") || strings.Contains(uLower, ".pdf#") {
		return nil, fmt.Errorf("%w: target URL is a PDF document (%s)", ErrUnsupportedFormat, targetURL)
	}

	// 3. HTTP GET
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}
	req.Header.Set("User-Agent", directUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "it-IT,it;q=0.9,en-US;q=0.8,en;q=0.7")

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http reader get failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, statusError("http", resp)
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/pdf") {
		return nil, fmt.Errorf("%w: %w (content-type %s)", ErrUnsupportedFormat, ErrPDFContent, ct)
	}
	if strings.Contains(ct, "application/octet-stream") {
		// Some servers send PDFs as generic binaries: look at the signature.
		head := make([]byte, len(pdfMagic))
		if n, _ := io.ReadFull(resp.Body, head); n == len(head) && bytes.Equal(head, pdfMagic) {
			return nil, fmt.Errorf("%w: %w (content-type %s, %%PDF- signature)", ErrUnsupportedFormat, ErrPDFContent, ct)
		}
		return nil, fmt.Errorf("%w: content-type %s indicates a binary document", ErrUnsupportedFormat, ct)
	}

	// 4. Read body with limit
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, h.maxBodySize))
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// 5. Parse HTML & extract text
	if int64(len(bodyBytes)) >= h.maxBodySize {
		// Cut at the limit, perhaps inside a letter: that is not another encoding.
		bodyBytes = wholeRunes(bodyBytes)
	}
	title, desc, content, err := extractHTMLTextFrom(toUTF8(bodyBytes, resp.Header.Get("Content-Type")), resp.Request.URL.String(), httpReaderV2.Load())
	if err != nil {
		return nil, fmt.Errorf("failed to parse html: %w", err)
	}

	trimmedContent := strings.TrimSpace(content)
	if len(trimmedContent) < 100 {
		return nil, fmt.Errorf("%w: content length %d is below minimum readable threshold", ErrLowDensityContent, len(trimmedContent))
	}

	if title == "" {
		title = "No Title"
	}

	return &Document{
		Title:       title,
		URL:         targetURL,
		Content:     trimmedContent,
		ExtractedAt: time.Now(),
		Metadata: map[string]string{
			"description": desc,
			"source_type": "html",
		},
	}, nil
}

// toUTF8 converts a page in an older encoding, such as Latin-1, using what the page
// and the server declare. Without it every accented letter of the page is lost
// before the model reads it. Text that is already valid UTF-8 stays as it is, even
// if the page declares otherwise: declarations are often wrong, and valid UTF-8
// with accents does not happen by chance.
func toUTF8(raw []byte, contentType string) []byte {
	if utf8.Valid(raw) {
		return raw
	}
	enc, _, _ := charset.DetermineEncoding(raw, contentType)
	out, _, err := transform.Bytes(enc.NewDecoder(), raw)
	if err != nil {
		return bytes.ToValidUTF8(raw, []byte("\uFFFD"))
	}
	return out
}

// wholeRunes drops a UTF-8 letter that is not whole at the end.
func wholeRunes(raw []byte) []byte {
	for i := len(raw) - 1; i >= 0 && i >= len(raw)-utf8.UTFMax; i-- {
		if utf8.RuneStart(raw[i]) {
			if !utf8.FullRune(raw[i:]) {
				return raw[:i]
			}
			break
		}
	}
	return raw
}

// extractHTMLText parses the HTML byte slice and extracts title, meta description, and cleaned text.
// directUserAgent is the user agent of the http and browser readers: a desktop
// Chrome that says it is Vedetta.
const directUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36 VedettaBot/1.0"

// ExtractHTMLText is extractHTMLText for the tools of this module that compare
// readers on rendered HTML (scripts/htmltext, R7).
func ExtractHTMLText(raw []byte) (title, description, content string, err error) {
	return extractHTMLText(raw)
}

func extractHTMLText(raw []byte) (title, description, content string, err error) {
	return extractHTMLTextV1(raw)
}

// httpReaderV2 is VEDETTA_HTTP_READER=2 for the readers that extract HTML here (http,
// browser): links, the article's header and table cells are kept (Piano dopo audit
// 3.5 / R8). Set once at start; version 1 is today's.
var httpReaderV2 atomic.Bool

// SetHTTPReaderVersion chooses how the http and browser readers turn HTML into text:
// 1 (today) or 2.
func SetHTTPReaderVersion(v int) { httpReaderV2.Store(v >= 2) }

// extractHTMLTextFrom is the extraction of the version chosen, for the page at
// pageURL (after redirects): version 2 needs it to write links in full.
func extractHTMLTextFrom(raw []byte, pageURL string, v2 bool) (title, description, content string, err error) {
	if v2 {
		return extractHTMLTextV2(raw, pageURL)
	}
	return extractHTMLTextV1(raw)
}

func extractHTMLTextV1(raw []byte) (title, description, content string, err error) {
	node, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return "", "", "", err
	}

	var sb strings.Builder
	var foundTitle, foundDesc string

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			tag := strings.ToLower(n.Data)

			// Extract title
			if tag == "title" && n.FirstChild != nil && foundTitle == "" {
				foundTitle = strings.TrimSpace(n.FirstChild.Data)
			}

			// Extract meta description
			if tag == "meta" {
				var isDesc bool
				var contentVal string
				for _, attr := range n.Attr {
					if strings.EqualFold(attr.Key, "name") && strings.EqualFold(attr.Val, "description") {
						isDesc = true
					}
					if strings.EqualFold(attr.Key, "content") {
						contentVal = attr.Val
					}
				}
				if isDesc && contentVal != "" && foundDesc == "" {
					foundDesc = strings.TrimSpace(contentVal)
				}
			}

			// Skip non-content tags completely
			switch tag {
			case "script", "style", "noscript", "svg", "canvas", "iframe", "frame", "nav", "footer", "aside", "header":
				return
			}
		}

		if n.Type == html.TextNode {
			t := strings.TrimSpace(n.Data)
			if t != "" {
				sb.WriteString(t)
				sb.WriteString(" ")
			}
		}

		// Recurse into children
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}

		// Append newlines for block elements
		if n.Type == html.ElementNode {
			switch strings.ToLower(n.Data) {
			case "p", "div", "article", "section", "h1", "h2", "h3", "h4", "h5", "h6", "li", "blockquote", "tr", "br", "hr":
				sb.WriteString("\n")
			}
		}
	}

	walk(node)

	// Clean up whitespace: normalize double newlines
	lines := strings.Split(sb.String(), "\n")
	var cleanedLines []string
	for _, line := range lines {
		l := strings.TrimSpace(line)
		if l != "" {
			cleanedLines = append(cleanedLines, l)
		}
	}

	return foundTitle, foundDesc, strings.Join(cleanedLines, "\n\n"), nil
}

// extractHTMLTextV2 is version 1 with three things kept and one dropped (R8):
// links of the main text as [words](address), made absolute against the page or its
// <base href>; the <header> of an <article> or <main> (dates, authors), while the
// page's own header stays out; table cells separated by " | ", empty ones too, so a
// number keeps its column; and the <title>, already in Title, out of the text.
func extractHTMLTextV2(raw []byte, pageURL string) (title, description, content string, err error) {
	node, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return "", "", "", err
	}
	var base *url.URL
	if u, err := url.Parse(strings.TrimSpace(pageURL)); err == nil && u.IsAbs() {
		base = u
	}
	if href := baseHref(node); href != "" {
		if u, err := url.Parse(href); err == nil {
			if base != nil {
				u = base.ResolveReference(u)
			}
			if u.IsAbs() {
				base = u
			}
		}
	}

	var foundTitle, foundDesc string
	var walk func(n *html.Node, sb *strings.Builder, inArticle, inLink bool)
	walk = func(n *html.Node, sb *strings.Builder, inArticle, inLink bool) {
		if n.Type == html.ElementNode {
			switch tag := strings.ToLower(n.Data); tag {
			case "title":
				if n.FirstChild != nil && foundTitle == "" {
					foundTitle = strings.TrimSpace(n.FirstChild.Data)
				}
				return
			case "meta":
				if strings.EqualFold(attrOf(n, "name"), "description") && foundDesc == "" {
					foundDesc = strings.TrimSpace(attrOf(n, "content"))
				}
			case "script", "style", "noscript", "svg", "canvas", "iframe", "frame", "nav", "footer", "aside":
				return
			case "header":
				if !inArticle {
					return
				}
			case "article", "main":
				inArticle = true
			case "a":
				if href := webAddress(attrOf(n, "href"), base); href != "" && !inLink {
					var inner strings.Builder
					for c := n.FirstChild; c != nil; c = c.NextSibling {
						walk(c, &inner, inArticle, true)
					}
					if words := strings.Join(strings.Fields(inner.String()), " "); words != "" {
						fmt.Fprintf(sb, "[%s](%s) ", words, href)
					}
					return
				}
			case "tr":
				var cells []string
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					if c.Type == html.ElementNode && (c.Data == "td" || c.Data == "th") {
						var cell strings.Builder
						for cc := c.FirstChild; cc != nil; cc = cc.NextSibling {
							walk(cc, &cell, inArticle, inLink)
						}
						cells = append(cells, strings.Join(strings.Fields(cell.String()), " "))
					}
				}
				if len(cells) > 0 {
					sb.WriteString(strings.Join(cells, " | "))
					sb.WriteString("\n")
					return
				}
			}
		}
		if n.Type == html.TextNode {
			if t := strings.TrimSpace(n.Data); t != "" {
				sb.WriteString(t)
				sb.WriteString(" ")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, sb, inArticle, inLink)
		}
		if n.Type == html.ElementNode {
			switch strings.ToLower(n.Data) {
			case "p", "div", "article", "section", "h1", "h2", "h3", "h4", "h5", "h6", "li", "blockquote", "tr", "br", "hr", "header":
				sb.WriteString("\n")
			}
		}
	}
	var sb strings.Builder
	walk(node, &sb, false, false)

	var cleanedLines []string
	for _, line := range strings.Split(sb.String(), "\n") {
		if l := strings.TrimSpace(line); l != "" {
			cleanedLines = append(cleanedLines, l)
		}
	}
	return foundTitle, foundDesc, strings.Join(cleanedLines, "\n\n"), nil
}

// attrOf is the value of an attribute of n, "" when absent.
func attrOf(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

// baseHref is the href of the page's <base>, "" when there is none.
func baseHref(n *html.Node) string {
	if n.Type == html.ElementNode && strings.EqualFold(n.Data, "base") {
		if h := strings.TrimSpace(attrOf(n, "href")); h != "" {
			return h
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if h := baseHref(c); h != "" {
			return h
		}
	}
	return ""
}

// webAddress is href as an absolute http or https address, "" for anything else:
// a link inside the page (#…), mail, scripts, or a relative link with no page
// address to resolve it against.
func webAddress(href string, base *url.URL) string {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") {
		return ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.String()
}
