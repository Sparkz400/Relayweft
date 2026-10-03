package web

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Security model:
//
//   - The server listens on 127.0.0.1 only.
//   - Every run has a random token. The URL sy prints and opens carries it
//     once (/?t=TOKEN); the server answers with an HttpOnly, SameSite=Strict
//     cookie and redirects to a clean URL. Every other request needs that
//     cookie or the token in a header.
//   - The Host header must name this server (127.0.0.1/localhost/[::1] with
//     its port), which defeats DNS rebinding; a request with an Origin must
//     come from the same origin, and state-changing requests must be JSON,
//     which a cross-site form cannot send without a CORS preflight that is
//     never granted.

// TokenHeader carries the token for scripts and tests.
const TokenHeader = "X-Switchyard-Token"

func (s *Server) cookieName() string {
	_, port, _ := net.SplitHostPort(s.addr)
	return "sy_" + port
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

func (s *Server) tokenOK(v string) bool {
	return v != "" && subtle.ConstantTimeCompare([]byte(v), []byte(s.token)) == 1
}

// authed reports whether the request carries the token.
func (s *Server) authed(r *http.Request) bool {
	if s.tokenOK(r.Header.Get(TokenHeader)) {
		return true
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") && s.tokenOK(strings.TrimPrefix(a, "Bearer ")) {
		return true
	}
	if c, err := r.Cookie(s.cookieName()); err == nil && s.tokenOK(c.Value) {
		return true
	}
	return false
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

// guard wraps every handler with the host, origin and token checks and
// the security headers.
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
		// The one-time token URL: set the cookie, then drop the token from
		// the address bar.
		if r.URL.Path == "/" && r.URL.Query().Has("t") {
			if !s.tokenOK(r.URL.Query().Get("t")) {
				unauthorized(w)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: s.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if !s.authed(r) {
			unauthorized(w)
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
		next.ServeHTTP(w, r)
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>Switchyard</title>
<body style="font:15px system-ui;background:#0b0d10;color:#e6e8eb;display:grid;place-items:center;height:100vh;margin:0">
<div style="max-width:440px;text-align:center"><h2>Switchyard</h2><p style="color:#8a919c">This page needs the link that <code>sy web</code> printed in your terminal (it contains a one-time access token).</p></div>`))
}
