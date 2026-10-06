package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestWebDemoServer starts `rw web --demo` in-process and goes through the
// page's flow: the printed link's #b= bootstrap is traded for a session,
// the API answers with it, and a demo task runs to the end.
func TestWebDemoServer(t *testing.T) {
	isolate(t)
	chdir(t, t.TempDir())
	var c common
	w, err := startWeb(&c, 0, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.stop()
	link := w.srv.NewLink()
	base := "http://" + w.srv.Addr()
	if !strings.HasPrefix(link, base+"/#b=") {
		t.Fatalf("link %s", link)
	}
	cl := &http.Client{Timeout: 10 * time.Second}
	res, err := cl.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(page), "Relayweft") || len(res.Cookies()) > 0 {
		t.Fatalf("page: %d cookies=%v", res.StatusCode, res.Cookies())
	}
	session := ""
	call := func(method, path, body string) []byte {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if session != "" {
			req.Header.Set("X-Relayweft-Session", session)
		}
		res, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, data)
		}
		return data
	}
	var s struct {
		Session string `json:"session"`
	}
	json.Unmarshal(call("POST", "/api/session", `{"bootstrap": "`+link[strings.Index(link, "#b=")+3:]+`"}`), &s)
	if s.Session == "" {
		t.Fatal("no session")
	}
	session = s.Session
	call("POST", "/api/settings", `{"approve_plan": false}`)
	call("POST", "/api/task", `{"text": "`+demoTask+`"}`)
	deadline := time.Now().Add(30 * time.Second)
	for {
		var st struct {
			Running bool `json:"running"`
			Last    *struct {
				Text string `json:"text"`
			} `json:"last"`
		}
		json.Unmarshal(call("GET", "/api/state", ""), &st)
		if !st.Running && st.Last != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the demo task did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPrintNewLinks(t *testing.T) {
	var out bytes.Buffer
	n := 0
	printNewLinks(strings.NewReader("\n\n"), &out, func() string { n++; return "L" + string(rune('0'+n)) }, nil)
	if out.String() != "open: L1\nopen: L2\n" {
		t.Fatalf("output %q", out.String())
	}
}
