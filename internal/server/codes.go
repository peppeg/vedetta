package server

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"vedetta/config"
)

// Map R6b: the access code and the settings code can be set from
// Settings, not only in .env. A code set there is saved as a salted PBKDF2 hash in
// config.local, never as the code. A code in .env stays the owner's: Settings shows
// it and does not change it (it wins while it is there).

const (
	codeIterations = 100_000
	codeMinLen     = 6
)

// hashCode returns "pbkdf2-sha256$<iterations>$<salt>$<key>".
func hashCode(code string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, code, salt, codeIterations, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", codeIterations, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// checkCodeHash reports whether code matches a hash made by hashCode.
func checkCodeHash(code, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" || code == "" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, code, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// codeCache remembers the codes already checked against a hash, so a script or the
// settings page sending its code on every request costs one PBKDF2, not one each.
type codeCache struct {
	mu sync.Mutex
	ok map[string]string // sha256 of the code -> the hash it matched
}

func (c *codeCache) matches(code, stored string) bool {
	if code == "" || stored == "" {
		return false
	}
	sum := sha256.Sum256([]byte(code))
	k := string(sum[:])
	c.mu.Lock()
	hit := c.ok[k] == stored
	c.mu.Unlock()
	if hit {
		return true
	}
	if !checkCodeHash(code, stored) {
		return false
	}
	c.mu.Lock()
	if c.ok == nil || len(c.ok) > 64 {
		c.ok = map[string]string{}
	}
	c.ok[k] = stored
	c.mu.Unlock()
	return true
}

// codeState is one code as the server sees it: the .env one wins.
type codeState struct {
	plain, hash string
}

func (c codeState) set() bool { return c.plain != "" || c.hash != "" }

// source: "env", "settings" or "".
func (c codeState) source() string {
	switch {
	case c.plain != "":
		return "env"
	case c.hash != "":
		return "settings"
	}
	return ""
}

// key is the material the access cookies are signed with: a new code ends every
// device's access.
func (c codeState) key() string {
	if c.plain != "" {
		return "code:" + c.plain
	}
	return "hash:" + c.hash
}

func (h *Handlers) codes() (access, settings codeState) {
	h.cfgMu.RLock()
	defer h.cfgMu.RUnlock()
	return codeState{h.cfg.AccessCode, h.cfg.AccessCodeHash}, codeState{h.cfg.ConfigToken, h.cfg.ConfigTokenHash}
}

// codeMatches checks a presented code against one code state.
func (h *Handlers) codeMatches(p string, c codeState) bool {
	if p == "" {
		return false
	}
	if c.plain != "" {
		return codeEqual(p, c.plain)
	}
	return h.checked.matches(p, c.hash)
}

// AccessUpdate is POST /api/access: each field, when present, sets a code ("" takes
// a code set in Settings away). Behind the settings code, from Vedetta's page.
type AccessUpdate struct {
	AccessCode   *string `json:"access_code,omitempty"`
	SettingsCode *string `json:"settings_code,omitempty"`
	// CurrentCode is the access code of now: needed to change it when no settings code
	// protects the settings.
	CurrentCode string `json:"current_code,omitempty"`
}

// SetCodes answers POST /api/access.
func (h *Handlers) SetCodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.authorizeConfig(w, r) {
		return
	}
	if !sameOrigin(r) {
		writeJSONError(w, http.StatusForbidden, notOwnPage)
		return
	}
	var req AccessUpdate
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	access, settings := h.codes()
	// With a settings code, whoever is here has shown it. Without one, anybody who uses
	// Vedetta is here: the access code changes only for who knows it, and the first
	// settings code is set from Vedetta's machine, or a guest could close the owner out.
	if !settings.set() {
		if req.AccessCode != nil && access.source() == "settings" {
			ok, blocked := h.logins.try(clientKey(r), time.Now(), func() bool { return h.codeMatches(strings.TrimSpace(req.CurrentCode), access) })
			if blocked {
				writeJSONError(w, http.StatusTooManyRequests, tooManyCodes)
				return
			}
			if !ok {
				time.Sleep(loginDelay)
				writeJSONError(w, http.StatusForbidden, "current_code: the access code of now is needed")
				return
			}
		}
		if req.SettingsCode != nil && strings.TrimSpace(*req.SettingsCode) != "" && !h.localRequest(r) {
			writeJSONError(w, http.StatusForbidden, "local_only: the first settings code is set from Vedetta's machine, or in .env")
			return
		}
	}
	newHash := func(p *string, c codeState) (string, bool, string) {
		if p == nil {
			return c.hash, false, ""
		}
		if c.plain != "" {
			return "", false, "this code is set in .env: change it there"
		}
		v := strings.TrimSpace(*p)
		if v == "" {
			return "", true, ""
		}
		if len([]rune(v)) < codeMinLen {
			return "", false, fmt.Sprintf("a code needs at least %d characters", codeMinLen)
		}
		hsh, err := hashCode(v)
		if err != nil {
			return "", false, "the code could not be saved"
		}
		return hsh, true, ""
	}
	aHash, aChanged, aErr := newHash(req.AccessCode, access)
	sHash, sChanged, sErr := newHash(req.SettingsCode, settings)
	if aErr != "" || sErr != "" {
		writeJSONError(w, http.StatusBadRequest, strings.TrimSpace(aErr+" "+sErr))
		return
	}

	h.cfgMu.Lock()
	before := *h.cfg
	if aChanged {
		h.cfg.AccessCodeHash = aHash
	}
	if sChanged {
		h.cfg.ConfigTokenHash = sHash
	}
	if err := config.SaveLocalConfig(h.cfg.ConfigPath, h.cfg); err != nil {
		// a code that is gone at the next start is worse than one that is not set
		h.cfg.AccessCodeHash, h.cfg.ConfigTokenHash = before.AccessCodeHash, before.ConfigTokenHash
		h.cfgMu.Unlock()
		log.Printf("[config] the codes could not be saved: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "the code could not be saved")
		return
	}
	h.cfgMu.Unlock()
	h.capsMu.Lock()
	h.svcCache, h.capsCache = nil, nil
	h.capsMu.Unlock()

	// the device that set the access code stays in
	access, settings = h.codes()
	if aChanged && access.set() {
		expires := time.Now().Add(accessTTL)
		http.SetCookie(w, &http.Cookie{
			Name: accessCookie, Value: accessCookieValue(access.key(), expires), Path: "/",
			Expires: expires, MaxAge: int(accessTTL.Seconds()),
			HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: requestIsHTTPS(r),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"saved": true, "code_source": access.source(), "settings_source": settings.source(),
	})
}
