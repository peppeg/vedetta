package cache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"vedetta/internal/reader"
)

type fakeReader struct {
	calls int
	doc   *reader.Document
	err   error
}

func (f *fakeReader) Name() string { return "jina" }
func (f *fakeReader) Read(ctx context.Context, u string) (*reader.Document, error) {
	f.calls++
	return f.doc, f.err
}

var longDoc = &reader.Document{URL: "https://a.example/x", Title: "Article", Content: strings.Repeat("real content ", 50)}

func TestReader_RecordReusesPage(t *testing.T) {
	c, clk := newTestCache(t, ModeRecord)
	inner := &fakeReader{doc: longDoc}
	s := c.Session()
	r := s.WrapReader(inner)

	r.Read(context.Background(), "https://a.example/x")
	doc, err := r.Read(context.Background(), "https://A.example/x#top")
	if err != nil || inner.calls != 1 || doc.Content != longDoc.Content {
		t.Fatalf("second read should be a hit: calls=%d err=%v", inner.calls, err)
	}
	clk.t = clk.t.Add(169 * time.Hour)
	r.Read(context.Background(), "https://a.example/x")
	if inner.calls != 2 {
		t.Fatalf("expired page should be read again, calls=%d", inner.calls)
	}
	if got := s.Stats(); got.PageHits != 1 || got.PageMisses != 2 {
		t.Errorf("stats = %+v", got)
	}
}

func TestReader_ShortOrBlockedPageShortExpiry(t *testing.T) {
	for name, doc := range map[string]*reader.Document{
		"short":   {URL: "https://a.example/s", Content: "tiny"},
		"blocked": {URL: "https://a.example/b", Title: "Just a moment...", Content: "Checking your browser before accessing"},
	} {
		c, clk := newTestCache(t, ModeRecord)
		inner := &fakeReader{doc: doc}
		r := c.Session().WrapReader(inner)
		r.Read(context.Background(), doc.URL)
		r.Read(context.Background(), doc.URL)
		if inner.calls != 1 {
			t.Errorf("%s: page should be cached for a while, calls=%d", name, inner.calls)
		}
		clk.t = clk.t.Add(2 * time.Hour)
		r.Read(context.Background(), doc.URL)
		if inner.calls != 2 {
			t.Errorf("%s: page should expire after %v, calls=%d", name, shortTTL, inner.calls)
		}
	}
}

func TestReader_StableErrorReplayedWithoutNetwork(t *testing.T) {
	c, clk := newTestCache(t, ModeRecord)
	inner := &fakeReader{err: fmt.Errorf("%w: content length 12 is below minimum readable threshold", reader.ErrLowDensityContent)}
	c.Session().WrapReader(inner).Read(context.Background(), "https://spa.example")

	replay := New(c.store, ModeReplay, time.Hour, time.Hour)
	clk.t = clk.t.Add(100 * time.Hour)
	replay.now = clk.now
	s := replay.Session()
	_, err := s.WrapReader(inner).Read(context.Background(), "https://spa.example")
	if inner.calls != 1 {
		t.Fatalf("replay should not go to the network, calls=%d", inner.calls)
	}
	if !errors.Is(err, reader.ErrLowDensityContent) {
		t.Errorf("cached error should keep its kind, got %v", err)
	}
	if s.Stats().PageHits != 1 {
		t.Errorf("stats = %+v", s.Stats())
	}
}

func TestReader_TransientErrorsNotStored(t *testing.T) {
	for _, err := range []error{
		context.DeadlineExceeded,
		errors.New("jina request failed: connection refused"),
		fmt.Errorf("%w: last error: low density", reader.ErrAllReadersFailed),
	} {
		c, _ := newTestCache(t, ModeRecord)
		inner := &fakeReader{err: err}
		r := c.Session().WrapReader(inner)
		r.Read(context.Background(), "https://a.example")
		r.Read(context.Background(), "https://a.example")
		if inner.calls != 2 {
			t.Errorf("%v must not be cached, calls=%d", err, inner.calls)
		}
	}
}

func TestReader_ForwardsName(t *testing.T) {
	c, _ := newTestCache(t, ModeRecord)
	r := c.Session().WrapReader(&fakeReader{})
	if r.Name() != "jina" {
		t.Errorf("Name = %q", r.Name())
	}
	if used, _ := r.(reader.FallbackReporter).FallbackStatus(); used {
		t.Error("inner without FallbackReporter reports no fallback")
	}
}

// Piano dopo audit 4.4: a follow-up takes the pages of the earlier turns from the
// cache. Looking is not reading: nothing is fetched, no hit is counted.
func TestSession_PageIsOnlyWhatTheCacheHolds(t *testing.T) {
	c, clk := newTestCache(t, ModeRecord)
	inner := &fakeReader{doc: longDoc}
	s := c.Session()
	if _, ok := s.Page("https://a.example/x"); ok {
		t.Fatal("a page never read is not in the cache")
	}
	s.WrapReader(inner).Read(context.Background(), "https://a.example/x")
	doc, ok := s.Page("https://A.example/x#top")
	if !ok || doc.Content != longDoc.Content {
		t.Fatalf("the page read is in the cache: ok=%t doc=%v", ok, doc)
	}
	if got := s.Stats(); inner.calls != 1 || got.PageHits != 0 || got.PageMisses != 1 {
		t.Errorf("looking read or counted: calls=%d stats=%+v", inner.calls, got)
	}
	clk.t = clk.t.Add(169 * time.Hour)
	if _, ok := s.Page("https://a.example/x"); ok {
		t.Error("an expired page does not count")
	}
	var none *Session
	if _, ok := none.Page("https://a.example/x"); ok {
		t.Error("without a cache there is no page")
	}
}

// Map R2b: a page asked "now" skips the stored copy and replaces it; a benchmark
// corpus keeps answering from the corpus.
func TestReader_FreshSkipsTheMemory(t *testing.T) {
	for _, mode := range []string{ModeRecord, ModeReplay} {
		st, _ := NewStore(t.TempDir())
		c := New(st, mode, time.Hour, time.Hour)
		inner := &countingReader{}
		r := c.Session().WrapReader(inner)
		ctx := context.Background()
		if _, err := r.Read(ctx, "https://example.org/prezzo"); err != nil {
			t.Fatal(err)
		}
		r.Read(ctx, "https://example.org/prezzo")
		if inner.n != 1 {
			t.Fatalf("%s: the second plain read must come from the memory, inner reads = %d", mode, inner.n)
		}
		r.Read(WithFresh(ctx), "https://example.org/prezzo")
		want := 2
		if mode == ModeReplay {
			want = 1
		}
		if inner.n != want {
			t.Errorf("%s: after a fresh read, inner reads = %d, want %d", mode, inner.n, want)
		}
	}
}

type countingReader struct{ n int }

func (r *countingReader) Name() string { return "counting" }
func (r *countingReader) Read(ctx context.Context, u string) (*reader.Document, error) {
	r.n++
	return &reader.Document{URL: u, Title: "Prezzo", Content: strings.Repeat("Il prezzo di oggi è 100. ", 20), ExtractedAt: time.Now()}, nil
}
