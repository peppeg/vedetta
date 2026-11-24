package reader

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

var (
	// ErrAllReadersFailed is returned by CompositeReader when all providers in the chain fail.
	ErrAllReadersFailed = errors.New("all configured content readers failed")
	// ErrUnsupportedFormat indicates that the reader cannot process this format (e.g. PDF on DirectHTTPClient).
	ErrUnsupportedFormat = errors.New("unsupported content format for reader")
	// ErrLowDensityContent indicates extracted text is below readable threshold (e.g. JS-only SPA).
	ErrLowDensityContent = errors.New("extracted content density is too low")
	// ErrTargetStatus indicates that the site answered with an error status (404,
	// 503…) that a reader service reported while serving the site's error page.
	ErrTargetStatus = errors.New("the site answered with an error status")
)

// TargetStatusError is a site that answered with an error status, as a reader saw
// it: the direct reader's own request, or a service (Jina, Firecrawl) that reported
// the site's status while serving its error page. It is ErrTargetStatus.
type TargetStatusError struct {
	Reader string // the reader that saw it
	Code   int    // the site's HTTP status
	Msg    string
}

func (e *TargetStatusError) Error() string        { return e.Msg }
func (e *TargetStatusError) Is(target error) bool { return target == ErrTargetStatus }

// statusError is the error of a response with a status other than 200.
func statusError(reader string, resp *http.Response) error {
	return &TargetStatusError{Reader: reader, Code: resp.StatusCode, Msg: fmt.Sprintf("%s reader returned status %d", reader, resp.StatusCode)}
}

// targetStatusError is the error of a site whose status a service reported: its
// error page is not a page read (Piano dopo audit 1.4 / R5).
func targetStatusError(reader string, code int, detail string) error {
	return &TargetStatusError{Reader: reader, Code: code, Msg: fmt.Sprintf("%s: %s: the site answered %d (%s)", ErrTargetStatus, reader, code, detail)}
}

// missingPage tells whether err says that the page does not exist (404, 410) and
// comes from a reader that builds the page as a browser does: no other reader will
// find it, so the chain stops and no paid reader runs for it (Piano dopo audit 1.12).
// The direct reader's 404 is not enough: a site built in JavaScript may answer 404
// to the first request and show the page once built.
func missingPage(r Reader, err error) bool {
	var tse *TargetStatusError
	return errors.As(err, &tse) && (tse.Code == http.StatusNotFound || tse.Code == http.StatusGone) && r.Name() != "http"
}

// Document represents an extracted text document from a web source.
type Document struct {
	Title       string            `json:"title"`
	URL         string            `json:"url"`
	Content     string            `json:"content"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	ExtractedAt time.Time         `json:"extracted_at"`
}

// Reader is the interface for fetching and extracting web documents.
type Reader interface {
	Name() string
	Read(ctx context.Context, targetURL string) (*Document, error)
}

// FallbackReporter exposes whether a reader fallback occurred during a session.
type FallbackReporter interface {
	FallbackStatus(sessionID ...string) (used bool, reason string)
}

// FallbackDetailer names the first reader of the chain and the other readers
// that produced pages in a session.
type FallbackDetailer interface {
	FallbackReaders(sessionID string) (primary string, servedBy []string)
}

// FallbackResetter allows resetting reader fallback status for a session.
type FallbackResetter interface {
	ResetFallback(sessionID ...string)
}

type readerContextKey string

const sessionContextKey readerContextKey = "vedetta_reader_session_id"

// WithSessionID returns a new context with the given session ID attached.
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	if sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionContextKey, sessionID)
}

// SessionIDFromContext extracts the session ID from the context if present.
func SessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(sessionContextKey).(string); ok {
		return v
	}
	return ""
}
