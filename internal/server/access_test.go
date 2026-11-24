package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vedetta/config"
	"vedetta/internal/agent"
)

func TestHostAllowed(t *testing.T) {
	allowed := []string{"vedetta.example.com", "*.casa.example.org"}
	for host, want := range map[string]bool{
		"127.0.0.1:3030": true, "[::1]:3030": true, "localhost:3030": true, "192.168.1.10:3030": true,
		"100.64.10.20:3030": true, "server:3030": true, "server.local": true, "nas.lan": true,
		"srv.home.arpa": true, "nas.home": true, "pc.localdomain": true, "server.fritz.box": false, "box.internal": true, "srv.tail1234.ts.net": true,
		"vedetta.example.com": true, "a.casa.example.org:443": true,
		"evil.com": false, "evil.com:3030": false, "localhost.evil.com": false, "example.com": false, "": false,
	} {
		if got := hostAllowed(host, allowed); got != want {
			t.Errorf("hostAllowed(%q) = %t, want %t", host, got, want)
		}
	}
}

func guardedServer(t *testing.T, mutate func(*config.Config)) http.Handler {
	t.Helper()
	cfg := &config.Config{Port: "3030", Bind: "0.0.0.0", ConfigPath: t.TempDir() + "/config.local"}
	if mutate != nil {
		mutate(cfg)
	}
	return NewServer(cfg, agent.NewEngine(&dummySearchProvider{}, nil, nil, nil)).Handler()
}

func call(h http.Handler, method, target string, body string, hdr map[string]string) *httptest.ResponseRecorder {
	var rd *bytes.Reader
	if body != "" {
		rd = bytes.NewReader([]byte(body))
	}
	var req *http.Request
	if rd != nil {
		req = httptest.NewRequest(method, target, rd)
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	req.Host = "127.0.0.1:3030"
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		if k == "RemoteAddr" {
			req.RemoteAddr = v
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Map R6: the protections that ask nothing of whoever uses Vedetta.
func TestGuard_InvisibleProtections(t *testing.T) {
	h := guardedServer(t, nil)
	if rec := call(h, "GET", "/api/sessions", "", map[string]string{"Host": "evil.com"}); rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("foreign host: %d, want 421", rec.Code)
	}
	if rec := call(h, "GET", "/api/sessions", "", nil); rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("own page: %d, ACAO %q (want 200 and none)", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
	if rec := call(h, "POST", "/api/search", `{"query":"x"}`, map[string]string{"Origin": "https://evil.example"}); rec.Code != http.StatusForbidden {
		t.Errorf("search from another site: %d, want 403", rec.Code)
	}
	if rec := call(h, "DELETE", "/api/sessions/abc", "", map[string]string{"Origin": "https://evil.example"}); rec.Code != http.StatusForbidden {
		t.Errorf("delete from another site: %d, want 403", rec.Code)
	}
	if rec := call(h, "OPTIONS", "/api/search", "", map[string]string{"Origin": "https://evil.example"}); rec.Code != http.StatusForbidden || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("preflight from another site: %d", rec.Code)
	}
	if rec := call(h, "GET", "/api/search?q=x", "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("search with GET: %d, want 405", rec.Code)
	}
	big := `{"contact_email":"` + strings.Repeat("a", maxBodyBytes+10) + `"}`
	if rec := call(h, "POST", "/api/config", big, nil); rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "too large") {
		t.Errorf("1 MB body: %d %.80s", rec.Code, rec.Body.String())
	}
	// a listed site may call
	h = guardedServer(t, func(c *config.Config) { c.CORSOrigins = []string{"https://cartografo.example"} })
	rec := call(h, "OPTIONS", "/api/search", "", map[string]string{"Origin": "https://cartografo.example"})
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "https://cartografo.example" {
		t.Errorf("listed site preflight: %d ACAO %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

// Map R6: with an access code, a device types it once and keeps a cookie for a year;
// scripts send it as a header; the page and the health check stay open.
func TestGuard_AccessCode(t *testing.T) {
	loginDelay = 0
	h := guardedServer(t, func(c *config.Config) { c.AccessCode = "casa-mia"; c.ConfigToken = "solo-peppe" })

	if rec := call(h, "GET", "/api/sessions", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no code: %d, want 401", rec.Code)
	}
	if rec := call(h, "GET", "/", "", nil); rec.Code != http.StatusOK {
		t.Errorf("the page: %d, want 200", rec.Code)
	}
	if rec := call(h, "GET", "/api/health", "", nil); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "llm_base_url") {
		t.Errorf("health without the code: %d %s", rec.Code, rec.Body.String())
	}
	var st map[string]interface{}
	json.Unmarshal(call(h, "GET", "/api/auth", "", nil).Body.Bytes(), &st)
	if st["required"] != true || st["ok"] != false || st["exposed"] != true {
		t.Errorf("auth state before: %v", st)
	}
	if rec := call(h, "POST", "/api/login", `{"code":"sbagliato"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong code: %d", rec.Code)
	}
	rec := call(h, "POST", "/api/login", `{"code":"casa-mia"}`, nil)
	cookies := rec.Result().Cookies()
	if rec.Code != http.StatusOK || len(cookies) != 1 {
		t.Fatalf("right code: %d, %d cookies", rec.Code, len(cookies))
	}
	c := cookies[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.MaxAge < 364*24*3600 {
		t.Errorf("cookie: HttpOnly %t SameSite %v MaxAge %d", c.HttpOnly, c.SameSite, c.MaxAge)
	}
	withCookie := map[string]string{"Cookie": c.Name + "=" + c.Value}
	if rec := call(h, "GET", "/api/sessions", "", withCookie); rec.Code != http.StatusOK {
		t.Errorf("with the cookie: %d", rec.Code)
	}
	json.Unmarshal(call(h, "GET", "/api/auth", "", withCookie).Body.Bytes(), &st)
	if st["ok"] != true || st["until"] == nil {
		t.Errorf("auth state after: %v", st)
	}
	for name, hdr := range map[string]map[string]string{
		"bearer access code": {"Authorization": "Bearer casa-mia"},
		"settings code":      {"X-Vedetta-Token": "solo-peppe"},
	} {
		if rec := call(h, "GET", "/api/sessions", "", hdr); rec.Code != http.StatusOK {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	if rec := call(h, "GET", "/api/sessions", "", map[string]string{"Authorization": "Bearer altro"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong bearer: %d", rec.Code)
	}
	out := call(h, "POST", "/api/logout", "", withCookie).Result().Cookies()
	if len(out) != 1 || out[0].MaxAge >= 0 {
		t.Errorf("logout must expire the cookie: %+v", out)
	}
	// a new code ends the old cookies; an expired cookie is refused
	if accessCookieValid("codice-nuovo", c.Value, time.Now()) {
		t.Error("a cookie of the old code is valid with the new one")
	}
	if accessCookieValid("casa-mia", accessCookieValue("casa-mia", time.Now().Add(-time.Minute)), time.Now()) {
		t.Error("an expired cookie is valid")
	}
	// wrong codes: at most 10 a minute, from everyone together
	for i := 0; i < 12; i++ {
		call(h, "POST", "/api/login", `{"code":"tentativo"}`, nil)
	}
	if rec := call(h, "POST", "/api/login", `{"code":"casa-mia"}`, nil); rec.Code != http.StatusTooManyRequests {
		t.Errorf("after many wrong codes: %d, want 429", rec.Code)
	}
}

func TestGuard_LocalWithoutCode(t *testing.T) {
	h := guardedServer(t, func(c *config.Config) { c.Bind = "127.0.0.1" })
	var st map[string]interface{}
	json.Unmarshal(call(h, "GET", "/api/auth", "", nil).Body.Bytes(), &st)
	if st["required"] != false || st["ok"] != true || st["exposed"] != false {
		t.Errorf("local, no code: %v", st)
	}
	if rec := call(h, "GET", "/api/health", "", nil); !strings.Contains(rec.Body.String(), "llm_base_url") {
		t.Errorf("health without a code shows the services: %s", rec.Body.String())
	}
}

// Map R8: in a container Vedetta listens on 0.0.0.0, but the compose publishes the
// port on 127.0.0.1 and says so: Settings must not claim other devices reach it.
func TestGuard_ContainerPublishedLocally(t *testing.T) {
	h := guardedServer(t, func(c *config.Config) { c.Bind = "0.0.0.0"; c.Publish = "127.0.0.1" })
	var st map[string]interface{}
	json.Unmarshal(call(h, "GET", "/api/auth", "", nil).Body.Bytes(), &st)
	if st["exposed"] != false {
		t.Errorf("published on 127.0.0.1: %v", st)
	}
	h = guardedServer(t, func(c *config.Config) { c.Bind = "0.0.0.0"; c.Publish = "0.0.0.0" })
	json.Unmarshal(call(h, "GET", "/api/auth", "", nil).Body.Bytes(), &st)
	if st["exposed"] != true {
		t.Errorf("published on 0.0.0.0: %v", st)
	}
}

// G2 (24/11/2026): 40 wrong codes together had 13 looked at instead of 10, and the
// limit was the clock's minute, which started again at second 0.
func TestLoginLimiter(t *testing.T) {
	var l loginLimiter
	var looked atomic.Int32
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.try("casa", now, func() bool { looked.Add(1); time.Sleep(time.Millisecond); return false })
		}()
	}
	wg.Wait()
	if looked.Load() != loginFailures {
		t.Errorf("codes looked at = %d, want %d", looked.Load(), loginFailures)
	}
	right := func() bool { return true }
	if ok, blocked := l.try("casa", now.Add(59*time.Second), right); ok || !blocked {
		t.Error("the right code is accepted before a minute has passed")
	}
	if ok, blocked := l.try("casa", now.Add(time.Minute), right); !ok || blocked {
		t.Error("the right code is refused after a minute")
	}
	for i := 0; i < 30; i++ {
		if ok, _ := l.try("casa", now.Add(time.Minute), right); !ok {
			t.Fatal("right codes must not count")
		}
	}
}
