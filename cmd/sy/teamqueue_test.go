package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/forge"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// teamComment is a comment on the fake forge.
type teamComment struct {
	ID    int64                  `json:"id"`
	Body  string                 `json:"body"`
	User  struct{ Login string } `json:"user"`
	Assoc string                 `json:"author_association"`
}

// fakeTeam is a GitHub with issue comments that can be listed, posted and
// edited. beforePost runs (under the lock) before a comment is added, so a
// test can slip in another machine's claim.
type fakeTeam struct {
	mu         sync.Mutex
	issues     map[int]string
	list       string
	pulls      string
	comments   map[int][]teamComment
	nextID     int64
	created    int
	beforePost func(n int)
}

func (f *fakeTeam) add(n int, author, assoc, body string) {
	f.nextID++
	c := teamComment{ID: f.nextID, Body: body, Assoc: assoc}
	c.User.Login = author
	if f.comments == nil {
		f.comments = map[int][]teamComment{}
	}
	f.comments[n] = append(f.comments[n], c)
}

func (f *fakeTeam) bodies(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.comments[n] {
		out = append(out, c.Body)
	}
	return out
}

func (f *fakeTeam) server(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method != "GET" && r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		p := r.URL.Path
		var n int
		var id int64
		switch {
		case r.Method == "GET" && p == "/user":
			io.WriteString(w, `{"login":"me"}`)
		case r.Method == "GET" && p == "/repos/o/r":
			io.WriteString(w, `{"default_branch":"main"}`)
		case r.Method == "GET" && p == "/repos/o/r/issues":
			io.WriteString(w, f.list)
		case r.Method == "GET" && p == "/repos/o/r/pulls":
			io.WriteString(w, f.pulls)
		case r.Method == "POST" && p == "/repos/o/r/pulls":
			f.created++
			fmt.Fprintf(w, `{"number":%d,"html_url":"https://github.com/o/r/pull/%d"}`, 100+f.created, 100+f.created)
		case scan(p, "/repos/o/r/issues/comments/%d", &id) && r.Method == "PATCH":
			var in struct{ Body string }
			json.NewDecoder(r.Body).Decode(&in)
			for k, cs := range f.comments {
				for i := range cs {
					if cs[i].ID == id {
						f.comments[k][i].Body = in.Body
						io.WriteString(w, `{}`)
						return
					}
				}
			}
			w.WriteHeader(404)
		case scan(p, "/repos/o/r/issues/%d/comments", &n) && r.Method == "GET":
			json.NewEncoder(w).Encode(append([]teamComment{}, f.comments[n]...))
		case scan(p, "/repos/o/r/issues/%d/comments", &n) && r.Method == "POST":
			var in struct{ Body string }
			json.NewDecoder(r.Body).Decode(&in)
			if f.beforePost != nil {
				f.beforePost(n)
			}
			f.add(n, "me", "OWNER", in.Body)
			w.WriteHeader(201)
			io.WriteString(w, `{}`)
		case scan(p, "/repos/o/r/issues/%d", &n) && r.Method == "GET":
			if js, ok := f.issues[n]; ok {
				io.WriteString(w, js)
				return
			}
			w.WriteHeader(404)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// scan matches a path exactly against a pattern with one number.
func scan(p, pattern string, v any) bool {
	if _, err := fmt.Sscanf(p, pattern, v); err != nil {
		return false
	}
	return fmt.Sprintf(pattern, deref(v)) == p
}

func deref(v any) any {
	switch x := v.(type) {
	case *int:
		return *x
	case *int64:
		return *x
	}
	return nil
}

// otherClaim is another machine's claim comment.
func otherClaim(id, machine, state string, until time.Time) string {
	return "Switchyard is working on this issue.\n\n" + queueMarker(id, machine, state, until)
}

func teamSetup(t *testing.T) (dir string, api *fakeTeam, url string) {
	dir = prRepo(t)
	prToken = func(forge.Kind, string) (string, string) { return "tok", "test" }
	prPush = func(root, remote, branch string) error { return nil }
	idFile := filepath.Join(t.TempDir(), "machine-id")
	old := sessionlog.MachineIDFile
	sessionlog.MachineIDFile = func() string { return idFile }
	t.Cleanup(func() { sessionlog.MachineIDFile = old })
	api = &fakeTeam{
		issues: map[int]string{
			3: `{"number":3,"title":"Shout the first line","body":"a.txt should start loud","state":"open"}`,
			9: `{"number":9,"title":"Add a second file","body":"please","state":"open"}`,
		},
		list:  `[{"number":3,"title":"Shout the first line"},{"number":9,"title":"Add a second file"}]`,
		pulls: `[]`,
	}
	return dir, api, api.server(t)
}

func teamFlags(t *testing.T, dir, url string, extra ...string) *issueFlags {
	t.Helper()
	f, fs := parseIssueFlags(t, append([]string{"--issues", "label:sy", "--pr", "--team", "--api", url}, extra...)...)
	if err := f.prepare(fs, dir); err != nil {
		t.Fatal(err)
	}
	return f
}

func testQueue(t *testing.T, f *issueFlags) *teamQueue {
	t.Helper()
	q, err := newTeamQueue(f)
	if err != nil {
		t.Fatal(err)
	}
	q.settle = time.Millisecond
	return q
}

func TestEvalQueue(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	later, earlier := now.Add(time.Hour), now.Add(-time.Minute)
	c := func(id int64, author, body string) forge.Comment {
		return forge.Comment{ID: id, Author: author, Body: body}
	}
	trusted := func(c forge.Comment) bool { return c.Author != "stranger" }
	cs := []forge.Comment{
		c(1, "alice", "plain comment"),
		c(2, "alice", otherClaim("00000000000000aa", "m1", claimHeld, earlier)), // lapsed
		c(3, "stranger", otherClaim("00000000000000bb", "evil", claimHeld, later)),
		c(4, "bob", otherClaim("00000000000000cc", "m2", claimHeld, later)),
		c(5, "alice", otherClaim("00000000000000dd", "m3", claimHeld, later)),
		c(6, "alice", otherClaim("00000000000000ee", "m4", claimLost, now)),
	}
	v := evalQueue(cs, trusted, now)
	if v.holder == nil || v.holder.machine != "m2" || v.holder.comment != 4 {
		t.Fatalf("holder = %+v (want m2: the earliest live trusted claim)", v.holder)
	}
	if v.last == nil || v.last.machine != "m3" {
		t.Fatalf("last = %+v (lost claims do not count)", v.last)
	}
	failed := []forge.Comment{c(1, "alice", otherClaim("00000000000000aa", "m1", claimFailed, earlier))}
	if v := evalQueue(failed, trusted, now); v.holder != nil || v.last.state != claimFailed {
		t.Fatalf("failed: %+v", v)
	}
	if v := evalQueue(cs[:3], trusted, now); v.holder != nil {
		t.Fatalf("a lapsed claim and a stranger's hold nothing: %+v", v.holder)
	}
}

func TestTeamTakeAndEnd(t *testing.T) {
	dir, api, url := teamSetup(t)
	q := testQueue(t, teamFlags(t, dir, url))

	cl, skip, err := q.take(3)
	if err != nil || cl == nil {
		t.Fatalf("take: %v %q", err, skip)
	}
	if b := api.bodies(3); len(b) != 1 || !strings.Contains(b[0], "state=claimed") || !strings.Contains(b[0], "machine="+q.machine) || !strings.Contains(b[0], syMark) {
		t.Fatalf("claim comment: %q", b)
	}
	// A second machine (same account) sees the claim and stands aside
	// without posting anything.
	q2 := testQueue(t, teamFlags(t, dir, url))
	q2.machine = "othermachine"
	if cl2, skip, err := q2.take(3); cl2 != nil || err != nil || !strings.Contains(skip, "holds it") {
		t.Fatalf("second machine: %v %q %v", cl2, skip, err)
	}
	if len(api.bodies(3)) != 1 {
		t.Fatalf("second machine posted: %q", api.bodies(3))
	}

	cl.end(claimDone, "Pull request: https://github.com/o/r/pull/101")
	b := api.bodies(3)
	if len(b) != 1 || !strings.Contains(b[0], "state=done") || !strings.Contains(b[0], "pull/101") {
		t.Fatalf("result: %q", b)
	}
	cl.end(claimFailed, "twice") // a second end changes nothing
	if b2 := api.bodies(3); b2[0] != b[0] {
		t.Fatalf("second end edited: %q", b2)
	}
	// Done frees the issue (its pull request keeps it out of the queue).
	if cl2, skip, _ := q2.take(3); cl2 == nil {
		t.Fatalf("after done: %q", skip)
	} else {
		cl2.end(claimFailed, "boom")
	}
	// Failed stays out of the queue unless --retry-failed.
	if cl3, skip, _ := q.take(3); cl3 != nil || !strings.Contains(skip, "failed on machine othermachine") {
		t.Fatalf("failed issue taken: %q", skip)
	}
	q.retryFailed = true
	if cl3, _, _ := q.take(3); cl3 == nil {
		t.Fatal("--retry-failed did not take it")
	} else {
		cl3.end(claimReleased, "")
	}
}

// Two machines claim at once: the earlier comment wins, and the later
// machine marks its own claim lost.
func TestTeamTakeRace(t *testing.T) {
	dir, api, url := teamSetup(t)
	q := testQueue(t, teamFlags(t, dir, url))
	api.beforePost = func(n int) {
		api.add(n, "teammate", "MEMBER", otherClaim("0123456789abcdef", "fastmachine", claimHeld, time.Now().Add(time.Hour)))
	}
	cl, skip, err := q.take(9)
	if cl != nil || err != nil || !strings.Contains(skip, "fastmachine claimed it first") {
		t.Fatalf("race: %v %q %v", cl, skip, err)
	}
	b := api.bodies(9)
	if len(b) != 2 || !strings.Contains(b[1], "state=lost") || !strings.Contains(b[0], "state=claimed") {
		t.Fatalf("comments: %q", b)
	}
	// A stranger's claim in the same spot holds nothing.
	api.beforePost = func(n int) {
		api.add(n, "stranger", "NONE", otherClaim("0123456789abcdee", "evil", claimHeld, time.Now().Add(time.Hour)))
	}
	if cl, skip, err := q.take(3); cl == nil {
		t.Fatalf("stranger's claim won: %q %v", skip, err)
	} else {
		cl.end(claimReleased, "")
	}
}

func TestTeamRenewsLease(t *testing.T) {
	dir, api, url := teamSetup(t)
	q := testQueue(t, teamFlags(t, dir, url))
	q.lease = 30 * time.Millisecond
	cl, _, err := q.take(3)
	if err != nil || cl == nil {
		t.Fatal(err)
	}
	first := api.bodies(3)[0]
	deadline := time.Now().Add(2 * time.Second)
	for api.bodies(3)[0] == first {
		if time.Now().After(deadline) {
			t.Fatal("the claim was not renewed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cl.end(claimReleased, "")
	if b := api.bodies(3)[0]; !strings.Contains(b, "state=released") {
		t.Fatalf("after end: %q", b)
	}
}

// A whole pass: both issues are claimed one at a time, run, turned into
// pull requests, and their claims carry the links; an issue another
// machine holds is left alone.
func TestTeamPass(t *testing.T) {
	dir, api, url := teamSetup(t)
	api.mu.Lock()
	api.issues[5] = `{"number":5,"title":"held","state":"open"}`
	api.list = `[{"number":5,"title":"held"},{"number":3,"title":"Shout the first line"},{"number":9,"title":"Add a second file"}]`
	api.add(5, "teammate", "COLLABORATOR", otherClaim("0123456789abcdef", "busy", claimHeld, time.Now().Add(time.Hour)))
	api.mu.Unlock()

	f := teamFlags(t, dir, url)
	q := testQueue(t, f)
	h := &headless{ctx: context.Background(), cfg: config.Default(), dir: dir}
	var ran []string
	edits := []func(string) []string{taskEdit, func(d string) []string {
		write(t, d, "second.txt", "2\n")
		return []string{"second.txt"}
	}}
	run := func(task string) orchestrator.TaskResult {
		for _, n := range []int{3, 9} {
			if b := api.bodies(n); strings.Contains(task, fmt.Sprintf("#%d:", n)) && (len(b) != 1 || !strings.Contains(b[0], "state=claimed")) {
				t.Errorf("#%d runs without its claim: %q", n, b)
			}
		}
		res := runTask(t, dir, task, edits[len(ran)])
		ran = append(ran, oneLine(task, 40))
		return res
	}
	n, failed, stop, err := teamPass(h, f, q, run)
	if err != nil || stop || n != 2 || failed != 0 {
		t.Fatalf("pass: ran %d failed %d stop %v err %v", n, failed, stop, err)
	}
	if len(ran) != 2 || !strings.Contains(ran[0], "#3") || !strings.Contains(ran[1], "#9") {
		t.Fatalf("ran %q", ran)
	}
	for i, n := range []int{3, 9} {
		b := api.bodies(n)
		if len(b) != 1 || !strings.Contains(b[0], "state=done") || !strings.Contains(b[0], fmt.Sprintf("pull/%d", 101+i)) {
			t.Fatalf("#%d: %q", n, b)
		}
	}
	if b := api.bodies(5); len(b) != 1 {
		t.Fatalf("held issue touched: %q", b)
	}
	if s := gitOut(t, dir, "status", "--porcelain"); s != "" {
		t.Fatalf("tree not clean:\n%s", s)
	}
}

func TestTeamFlags(t *testing.T) {
	dir := prRepo(t)
	for _, args := range [][]string{
		{"--issues", "label:sy", "--pr", "--every", "10m"},
		{"--issues", "label:sy", "--pr", "--retry-failed"},
		{"--issues", "label:sy", "--pr", "--team", "--every", "10s"},
		{"--issues", "label:sy", "--pr", "--team", "--lease", "1m"},
		{"--issue", "3", "--team"},
	} {
		f, fs := parseIssueFlags(t, append(args, "--api", "http://127.0.0.1:1")...)
		if err := f.prepare(fs, dir); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	// Without a token a team cannot claim anything.
	prToken = func(forge.Kind, string) (string, string) { return "", "" }
	f, fs := parseIssueFlags(t, "--issues", "label:sy", "--pr", "--team", "--api", "http://127.0.0.1:1")
	if err := f.prepare(fs, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := newTeamQueue(f); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("no token: %v", err)
	}
}

// Claims are no part of a task's text.
func TestTeamClaimsLeftOutOfTask(t *testing.T) {
	dir, api, url := teamSetup(t)
	api.mu.Lock()
	api.add(3, "bob", "MEMBER", "same here")
	api.add(3, "me", "OWNER", otherClaim("0123456789abcdef", "m", claimDone, time.Now()))
	api.mu.Unlock()
	f := teamFlags(t, dir, url, "--with-comments")
	it, err := f.fetchOne(f.client(f.origin), f.origin, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(it.task, "same here") || strings.Contains(it.task, "switchyard:queue") {
		t.Fatalf("task: %q", it.task)
	}
}

// With --every, a pass that must stop (a dirty working tree) ends the run
// with its error instead of waiting for the next pass.
func TestTeamEveryStopsOnDirtyTree(t *testing.T) {
	dir, _, url := teamSetup(t)
	f := teamFlags(t, dir, url, "--every", "10m")
	write(t, dir, "notes.txt", "mine\n")
	h := &headless{ctx: context.Background(), cfg: config.Default(), dir: dir}
	done := make(chan error, 1)
	go func() {
		_, _, err := runTeam(h, f, true, func(string) orchestrator.TaskResult {
			t.Error("a task ran on a dirty tree")
			return orchestrator.TaskResult{}
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "clean working tree") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runTeam waited for the next pass")
	}
}
