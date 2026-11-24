package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"vedetta/config"
)

func fakeSearxngWith(searchCode int, results string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config":
			w.Write([]byte(`{"version":"2026.11.18","engines":[{"name":"yandex","enabled":true}],"limiter":{"enabled":false}}`))
		case "/search":
			if r.URL.Query().Get("q") == "" {
				w.WriteHeader(searchCode)
				return
			}
			w.Write([]byte(`{"results":` + results + `}`))
		}
	}))
}

func fakeJina() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("[Usage 1] https://r.jina.ai/YOUR_URL\n"))
	}))
}

func fakePage() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>router login</html>"))
	}))
}

func deadURL() string {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s.Close()
	return s.URL
}

// Map R8: the first run shows the SearXNG and Jina it finds at the usual
// addresses, working ones first, and says why one cannot be used yet; dead
// addresses and pages that are something else stay hidden unless typed or set.
func TestDiscover(t *testing.T) {
	good := fakeSearxngWith(http.StatusBadRequest, `[{"url":"https://a.example"},{"url":"https://b.example"}]`)
	defer good.Close()
	blocked := fakeSearxngWith(http.StatusBadRequest, `[]`)
	defer blocked.Close()
	jsonOff := fakeSearxngWith(http.StatusForbidden, `[]`)
	defer jsonOff.Close()
	page := fakePage()
	defer page.Close()
	jina := fakeJina()
	defer jina.Close()
	dead := deadURL()

	oldS, oldJ, oldT := searxngCandidates, jinaCandidates, discoverProbeTime
	defer func() { searxngCandidates, jinaCandidates, discoverProbeTime = oldS, oldJ, oldT }()
	searxngCandidates = []string{dead, jsonOff.URL, page.URL, good.URL + "/", blocked.URL}
	jinaCandidates = []string{page.URL, jina.URL, dead}
	discoverProbeTime = 2 * time.Second

	h := guardedServer(t, func(c *config.Config) {
		c.Bind = "127.0.0.1"
		c.SearxngURL = "http://127.0.0.1:1" // set now, nothing there
		c.JinaURL = jina.URL
		c.Publish = "127.0.0.1"
	})
	typed := deadURL()
	rec := call(h, "GET", "/api/setup/discover?searxng="+url.QueryEscape(typed[len("http://"):]), "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got DiscoverResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.InContainer {
		t.Error("VEDETTA_PUBLISH set: in_container should be true")
	}

	byURL := map[string]FoundService{}
	for _, f := range got.Search {
		byURL[f.URL] = f
	}
	if len(got.Search) != 5 {
		t.Errorf("search: want good, blocked, json off, typed, current; got %+v", got.Search)
	}
	if f := got.Search[0]; f.URL != good.URL || f.Status != "ok" || f.Results == nil || *f.Results != 2 || f.Problem != "" || f.Version != "2026.11.18" {
		t.Errorf("first should be the working SearXNG: %+v", f)
	}
	if f := byURL[blocked.URL]; f.Status != "ok" || f.Problem != "no_results" {
		t.Errorf("SearXNG with no results: %+v", f)
	}
	if f := byURL[jsonOff.URL]; f.Status != "down" || f.Problem != "json_off" {
		t.Errorf("SearXNG with JSON off: %+v", f)
	}
	if f := byURL[typed]; !f.Typed || f.Problem != "unreachable" {
		t.Errorf("typed address: %+v", f)
	}
	if f := byURL["http://127.0.0.1:1"]; !f.Current || f.Status != "down" {
		t.Errorf("current address: %+v", f)
	}
	if _, ok := byURL[page.URL]; ok {
		t.Error("a page that is not SearXNG, at a usual address, should stay hidden")
	}
	if _, ok := byURL[dead]; ok {
		t.Error("a dead usual address should stay hidden")
	}

	if len(got.Reader) != 1 || got.Reader[0].URL != jina.URL || got.Reader[0].Status != "ok" || !got.Reader[0].Current {
		t.Errorf("reader: want only the Jina set now; got %+v", got.Reader)
	}
}

// The search for services is a settings matter: with a settings code, not without.
func TestDiscover_NeedsSettingsCode(t *testing.T) {
	oldS, oldJ := searxngCandidates, jinaCandidates
	defer func() { searxngCandidates, jinaCandidates = oldS, oldJ }()
	searxngCandidates, jinaCandidates = nil, nil
	h := guardedServer(t, func(c *config.Config) { c.Bind = "127.0.0.1"; c.ConfigToken = "impostazioni-segrete" })
	if rec := call(h, "GET", "/api/setup/discover", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("without the settings code: %d, want 401", rec.Code)
	}
	if rec := call(h, "GET", "/api/setup/discover", "", map[string]string{"X-Vedetta-Token": "impostazioni-segrete"}); rec.Code != http.StatusOK {
		t.Errorf("with the settings code: %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

func TestNormalizeServiceURL(t *testing.T) {
	for in, want := range map[string]string{
		"server:8888":             "http://server:8888",
		" http://searxng:8080/ ":  "http://searxng:8080",
		"https://s.example.org/x": "https://s.example.org/x",
		"ftp://x":                 "",
		"":                        "",
	} {
		if got := normalizeServiceURL(in); got != want {
			t.Errorf("normalizeServiceURL(%q) = %q, want %q", in, got, want)
		}
	}
}
