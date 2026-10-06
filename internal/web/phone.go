package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"rsc.io/qr"
)

// Phone access (rw web --phone): approve from your phone, away from the PC.
//
//   - A second listener on a Tailscale (100.64.0.0/10, fd7a:115c:a1e0::/48)
//     or private LAN address; never a public one. The loopback listener
//     stays as it is.
//   - The same single-use bootstrap links, shown as a QR code: in the
//     terminal, or from the desktop page (GET /api/phone, local sessions
//     only). A bootstrap traded on the phone listener gives a phone
//     session; a desktop session is refused there.
//   - A phone session lasts until rw web stops, and the page keeps it in
//     localStorage (the phone's tab may be reloaded or evicted while you
//     are away).
//   - A phone session can look, answer approvals and stop things, but
//     never give agents new instructions: no tasks, no plan edits (an
//     approval approves the plan as proposed), no review feedback, no
//     settings (verify commands run in a shell). If the link or the
//     session leaks on an untrusted Wi-Fi (plain HTTP), the worst is a
//     yes or no to work you queued yourself. Tailscale encrypts the way.

type ctxKey int

const (
	ctxPhoneConn    ctxKey = iota // the request came in on the phone listener
	ctxPhoneSession               // the request carries a phone session
)

func phoneConn(r *http.Request) bool    { v, _ := r.Context().Value(ctxPhoneConn).(bool); return v }
func phoneSession(r *http.Request) bool { v, _ := r.Context().Value(ctxPhoneSession).(bool); return v }

var (
	tailscale4 = mustCIDR("100.64.0.0/10")
	tailscale6 = mustCIDR("fd7a:115c:a1e0::/48")
)

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// PhoneIPAllowed reports whether ip may carry the phone listener: a
// Tailscale address or a private one (RFC 1918, IPv6 ULA), never a public
// or a loopback one.
func PhoneIPAllowed(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
		return false
	}
	return tailscale4.Contains(ip) || tailscale6.Contains(ip) || ip.IsPrivate()
}

// IsTailscale reports whether ip is a Tailscale address (the traffic is
// encrypted end to end).
func IsTailscale(ip net.IP) bool { return tailscale4.Contains(ip) || tailscale6.Contains(ip) }

// PickPhoneIP chooses the address for the phone listener from this
// machine's interface addresses: Tailscale first, then a private IPv4.
func PickPhoneIP(addrs []net.Addr) (net.IP, error) {
	var lan net.IP
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := n.IP
		if IsTailscale(ip) && ip.To4() != nil {
			return ip, nil
		}
		if lan == nil && ip.To4() != nil && PhoneIPAllowed(ip) && !ip.IsLinkLocalUnicast() {
			lan = ip
		}
	}
	if lan == nil {
		return nil, errors.New("no Tailscale or private network address on this machine: give one with --phone-addr")
	}
	return lan, nil
}

// ListenPhone opens the phone listener on ip, on the same port as the
// desktop one when it is free.
func (s *Server) ListenPhone(ip net.IP) (net.Listener, error) {
	if !PhoneIPAllowed(ip) {
		return nil, fmt.Errorf("%s is not a Tailscale or private network address", ip)
	}
	_, port, _ := net.SplitHostPort(s.addr)
	ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), port))
	if err != nil {
		if ln, err = net.Listen("tcp", net.JoinHostPort(ip.String(), "0")); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	s.phoneAddr = ln.Addr().String()
	s.mu.Unlock()
	return ln, nil
}

// PhoneAddr is the phone listener's host:port ("" = phone access is off).
func (s *Server) PhoneAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phoneAddr
}

// PhoneURL is the page's address for the phone, without any secret: a
// phone that was paired opens it with its kept session ("" = off).
func (s *Server) PhoneURL() string {
	if a := s.PhoneAddr(); a != "" {
		return "http://" + a + "/"
	}
	return ""
}

// NewPhoneLink is a single-use pairing link for the phone ("" = off).
func (s *Server) NewPhoneLink() string {
	if a := s.PhoneAddr(); a != "" {
		return "http://" + a + "/#b=" + s.auth.newBootstrap()
	}
	return ""
}

// phoneAllowed lists what a phone session may do: look, answer approvals,
// pause, cancel, stop agents and drop queued tasks.
func phoneAllowed(method, path string) bool {
	if method == http.MethodGet {
		switch path {
		case "/api/phone":
			return false // pairing another device needs the desktop
		}
		return true
	}
	switch path {
	case "/api/cancel", "/api/pause", "/api/kill", "/api/queue/remove", "/api/queue/clear", "/api/bye":
		return true
	}
	if rest, ok := strings.CutPrefix(path, "/api/approvals/"); ok {
		_, kind, _ := strings.Cut(rest, "/")
		switch kind {
		case "plan", "estimate", "changes", "budget", "conflict":
			return true
		}
	}
	return false
}

var errPhoneOnly = errors.New("not from a phone: a phone can answer approvals, pause and cancel; tasks, edits and settings need the PC")

// handleWhoami tells the page whether it runs on a phone session and
// whether phone access is on.
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"phone": phoneSession(r), "phone_on": s.PhoneAddr() != ""})
}

// handlePhone mints a pairing link for the phone and its QR code (rows of
// "1" dark and "0" light modules), for the desktop page.
func (s *Server) handlePhone(w http.ResponseWriter, r *http.Request) {
	link := s.NewPhoneLink()
	if link == "" {
		fail(w, http.StatusConflict, errors.New("phone access is off: start rw web --phone"))
		return
	}
	rows, err := QRRows(link)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	ip, _, _ := net.SplitHostPort(s.PhoneAddr())
	writeJSON(w, map[string]any{"url": link, "qr": rows, "tailscale": IsTailscale(net.ParseIP(ip))})
}

// QRRows encodes text as a QR code: one string per row, "1" dark.
func QRRows(text string) ([]string, error) {
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return nil, err
	}
	rows := make([]string, c.Size)
	for y := 0; y < c.Size; y++ {
		var b strings.Builder
		for x := 0; x < c.Size; x++ {
			b.WriteByte("01"[btoi(c.Black(x, y))])
		}
		rows[y] = b.String()
	}
	return rows, nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// QRText renders a QR code for a terminal: two rows per line in half
// blocks, dark on light with a quiet zone, so phones read it on dark and
// light terminals alike.
func QRText(text string) (string, error) {
	rows, err := QRRows(text)
	if err != nil {
		return "", err
	}
	const quiet = 2
	n := len(rows)
	dark := func(x, y int) bool {
		x, y = x-quiet, y-quiet
		return x >= 0 && y >= 0 && x < n && y < n && rows[y][x] == '1'
	}
	size := n + 2*quiet
	var b strings.Builder
	for y := 0; y < size; y += 2 {
		for x := 0; x < size; x++ {
			top, bottom := dark(x, y), y+1 < size && dark(x, y+1)
			// Foreground paints the upper half, background the lower.
			fg, bg := 97, 107 // white
			if top {
				fg = 30
			}
			if bottom {
				bg = 40
			}
			b.WriteString("\x1b[" + strconv.Itoa(fg) + ";" + strconv.Itoa(bg) + "m▀")
		}
		b.WriteString("\x1b[0m\n")
	}
	return b.String(), nil
}

// markPhone tags connections that arrived on the phone listener.
func (s *Server) markPhone(ctx context.Context, c net.Conn) context.Context {
	if a := s.PhoneAddr(); a != "" && c.LocalAddr().String() == a {
		return context.WithValue(ctx, ctxPhoneConn, true)
	}
	return ctx
}
