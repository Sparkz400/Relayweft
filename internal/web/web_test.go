package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/limits"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/runner"
	"github.com/sparkz400/switchyard/internal/sysload"
)

type testEnv struct {
	t    *testing.T
	srv  *Server
	ts   *httptest.Server
	base string

	mu     sync.Mutex
	events []event.Event
}

// newEnv starts a server on fake runners (no delays, no git, no files).
// longTask is long enough that the planner runs (short tasks skip it).
const longTask = "Make the parser keep trailing empty fields and add a --strict flag that rejects malformed lines, with tests and README docs"

func newEnv(t *testing.T, mutate func(c *config.Config)) *testEnv {
	t.Helper()
	cfg := config.Default()
	cfg.Orchestrator.ApprovePlan = false
	if mutate != nil {
		mutate(cfg)
	}
	store := config.NewStore(cfg, filepath.Join(t.TempDir(), "switchyard.yaml"))
	fake := runner.Set{event.Codex: &runner.Fake{Provider: event.Codex}, event.Claude: &runner.Fake{Provider: event.Claude}}
	events := make(chan event.Event, 4096)
	tap := make(chan event.Event, 4096)
	ap := NewApprover()
	orc := orchestrator.New(orchestrator.Options{
		Dir: t.TempDir(), Store: store, Runners: func(*config.Config) runner.Set { return fake }, Tracker: limits.NewTracker(),
		Events: events, NoGit: true, Mode: "demo", Approver: ap, Load: func() sysload.Sample { return sysload.Sample{} },
	})
	env := &testEnv{t: t}
	// Tee the events so tests can look at them too.
	go func() {
		for e := range events {
			env.mu.Lock()
			env.events = append(env.events, e)
			env.mu.Unlock()
			tap <- e
		}
	}()
	srv, err := New(Options{Orc: orc, Events: tap, Approver: ap, Dir: t.TempDir(), Demo: true, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.Bind(ln)
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Listener.Close()
	ts.Listener = ln
	ts.Start()
	env.srv, env.ts, env.base = srv, ts, "http://"+srv.Addr()
	t.Cleanup(func() {
		srv.Shutdown()
		ts.Close()
	})
	return env
}

func (e *testEnv) do(method, path string, body any, hdr map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(TokenHeader, e.srv.Token())
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v // the client ignores a Host header
			continue
		}
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res, data
}

// call does an authorized request and decodes the JSON answer; it fails
// the test on a non-2xx status.
func (e *testEnv) call(method, path string, body, out any) {
	e.t.Helper()
	res, data := e.do(method, path, body, nil)
	if res.StatusCode/100 != 2 {
		e.t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			e.t.Fatalf("%s %s: %v in %s", method, path, err, data)
		}
	}
}

func (e *testEnv) state() stateView {
	var v stateView
	e.call("GET", "/api/state", nil, &v)
	return v
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (e *testEnv) count(kind event.Kind) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, ev := range e.events {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

func (e *testEnv) agentRan(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.events {
		if ev.AgentID == id && ev.Kind == event.Started {
			return true
		}
	}
	return false
}

func TestSecurityChecks(t *testing.T) {
	env := newEnv(t, nil)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// No token: refused, for the page and the API.
	for _, p := range []string{"/", "/api/state", "/api/events", "/assets/app.js"} {
		res, err := http.Get(env.base + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without token: %d, want 401", p, res.StatusCode)
		}
	}
	// A wrong token in the URL is refused too.
	res, err := noRedirect.Get(env.base + "/?t=nope")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", res.StatusCode)
	}

	// The token URL sets the cookie and redirects to a clean URL.
	res, err = noRedirect.Get(env.base + "/?t=" + env.srv.Token())
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Fatalf("token URL: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == env.srv.cookieName() {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Value != env.srv.Token() {
		t.Fatalf("cookie = %+v", cookie)
	}
	req, _ := http.NewRequest("GET", env.base+"/", nil)
	req.AddCookie(cookie)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(page), "/assets/app.js") {
		t.Fatalf("page with cookie: %d", res.StatusCode)
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}

	// DNS rebinding: a foreign Host is refused even with the token.
	r, _ := env.do("GET", "/api/state", nil, map[string]string{"Host": "evil.example:" + portOf(env.srv.Addr())})
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("foreign Host: %d, want 403", r.StatusCode)
	}
	// localhost with the right port is fine.
	r, _ = env.do("GET", "/api/state", nil, map[string]string{"Host": "localhost:" + portOf(env.srv.Addr())})
	if r.StatusCode != 200 {
		t.Errorf("localhost Host: %d", r.StatusCode)
	}
	// Cross-site requests are refused.
	r, _ = env.do("POST", "/api/pause", map[string]bool{"paused": true}, map[string]string{"Origin": "http://evil.example"})
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("foreign Origin: %d, want 403", r.StatusCode)
	}
	r, _ = env.do("POST", "/api/pause", map[string]bool{"paused": true}, map[string]string{"Sec-Fetch-Site": "cross-site"})
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("Sec-Fetch-Site cross-site: %d, want 403", r.StatusCode)
	}
	r, _ = env.do("POST", "/api/pause", map[string]bool{"paused": true}, map[string]string{"Origin": "http://127.0.0.1:" + portOf(env.srv.Addr())})
	if r.StatusCode != 200 {
		t.Errorf("same Origin: %d", r.StatusCode)
	}
	// A form post (not JSON) is refused.
	r, _ = env.do("POST", "/api/cancel", nil, map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if r.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("form post: %d, want 415", r.StatusCode)
	}
	// A bearer token works for scripts.
	r, _ = env.do("GET", "/api/state", nil, map[string]string{TokenHeader: "", "Authorization": "Bearer " + env.srv.Token()})
	if r.StatusCode != 200 {
		t.Errorf("bearer: %d", r.StatusCode)
	}
	if !env.state().Paused {
		t.Error("pause did not apply")
	}
}

func portOf(addr string) string {
	_, p, _ := net.SplitHostPort(addr)
	return p
}

func TestListenIsLoopbackOnly(t *testing.T) {
	env := newEnv(t, nil)
	ln, err := env.srv.Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	host, _, _ := net.SplitHostPort(ln.Addr().String())
	if host != "127.0.0.1" {
		t.Fatalf("listening on %s", ln.Addr())
	}
	if !strings.HasPrefix(env.srv.URL(), "http://127.0.0.1:") || !strings.Contains(env.srv.URL(), "/?t="+env.srv.Token()) {
		t.Fatalf("URL = %s", env.srv.URL())
	}
	if len(env.srv.Token()) < 32 {
		t.Fatalf("token too short: %q", env.srv.Token())
	}
}

func TestPlanApprovalRoundTrip(t *testing.T) {
	env := newEnv(t, func(c *config.Config) { c.Orchestrator.ApprovePlan = true })
	var sub submitResult
	env.call("POST", "/api/task", map[string]string{"text": "Make the parser keep trailing empty fields and add a --strict flag"}, &sub)
	if sub.Status != "started" {
		t.Fatalf("submit: %+v", sub)
	}
	var req *Request
	waitFor(t, "a plan approval", func() bool {
		st := env.state()
		if len(st.Approvals) > 0 {
			req = st.Approvals[0]
		}
		return req != nil
	})
	if req.Type != "plan" || req.Plan == nil || len(req.Plan.Subtasks) < 2 {
		t.Fatalf("request = %+v", req)
	}
	// An empty plan cannot run: refused, and the request stays open.
	res, data := env.do("POST", "/api/approvals/"+req.ID+"/plan", map[string]any{"ok": true, "plan": orchestrator.Plan{}}, nil)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "no subtasks") {
		t.Fatalf("empty plan: %d %s", res.StatusCode, data)
	}
	if len(env.state().Approvals) != 1 {
		t.Fatal("request closed after a refused answer")
	}
	// Edit: drop the "docs" step (and any dependency on it), rename one.
	p := *req.Plan
	var kept []orchestrator.Subtask
	for _, st := range p.Subtasks {
		if st.ID == "docs" {
			continue
		}
		var deps []string
		for _, d := range st.DependsOn {
			if d != "docs" {
				deps = append(deps, d)
			}
		}
		st.DependsOn = deps
		if st.ID == "flag" {
			st.Title = "Add the strict flag (edited)"
		}
		kept = append(kept, st)
	}
	p.Subtasks = kept
	var ans struct {
		Plan orchestrator.Plan `json:"plan"`
	}
	env.call("POST", "/api/approvals/"+req.ID+"/plan", map[string]any{"ok": true, "plan": p}, &ans)
	if len(ans.Plan.Subtasks) != len(kept) {
		t.Fatalf("normalized plan has %d subtasks, want %d", len(ans.Plan.Subtasks), len(kept))
	}
	waitFor(t, "the task to finish", func() bool { return env.count(event.TaskDone) == 1 && !env.state().Running })
	if env.agentRan("docs") {
		t.Error("the deleted step ran")
	}
	if !env.agentRan("flag") || !env.agentRan("parser") {
		t.Error("approved steps did not run")
	}
	// Answering again is gone.
	res, _ = env.do("POST", "/api/approvals/"+req.ID+"/plan", map[string]any{"ok": true, "plan": p}, nil)
	if res.StatusCode != http.StatusGone {
		t.Errorf("second answer: %d, want 410", res.StatusCode)
	}
}

func TestPlanRejectCancelsTask(t *testing.T) {
	env := newEnv(t, func(c *config.Config) { c.Orchestrator.ApprovePlan = true })
	env.call("POST", "/api/task", map[string]string{"text": longTask}, nil)
	var id string
	waitFor(t, "a plan approval", func() bool {
		if a := env.state().Approvals; len(a) > 0 {
			id = a[0].ID
		}
		return id != ""
	})
	env.call("POST", "/api/approvals/"+id+"/plan", map[string]any{"ok": false}, nil)
	waitFor(t, "the task to end", func() bool { return env.count(event.TaskDone) == 1 && !env.state().Running })
	if env.agentRan("parser") {
		t.Error("a step ran after the plan was rejected")
	}
}

func TestApproverCancelledContextDropsRequest(t *testing.T) {
	ap := NewApprover()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool)
	go func() {
		_, ok := ap.ApprovePlan(ctx, "t", orchestrator.Plan{Subtasks: []orchestrator.Subtask{{ID: "a", Prompt: "x"}}})
		done <- ok
	}()
	waitFor(t, "the request", func() bool { return len(ap.Pending()) == 1 })
	cancel()
	if <-done {
		t.Fatal("a cancelled approval returned ok")
	}
	waitFor(t, "the request to go", func() bool { return len(ap.Pending()) == 0 })

	// Close unblocks waiting requests as "no".
	go func() {
		d := ap.ReviewChanges(context.Background(), demoChangeSet())
		done <- len(d.Apply) > 0
	}()
	waitFor(t, "the review", func() bool { return len(ap.Pending()) == 1 })
	ap.Close()
	if <-done {
		t.Fatal("a closed approver applied changes")
	}
}

func TestReviewDecisionWithHunks(t *testing.T) {
	env := newEnv(t, nil)
	cs := demoChangeSet()
	got := make(chan orchestrator.ChangeDecision, 3)
	ask := func() string {
		go func() { got <- env.srv.ap.ReviewChanges(context.Background(), cs) }()
		var id string
		waitFor(t, "the review request", func() bool {
			for _, a := range env.state().Approvals {
				if a.Type == "changes" {
					id = a.ID
				}
			}
			return id != ""
		})
		return id
	}

	id := ask()
	st := env.state()
	cv := st.Approvals[0].Changes
	if cv.StepID != "parser" || len(cv.Files) != 3 {
		t.Fatalf("change view = %+v", cv)
	}
	if !cv.Files[0].Splittable || len(cv.Files[0].Hunks) != 2 || cv.Files[1].Splittable || cv.Files[2].Splittable {
		t.Fatalf("splittable flags wrong: %+v", cv.Files)
	}
	// Keep hunk 0 of parse.go, all of the test file, drop README; an
	// unknown path and an out-of-range hunk are ignored.
	env.call("POST", "/api/approvals/"+id+"/changes", map[string]any{
		"apply": []string{"internal/parse.go", "internal/parse_test.go", "nope.go"},
		"hunks": map[string][]int{"internal/parse.go": {0, 7}, "internal/parse_test.go": {0}},
	}, nil)
	d := <-got
	if strings.Join(d.Apply, ",") != "internal/parse.go,internal/parse_test.go" {
		t.Errorf("apply = %v", d.Apply)
	}
	if len(d.Hunks) != 1 || len(d.Hunks["internal/parse.go"]) != 1 || d.Hunks["internal/parse.go"][0] != 0 {
		t.Errorf("hunks = %v (the unsplittable file must not carry hunks)", d.Hunks)
	}

	// Every hunk selected applies the file whole; no hunk drops it.
	id = ask()
	env.call("POST", "/api/approvals/"+id+"/changes", map[string]any{
		"apply": []string{"internal/parse.go", "README.md"},
		"hunks": map[string][]int{"internal/parse.go": {1, 0}, "README.md": {}},
	}, nil)
	d = <-got
	if strings.Join(d.Apply, ",") != "internal/parse.go,README.md" || len(d.Hunks) != 0 {
		t.Errorf("all hunks: apply=%v hunks=%v", d.Apply, d.Hunks)
	}
	id = ask()
	env.call("POST", "/api/approvals/"+id+"/changes", map[string]any{
		"apply": []string{"internal/parse.go"}, "hunks": map[string][]int{"internal/parse.go": {}},
	}, nil)
	if d = <-got; len(d.Apply) != 0 {
		t.Errorf("no hunk kept: apply=%v", d.Apply)
	}

	// Feedback never applies anything.
	id = ask()
	env.call("POST", "/api/approvals/"+id+"/changes", map[string]any{"apply": []string{"README.md"}, "feedback": "  wrap the error  "}, nil)
	if d = <-got; d.Feedback != "wrap the error" || len(d.Apply) != 0 {
		t.Errorf("feedback: %+v", d)
	}
	// A plan answer to a change request is refused.
	id = ask()
	res, _ := env.do("POST", "/api/approvals/"+id+"/plan", map[string]any{"ok": true}, nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("plan answer to a review: %d", res.StatusCode)
	}
	env.call("POST", "/api/approvals/"+id+"/changes", map[string]any{"apply": []string{}}, nil)
	if d = <-got; len(d.Apply) != 0 {
		t.Errorf("reject: %+v", d)
	}
}

func TestQueueRunsUnattendedAfterTask(t *testing.T) {
	env := newEnv(t, func(c *config.Config) { c.Orchestrator.ApprovePlan = true })
	var r1, r2, r3 submitResult
	env.call("POST", "/api/task", map[string]string{"text": longTask}, &r1)
	var id string
	waitFor(t, "the first plan approval", func() bool {
		if a := env.state().Approvals; len(a) > 0 {
			id = a[0].ID
		}
		return id != ""
	})
	env.call("POST", "/api/task", map[string]string{"text": "second task"}, &r2)
	env.call("POST", "/api/task", map[string]string{"text": "third task"}, &r3)
	if r1.Status != "started" || r2.Status != "queued" || r3.Status != "queued" {
		t.Fatalf("statuses: %s %s %s", r1.Status, r2.Status, r3.Status)
	}
	var q []jobView
	env.call("GET", "/api/queue", nil, &q)
	if len(q) != 2 || q[0].Label != "second task" || q[1].Label != "third task" {
		t.Fatalf("queue = %+v", q)
	}
	// Remove the third one.
	env.call("POST", "/api/queue/remove", map[string]int{"id": r3.JobID}, nil)
	if res, _ := env.do("POST", "/api/queue/remove", map[string]int{"id": r3.JobID}, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("removing twice: %d", res.StatusCode)
	}
	// Resume is refused while a task runs.
	if res, _ := env.do("POST", "/api/resume", map[string]string{}, nil); res.StatusCode/100 == 2 {
		t.Errorf("resume while running: %d", res.StatusCode)
	}
	// Approve the first plan as is: the queued task then runs without
	// asking (unattended), and the queue empties.
	env.call("POST", "/api/approvals/"+id+"/plan", map[string]any{"ok": true, "plan": env.state().Approvals[0].Plan}, nil)
	waitFor(t, "both tasks", func() bool { return env.count(event.TaskDone) == 2 && !env.state().Running })
	st := env.state()
	if len(st.Queue) != 0 || len(st.Approvals) != 0 {
		t.Fatalf("after: queue=%v approvals=%v", st.Queue, st.Approvals)
	}
	if n := env.count(event.TaskStart); n != 2 {
		t.Errorf("%d tasks started, want 2 (the removed one must not run)", n)
	}

	// Clearing.
	env.call("POST", "/api/queue/clear", map[string]any{}, nil)
}

func TestCancelPauseKill(t *testing.T) {
	env := newEnv(t, func(c *config.Config) { c.Orchestrator.ApprovePlan = true })
	if res, _ := env.do("POST", "/api/cancel", map[string]any{}, nil); res.StatusCode != http.StatusConflict {
		t.Errorf("cancel with nothing running: %d", res.StatusCode)
	}
	env.call("POST", "/api/task", map[string]string{"text": longTask}, nil)
	waitFor(t, "a plan approval", func() bool { return len(env.state().Approvals) > 0 })
	env.call("POST", "/api/cancel", map[string]any{}, nil)
	waitFor(t, "the task to end", func() bool { return env.count(event.TaskDone) == 1 && !env.state().Running })
	if len(env.state().Approvals) != 0 {
		t.Error("the approval outlived the cancelled task")
	}
	env.call("POST", "/api/pause", map[string]bool{"paused": true}, nil)
	if !env.state().Paused {
		t.Error("not paused")
	}
	env.call("POST", "/api/pause", map[string]bool{"paused": false}, nil)
	if res, _ := env.do("POST", "/api/kill", map[string]string{"agent": "nobody"}, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("kill unknown: %d", res.StatusCode)
	}
}

func TestFollowUpAndSessions(t *testing.T) {
	env := newEnv(t, nil)
	env.call("POST", "/api/task", map[string]string{"text": "fix the parser"}, nil)
	waitFor(t, "the task", func() bool { return env.count(event.TaskDone) == 1 && !env.state().Running })
	var ss []sessionRow
	env.call("GET", "/api/sessions", nil, &ss)
	if len(ss) == 0 {
		t.Fatal("no sessions after a task")
	}
	// "@types/node" is a task, "@nobody msg" an unknown agent.
	if res, data := env.do("POST", "/api/task", map[string]string{"text": "@nobody hi"}, nil); res.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "nobody") {
		t.Errorf("unknown agent: %d %s", res.StatusCode, data)
	}
	if res, _ := env.do("POST", "/api/task", map[string]string{"text": "@parser"}, nil); res.StatusCode != http.StatusBadRequest {
		t.Errorf("follow-up without a message: %d", res.StatusCode)
	}
	var sub submitResult
	env.call("POST", "/api/task", map[string]string{"text": "@" + ss[0].Agent + " make the error clearer"}, &sub)
	if sub.Status != "started" {
		t.Fatalf("follow-up: %+v", sub)
	}
	waitFor(t, "the follow-up", func() bool { return env.count(event.TaskDone) == 2 && !env.state().Running })
}

func TestParseFollowUp(t *testing.T) {
	for _, c := range []struct {
		in, agent, msg string
		ok             bool
	}{
		{"@parser fix it", "parser", "fix it", true},
		{"@ fix it", "", "fix it", true},
		{"@last fix", "", "fix", true},
		{"@types/node upgrade", "", "", false},
		{"@Component rename", "", "", false},
		{"plain task", "", "", false},
		{"@parser", "parser", "", true},
	} {
		a, m, ok := parseFollowUp(c.in)
		if a != c.agent || m != c.msg || ok != c.ok {
			t.Errorf("parseFollowUp(%q) = %q %q %v", c.in, a, m, ok)
		}
	}
}

func TestRoutesAndSettings(t *testing.T) {
	env := newEnv(t, nil)
	var rv routesView
	env.call("GET", "/api/routes", nil, &rv)
	if len(rv.Roles) != len(event.Roles) || len(rv.Providers[event.Codex].Models) == 0 || rv.Dirty {
		t.Fatalf("routes = %+v", rv)
	}
	model := "sonnet"
	env.call("POST", "/api/routes", map[string]any{"role": "worker", "provider": "claude", "model": model, "effort": "high"}, &rv)
	for _, r := range rv.Roles {
		if r.Role == "worker" && (r.Claude.Model != "sonnet" || r.Claude.Effort != "high") {
			t.Errorf("worker claude = %+v", r.Claude)
		}
	}
	if !rv.Dirty {
		t.Error("not marked unsaved")
	}
	env.call("POST", "/api/routes", map[string]any{"role": "worker", "prefer": "claude"}, &rv)
	if env.srv.store.Get().Roles["worker"].Prefer != "claude" {
		t.Error("prefer not applied")
	}
	if res, _ := env.do("POST", "/api/routes", map[string]any{"role": "nope", "prefer": "claude"}, nil); res.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown role: %d", res.StatusCode)
	}
	var sv settingsView
	env.call("POST", "/api/settings", map[string]any{"review_changes": true, "verify": []string{" go test ./... ", ""}, "max_threads": 5}, &sv)
	if !sv.ReviewChanges || len(sv.Verify) != 1 || sv.Verify[0] != "go test ./..." || sv.MaxThreads != 5 {
		t.Errorf("settings = %+v", sv)
	}
	if res, _ := env.do("POST", "/api/settings", map[string]any{"max_threads": 0}, nil); res.StatusCode != http.StatusBadRequest {
		t.Errorf("threads 0: %d", res.StatusCode)
	}
	env.call("POST", "/api/config/save", map[string]any{}, nil)
	if env.state().Dirty {
		t.Error("still dirty after save")
	}
	saved, _, err := config.Load(env.srv.store.Path())
	if err != nil || !saved.Orchestrator.ReviewChanges || saved.Roles["worker"].Claude.Model != "sonnet" {
		t.Errorf("saved config: %v %+v", err, saved.Roles["worker"])
	}
	env.call("POST", "/api/limit", map[string]string{"provider": "codex", "action": "set"}, nil)
	if env.state().Providers["codex"].LimitedUntil == nil {
		t.Error("codex not limited")
	}
	env.call("POST", "/api/limit", map[string]string{"provider": "codex", "action": "reset"}, nil)
	if env.state().Providers["codex"].LimitedUntil != nil {
		t.Error("codex still limited")
	}
}

func TestStatsHistoryEndpoints(t *testing.T) {
	env := newEnv(t, func(c *config.Config) { c.LogDir = t.TempDir() })
	var sv statsView
	env.call("GET", "/api/stats?since=7d&here=1", nil, &sv)
	if sv.Tasks != 0 || sv.Suggestions == nil {
		t.Errorf("stats = %+v", sv)
	}
	if res, _ := env.do("GET", "/api/stats?since=xyz", nil, nil); res.StatusCode != http.StatusBadRequest {
		t.Errorf("bad since: %d", res.StatusCode)
	}
	var rows []historyRow
	env.call("GET", "/api/history", nil, &rows)
	if res, _ := env.do("POST", "/api/resume", map[string]string{"id": "no-such-task"}, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("resume unknown: %d", res.StatusCode)
	}
}

// TestEventStreamReplay connects after a task ran: the page gets the state,
// a reset, the task's events and "synced".
func TestEventStreamReplay(t *testing.T) {
	env := newEnv(t, nil)
	env.call("POST", "/api/task", map[string]string{"text": "fix the parser"}, nil)
	waitFor(t, "the task", func() bool { return env.count(event.TaskDone) == 1 && !env.state().Running })
	// The pump forwards asynchronously: wait until the replay has TaskDone.
	waitFor(t, "the replay", func() bool {
		env.srv.hub.mu.Lock()
		defer env.srv.hub.mu.Unlock()
		for _, f := range env.srv.hub.replay {
			if bytes.Contains(f, []byte(`"kind":"task_done"`)) {
				return true
			}
		}
		return false
	})
	req, _ := http.NewRequest("GET", env.base+"/api/events", nil)
	req.Header.Set(TokenHeader, env.srv.Token())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var names []string
	kinds := map[string]bool{}
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			names = append(names, strings.TrimPrefix(line, "event: "))
		}
		if strings.HasPrefix(line, "data: ") && len(names) > 0 && names[len(names)-1] == "ev" {
			var e struct {
				Kind string `json:"kind"`
			}
			json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e)
			kinds[e.Kind] = true
		}
		if len(names) > 0 && names[len(names)-1] == "synced" {
			break
		}
	}
	if len(names) < 4 || names[0] != "state" || names[1] != "reset" || names[len(names)-1] != "synced" {
		t.Fatalf("stream order: %v", names)
	}
	for _, k := range []string{"task_start", "route", "started", "done", "task_done"} {
		if !kinds[k] {
			t.Errorf("replay lacks %s events", k)
		}
	}
	n, ever, _ := env.srv.hub.connections()
	if n != 1 || !ever {
		t.Errorf("connections = %d ever=%v", n, ever)
	}
}

func TestDesktopAlertOnlyWithoutPage(t *testing.T) {
	env := newEnv(t, nil)
	env.srv.opt.Demo = false // alerts are off in demo mode
	var mu sync.Mutex
	var got []string
	old := sendNotify
	sendNotify = func(title, body string) error {
		mu.Lock()
		got = append(got, title)
		mu.Unlock()
		return nil
	}
	defer func() { sendNotify = old }()
	env.srv.desktopAlert("no page", "x")
	c, _ := env.srv.hub.subscribe()
	env.srv.desktopAlert("with page", "x")
	env.srv.hub.unsubscribe(c)
	waitFor(t, "the alert", func() bool { mu.Lock(); defer mu.Unlock(); return len(got) >= 1 })
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "no page" {
		t.Errorf("alerts = %v", got)
	}
}

func TestHubDropsSlowClient(t *testing.T) {
	h := newHub()
	c, _ := h.subscribe()
	for i := 0; i < clientBuffer+5; i++ {
		h.publish([]byte("x"), true, false)
	}
	if n, _, _ := h.connections(); n != 0 {
		t.Fatal("a client that cannot keep up was not dropped")
	}
	// Its channel is closed (the handler returns, the page reconnects).
	for range c.ch {
	}
	// The replay keeps the newest frames, and a new task trims old context.
	for i := 0; i < maxReplay; i++ {
		h.publish([]byte("y"), true, false)
	}
	if len(h.replay) > maxReplay {
		t.Fatalf("replay grew to %d", len(h.replay))
	}
	h.publish([]byte("start"), true, true)
	if len(h.replay) != keepBeforeTask+1 {
		t.Fatalf("after a task start the replay has %d frames, want %d", len(h.replay), keepBeforeTask+1)
	}
}

func TestAppCommand(t *testing.T) {
	url := "http://127.0.0.1:1234/?t=x"
	none := func(string) (string, error) { return "", io.EOF }
	env := map[string]string{"ProgramFiles(x86)": `C:\PF86`, "ProgramFiles": `C:\PF`, "LOCALAPPDATA": `C:\LAD`, "HOME": "/Users/me"}
	getenv := func(k string) string { return env[k] }

	edge86 := filepath.Join(`C:\PF86`, "Microsoft", "Edge", "Application", "msedge.exe")
	name, args, label, ok := appCommand("windows", url, getenv, func(p string) bool { return p == edge86 }, none)
	if !ok || name != edge86 || label != "Edge" || args[0] != "--app="+url || args[1] != "--new-window" {
		t.Errorf("windows edge: %s %v %s %v", name, args, label, ok)
	}
	chromeLocal := filepath.Join(`C:\LAD`, "Google", "Chrome", "Application", "chrome.exe")
	name, _, label, ok = appCommand("windows", url, getenv, func(p string) bool { return p == chromeLocal }, none)
	if !ok || name != chromeLocal || label != "Chrome" {
		t.Errorf("windows chrome fallback: %s %s %v", name, label, ok)
	}
	name, _, _, ok = appCommand("windows", url, getenv, func(string) bool { return false }, func(b string) (string, error) {
		if b == "msedge" {
			return `D:\edge\msedge.exe`, nil
		}
		return "", io.EOF
	})
	if !ok || name != `D:\edge\msedge.exe` {
		t.Errorf("windows PATH: %s %v", name, ok)
	}
	if _, _, _, ok = appCommand("windows", url, getenv, func(string) bool { return false }, none); ok {
		t.Error("windows without a browser found one")
	}

	name, args, _, ok = appCommand("darwin", url, getenv, func(p string) bool { return p == "/Applications/Microsoft Edge.app" }, none)
	if !ok || name != "open" || strings.Join(args[:3], "|") != "-na|Microsoft Edge|--args" || args[3] != "--app="+url {
		t.Errorf("darwin: %s %v %v", name, args, ok)
	}
	name, args, _, ok = appCommand("linux", url, getenv, func(string) bool { return false }, func(b string) (string, error) {
		if b == "chromium" {
			return "/usr/bin/chromium", nil
		}
		return "", io.EOF
	})
	if !ok || name != "/usr/bin/chromium" || args[0] != "--app="+url {
		t.Errorf("linux: %s %v %v", name, args, ok)
	}

	for goos, want := range map[string]string{"windows": "rundll32", "darwin": "open", "linux": "xdg-open"} {
		n, a := browserCommand(goos, url)
		if n != want || a[len(a)-1] != url {
			t.Errorf("browserCommand(%s) = %s %v", goos, n, a)
		}
	}
}

func TestStaticAssetsEmbedded(t *testing.T) {
	env := newEnv(t, nil)
	for _, p := range []string{"/assets/app.js", "/assets/app.css", "/assets/theme.js"} {
		res, data := env.do("GET", p, nil, nil)
		if res.StatusCode != 200 || len(data) < 100 {
			t.Errorf("%s: %d (%d bytes)", p, res.StatusCode, len(data))
		}
	}
	// No external resources: the page must work offline.
	for _, p := range []string{"/", "/assets/app.js", "/assets/app.css"} {
		_, data := env.do("GET", p, nil, nil)
		for _, bad := range []string{"https://", "http://cdn", "//fonts.", "googleapis"} {
			if bytes.Contains(data, []byte(bad)) {
				t.Errorf("%s references %q", p, bad)
			}
		}
	}
}
