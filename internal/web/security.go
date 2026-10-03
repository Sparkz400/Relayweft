package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Security model (no cookies, no secret in any URL that reaches a server
// or another process for longer than a moment):
//
//   - The server listens on 127.0.0.1 only.
//   - sy prints (and opens) http://127.0.0.1:P/#b=<BOOTSTRAP>. The fragment
//     is never sent to the server or in a Referer. A bootstrap is single
//     use and expires after bootstrapTTL; sy prints a fresh one on Enter.
//   - The page reads the fragment, removes it from the address bar and
//     trades it (POST /api/session) for a random session secret, kept in
//     the tab's sessionStorage. Every /api request must carry it in
//     X-Switchyard-Session (or Authorization: Bearer); the event stream,
//     which cannot set headers, takes it as ?s=.
//   - A bootstrap presented again after it was used is refused and sy warns
//     on its terminal: someone else may have read the link.
//   - Static files (the page, js, css) hold no secrets and need no session.
//   - The Host header must name this server (127.0.0.1/localhost/[::1]
//     with its port), which defeats DNS rebinding; a request with an Origin
//     must come from the same origin, and state-changing requests must be
//     JSON, which a cross-site form cannot send without a CORS preflight
//     that is never granted.
//   - Secrets are looked up by their SHA-256, so comparing them does not
//     leak their bytes through timing.

// SessionHeader carries the session secret.
const SessionHeader = "X-Switchyard-Session"

const (
	bootstrapTTL = 2 * time.Minute
	maxSessions  = 32 // pages (tabs) with a live session; the oldest is dropped
	maxUsed      = 256
)

// auth holds the bootstraps and sessions.
type auth struct {
	mu         sync.Mutex
	bootstraps map[string]time.Time // hash -> expiry (unused ones)
	used       map[string]bool      // hashes of bootstraps already traded
	sessions   map[string]time.Time // hash -> created
	now        func() time.Time
}

func newAuth() *auth {
	return &auth{bootstraps: map[string]time.Time{}, used: map[string]bool{}, sessions: map[string]time.Time{}, now: time.Now}
}

func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func randomSecret() string {
	b := make([]byte, 32) // 256 bits
	if _, err := rand.Read(b); err != nil {
		panic("web: no randomness: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// newBootstrap mints a single-use bootstrap.
func (a *auth) newBootstrap() string {
	b := randomSecret()
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for h, exp := range a.bootstraps {
		if now.After(exp) {
			delete(a.bootstraps, h)
		}
	}
	a.bootstraps[hashSecret(b)] = now.Add(bootstrapTTL)
	return b
}

var (
	errBootstrapUsed    = errors.New("this link was already used")
	errBootstrapExpired = errors.New("this link has expired")
	errBootstrapUnknown = errors.New("this link is not valid for this sy (was sy restarted?)")
)

// trade spends a bootstrap and returns a new session secret.
func (a *auth) trade(bootstrap string) (string, error) {
	if bootstrap == "" {
		return "", errBootstrapUnknown
	}
	h := hashSecret(bootstrap)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.used[h] {
		return "", errBootstrapUsed
	}
	exp, ok := a.bootstraps[h]
	if !ok {
		return "", errBootstrapUnknown
	}
	delete(a.bootstraps, h)
	if a.now().After(exp) {
		return "", errBootstrapExpired
	}
	if len(a.used) >= maxUsed {
		a.used = map[string]bool{}
	}
	a.used[h] = true
	sess := randomSecret()
	if len(a.sessions) >= maxSessions {
		oldest, at := "", time.Time{}
		for k, t := range a.sessions {
			if oldest == "" || t.Before(at) {
				oldest, at = k, t
			}
		}
		delete(a.sessions, oldest)
	}
	a.sessions[hashSecret(sess)] = a.now()
	return sess, nil
}

func (a *auth) sessionOK(s string) bool {
	if s == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.sessions[hashSecret(s)]
	return ok
}

// hostOK reports whether host (a Host header or an Origin's host) is this
// server.
func (s *Server) hostOK(host string) bool {
	_, port, err := net.SplitHostPort(s.addr)
	if err != nil || host == "" {
		return false
	}
	h, p, err := net.SplitHostPort(host)
	if err != nil || p != port {
		return false
	}
	switch strings.ToLower(h) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// sessionOf returns the session secret a request carries.
func sessionOf(r *http.Request) string {
	if v := r.Header.Get(SessionHeader); v != "" {
		return v
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimPrefix(a, "Bearer ")
	}
	if r.URL.Path == "/api/events" {
		return r.URL.Query().Get("s")
	}
	return ""
}

// originOK checks Origin (and Sec-Fetch-Site) for cross-site requests.
func (s *Server) originOK(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Scheme != "http" || !s.hostOK(u.Host) {
			return false
		}
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	return false
}

// guard wraps every handler with the host, origin, session and content
// type checks and the security headers.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		if !s.hostOK(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if !s.originOK(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			ct := r.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				http.Error(w, "want Content-Type: application/json", http.StatusUnsupportedMediaType)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
		}
		// The page and its assets are public; the API needs a session,
		// except the call that trades a bootstrap for one.
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/session" && !s.auth.sessionOK(sessionOf(r)) {
			fail(w, http.StatusUnauthorized, errors.New("no session - open the link printed by sy (press Enter in its terminal for a new one)"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleSession trades a bootstrap (from the link's #b= fragment) for a
// session.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Bootstrap string `json:"bootstrap"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	sess, err := s.auth.trade(req.Bootstrap)
	if err != nil {
		if errors.Is(err, errBootstrapUsed) && s.opt.Warn != nil {
			s.opt.Warn("warning: a sy web link was used twice. If you did not open it twice, someone else on this machine may have read it - restart sy web.")
		}
		fail(w, http.StatusUnauthorized, err)
		return
	}
	writeJSON(w, map[string]string{"session": sess})
}

// reTaskID matches the task ids sy writes (no path separators).
var reTaskID = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func validTaskID(id string) bool {
	return reTaskID.MatchString(id) && !strings.Contains(id, "..")
}
