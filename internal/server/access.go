package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"vedetta/config"
)

// Map R6 (docs/SPEC_R6_SICUREZZA.md): protections that ask nothing of whoever uses
// Vedetta at home, and an optional access code. No login, so Vedetta starts without
// a code and says so; with the code, each device types it once and is remembered
// for a year.

const (
	accessCookie  = "vedetta_access"
	accessTTL     = 365 * 24 * time.Hour
	maxBodyBytes  = 1 << 20 // a JSON request body, at most
	loginFailures = 10      // failed codes accepted per minute from one address
	loginFailAll  = 200     // and from everyone together
	tooManyCodes  = "too many wrong codes: wait a minute"
)

// hostAllowed reports whether a request's Host names this Vedetta: an IP address,
// localhost, a home name (no dot, or a local suffix) or a name in the list. Anything
// else may be a site that made its own name point at a home address (DNS rebinding).
func hostAllowed(hostport string, allowed []string) bool {
	h := hostport
	if hh, _, err := net.SplitHostPort(hostport); err == nil {
		h = hh
	}
	h = strings.ToLower(strings.TrimSuffix(strings.Trim(h, "[]"), "."))
	if h == "" {
		return false
	}
	if net.ParseIP(h) != nil || h == "localhost" || !strings.Contains(h, ".") {
		return true
	}
	for _, suffix := range []string{".localhost", ".local", ".lan", ".home", ".localdomain", ".home.arpa", ".internal", ".ts.net"} {
		if strings.HasSuffix(h, suffix) {
			return true
		}
	}
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == h || (strings.HasPrefix(a, "*.") && strings.HasSuffix(h, a[1:])) {
			return true
		}
	}
	return false
}

// originListed reports whether a web origin is in the configured list.
func originListed(origin string, list []string) bool {
	origin = strings.TrimRight(strings.ToLower(origin), "/")
	for _, o := range list {
		if strings.ToLower(o) == origin {
			return true
		}
	}
	return false
}

// accessKey is the key of the cookies of one access code (codeState.key): changing
// the code ends every device's access.
func accessKey(material string) []byte {
	k := sha256.Sum256([]byte("vedetta-access-v1|" + material))
	return k[:]
}

// accessCookieValue is "<expiry unix>.<hmac>" for the code's key material.
func accessCookieValue(material string, expires time.Time) string {
	exp := strconv.FormatInt(expires.Unix(), 10)
	mac := hmac.New(sha256.New, accessKey(material))
	mac.Write([]byte("access|" + exp))
	return exp + "." + hex.EncodeToString(mac.Sum(nil))
}

// accessCookieValid checks a cookie value against the code's key material and the clock.
func accessCookieValid(material, value string, now time.Time) bool {
	exp, _, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	unix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || now.Unix() >= unix {
		return false
	}
	want := accessCookieValue(material, time.Unix(unix, 0))
	return subtle.ConstantTimeCompare([]byte(value), []byte(want)) == 1
}

func codeEqual(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// presentedCode is a code sent by a script or by the settings page: a Bearer header
// or the settings header.
func presentedCode(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Vedetta-Token")); v != "" {
		return v
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	return ""
}

// accessOK reports whether the request may use Vedetta: no access code set, a valid
// cookie, or the access code or the settings code sent by a script. Wrong codes
// count with the login's: past the limit, none from there is looked at for a minute.
func (h *Handlers) accessOK(r *http.Request) bool {
	ok, _ := h.accessCheck(r)
	return ok
}

func (h *Handlers) accessCheck(r *http.Request) (ok, blocked bool) {
	access, settings := h.codes()
	if !access.set() {
		return true, false
	}
	if c, err := r.Cookie(accessCookie); err == nil && accessCookieValid(access.key(), c.Value, time.Now()) {
		return true, false
	}
	p := presentedCode(r)
	if p == "" {
		return false, false
	}
	return h.logins.try(clientKey(r), time.Now(), func() bool { return h.codeMatches(p, access) || h.codeMatches(p, settings) })
}

// clientKey is where a request comes from, for the count of the wrong codes: the
// address, or the /64 network of an IPv6 one. What a proxy says is not believed: it
// is written by whoever asks.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if ip.To4() == nil {
		ip = ip.Mask(net.CIDRMask(64, 128))
	}
	return ip.String()
}

// localRequest reports whether the request comes from the machine Vedetta runs on: it
// listens, or is published, only there, or the request arrives from it and no proxy
// brought it.
func (h *Handlers) localRequest(r *http.Request) bool {
	h.cfgMu.RLock()
	reach := h.cfg.ReachAddr()
	h.cfgMu.RUnlock()
	for _, k := range []string{"X-Forwarded-For", "X-Real-Ip", "Forwarded", "X-Forwarded-Host"} {
		if r.Header.Get(k) != "" {
			return false
		}
	}
	if !config.ListensBeyondMachine(reach) {
		return true
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// openedAsPage: a browser opens the address as a page, it is not Vedetta's page or a
// script asking for data. Jina Reader opens pages that way, from inside the network,
// and a page it reads may send it to Vedetta: the searches would come back as the
// text of that page (external audit 24/11/2026, SEC-02).
func openedAsPage(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/api/") && strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")
}

// publicPath: what loads without the code: the page itself (it asks for the code),
// the health check, and the login.
func publicPath(p string) bool {
	return !strings.HasPrefix(p, "/api/") || p == "/api/health" || p == "/api/auth" || p == "/api/login" || p == "/api/logout"
}

// guard wraps every request: host, other sites, body size, access code.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handlers.cfgMu.RLock()
		allowedHosts, corsOrigins := s.cfg.AllowedHosts, s.cfg.CORSOrigins
		s.handlers.cfgMu.RUnlock()
		if !hostAllowed(r.Host, allowedHosts) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusMisdirectedRequest)
			fmt.Fprintf(w, "Vedetta: this address (%s) is not accepted. If it is yours, add it to VEDETTA_ALLOWED_HOSTS.\n", r.Host)
			return
		}
		if openedAsPage(r) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusNotAcceptable)
			fmt.Fprintln(w, "Vedetta: this address answers Vedetta's page and scripts, it is not a page to open.")
			return
		}
		origin := r.Header.Get("Origin")
		listed := origin != "" && originListed(origin, corsOrigins)
		if listed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Vedetta-Token")
			w.Header().Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			if listed {
				w.WriteHeader(http.StatusNoContent)
			} else {
				w.WriteHeader(http.StatusForbidden)
			}
			return
		}
		// A request that changes something (a search, a delete, a save) comes only
		// from Vedetta's own page, a listed site, or outside a browser.
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !listed && !sameOrigin(r) {
			writeJSONError(w, http.StatusForbidden, notOwnPage)
			return
		}
		if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		if !publicPath(r.URL.Path) {
			ok, blocked := s.handlers.accessCheck(r)
			if blocked {
				writeJSONError(w, http.StatusTooManyRequests, tooManyCodes)
				return
			}
			if !ok {
				writeJSONError(w, http.StatusUnauthorized, "access code required")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// loginDelay slows down each wrong code a little.
var loginDelay = 400 * time.Millisecond

// loginLimiter remembers the failed codes of the last minute and where they came from:
// with loginFailures of them from one address, none from there is looked at, and the
// others go on (external audit 24/11/2026, SEC-04: ten wrong codes of anybody closed
// the settings to everybody).
type loginLimiter struct {
	mu    sync.Mutex
	fails []loginFail // oldest first
}

type loginFail struct {
	at   time.Time
	from string
}

// try looks at a code unless there were too many wrong ones. Looking and counting are
// one step: requests arriving together cannot pass the limit (G2, 24/11/2026).
func (l *loginLimiter) try(from string, now time.Time, match func() bool) (ok, blocked bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(l.fails) > 0 && now.Sub(l.fails[0].at) >= time.Minute {
		l.fails = l.fails[1:]
	}
	n := 0
	for _, f := range l.fails {
		if f.from == from {
			n++
		}
	}
	if n >= loginFailures || len(l.fails) >= loginFailAll {
		return false, true
	}
	if match() {
		return true, false
	}
	l.fails = append(l.fails, loginFail{now, from})
	return false, false
}

// AuthState answers GET /api/auth: whether a code is needed and whether this device
// is in, and whether Vedetta listens beyond this machine (for the Settings line).
func (h *Handlers) AuthState(w http.ResponseWriter, r *http.Request) {
	access, settings := h.codes()
	h.cfgMu.RLock()
	reach := h.cfg.ReachAddr()
	h.cfgMu.RUnlock()
	required := access.set()
	state := map[string]interface{}{"required": required, "ok": h.accessOK(r), "exposed": config.ListensBeyondMachine(reach),
		"code_source": access.source(), "settings_source": settings.source(),
		// where the first settings code may be set from (SetCodes)
		"settings_settable": settings.set() || h.localRequest(r)}
	if c, err := r.Cookie(accessCookie); err == nil && required && h.accessOK(r) {
		if exp, _, ok := strings.Cut(c.Value, "."); ok {
			if unix, err := strconv.ParseInt(exp, 10, 64); err == nil {
				state["until"] = time.Unix(unix, 0).UTC().Format(time.RFC3339)
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}

// Login answers POST /api/login {"code": "..."}: with the right code, a cookie this
// device keeps for a year, unreadable by scripts and not sent by other sites.
func (h *Handlers) Login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	access, _ := h.codes()
	if !access.set() {
		writeJSONError(w, http.StatusConflict, "no access code is set")
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	now := time.Now()
	ok, blocked := h.logins.try(clientKey(r), now, func() bool { return h.codeMatches(strings.TrimSpace(req.Code), access) })
	if blocked {
		writeJSONError(w, http.StatusTooManyRequests, tooManyCodes)
		return
	}
	if !ok {
		time.Sleep(loginDelay)
		writeJSONError(w, http.StatusUnauthorized, "wrong access code")
		return
	}
	expires := now.Add(accessTTL)
	http.SetCookie(w, &http.Cookie{
		Name: accessCookie, Value: accessCookieValue(access.key(), expires), Path: "/",
		Expires: expires, MaxAge: int(accessTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: requestIsHTTPS(r),
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "until": expires.UTC().Format(time.RFC3339)})
}

// Logout answers POST /api/logout: this device forgets the code.
func (h *Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: accessCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: requestIsHTTPS(r),
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
