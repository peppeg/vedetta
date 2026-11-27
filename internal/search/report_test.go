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

// degoog answers SearXNG's shape at /api/search, but names the engine of a
// result in "source" and reports each engine in "engineTimings" (shape taken
// from a real degoog 1.x answer on 27/11/2026, shortened).
func TestSearxng_ReadsDegoogEnginesAndFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"title":"A","url":"https://a.example/x","content":"c","snippet":"c","source":"DuckDuckGo","sources":["DuckDuckGo","Google CSE"],"score":25},
			{"title":"B","url":"https://b.example/y","content":"c","engine":"bing","source":"Bing","score":9}],
			"engineTimings":[
			{"name":"Bing","time":360,"resultCount":10,"status":"ok"},
			{"name":"Brave Search","time":490,"resultCount":0,"status":"rate_limited","errorReason":"Brave Search upstream returned HTTP 429","httpStatus":429},
			{"name":"Ecosia","time":223,"resultCount":0,"status":"blocked","errorReason":"Ecosia upstream returned HTTP 403","httpStatus":403},
			{"name":"Google","time":1152,"resultCount":0,"status":"interstitial"},
			{"name":"Wikipedia","time":380,"resultCount":0,"status":"ok"}],
			"type":"web"}`))
	}))
	defer srv.Close()

	rep := NewReport()
	res, err := NewSearxngProvider(srv.URL).Search(WithReport(context.Background(), rep), "q", Options{})
	if err != nil || len(res) != 2 {
		t.Fatalf("res=%d err=%v", len(res), err)
	}
	if res[0].Engine != "DuckDuckGo" {
		t.Errorf("engine from source: got %q", res[0].Engine)
	}
	if res[1].Engine != "bing" {
		t.Errorf("engine wins over source: got %q", res[1].Engine)
	}

	got := rep.Unresponsive()
	want := map[string]string{"Brave Search": "rate limited", "Ecosia": "blocked", "Google": "interstitial"}
	if len(got) != len(want) {
		t.Fatalf("want %d engines (an ok engine with no results answered), got %+v", len(want), got)
	}
	for _, f := range got {
		if want[f.Engine] != f.Reason || f.Count != 1 {
			t.Errorf("unexpected entry %+v", f)
		}
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
