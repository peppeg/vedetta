package server

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"vedetta/config"
)

func TestHashCode(t *testing.T) {
	a, err := hashCode("codice-di-casa")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := hashCode("codice-di-casa")
	if a == b || strings.Contains(a, "codice") {
		t.Errorf("hashes must be salted and not hold the code: %q %q", a, b)
	}
	if !checkCodeHash("codice-di-casa", a) || checkCodeHash("codice-di-cas", a) || checkCodeHash("", a) || checkCodeHash("x", "garbage") {
		t.Error("checkCodeHash gives a wrong answer")
	}
}

// Map R6b: the access code and the settings code set from Settings, saved as hashes,
// working at once; a code in .env is not changed from there.
func TestSetCodes_FromSettings(t *testing.T) {
	loginDelay = 0
	path := t.TempDir() + "/config.local"
	h := guardedServer(t, func(c *config.Config) { c.ConfigPath = path })
	auth := func(hdr map[string]string) map[string]interface{} {
		var st map[string]interface{}
		json.Unmarshal(call(h, "GET", "/api/auth", "", hdr).Body.Bytes(), &st)
		return st
	}

	if rec := call(h, "POST", "/api/access", `{"access_code":"corto"}`, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("a 5-character code: %d, want 400", rec.Code)
	}
	rec := call(h, "POST", "/api/access", `{"access_code":"codice-di-casa"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("set the access code: %d %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("the device that sets the code must stay in: %d cookies", len(cookies))
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "codice-di-casa") || !strings.Contains(string(raw), `"access_code_hash": "pbkdf2-sha256$`) {
		t.Fatalf("config.local must hold the hash, not the code: %s", raw)
	}
	withCookie := map[string]string{"Cookie": cookies[0].Name + "=" + cookies[0].Value}
	if st := auth(withCookie); st["required"] != true || st["ok"] != true || st["code_source"] != "settings" {
		t.Errorf("auth state with the cookie: %v", st)
	}
	if call(h, "GET", "/api/sessions", "", nil).Code != http.StatusUnauthorized {
		t.Error("without the cookie the API must ask the code")
	}
	for i := 0; i < 3; i++ { // the second time the cache answers
		if rec := call(h, "GET", "/api/sessions", "", map[string]string{"Authorization": "Bearer codice-di-casa"}); rec.Code != http.StatusOK {
			t.Fatalf("bearer with the code set in Settings: %d", rec.Code)
		}
	}
	if rec := call(h, "POST", "/api/login", `{"code":"codice-di-casa"}`, nil); rec.Code != http.StatusOK {
		t.Errorf("login with the code set in Settings: %d", rec.Code)
	}

	// a settings code, the first from Vedetta's machine: from now on Settings needs it
	here := map[string]string{"Cookie": withCookie["Cookie"], "RemoteAddr": "127.0.0.1:5"}
	if rec := call(h, "POST", "/api/access", `{"settings_code":"solo-per-me"}`, here); rec.Code != http.StatusOK {
		t.Fatalf("set the settings code: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(h, "GET", "/api/config", "", withCookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("settings without the settings code: %d", rec.Code)
	}
	withBoth := map[string]string{"Cookie": withCookie["Cookie"], "X-Vedetta-Token": "solo-per-me"}
	if rec := call(h, "GET", "/api/config", "", withBoth); rec.Code != http.StatusOK {
		t.Errorf("settings with the settings code: %d", rec.Code)
	}
	// taking the access code away: open again
	if rec := call(h, "POST", "/api/access", `{"access_code":""}`, withBoth); rec.Code != http.StatusOK {
		t.Fatalf("remove the access code: %d", rec.Code)
	}
	if st := auth(nil); st["required"] != false || st["code_source"] != "" {
		t.Errorf("after removing: %v", st)
	}

	// a code in .env: shown, not changed from Settings
	h = guardedServer(t, func(c *config.Config) { c.AccessCode = "dal-file-env"; c.ConfigPath = t.TempDir() + "/c.local" })
	env := map[string]string{"Authorization": "Bearer dal-file-env"}
	if rec := call(h, "POST", "/api/access", `{"access_code":"un-altro-codice"}`, env); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ".env") {
		t.Errorf("changing a .env code from Settings: %d %s", rec.Code, rec.Body.String())
	}
	var st map[string]interface{}
	json.Unmarshal(call(h, "GET", "/api/auth", "", env).Body.Bytes(), &st)
	if st["code_source"] != "env" {
		t.Errorf("code source = %v, want env", st["code_source"])
	}
}
