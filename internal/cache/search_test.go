package cache

import (
	"context"
	"errors"
	"testing"
	"time"

	"vedetta/internal/search"
)

type fakeSearch struct {
	calls   int
	results []search.Result
	err     error
	resets  int
}

func (f *fakeSearch) Name() string { return "searxng" }
func (f *fakeSearch) Search(ctx context.Context, q string, o search.Options) ([]search.Result, error) {
	f.calls++
	return f.results, f.err
}
func (f *fakeSearch) FallbackStatus(sessionID ...string) (bool, string) { return true, "primary down" }
func (f *fakeSearch) ResetFallback(sessionID ...string)                 { f.resets++ }

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestCache(t *testing.T, mode string) (*Cache, *clock) {
	t.Helper()
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := New(st, mode, 24*time.Hour, 168*time.Hour)
	clk := &clock{t: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
	c.now = clk.now
	return c, clk
}

var hit = []search.Result{{Title: "A", URL: "https://a.example", Engine: "duckduckgo"}}

func TestSearch_RecordReusesUntilExpiry(t *testing.T) {
	c, clk := newTestCache(t, ModeRecord)
	inner := &fakeSearch{results: hit}
	s := c.Session()
	p := s.WrapSearch(inner)
	ctx := context.Background()

	if _, err := p.Search(ctx, "Foo", search.Options{SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	res, _ := p.Search(ctx, " foo ", search.Options{SessionID: "s2"})
	if inner.calls != 1 || len(res) != 1 || res[0].Engine != "duckduckgo" {
		t.Fatalf("second identical search should be a hit: calls=%d res=%+v", inner.calls, res)
	}

	clk.t = clk.t.Add(25 * time.Hour)
	p.Search(ctx, "foo", search.Options{})
	if inner.calls != 2 {
		t.Fatalf("expired entry should go to the network, calls=%d", inner.calls)
	}
	if got := s.Stats(); got.SearchHits != 1 || got.SearchMisses != 2 || got.Mode != ModeRecord {
		t.Errorf("stats = %+v", got)
	}
}

func TestSearch_ReplayIgnoresExpiry(t *testing.T) {
	c, clk := newTestCache(t, ModeRecord)
	inner := &fakeSearch{results: hit}
	c.Session().WrapSearch(inner).Search(context.Background(), "foo", search.Options{})

	replay := New(c.store, ModeReplay, time.Hour, time.Hour)
	clk.t = clk.t.Add(1000 * time.Hour)
	replay.now = clk.now
	res, _ := replay.Session().WrapSearch(inner).Search(context.Background(), "foo", search.Options{})
	if inner.calls != 1 || len(res) != 1 {
		t.Fatalf("replay should serve an expired entry: calls=%d", inner.calls)
	}
}

func TestSearch_ErrorsAreNotStored(t *testing.T) {
	c, _ := newTestCache(t, ModeRecord)
	inner := &fakeSearch{err: errors.New("searxng request failed: timeout")}
	p := c.Session().WrapSearch(inner)
	p.Search(context.Background(), "foo", search.Options{})
	p.Search(context.Background(), "foo", search.Options{})
	if inner.calls != 2 {
		t.Fatalf("a failed search must not be cached, calls=%d", inner.calls)
	}
}

func TestSearch_EmptyResultShortExpiry(t *testing.T) {
	c, clk := newTestCache(t, ModeRecord)
	inner := &fakeSearch{}
	p := c.Session().WrapSearch(inner)
	p.Search(context.Background(), "foo", search.Options{})
	p.Search(context.Background(), "foo", search.Options{})
	if inner.calls != 1 {
		t.Fatalf("an empty result is an outcome and is cached, calls=%d", inner.calls)
	}
	clk.t = clk.t.Add(2 * time.Hour)
	p.Search(context.Background(), "foo", search.Options{})
	if inner.calls != 2 {
		t.Fatalf("an empty result expires after %v, calls=%d", shortTTL, inner.calls)
	}
}

func TestSearch_ForwardsNameAndFallback(t *testing.T) {
	c, _ := newTestCache(t, ModeRecord)
	inner := &fakeSearch{}
	p := c.Session().WrapSearch(inner)
	if p.Name() != "searxng" {
		t.Errorf("Name = %q", p.Name())
	}
	fr, ok := p.(search.FallbackReporter)
	if !ok {
		t.Fatal("wrapper must implement FallbackReporter")
	}
	if used, reason := fr.FallbackStatus("s1"); !used || reason != "primary down" {
		t.Errorf("FallbackStatus not forwarded: %v %q", used, reason)
	}
	p.(search.FallbackResetter).ResetFallback("s1")
	if inner.resets != 1 {
		t.Error("ResetFallback not forwarded")
	}
}
