package web

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// rawDo sends a request exactly as given: no session or other headers are
// added (a non-browser client such as the VS Code extension sends no
// Origin and no Sec-Fetch-* headers).
func (e *testEnv) rawDo(method, path, body string, hdr map[string]string) (*http.Response, string) {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, e.base+path, rd)
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res, string(data)
}

// TestClientWithoutOrigin pins the rule editor clients rely on: a request
// without an Origin header is not a cross-site request, but it gets no
// further than any other without a valid session, a matching Host and a
// JSON body.
func TestClientWithoutOrigin(t *testing.T) {
	env := newEnv(t, nil)
	jsonHdr := map[string]string{"Content-Type": "application/json"}
	with := func(extra map[string]string) map[string]string {
		h := map[string]string{"Content-Type": "application/json"}
		for k, v := range extra {
			h[k] = v
		}
		return h
	}

	// No Origin, no session: refused on every API route.
	for _, p := range []string{"/api/state", "/api/events", "/api/sessions"} {
		if res, _ := env.rawDo("GET", p, "", nil); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without session: %d, want 401", p, res.StatusCode)
		}
	}
	if res, _ := env.rawDo("POST", "/api/task", `{"text":"x"}`, jsonHdr); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /api/task without session: %d, want 401", res.StatusCode)
	}

	// The client trades a bootstrap like a page would; the bootstrap is
	// single use.
	b := env.srv.NewBootstrap()
	res, body := env.rawDo("POST", "/api/session", `{"bootstrap":"`+b+`"}`, jsonHdr)
	var v struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil || res.StatusCode != 200 || len(v.Session) != 64 {
		t.Fatalf("trade: %d %s", res.StatusCode, body)
	}
	if res, _ := env.rawDo("POST", "/api/session", `{"bootstrap":"`+b+`"}`, jsonHdr); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("reused bootstrap: %d, want 401", res.StatusCode)
	}
	bearer := map[string]string{"Authorization": "Bearer " + v.Session}
	if res, _ := env.rawDo("GET", "/api/state", "", bearer); res.StatusCode != 200 {
		t.Errorf("state with bearer, no Origin: %d", res.StatusCode)
	}
	if res, _ := env.rawDo("GET", "/api/state", "", map[string]string{SessionHeader: v.Session}); res.StatusCode != 200 {
		t.Errorf("state with session header, no Origin: %d", res.StatusCode)
	}

	// The session does not lift the other checks.
	port := portOf(env.srv.Addr())
	for name, c := range map[string]struct {
		method, body string
		hdr          map[string]string
		want         int
	}{
		"foreign Host":           {"GET", "", map[string]string{"Authorization": bearer["Authorization"], "Host": "evil.example:" + port}, http.StatusForbidden},
		"webview Origin":         {"POST", `{"paused":true}`, with(map[string]string{"Authorization": bearer["Authorization"], "Origin": "vscode-webview://abc"}), http.StatusForbidden},
		"null Origin":            {"POST", `{"paused":true}`, with(map[string]string{"Authorization": bearer["Authorization"], "Origin": "null"}), http.StatusForbidden},
		"https same host Origin": {"POST", `{"paused":true}`, with(map[string]string{"Authorization": bearer["Authorization"], "Origin": "https://127.0.0.1:" + port}), http.StatusForbidden},
		"cross-site fetch":       {"POST", `{"paused":true}`, with(map[string]string{"Authorization": bearer["Authorization"], "Sec-Fetch-Site": "cross-site"}), http.StatusForbidden},
		"form body":              {"POST", `paused=true`, map[string]string{"Authorization": bearer["Authorization"], "Content-Type": "text/plain"}, http.StatusUnsupportedMediaType},
	} {
		path := "/api/pause"
		if c.method == "GET" {
			path = "/api/state"
		}
		if res, _ := env.rawDo(c.method, path, c.body, c.hdr); res.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", name, res.StatusCode, c.want)
		}
	}
	if env.state().Paused {
		t.Fatal("a refused request paused the orchestrator")
	}

	// The event stream accepts the session in a header (no ?s= needed).
	req, _ := http.NewRequest("GET", env.base+"/api/events", nil)
	req.Header.Set("Authorization", "Bearer "+v.Session)
	sres, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer sres.Body.Close()
	if sres.StatusCode != 200 || !strings.HasPrefix(sres.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("events: %d %s", sres.StatusCode, sres.Header.Get("Content-Type"))
	}
	sc := bufio.NewScanner(sres.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	deadline := time.Now().Add(5 * time.Second)
	for sc.Scan() {
		if sc.Text() == "event: synced" {
			return
		}
		if time.Now().After(deadline) {
			break
		}
	}
	t.Fatal("no synced frame on the stream")
}

func TestHello(t *testing.T) {
	env := newEnv(t, nil)
	h := env.srv.Hello(42)
	if h.Switchyard != "web-client" || h.Protocol != ClientProtocol || h.PID != 42 || h.Addr != env.srv.Addr() || h.URL != "http://"+env.srv.Addr() || !h.Demo {
		t.Fatalf("hello %+v", h)
	}
	if code, sess := env.trade(h.Bootstrap); code != 200 || sess == "" {
		t.Fatalf("hello bootstrap: %d", code)
	}
}
