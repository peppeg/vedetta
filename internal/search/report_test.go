package search

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSearxng_RecordsUnresponsiveEngines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"url":"https://a.example/x","title":"A","content":"c","engine":"duckduckgo web"}],
			"unresponsive_engines":[["google cse","too many requests"],["brave","CAPTCHA"]]}`))
	}))
	defer srv.Close()

	p := NewSearxngProvider(srv.URL)
	rep := NewReport()
	ctx := WithReport(context.Background(), rep)
	for i := 0; i < 2; i++ {
		res, err := p.Search(ctx, "q", Options{})
		if err != nil || len(res) != 1 {
			t.Fatalf("search %d: res=%d err=%v", i, len(res), err)
		}
	}

	got := rep.Unresponsive()
	if len(got) != 2 {
		t.Fatalf("want 2 engines, got %+v", got)
	}
	if got[0].Engine != "brave" || got[0].Count != 2 || got[0].Reason != "CAPTCHA" {
		t.Errorf("unexpected first entry %+v", got[0])
	}
	if s := got[1].String(); s != "google cse (too many requests ×2)" {
		t.Errorf("unexpected format %q", s)
	}
}

func TestSearxng_NoReportInContextIsFine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[],"unresponsive_engines":[["yandex","timeout"]]}`))
	}))
	defer srv.Close()
	if _, err := NewSearxngProvider(srv.URL).Search(context.Background(), "q", Options{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReport_SuspendedPrefixAndSearchCounts(t *testing.T) {
	rep := NewReport()
	rep.AddUnresponsive("google cse", "too many requests")
	rep.AddUnresponsive("google cse", "Suspended: too many requests")
	rep.AddUnresponsive(" ", "ignored")
	rep.AddSearch(false)
	rep.AddSearch(true)
	rep.AddSearch(false)

	got := rep.Unresponsive()
	if len(got) != 1 || got[0].Count != 2 || got[0].Reason != "too many requests" {
		t.Fatalf("unexpected %+v", got)
	}
	if q, e := rep.Searches(); q != 3 || e != 1 {
		t.Errorf("want 3 searches 1 empty, got %d %d", q, e)
	}

	var nilRep *Report
	nilRep.AddSearch(true)
	nilRep.AddUnresponsive("x", "y")
	if nilRep.Unresponsive() != nil {
		t.Error("nil report must return nil")
	}
}
