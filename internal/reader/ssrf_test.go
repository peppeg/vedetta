package reader

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateSafeURL_Blocked(t *testing.T) {
	blockedURLs := []string{
		"http://localhost/admin",
		"http://127.0.0.1:8080/secrets",
		"http://192.168.1.1/router",
		"http://10.0.0.5/internal",
		"http://172.16.0.1/docker",
		"http://169.254.169.254/latest/meta-data/",
		"ftp://example.com/file",
		"file:///etc/passwd",
		// The machine itself under other names, and the tailnet (Piano dopo audit 1.1 / S1).
		"http://0.0.0.0:3030/",
		"http://[::]:3030/",
		"http://100.64.10.20:8000/",
		"http://[::ffff:0.0.0.0]/",
		"http://0.1.2.3/",
		"http://239.255.255.250:1900/",
	}

	for _, u := range blockedURLs {
		err := ValidateSafeURL(u)
		if err == nil {
			t.Errorf("expected URL %q to be blocked, but it passed", u)
		}
	}
}

func TestValidateSafeURL_Allowed(t *testing.T) {
	allowedURLs := []string{
		"https://kernel.org/",
		"https://en.wikipedia.org/wiki/Go_(programming_language)",
		"http://example.com",
	}

	for _, u := range allowedURLs {
		err := ValidateSafeURL(u)
		if err != nil {
			t.Errorf("expected URL %q to be allowed, got error: %v", u, err)
		}
	}
}

// The check at connection time looks at the address actually dialled, after the
// name was resolved again and after any redirect (Piano dopo audit 1.1 / S1).
func TestSafeDialControl(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:80", "[::1]:80", "0.0.0.0:80", "[::]:80", "100.64.1.1:80", "192.168.1.1:443", "10.0.0.5:8080", "[::ffff:127.0.0.1]:80"} {
		if err := safeDialControl("tcp4", addr, nil); err == nil {
			t.Errorf("%s: want refused, got allowed", addr)
		}
	}
	for _, addr := range []string{"93.184.216.34:443", "[2606:2800:220:1:248:1893:25c8:1946]:443"} {
		if err := safeDialControl("tcp", addr, nil); err != nil {
			t.Errorf("%s: want allowed, got %v", addr, err)
		}
	}
	if err := safeDialControl("tcp", "not-an-address", nil); err == nil {
		t.Errorf("an address that is not host:port must be refused")
	}
}

// The two readers that open pages themselves refuse a private address when they
// connect, not only when they check the name: their clients go straight to a
// local server here, past ValidateSafeURL.
func TestReaders_RefusePrivateAddressAtConnection(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.4\nsecret"))
	}))
	defer ts.Close()

	resp, err := NewDirectHTTPClient().client.Get(ts.URL)
	if err == nil {
		resp.Body.Close()
		t.Errorf("http reader: connection to %s allowed", ts.URL)
	} else if !strings.Contains(err.Error(), "SSRF") {
		t.Errorf("http reader: want SSRF error, got %v", err)
	}

	if _, err := NewPDFReader(PDFReaderConfig{}).download(context.Background(), ts.URL); err == nil {
		t.Errorf("pdf reader: download from %s allowed", ts.URL)
	} else if !strings.Contains(err.Error(), "SSRF") {
		t.Errorf("pdf reader: want SSRF error, got %v", err)
	}
}
