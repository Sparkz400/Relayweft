package web

// Editor clients (`sy web --client`, used by the VS Code extension).
//
// A client is a local program that started sy itself and reads sy's
// stdout through a pipe. It authenticates exactly like a browser tab: it
// trades a single-use bootstrap for a session (POST /api/session) and
// sends the session in X-Switchyard-Session or Authorization: Bearer on
// every /api request, including the event stream. Nothing is relaxed for
// it: a request without an Origin header was always accepted by originOK
// (a browser always sends one on cross-origin requests), but every /api
// route still needs a valid session, the Host must name this server, and
// a request that does carry a foreign Origin (a web page, a webview) is
// refused even with a valid session.

// ClientProtocol is the version of the `sy web --client` hello line; a
// client refuses a hello with a version it does not know.
const ClientProtocol = 1

// ClientHello is the one JSON line `sy web --client` prints on stdout once
// the server listens.
type ClientHello struct {
	Switchyard string `json:"switchyard"` // always "web-client"
	Protocol   int    `json:"protocol"`
	Version    string `json:"version"`
	URL        string `json:"url"`  // http://127.0.0.1:PORT (no secret)
	Addr       string `json:"addr"` // 127.0.0.1:PORT
	Bootstrap  string `json:"bootstrap"`
	Dir        string `json:"dir"`
	PID        int    `json:"pid"`
	Demo       bool   `json:"demo,omitempty"`
}

// NewBootstrap mints a single-use bootstrap (valid for bootstrapTTL) for
// a client that trades it itself instead of opening a link.
func (s *Server) NewBootstrap() string { return s.auth.newBootstrap() }

// Hello returns the hello line for a client, with a fresh bootstrap.
func (s *Server) Hello(pid int) ClientHello {
	return ClientHello{
		Switchyard: "web-client", Protocol: ClientProtocol, Version: s.opt.Version,
		URL: "http://" + s.addr, Addr: s.addr, Bootstrap: s.NewBootstrap(), Dir: s.opt.Dir, PID: pid, Demo: s.opt.Demo,
	}
}
