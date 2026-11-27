package search

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// degoogAnswer is a real degoog 1.1 answer of 27/11/2026, shortened: the engine
// of a result in "source", each engine's outcome in "engineTimings".
const degoogAnswer = `{"query":"q","type":"web","totalTime":1200,"relatedSearches":[],"results":[
	{"title":"A","url":"https://a.example/x","snippet":"from the snippet","content":"from the snippet","source":"DuckDuckGo","sources":["DuckDuckGo","Startpage"],"score":25},
	{"title":"B","url":"https://b.example/y","content":"only content","source":"Bing","score":9},
	{"title":"A again","url":"https://a.example/x","snippet":"dup","source":"Bing","score":3}],
	"engineTimings":[
	{"name":"Bing","time":360,"resultCount":10,"status":"ok"},
	{"name":"Brave Search","time":490,"resultCount":0,"status":"rate_limited","errorReason":"Brave Search upstream returned HTTP 429","httpStatus":429},
	{"name":"Ecosia","time":223,"resultCount":0,"status":"blocked","errorReason":"Ecosia upstream returned HTTP 403","httpStatus":403},
	{"name":"Google CSE","time":800,"resultCount":0,"status":"rate-limited"},
	{"name":"Wikipedia","time":380,"resultCount":0,"status":"ok"}]}`

func TestDegoog_SearchReadsDegoogsOwnAnswer(t *testing.T) {
	var got url.Values
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, got = r.URL.Path, r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(degoogAnswer))
	}))
	defer srv.Close()

	rep := NewReport()
	// pasted with /api at the end: the provider finds the base itself
	res, err := NewDegoogProvider(srv.URL+"/api/").Search(WithReport(context.Background(), rep), "bonus casa", Options{Language: "it-IT", Categories: []string{"general"}})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/api/search" || got.Get("q") != "bonus casa" || got.Get("lang") != "it" {
		t.Errorf("request: path %q, query %v", path, got)
	}
	if got.Has("format") || got.Has("language") || got.Has("categories") {
		t.Errorf("SearXNG parameters sent to degoog: %v", got)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 results (one address twice), got %+v", res)
	}
	if res[0].Engine != "DuckDuckGo" || res[0].Content != "from the snippet" || res[0].Score != 25 {
		t.Errorf("first result %+v", res[0])
	}
	if res[1].Engine != "Bing" || res[1].Content != "only content" {
		t.Errorf("without snippet the content is read: %+v", res[1])
	}

	want := map[string]string{"Brave Search": "rate limited", "Ecosia": "blocked", "Google CSE": "rate limited"}
	silent := rep.Unresponsive()
	if len(silent) != len(want) {
		t.Fatalf("want %d engines (an ok engine with no results answered), got %+v", len(want), silent)
	}
	for _, f := range silent {
		if want[f.Engine] != f.Reason || f.Count != 1 {
			t.Errorf("unexpected entry %+v", f)
		}
	}
}

func TestDegoog_NoLanguageIsNotSent(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_, _ = w.Write([]byte(`{"results":[],"engineTimings":[]}`))
	}))
	defer srv.Close()
	for _, lang := range []string{"", "auto", "AUTO", "x", "12"} {
		if _, err := NewDegoogProvider(srv.URL).Search(context.Background(), "q", Options{Language: lang}); err != nil {
			t.Fatalf("%q: %v", lang, err)
		}
		if got.Has("lang") {
			t.Errorf("language %q sent as lang=%q", lang, got.Get("lang"))
		}
	}
}

func TestDegoog_LangAndBase(t *testing.T) {
	for in, want := range map[string]string{"it": "it", "EN": "en", "pt_BR": "pt", "zh-Hant-TW": "zh", "auto": "", "": "", "i": "", "e1": ""} {
		if got := degoogLang(in); got != want {
			t.Errorf("degoogLang(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"http://h:4444": "http://h:4444", " http://h:4444/ ": "http://h:4444", "http://h:4444/api": "http://h:4444",
		"http://h:4444/api/search": "http://h:4444", "http://h:4444/search": "http://h:4444", "https://x.org/degoog/api/": "https://x.org/degoog", "": "",
	} {
		if got := DegoogBase(in); got != want {
			t.Errorf("DegoogBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDegoog_SearchErrors(t *testing.T) {
	for code, quota := range map[int]bool{http.StatusTooManyRequests: true, http.StatusUnauthorized: false, http.StatusNotFound: false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		_, err := NewDegoogProvider(srv.URL).Search(context.Background(), "q", Options{})
		srv.Close()
		if err == nil || (err == ErrQuotaExhausted) != quota {
			t.Errorf("status %d: err %v", code, err)
		}
	}
	if _, err := Get("degoog", ProviderConfig{}); err == nil {
		t.Error("degoog without an address must not start")
	}
	if p, err := Get("degoog", ProviderConfig{DegoogURL: "http://h:4444/api"}); err != nil || p.Name() != "degoog" {
		t.Errorf("registry: %v %v", p, err)
	}
}

// fakeDegoog answers like a real degoog 1.1: its web page at / and /search, its
// JSON search under /api (400 without a query, 401 when an API key is wanted),
// the SearXNG-style JSON only when its admin turned it on (off here).
func fakeDegoog(wantKey bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/search":
			w.Header().Set("Content-Type", "application/json")
			switch {
			case wantKey:
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"You shall not pass!"}`))
			case r.URL.Query().Get("q") == "":
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"Missing or invalid query parameter 'q'"}`))
			default:
				_, _ = w.Write([]byte(degoogAnswer))
			}
		case "/api/config":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Unauthorized","code":401}`))
		case "/", "/search":
			w.Header().Set("Content-Type", "text/html; charset=UTF-8")
			_, _ = w.Write([]byte("<!doctype html><html>degoog</html>"))
		default:
			w.Header().Set("Content-Type", "text/html; charset=UTF-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("<!doctype html><html>not found</html>"))
		}
	}))
}

func TestProbeProvider_Degoog(t *testing.T) {
	srv := fakeDegoog(false)
	defer srv.Close()
	for _, addr := range []string{srv.URL, srv.URL + "/", srv.URL + "/api"} {
		if got := ProbeProvider(context.Background(), "degoog", ProviderConfig{DegoogURL: addr}); got.Status != "ok" || got.Problem != "" || got.Name != "degoog" {
			t.Errorf("%s: got %+v", addr, got)
		}
	}
	if n, err := ProbeDegoogResults(context.Background(), srv.URL); err != nil || n != 2 {
		t.Errorf("test search: got %d results, err %v", n, err)
	}

	keyed := fakeDegoog(true)
	defer keyed.Close()
	if got := ProbeProvider(context.Background(), "degoog", ProviderConfig{DegoogURL: keyed.URL}); got.Status != "down" || got.Problem != "degoog_key" {
		t.Errorf("API key wanted: got %+v", got)
	}

	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }))
	defer limited.Close()
	if got := ProbeProvider(context.Background(), "degoog", ProviderConfig{DegoogURL: limited.URL}); got.Status != "down" || got.Problem != "degoog_limit" {
		t.Errorf("rate limit: got %+v", got)
	}

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>router login</html>"))
	}))
	defer page.Close()
	if got := ProbeProvider(context.Background(), "degoog", ProviderConfig{DegoogURL: page.URL}); got.Status != "down" || got.Problem != "not_degoog" {
		t.Errorf("a web page: got %+v", got)
	}

	if got := ProbeProvider(context.Background(), "degoog", ProviderConfig{}); got.Status != "unknown" || got.Problem != "" {
		t.Errorf("no address: got %+v", got)
	}
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closed.Close()
	if got := ProbeProvider(context.Background(), "degoog", ProviderConfig{DegoogURL: closed.URL}); got.Status != "down" || got.Problem != "unreachable" {
		t.Errorf("closed: got %+v", got)
	}
}

// The SearXNG doctor at a degoog address points to the degoog service, with
// or without /api; a service that copies SearXNG's search at / still works.
func TestProbeProvider_SearxngAtDegoogSaysSo(t *testing.T) {
	srv := fakeDegoog(false)
	defer srv.Close()
	for _, addr := range []string{srv.URL, srv.URL + "/api/"} {
		got := ProbeProvider(context.Background(), "searxng", ProviderConfig{SearxngURL: addr})
		if got.Status != "down" || got.Problem != "is_degoog" || got.Detail != srv.URL {
			t.Errorf("%s: got %+v", addr, got)
		}
	}

	clone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search" && r.URL.Query().Get("format") == "json" && r.URL.Query().Get("q") == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"no query"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer clone.Close()
	if got := ProbeProvider(context.Background(), "searxng", ProviderConfig{SearxngURL: clone.URL}); got.Status != "ok" || got.Problem != "" {
		t.Errorf("SearXNG-style clone: got %+v", got)
	}
}
