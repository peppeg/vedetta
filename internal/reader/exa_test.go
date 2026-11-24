package reader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestExaReader_Read(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	var got exaContentsRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/contents" || r.Header.Get("x-api-key") != "test-key" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		var body string
		switch got.URLs[0] {
		case "https://www.researchgate.net/ok":
			body = `{"results":[{"title":"Speranza attiva","url":"https://www.researchgate.net/ok","text":"  Il testo della pagina.  "}],
				"statuses":[{"id":"https://www.researchgate.net/ok","status":"success","source":"crawled"}],"costDollars":{"total":0.001}}`
		case "https://example.com/blocked":
			body = `{"results":[],"statuses":[{"id":"https://example.com/blocked","status":"error","error":{"tag":"CRAWL_NOT_FOUND","httpStatusCode":404}}],"costDollars":{"total":0}}`
		default:
			body = `{"results":[{"title":"","url":"https://example.com/empty","text":"   "}],"statuses":[{"status":"success","source":"cached"}]}`
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	e := NewExaReader("test-key", server.URL)
	ctx := WithSessionID(context.Background(), "s-exa")

	doc, err := e.Read(ctx, "https://www.researchgate.net/ok")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if doc.Content != "Il testo della pagina." || doc.Title != "Speranza attiva" || doc.Metadata["exa_source"] != "crawled" || doc.Metadata["source_type"] != "exa" {
		t.Errorf("doc = %+v", doc)
	}
	if len(got.URLs) != 1 || got.Text.MaxCharacters != exaMaxChars || got.LivecrawlTimeout != 15000 {
		t.Errorf("request = %+v", got)
	}
	if !strings.Contains(buf.String(), "[exa] session=s-exa kind=contents url=https://www.researchgate.net/ok source=crawled chars=22 cost=$0.0010") {
		t.Errorf("log = %q", buf.String())
	}

	if _, err := e.Read(ctx, "https://example.com/blocked"); err == nil || !strings.Contains(err.Error(), "CRAWL_NOT_FOUND (http 404)") {
		t.Errorf("blocked: got %v", err)
	}
	if _, err := e.Read(ctx, "https://example.com/empty"); !errors.Is(err, ErrLowDensityContent) {
		t.Errorf("empty: want ErrLowDensityContent, got %v", err)
	}
}

func TestExaReader_Errors(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusUnauthorized:    "check EXA_API_KEY",
		http.StatusPaymentRequired: "credits exhausted",
		http.StatusTooManyRequests: "rate limit",
		http.StatusBadGateway:      "status 502",
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", status)
		}))
		_, err := NewExaReader("k", server.URL).Read(context.Background(), "https://example.com/page")
		server.Close()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("status %d: want %q, got %v", status, want, err)
		}
	}
	if _, err := NewExaReader("k").Read(context.Background(), "http://127.0.0.1/x"); err == nil || !strings.Contains(err.Error(), "ssrf") {
		t.Errorf("private address: want ssrf error, got %v", err)
	}
	if _, err := NewReader(ReaderConfig{Providers: []string{"http", "exa"}}); err == nil {
		t.Error("exa reader without key: want error")
	}
	if r, err := NewReader(ReaderConfig{Providers: []string{"http", "exa"}, ExaAPIKey: "k"}); err != nil || r == nil {
		t.Errorf("exa reader with key: %v", err)
	}
}
