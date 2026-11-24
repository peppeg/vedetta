package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"vedetta/config"
)

// External audit of 24/11/2026 (docs/VERIFICA_AUDIT_ESTERNO_2026-11-24.md).

// SEC-01: a "${NAME}" key goes only where it belongs.
func TestKeyRef_GoesOnlyWhereItBelongs(t *testing.T) {
	t.Setenv("CASA_API_KEY", "valore")
	t.Setenv("VEDETTA_CONFIG_TOKEN_KEY", "valore")
	got := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer srv.Close()
	h := guardedServer(t, func(c *config.Config) {
		c.LLMEndpoints = []config.LLMEndpoint{{ID: "a", BaseURL: "https://modello.test/v1", Model: "m", APIKey: "x", KeyEnv: "USATA_API_KEY"}}
	})
	ask := func(key string) int {
		got = ""
		return call(h, "POST", "/api/services/models", `{"base_url":"`+srv.URL+`","api_key":"`+key+`"}`, nil).Code
	}
	for _, name := range []string{"PATH", "HOME", "LLM_API_KEY", "BRAVE_API_KEY", "VEDETTA_CONFIG_TOKEN_KEY", "VEDETTA_ACCESS_CODE", "OPENROUTER_API_KEY", "USATA_API_KEY", "casa_api_key"} {
		if code := ask("${" + name + "}"); code != http.StatusBadRequest || got != "" {
			t.Errorf("%s: status %d, the other server received %q", name, code, got)
		}
	}
	if code := ask("${CASA_API_KEY}"); code != http.StatusOK || got != "Bearer valore" {
		t.Errorf("a model's key of .env must still work: %d %q", code, got)
	}
	if code := ask("chiave-scritta"); code != http.StatusOK || got != "Bearer chiave-scritta" {
		t.Errorf("a key written out: %d %q", code, got)
	}

	for _, body := range []string{
		`{"llm_endpoints":[{"id":"a","base_url":"https://ladro.test/v1","model":"m","api_key":"${USATA_API_KEY}"}]}`,
		`{"llm_endpoints":[{"id":"a","base_url":"http://modello.test/v1","model":"m","api_key":"${USATA_API_KEY}"}]}`,
		`{"llm_base_url":"https://ladro.test/v1","llm_api_key":"${BRAVE_API_KEY}"}`,
		`{"llm_base_url":"https://ladro.test/v1","llm_api_key":"${USATA_API_KEY}"}`,
	} {
		if rec := call(h, "POST", "/api/config", body, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestKeyRefAllowed(t *testing.T) {
	eps := []config.LLMEndpoint{{ID: "a", BaseURL: "https://modello.test/v1", KeyEnv: "USATA_API_KEY"}}
	for _, c := range []struct {
		name, base string
		want       bool
	}{
		{"OPENROUTER_API_KEY", "https://openrouter.ai/api/v1", true},
		{"OPENROUTER_API_KEY", "https://openrouter.ai.ladro.test/v1", false},
		{"KILO_API_KEY", "https://api.kilo.ai/api/gateway", true},
		{"USATA_API_KEY", "https://modello.test/altro", true},
		{"USATA_API_KEY", "https://altro.test/v1", false},
		{"NUOVA_TOKEN", "https://altro.test/v1", true},
		{"NUOVA", "https://altro.test/v1", false},
	} {
		if got := keyRefAllowed(c.name, c.base, eps); got != c.want {
			t.Errorf("%s to %s: %t, want %t", c.name, c.base, got, c.want)
		}
	}
	if sameServiceHost("https://host.test/v1", "http://host.test/v1") {
		t.Error("a saved key must not go out in the clear")
	}
	if !sameServiceHost("http://host.test:1/v1", "http://host.test:2/v1") || !sameServiceHost("http://host.test", "https://host.test") {
		t.Error("another port, or the same host encrypted, keeps the key")
	}
}

// SEC-03: who uses Vedetta with the access code cannot close the owner out.
func TestSetCodes_GuestCannotCloseTheOwnerOut(t *testing.T) {
	loginDelay = 0
	path := t.TempDir() + "/config.local"
	h := guardedServer(t, func(c *config.Config) { c.ConfigPath = path })
	if rec := call(h, "POST", "/api/access", `{"access_code":"codice-di-casa"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("the first access code: %d %s", rec.Code, rec.Body.String())
	}
	guest := map[string]string{"Authorization": "Bearer codice-di-casa"}
	for body, want := range map[string]int{
		`{"settings_code":"codice-dell-ospite"}`:                                 http.StatusForbidden,
		`{"access_code":"codice-dell-ospite"}`:                                   http.StatusForbidden,
		`{"access_code":""}`:                                                     http.StatusForbidden,
		`{"access_code":"codice-dell-ospite","current_code":"sbagliato"}`:        http.StatusForbidden,
		`{"settings_code":"codice-dell-ospite","current_code":"codice-di-casa"}`: http.StatusForbidden,
	} {
		if rec := call(h, "POST", "/api/access", body, guest); rec.Code != want {
			t.Errorf("%s: %d %s, want %d", body, rec.Code, rec.Body.String(), want)
		}
	}
	// a proxy on the same machine is not the owner at the machine
	if rec := call(h, "POST", "/api/access", `{"settings_code":"codice-dell-ospite"}`, map[string]string{"Authorization": "Bearer codice-di-casa", "RemoteAddr": "127.0.0.1:5", "X-Forwarded-For": "8.8.8.8"}); rec.Code != http.StatusForbidden {
		t.Errorf("through a proxy: %d", rec.Code)
	}
	if rec := call(h, "POST", "/api/access", `{"access_code":"codice-di-casa-due","current_code":"codice-di-casa"}`, guest); rec.Code != http.StatusOK {
		t.Errorf("who knows the code changes it: %d %s", rec.Code, rec.Body.String())
	}

	// from Vedetta's machine the first settings code is set; then it is the one that counts
	here := map[string]string{"Authorization": "Bearer codice-di-casa-due", "RemoteAddr": "127.0.0.1:5"}
	if st := authState(t, h, guest); st["settings_settable"] != false {
		t.Errorf("from another device: %v", st)
	}
	if st := authState(t, h, here); st["settings_settable"] != true {
		t.Errorf("from the machine: %v", st)
	}
	if rec := call(h, "POST", "/api/access", `{"settings_code":"codice-impostazioni"}`, here); rec.Code != http.StatusOK {
		t.Fatalf("from the machine: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(h, "POST", "/api/access", `{"access_code":"codice-terzo"}`, here); rec.Code != http.StatusUnauthorized {
		t.Errorf("with a settings code, the access code alone changes nothing: %d", rec.Code)
	}
	if rec := call(h, "POST", "/api/access", `{"access_code":"codice-terzo"}`, map[string]string{"X-Vedetta-Token": "codice-impostazioni"}); rec.Code != http.StatusOK {
		t.Errorf("the owner, from anywhere, with the settings code: %d %s", rec.Code, rec.Body.String())
	}
}

// Published only on this machine, every request comes from it, also in a container
// where it arrives from the bridge.
func TestSetCodes_FirstSettingsCodeWhenPublishedHere(t *testing.T) {
	h := guardedServer(t, func(c *config.Config) { c.Publish = "127.0.0.1" })
	if rec := call(h, "POST", "/api/access", `{"settings_code":"codice-impostazioni"}`, map[string]string{"RemoteAddr": "172.18.0.1:5"}); rec.Code != http.StatusOK {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
}

// A code that cannot be saved is not set: it would be gone at the next start.
func TestSetCodes_NotSavedIsNotSet(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/file", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h := guardedServer(t, func(c *config.Config) { c.ConfigPath = dir + "/file/config.local" })
	if rec := call(h, "POST", "/api/access", `{"access_code":"codice-di-casa"}`, nil); rec.Code != http.StatusInternalServerError {
		t.Errorf("status %d", rec.Code)
	}
	if st := authState(t, h, nil); st["required"] != false {
		t.Errorf("the code is in force although it was not saved: %v", st)
	}
}

// SEC-04: the wrong codes of one address close that address, not the others.
func TestWrongCodes_CloseOnlyWhereTheyComeFrom(t *testing.T) {
	loginDelay = 0
	h := guardedServer(t, func(c *config.Config) { c.AccessCode = "codice-di-casa"; c.ConfigToken = "codice-impostazioni" })
	stranger := map[string]string{"RemoteAddr": "192.168.1.50:1000"}
	in := call(h, "POST", "/api/login", `{"code":"codice-di-casa"}`, stranger).Result().Cookies()[0]
	for i := 0; i < 10; i++ {
		if rec := call(h, "POST", "/api/login", `{"code":"sbagliato"}`, stranger); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong code %d: %d", i+1, rec.Code)
		}
	}
	for name, hdr := range map[string]map[string]string{
		"settings":   {"RemoteAddr": "192.168.1.50:1001", "X-Vedetta-Token": "codice-impostazioni"},
		"script":     {"RemoteAddr": "192.168.1.50:1002", "Authorization": "Bearer codice-di-casa"},
		"wrong code": {"RemoteAddr": "192.168.1.50:1003", "Authorization": "Bearer sbagliato"},
	} {
		if rec := call(h, "GET", "/api/config", "", hdr); rec.Code != http.StatusTooManyRequests {
			t.Errorf("%s, from there: %d, want 429 (401 makes the page forget a right code)", name, rec.Code)
		}
	}
	inside := map[string]string{"RemoteAddr": "192.168.1.50:1004", "Cookie": in.Name + "=" + in.Value}
	if rec := call(h, "GET", "/api/sessions", "", inside); rec.Code != http.StatusOK {
		t.Errorf("a device already in: %d", rec.Code)
	}
	inside["X-Vedetta-Token"] = "codice-impostazioni"
	if rec := call(h, "GET", "/api/config", "", inside); rec.Code != http.StatusTooManyRequests {
		t.Errorf("the settings from a device already in, after the wrong codes: %d, want 429", rec.Code)
	}
	owner := map[string]string{"RemoteAddr": "192.168.1.20:1", "X-Vedetta-Token": "codice-impostazioni"}
	if rec := call(h, "GET", "/api/config", "", owner); rec.Code != http.StatusOK {
		t.Errorf("the owner, from another device: %d", rec.Code)
	}
	if rec := call(h, "POST", "/api/login", `{"code":"codice-di-casa"}`, map[string]string{"RemoteAddr": "192.168.1.21:1"}); rec.Code != http.StatusOK {
		t.Errorf("a login from another device: %d", rec.Code)
	}
}

func TestLoginLimiter_FromEveryoneTogether(t *testing.T) {
	var l loginLimiter
	now := time.Now()
	wrong := func() bool { return false }
	looked := 0
	for i := 0; i < loginFailAll*2; i++ {
		if _, blocked := l.try(string(rune('a'+i)), now, wrong); !blocked {
			looked++
		}
	}
	if looked != loginFailAll {
		t.Errorf("codes looked at = %d, want %d", looked, loginFailAll)
	}
}

func TestClientKey(t *testing.T) {
	for addr, want := range map[string]string{
		"192.168.1.5:40": "192.168.1.5", "[2001:db8:1:2:3:4:5:6]:40": "2001:db8:1:2::", "[2001:db8:1:2:9::1]:1": "2001:db8:1:2::",
		"[::ffff:10.0.0.1]:3": "10.0.0.1", "@": "@",
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = addr
		r.Header.Set("X-Forwarded-For", "1.1.1.1")
		if got := clientKey(r); got != want {
			t.Errorf("%s: %q, want %q", addr, got, want)
		}
	}
}

// SEC-02: Jina Reader, sent here by a page, opens the address as a page.
func TestGuard_DataIsNotAPage(t *testing.T) {
	h := guardedServer(t, nil)
	chrome := map[string]string{"Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,*/*;q=0.8", "Host": "vedetta:3030"}
	for _, p := range []string{"/api/sessions", "/api/sessions/x", "/api/config", "/api/health", "/api/auth", "/api/services"} {
		rec := call(h, "GET", p, "", chrome)
		if rec.Code != http.StatusNotAcceptable || strings.Contains(rec.Body.String(), "{") {
			t.Errorf("%s opened as a page: %d %q", p, rec.Code, rec.Body.String())
		}
	}
	if rec := call(h, "GET", "/", "", chrome); rec.Code != http.StatusOK {
		t.Errorf("Vedetta's own page: %d", rec.Code)
	}
	for _, accept := range []string{"", "*/*", "application/json"} {
		if rec := call(h, "GET", "/api/auth", "", map[string]string{"Accept": accept}); rec.Code != http.StatusOK {
			t.Errorf("Accept %q: %d", accept, rec.Code)
		}
	}
}

func authState(t *testing.T, h http.Handler, hdr map[string]string) map[string]interface{} {
	t.Helper()
	var st map[string]interface{}
	if err := json.Unmarshal(call(h, "GET", "/api/auth", "", hdr).Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}
