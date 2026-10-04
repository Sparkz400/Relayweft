package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/orchestrator"
)

// hookServer records the JSON webhook posts it gets. Paths starting with
// /deny answer 403.
type hookServer struct {
	mu   sync.Mutex
	msgs []map[string]string
	url  string
}

func newHookServer(t *testing.T) *hookServer {
	t.Helper()
	h := &hookServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/deny") {
			http.Error(w, "nope", http.StatusForbidden)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]string
		if err := json.Unmarshal(b, &m); err != nil {
			t.Errorf("webhook body %q: %v", b, err)
		}
		h.mu.Lock()
		h.msgs = append(h.msgs, m)
		h.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	h.url = srv.URL
	return h
}

func (h *hookServer) got() []map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]string(nil), h.msgs...)
}

// A finished task long enough to notify is posted before sy exits; a
// short one is not; a batch ends with its summary line.
func TestHeadlessReportPostsWebhook(t *testing.T) {
	isolate(t)
	dir := gitInit(t)
	chdir(t, dir)
	hooks := newHookServer(t)
	write(t, dir, config.FileName, "notify: {enabled: false, min_task: 1m, webhooks: [{url: '"+hooks.url+"/sy', kind: json, events: [done, failed]}]}\n")
	trustLocalFile(t, dir) // your own file: its webhooks apply
	h, err := startHeadless(&common{}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() error {
		h.report(orchestrator.TaskResult{OK: true, Summary: "added the --strict flag", Duration: 2 * time.Minute})
		h.report(orchestrator.TaskResult{OK: false, Summary: "too short to tell", Duration: time.Second})
		h.report(orchestrator.TaskResult{OK: false, Summary: "tests fail", Duration: 3 * time.Minute})
		h.print(event.Event{Kind: event.ProviderState, Provider: event.Codex, Until: time.Now().Add(time.Hour), Text: "limit"}, true)
		h.close() // waits for the posts
		return nil
	})
	got := hooks.got()
	if len(got) != 2 {
		t.Fatalf("%d posts, want 2 (short task skipped, limit not in events): %v", len(got), got)
	}
	byEvent := map[string]map[string]string{}
	for _, m := range got {
		byEvent[m["event"]] = m
	}
	if m := byEvent["done"]; m == nil || m["title"] != "Switchyard: done · "+filepath.Base(dir) || !strings.HasPrefix(m["body"], "added the --strict flag\n2m0s · ") {
		t.Errorf("done = %v", m)
	}
	if m := byEvent["failed"]; m == nil || !strings.HasPrefix(m["body"], "tests fail") {
		t.Errorf("failed = %v", m)
	}
}

func TestNotifyCommand(t *testing.T) {
	isolate(t)
	dir := gitInit(t)
	chdir(t, dir)
	hooks := newHookServer(t)
	t.Setenv("SY_TEST_HOOK", hooks.url+"/secret-path")
	write(t, dir, config.FileName, "notify: {enabled: false, webhooks: [{url: '${SY_TEST_HOOK}', kind: json, events: [watch]}, {url: '"+hooks.url+"/deny-secret', kind: ntfy}]}\n")
	trustLocalFile(t, dir)
	var out bytes.Buffer
	notifyOut = &out
	defer func() { notifyOut = os.Stdout }()

	if err := cmdNotify(nil); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "desktop notifications: off") || !strings.Contains(s, "webhook 1: json 127.0.0.1:") ||
		!strings.Contains(s, "(watch)") || !strings.Contains(s, "webhook 2: ntfy 127.0.0.1:") || !strings.Contains(s, "(all events)") {
		t.Errorf("list:\n%s", s)
	}
	if strings.Contains(s, "secret") || len(hooks.got()) != 0 {
		t.Errorf("listing leaked a URL or posted:\n%s", s)
	}

	out.Reset()
	err := cmdNotify([]string{"--test"})
	if err == nil || err.Error() != "1 of 2 webhook(s) failed" {
		t.Errorf("err = %v", err)
	}
	s = out.String()
	if !strings.Contains(s, "webhook 1: json 127.0.0.1:") || !strings.Contains(s, ": sent") || !strings.Contains(s, "failed: 403 Forbidden: nope") || strings.Contains(s, "secret") {
		t.Errorf("test output:\n%s", s)
	}
	// The test message goes out whatever the webhook's events.
	if got := hooks.got(); len(got) != 1 || got[0]["event"] != "test" || got[0]["source"] != filepath.Base(dir) {
		t.Errorf("posts: %v", got)
	}
}

// A round's result and a merge reach the webhooks (event "watch").
func TestWatchPostsWebhooks(t *testing.T) {
	_, api, _, wr := watchSetup(t)
	hooks := newHookServer(t)
	cd, _ := os.UserConfigDir()
	write(t, filepath.Join(cd, "switchyard"), config.FileName, "orchestrator: {review_before_done: false}\nwatch: {max_rounds: 2}\n"+
		"notify: {enabled: false, webhooks: [{url: '"+hooks.url+"/w', kind: json, events: [watch]}]}\n")
	api.set(func() {
		api.comments = `[{"id":2,"path":"a.txt","line":1,"body":"rename it","user":{"login":"rev"},"author_association":"MEMBER"}]`
	})
	var out bytes.Buffer
	w := newWatcher(true)
	w.out = &out
	if err := w.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.sender.Wait()
	if wr.steps() != 1 {
		t.Fatalf("no round ran:\n%s", out.String())
	}
	got := hooks.got()
	if len(got) != 1 || got[0]["event"] != "watch" || !strings.Contains(got[0]["title"], "o/r#101 pushed a follow-up") ||
		!strings.HasPrefix(got[0]["body"], "round 1 of 2, 1 item(s): pushed ") || got[0]["link"] == "" {
		t.Fatalf("round post: %v\n%s", got, out.String())
	}

	// Merged: one post from your own config's webhooks.
	api.set(func() { api.closed, api.merged = true, true })
	cfg, _, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	w = newWatcher(true)
	w.out = &out
	w.hooks = cfg.Notify.Webhooks
	w.pass(context.Background())
	w.sender.Wait()
	if got := hooks.got(); len(got) != 2 || !strings.Contains(got[1]["title"], "o/r#101 merged") {
		t.Fatalf("merge post: %v", got)
	}
}

// trustLocalFile trusts dir's switchyard.yaml as if you had written it with
// sy: without that, its settings that run commands or send data (webhooks
// here) are ignored.
func trustLocalFile(t *testing.T, dir string) {
	t.Helper()
	if err := config.TrustLocal(filepath.Join(dir, config.FileName)); err != nil {
		t.Fatal(err)
	}
}
