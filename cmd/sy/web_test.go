package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"
)

// TestWebDemoServer starts `sy web --demo` in-process: the tokenized URL
// logs the browser in (cookie), the page and the API answer, and a demo
// task runs to the end.
func TestWebDemoServer(t *testing.T) {
	isolate(t)
	chdir(t, t.TempDir())
	var c common
	w, err := startWeb(&c, 0, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.stop()
	url := w.srv.URL()
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("URL %s is not loopback", url)
	}
	jar, _ := cookiejar.New(nil)
	cl := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	res, err := cl.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(page), "Switchyard") || res.Request.URL.RawQuery != "" {
		t.Fatalf("page: %d, final URL %s", res.StatusCode, res.Request.URL)
	}
	post := func(path, body string) {
		t.Helper()
		req, _ := http.NewRequest("POST", "http://"+w.srv.Addr()+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		res, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("POST %s: %d %s", path, res.StatusCode, data)
		}
	}
	post("/api/settings", `{"approve_plan": false}`)
	post("/api/task", `{"text": "`+demoTask+`"}`)
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err := cl.Get("http://" + w.srv.Addr() + "/api/state")
		if err != nil {
			t.Fatal(err)
		}
		var st struct {
			Running bool `json:"running"`
			Last    *struct {
				Text string `json:"text"`
			} `json:"last"`
		}
		json.NewDecoder(res.Body).Decode(&st)
		res.Body.Close()
		if !st.Running && st.Last != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the demo task did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
