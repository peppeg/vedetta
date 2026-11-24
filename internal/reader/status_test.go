package reader

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The site's error page is not a page read (Piano dopo audit 1.4 / R5). Jina
// answers 200 with the site's error page and says the real status in
// data.warning, as the Jina at home did on 11/11/2026.
func TestJinaReader_TargetErrorIsAFailure(t *testing.T) {
	page := strings.Repeat("Wikipedia does not have an article with this exact name. ", 40)
	cases := []struct {
		name, warning string
		wantErr       bool
	}{
		{"404", "Target URL returned error 404: Not Found", true},
		{"503", "Target URL returned error 503: Service Unavailable", true},
		{"403", "Target URL returned error 403: Forbidden", true},
		{"other warning", "This page maybe not yet fully loaded, consider explicitly specify a timeout.", false},
		{"no warning", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				body := `{"code":200,"data":{"title":"Page","url":"https://93.184.216.34/x","content":"` + page + `"`
				if tc.warning != "" {
					body += `,"warning":"` + tc.warning + `"`
				}
				_, _ = w.Write([]byte(body + `}}`))
			}))
			defer srv.Close()
			doc, err := NewJinaReader(srv.URL).Read(context.Background(), "https://93.184.216.34/x")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("the site's error page was served as a page (%d chars)", len(doc.Content))
				}
				if code := tc.name; !strings.Contains(err.Error(), code) {
					t.Errorf("error %q does not report the status %s", err, code)
				}
				return
			}
			if err != nil || doc == nil || doc.Content == "" {
				t.Errorf("want the page, got doc %v err %v", doc, err)
			}
		})
	}
}

func TestFirecrawlClient_TargetErrorIsAFailure(t *testing.T) {
	for _, tc := range []struct {
		status  string
		wantErr bool
	}{{"404", true}, {"500", true}, {"200", false}, {"", false}} {
		t.Run("status "+tc.status, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				meta := `"title":"T","sourceURL":"https://93.184.216.34/x"`
				if tc.status != "" {
					meta += `,"statusCode":` + tc.status
				}
				_, _ = w.Write([]byte(`{"success":true,"data":{"markdown":"# Page\n\nSome text of the page.","metadata":{` + meta + `}}}`))
			}))
			defer srv.Close()
			doc, err := NewFirecrawlClient("k", srv.URL).Read(context.Background(), "https://93.184.216.34/x")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("status %s served as a page: %q", tc.status, doc.Content)
				}
				if !strings.Contains(err.Error(), tc.status) {
					t.Errorf("error %q does not report the status %s", err, tc.status)
				}
				return
			}
			if err != nil {
				t.Errorf("want the page, got %v", err)
			}
		})
	}
}

// A page that does not exist does not start other readers (Piano dopo audit 1.12):
// a 404 or 410 confirmed by a reader that builds the page as a browser does (Jina,
// the browser, an external service) stops the chain, so no paid reader after it
// runs. The direct reader's 404 does not: a site built in JavaScript may answer
// 404 to the first request and show the page once built (manual.seafile.com,
// 11/11/2026). A refusal (403, 429) or a temporary error goes on as before.
func TestCompositeReader_MissingPageStopsTheChain(t *testing.T) {
	page := &Document{Content: strings.Repeat("A real page with enough text. ", 20)}
	for _, tc := range []struct {
		name      string
		first     Reader
		wantLast  int // calls to the last reader, the paid one
		wantError bool
	}{
		{"jina 404", &mockReader{name: "jina-reader", errToRet: targetStatusError("jina", 404, "Not Found")}, 0, true},
		{"jina 410", &mockReader{name: "jina-reader", errToRet: targetStatusError("jina", 410, "Gone")}, 0, true},
		{"jina 403", &mockReader{name: "jina-reader", errToRet: targetStatusError("jina", 403, "Forbidden")}, 1, false},
		{"jina 503", &mockReader{name: "jina-reader", errToRet: targetStatusError("jina", 503, "Service Unavailable")}, 1, false},
		{"http 404", &mockReader{name: "http", errToRet: targetStatusError("http", 404, "Not Found")}, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			middle := &mockReader{name: "browser", errToRet: errors.New("browser: page did not load")}
			paid := &mockReader{name: "firecrawl", docToRet: page}
			_, err := NewCompositeReader(CompositeReaderConfig{Chain: []Reader{tc.first, middle, paid}}).Read(context.Background(), "https://93.184.216.34/x")
			if got := len(paid.calledURLs); got != tc.wantLast {
				t.Errorf("paid reader called %d times, want %d", got, tc.wantLast)
			}
			if tc.wantError && !errors.Is(err, ErrTargetStatus) {
				t.Errorf("err = %v, want ErrTargetStatus", err)
			}
		})
	}
}

// The direct reader's answers other than 200 carry the status, as Jina's and
// Firecrawl's do.
func TestDirectHTTPClient_StatusIsTargetStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer srv.Close()
	h := NewDirectHTTPClient()
	h.client.Transport = http.DefaultTransport // the test server is local
	resp, err := h.client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	err = statusError("http", resp)
	var tse *TargetStatusError
	if !errors.As(err, &tse) || tse.Code != 404 || !errors.Is(err, ErrTargetStatus) || !strings.Contains(err.Error(), "http reader returned status 404") {
		t.Errorf("err = %v", err)
	}
}
