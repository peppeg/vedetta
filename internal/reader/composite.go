package reader

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

type sessionFallback struct {
	fallbackUsed   bool
	fallbackReason string
	lastSeen       time.Time
	// servedBy lists, in order of first use, the readers after the first one
	// that actually produced a page: what the answer's note names.
	servedBy []string
}

const maxSessionFallbackEntries = 512

// CompositeReaderConfig configures the reader chain.
type CompositeReaderConfig struct {
	Name  string
	Chain []Reader
	// PubMed, when set, reads PubMed article URLs before the chain (idea I3a);
	// on failure the chain is used as usual.
	PubMed Reader
	// PDF, when set, reads PDF documents before the chain (V25-PDF.3): URLs that
	// look like PDFs, and responses the first reader recognized as PDFs. On
	// failure (e.g. a scan without text) the chain is used as usual.
	PDF Reader
}

// CompositeReader manages an ordered chain of reader providers with transparent fallback.
type CompositeReader struct {
	name     string
	chain    []Reader
	pubmed   Reader
	pdf      Reader
	sessions map[string]*sessionFallback
	mu       sync.RWMutex
}

// NewCompositeReader creates a new composite reader over an ordered chain.
func NewCompositeReader(cfg CompositeReaderConfig) *CompositeReader {
	var validChain []Reader
	for _, r := range cfg.Chain {
		if r != nil {
			validChain = append(validChain, r)
		}
	}

	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		if len(validChain) > 0 {
			name = validChain[0].Name()
		} else {
			name = "composite-reader"
		}
	}

	return &CompositeReader{
		name:     name,
		chain:    validChain,
		pubmed:   cfg.PubMed,
		pdf:      cfg.PDF,
		sessions: make(map[string]*sessionFallback),
	}
}

func (c *CompositeReader) Name() string {
	return c.name
}

// FallbackStatus returns whether a fallback occurred for the given session ID, and the reason.
func (c *CompositeReader) FallbackStatus(sessionID ...string) (used bool, reason string) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	sid := ""
	if len(sessionID) > 0 {
		sid = sessionID[0]
	}

	st, exists := c.sessions[sid]
	if !exists || st == nil {
		return false, ""
	}
	return st.fallbackUsed, st.fallbackReason
}

// FallbackReaders tells which reader comes first in the chain and which other
// readers actually produced pages in the session (for the answer's note).
func (c *CompositeReader) FallbackReaders(sessionID string) (primary string, servedBy []string) {
	if len(c.chain) > 0 {
		primary = c.chain[0].Name()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if st := c.sessions[sessionID]; st != nil {
		servedBy = append(servedBy, st.servedBy...)
	}
	return primary, servedBy
}

// noteServed remembers a page produced by a reader other than the first.
func (c *CompositeReader) noteServed(sessionID, name string) {
	if len(c.chain) == 0 || name == c.chain[0].Name() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.sessions[sessionID]
	if st == nil {
		st = &sessionFallback{}
		c.sessions[sessionID] = st
	}
	st.lastSeen = time.Now()
	for _, n := range st.servedBy {
		if n == name {
			return
		}
	}
	st.servedBy = append(st.servedBy, name)
	c.pruneSessionsLocked()
}

// ResetFallback clears the fallback state for the given session ID.
func (c *CompositeReader) ResetFallback(sessionID ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	sid := ""
	if len(sessionID) > 0 {
		sid = sessionID[0]
	}
	delete(c.sessions, sid)
}

func (c *CompositeReader) recordFallback(sessionID, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	st, exists := c.sessions[sessionID]
	if !exists || st == nil {
		st = &sessionFallback{}
		c.sessions[sessionID] = st
	}
	st.fallbackUsed = true
	st.lastSeen = time.Now()
	if st.fallbackReason == "" {
		st.fallbackReason = reason
	} else if !strings.Contains(st.fallbackReason, reason) {
		st.fallbackReason += "; " + reason
	}
	c.pruneSessionsLocked()
}

// pruneSessionsLocked bounds the per-session map to avoid unbounded growth for
// sessions that are never revisited. Must be called with c.mu held.
func (c *CompositeReader) pruneSessionsLocked() {
	if len(c.sessions) <= maxSessionFallbackEntries {
		return
	}
	var oldestID string
	var oldest time.Time
	for id, st := range c.sessions {
		if oldestID == "" || st.lastSeen.Before(oldest) {
			oldestID = id
			oldest = st.lastSeen
		}
	}
	delete(c.sessions, oldestID)
}

// Read fetches the document using the ordered reader chain with selective fallback.
func (c *CompositeReader) Read(ctx context.Context, targetURL string) (*Document, error) {
	if len(c.chain) == 0 {
		return nil, fmt.Errorf("no reader configured in composite reader")
	}

	sessionID := SessionIDFromContext(ctx)

	// PubMed records through Europe PMC: PubMed answers automated readers with an
	// interstitial page, the API returns the abstract (idea I3a).
	if c.pubmed != nil && PubMedID(targetURL) != "" {
		doc, err := c.pubmed.Read(ctx, targetURL)
		if err == nil && doc != nil && strings.TrimSpace(doc.Content) != "" {
			log.Printf("[reader] session=%s url=%s read via %s (%d chars)", sessionID, targetURL, c.pubmed.Name(), len(doc.Content))
			return doc, nil
		}
		log.Printf("[reader] session=%s url=%s %s failed (%v); using the reader chain", sessionID, targetURL, c.pubmed.Name(), err)
	}

	if c.pdf != nil && isPDFURL(targetURL) {
		doc, err := c.readPDF(ctx, sessionID, targetURL)
		if doc != nil || errors.Is(err, ErrPrivateAddress) {
			return doc, err
		}
	}

	// Selective routing: If target URL is PDF and primary reader is "http"
	if isPDFURL(targetURL) && c.chain[0].Name() == "http" {
		if len(c.chain) == 1 {
			// Rule 6: Single provider fails hard
			return nil, fmt.Errorf("%w: http reader cannot parse PDF documents (%s)", ErrUnsupportedFormat, targetURL)
		}

		// Fallback selectively to next reader in chain that supports PDF (e.g. jina, firecrawl)
		c.recordFallback(sessionID, fmt.Sprintf("http reader cannot parse PDF; falling back to %s", c.chain[1].Name()))
		for j := 1; j < len(c.chain); j++ {
			doc, err := c.chain[j].Read(ctx, targetURL)
			if err == nil && doc != nil && len(strings.TrimSpace(doc.Content)) > 0 {
				logServed(sessionID, targetURL, c.chain[j].Name(), doc)
				c.noteServed(sessionID, c.chain[j].Name())
				return doc, nil
			}
			if missingPage(c.chain[j], err) {
				log.Printf("[reader] session=%s url=%s %s: the page does not exist (%v); the chain stops", sessionID, targetURL, c.chain[j].Name(), err)
				return nil, err
			}
			if j+1 < len(c.chain) {
				c.recordFallback(sessionID, fmt.Sprintf("reader %s failed (%v); falling back to %s", c.chain[j].Name(), err, c.chain[j+1].Name()))
			}
		}
		return nil, fmt.Errorf("%w: all readers failed for PDF %s", ErrAllReadersFailed, targetURL)
	}

	// Normal chain iteration
	var lastErr error
	for i, r := range c.chain {
		doc, err := r.Read(ctx, targetURL)
		if err == nil && doc != nil && len(strings.TrimSpace(doc.Content)) > 0 {
			// Selective fallback: if primary "http" yields low-density SPA text
			// or an anti-bot/error block page, probe every remaining reader in the
			// chain (N-chain aware) and pick the best available document.
			trimmed := strings.TrimSpace(doc.Content)
			blockPage := isBlockPage(doc)
			if blockPage {
				log.Printf("[reader] session=%s url=%s reader=%s block_page marker=%q chars=%d", sessionID, targetURL, r.Name(), BlockPageMarker(doc), len(trimmed))
			}
			if i == 0 && r.Name() == "http" && len(c.chain) > 1 && (len(trimmed) < 250 || blockPage) {
				bestDoc := doc
				bestName := r.Name()
				for j := 1; j < len(c.chain); j++ {
					candDoc, candErr := c.chain[j].Read(ctx, targetURL)
					if candErr == nil && isBetterDoc(candDoc, bestDoc) {
						bestDoc = candDoc
						bestName = c.chain[j].Name()
					}
					// Keep probing the rest of the chain until we hold a readable,
					// non-block page. A block page never satisfies this condition,
					// so the whole remaining chain is always examined for it.
					if !isBlockPage(bestDoc) && len(strings.TrimSpace(bestDoc.Content)) >= 250 {
						break
					}
				}
				if bestDoc != doc {
					cause := "low-density SPA"
					if blockPage {
						cause = "block page"
					}
					c.recordFallback(sessionID, fmt.Sprintf("http reader returned %s (%d chars); fell back to %s (%d chars)",
						cause, len(trimmed), bestName, len(strings.TrimSpace(bestDoc.Content))))
					logServed(sessionID, targetURL, bestName, bestDoc)
					c.noteServed(sessionID, bestName)
					return bestDoc, nil
				}
			}
			logServed(sessionID, targetURL, r.Name(), doc)
			c.noteServed(sessionID, r.Name())
			return doc, nil
		}

		// A response recognized as a PDF (Content-Type or signature) although the
		// URL does not say so goes to the PDF reader before the rest of the chain.
		if err != nil && c.pdf != nil && errors.Is(err, ErrPDFContent) {
			pdfDoc, pdfErr := c.readPDF(ctx, sessionID, targetURL)
			if pdfDoc != nil || errors.Is(pdfErr, ErrPrivateAddress) {
				return pdfDoc, pdfErr
			}
		}

		lastErr = err

		// An address that leads inside the home network is not handed on.
		if errors.Is(err, ErrPrivateAddress) {
			log.Printf("[reader] session=%s url=%s %s: refused (%v); the chain stops", sessionID, targetURL, r.Name(), err)
			return nil, err
		}

		// A page that does not exist does not start the rest of the chain.
		if missingPage(r, err) {
			log.Printf("[reader] session=%s url=%s %s: the page does not exist (%v); the chain stops", sessionID, targetURL, r.Name(), err)
			return nil, err
		}

		// Rule 6: Single provider fails hard
		if len(c.chain) == 1 {
			return nil, err
		}

		// Multi-provider chain: record fallback and proceed to next provider
		if i+1 < len(c.chain) {
			nextName := c.chain[i+1].Name()
			c.recordFallback(sessionID, fmt.Sprintf("reader %s failed (%v); falling back to %s", r.Name(), err, nextName))
		}
	}

	return nil, fmt.Errorf("%w: %v", ErrAllReadersFailed, lastErr)
}

// readPDF tries the built-in PDF reader; reading a PDF there is not a fallback.
// The document is nil when it could not.
func (c *CompositeReader) readPDF(ctx context.Context, sessionID, targetURL string) (*Document, error) {
	doc, err := c.pdf.Read(ctx, targetURL)
	if err == nil && doc != nil && strings.TrimSpace(doc.Content) != "" {
		logServed(sessionID, targetURL, c.pdf.Name(), doc)
		return doc, nil
	}
	if errors.Is(err, ErrPrivateAddress) {
		log.Printf("[reader] session=%s url=%s %s: refused (%v); the chain stops", sessionID, targetURL, c.pdf.Name(), err)
		return nil, err
	}
	log.Printf("[reader] session=%s url=%s %s failed (%v); using the reader chain", sessionID, targetURL, c.pdf.Name(), err)
	return nil, err
}

// logServed records which reader of the chain produced the page, so a round's
// log tells how many pages each reader served (M1.0).
func logServed(sessionID, targetURL, readerName string, doc *Document) {
	log.Printf("[reader] session=%s url=%s served_by=%s chars=%d", sessionID, targetURL, readerName, len(strings.TrimSpace(doc.Content)))
}

func isPDFURL(u string) bool {
	uLower := strings.ToLower(strings.TrimSpace(u))
	return strings.HasSuffix(uLower, ".pdf") || strings.Contains(uLower, ".pdf?") || strings.Contains(uLower, ".pdf#")
}
