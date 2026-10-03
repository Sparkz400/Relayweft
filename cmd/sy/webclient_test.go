package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/web"
)

// TestWebClientMode runs `sy web --client` in-process the way the VS Code
// extension drives it: read the hello line, trade its bootstrap for a
// session without any Origin header, use the API, ask for a page link on
// stdin, and stop when stdin closes.
func TestWebClientMode(t *testing.T) {
	isolate(t)
	chdir(t, t.TempDir())
	var c common
	w, err := startWeb(&c, 0, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.stop()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- serveClient(context.Background(), w, inR, outW) }()
	lines := bufio.NewScanner(outR)
	next := func() string {
		t.Helper()
		got := make(chan string, 1)
		go func() {
			if lines.Scan() {
				got <- lines.Text()
			} else {
				got <- ""
			}
		}()
		select {
		case l := <-got:
			return l
		case <-time.After(10 * time.Second):
			t.Fatal("no line from sy web --client")
			return ""
		}
	}

	var hello web.ClientHello
	line := next()
	if err := json.Unmarshal([]byte(line), &hello); err != nil {
		t.Fatalf("hello %q: %v", line, err)
	}
	if hello.Switchyard != "web-client" || hello.Protocol != web.ClientProtocol || hello.Addr != w.srv.Addr() ||
		hello.URL != "http://"+w.srv.Addr() || len(hello.Bootstrap) != 64 || !hello.Demo || hello.PID == 0 || hello.Dir == "" {
		t.Fatalf("hello %+v", hello)
	}
	if strings.Contains(hello.URL, hello.Bootstrap) {
		t.Fatal("the URL carries the bootstrap")
	}

	cl := &http.Client{Timeout: 10 * time.Second}
	post := func(path, body, session string) (int, string) {
		req, _ := http.NewRequest("POST", hello.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if session != "" {
			req.Header.Set("Authorization", "Bearer "+session)
		}
		res, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	code, body := post("/api/session", `{"bootstrap":"`+hello.Bootstrap+`"}`, "")
	var s struct {
		Session string `json:"session"`
	}
	json.Unmarshal([]byte(body), &s)
	if code != 200 || s.Session == "" {
		t.Fatalf("trade: %d %s", code, body)
	}
	if code, _ := post("/api/session", `{"bootstrap":"`+hello.Bootstrap+`"}`, ""); code != http.StatusUnauthorized {
		t.Fatalf("second trade of the hello bootstrap: %d, want 401", code)
	}
	if code, body := post("/api/pause", `{"paused":true}`, s.Session); code != 200 {
		t.Fatalf("pause: %d %s", code, body)
	}
	if code, _ := post("/api/pause", `{"paused":false}`, ""); code != http.StatusUnauthorized {
		t.Fatalf("no session: %d, want 401", code)
	}

	// Commands on stdin.
	io.WriteString(inW, "link\n")
	var l struct {
		Link string `json:"link"`
	}
	json.Unmarshal([]byte(next()), &l)
	if !strings.HasPrefix(l.Link, hello.URL+"/#b=") {
		t.Fatalf("link %q", l.Link)
	}
	io.WriteString(inW, "bootstrap\n")
	var b struct {
		Bootstrap string `json:"bootstrap"`
	}
	json.Unmarshal([]byte(next()), &b)
	if len(b.Bootstrap) != 64 || b.Bootstrap == hello.Bootstrap {
		t.Fatalf("bootstrap %q", b.Bootstrap)
	}
	if code, _ := post("/api/session", `{"bootstrap":"`+b.Bootstrap+`"}`, ""); code != 200 {
		t.Fatalf("trade of the second bootstrap: %d", code)
	}
	io.WriteString(inW, "rm -rf /\n")
	if l := next(); !strings.Contains(l, `"error"`) {
		t.Fatalf("unknown command answered %q", l)
	}

	// Closing stdin stops the client loop.
	inW.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serveClient did not return after stdin closed")
	}
}

// TestWebClientStopsOnContext: a signal (the context) stops it too.
func TestWebClientStopsOnContext(t *testing.T) {
	isolate(t)
	chdir(t, t.TempDir())
	var c common
	w, err := startWeb(&c, 0, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.stop()
	inR, inW := io.Pipe()
	defer inW.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveClient(ctx, w, inR, io.Discard) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serveClient did not return after cancel")
	}
}
