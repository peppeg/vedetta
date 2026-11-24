package cache

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"vedetta/internal/reader"
)

// minUsefulChars mirrors internal/agent/reading.go: a shorter page is
// discarded by the research, and may be a temporary failure, so it expires soon.
const minUsefulChars = 300

// Error kinds for read failures that describe the page, not the moment.
const (
	errLowDensity  = "low_density"
	errUnsupported = "unsupported"
)

// cachedReader wraps a content reader. Blocked or empty pages come back from
// the reader as documents and are stored as such: the research discards them
// again on replay, with the same rule.
type cachedReader struct {
	inner reader.Reader
	s     *Session
}

// WrapReader returns r with the cache in front of it.
func (s *Session) WrapReader(r reader.Reader) reader.Reader {
	if r == nil {
		return nil
	}
	return &cachedReader{inner: r, s: s}
}

func (w *cachedReader) Name() string { return w.inner.Name() }

// freshKey marks a context whose page must be read now, not taken from the memory.
type freshKey struct{}

// WithFresh asks for a page read now (map R2b: the model's "read it again now"):
// the stored copy is skipped and replaced by the new reading. A benchmark corpus
// (replay) still answers from the corpus.
func WithFresh(ctx context.Context) context.Context { return context.WithValue(ctx, freshKey{}, true) }

func freshWanted(ctx context.Context) bool { v, _ := ctx.Value(freshKey{}).(bool); return v }

func (w *cachedReader) Read(ctx context.Context, targetURL string) (*reader.Document, error) {
	c := w.s.c
	key := PageKey(targetURL)
	skip := freshWanted(ctx) && c.mode != ModeReplay
	if e, ok := c.store.Get(KindPage, key); ok && !skip && c.fresh(e) {
		w.s.pageHits.Add(1)
		if e.ErrorKind != "" {
			return nil, cachedError(e)
		}
		if e.Document != nil {
			return e.Document, nil
		}
	}
	w.s.pageMisses.Add(1)
	doc, err := w.inner.Read(ctx, targetURL)
	if err != nil {
		if kind := stableErrorKind(err); kind != "" {
			c.put(Entry{Key: key, Kind: KindPage, ErrorKind: kind, Error: err.Error()}, shortTTL)
		}
		return doc, err
	}
	if doc == nil {
		return doc, err
	}
	ttl := c.pageTTL
	if len([]rune(strings.TrimSpace(doc.Content))) < minUsefulChars || reader.BlockPageMarker(doc) != "" {
		ttl = shortTTL
	}
	c.put(Entry{Key: key, Kind: KindPage, Document: doc}, ttl)
	return doc, nil
}

// Page returns a page the cache still holds, without reading it and without counting
// a hit: what a session read in its earlier turns (Piano dopo audit 4.4).
func (s *Session) Page(targetURL string) (*reader.Document, bool) {
	if s == nil {
		return nil, false
	}
	e, ok := s.c.store.Get(KindPage, PageKey(targetURL))
	if !ok || !s.c.fresh(e) || e.Document == nil {
		return nil, false
	}
	return e.Document, true
}

// stableErrorKind classifies errors that say something about the page itself.
// Timeouts, network errors and failed chains return "" and are not stored.
func stableErrorKind(err error) string {
	switch {
	case errors.Is(err, reader.ErrLowDensityContent):
		return errLowDensity
	case errors.Is(err, reader.ErrUnsupportedFormat):
		return errUnsupported
	}
	return ""
}

func cachedError(e Entry) error {
	sentinel := reader.ErrLowDensityContent
	if e.ErrorKind == errUnsupported {
		sentinel = reader.ErrUnsupportedFormat
	}
	return fmt.Errorf("%w (cached): %s", sentinel, e.Error)
}

// FallbackReaders forwards to the wrapped reader. Pages served from the cache
// are not counted: the note speaks of this run's reads.
func (w *cachedReader) FallbackReaders(sessionID string) (string, []string) {
	if fd, ok := w.inner.(reader.FallbackDetailer); ok {
		return fd.FallbackReaders(sessionID)
	}
	return w.inner.Name(), nil
}

// FallbackStatus forwards to the wrapped reader.
func (w *cachedReader) FallbackStatus(sessionID ...string) (bool, string) {
	if fb, ok := w.inner.(reader.FallbackReporter); ok {
		return fb.FallbackStatus(sessionID...)
	}
	return false, ""
}

// ResetFallback forwards to the wrapped reader.
func (w *cachedReader) ResetFallback(sessionID ...string) {
	if fb, ok := w.inner.(reader.FallbackResetter); ok {
		fb.ResetFallback(sessionID...)
	}
}
