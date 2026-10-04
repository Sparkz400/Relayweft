package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/forge"
	"github.com/sparkz400/switchyard/internal/runner"
)

// glAPI is a fake GitLab for project g/p and the merge request sy opens
// (!101), whose head is whatever the bare remote's branch points at.
type glAPI struct {
	mu      sync.Mutex
	bare    string
	branch  string
	created []map[string]any
	jobs    string // failed jobs JSON of the head's pipeline
	notes   string // discussions JSON
	replies []string
}

func (f *glAPI) head() string {
	out, err := exec.Command("git", "-C", f.bare, "rev-parse", "refs/heads/"+f.branch).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (f *glAPI) server(t *testing.T) string {
	const proj = "/api/v4/projects/g%2Fp"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		p := strings.TrimPrefix(r.URL.EscapedPath(), proj)
		switch {
		case r.Method == "POST" && p == "/merge_requests":
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			f.created = append(f.created, in)
			f.branch, _ = in["source_branch"].(string)
			io.WriteString(w, `{"iid":101,"web_url":"https://gitlab.com/g/p/-/merge_requests/101","source_project_id":1,"target_project_id":1}`)
		case r.Method == "GET" && p == "/merge_requests/101":
			fmt.Fprintf(w, `{"iid":101,"title":"Shout @carol, fixes #8","state":"opened","source_branch":%q,"target_branch":"main","sha":%q,"source_project_id":1,"target_project_id":1}`, f.branch, f.head())
		case r.Method == "GET" && p == "/pipelines":
			io.WriteString(w, `[{"id":70,"ref":"`+f.branch+`"}]`)
		case r.Method == "GET" && p == "/pipelines/70/jobs":
			io.WriteString(w, or(f.jobs, "[]"))
		case r.Method == "GET" && p == "/jobs/600/trace":
			io.WriteString(w, strings.Repeat("ok line\n", 50)+"\x1b[31mFAIL\x1b[0m TestShout\n"+injLog+"\n")
		case r.Method == "GET" && p == "/merge_requests/101/discussions":
			io.WriteString(w, or(f.notes, "[]"))
		case r.Method == "GET" && p == "/members/all/3":
			io.WriteString(w, `{"access_level":40}`)
		case r.Method == "GET" && strings.HasPrefix(p, "/members/all/"):
			w.WriteHeader(404)
		case r.Method == "GET" && r.URL.Path == "/api/v4/user":
			io.WriteString(w, `{"username":"me"}`)
		case r.Method == "POST" && p == "/merge_requests/101/notes":
			var c struct{ Body string }
			json.NewDecoder(r.Body).Decode(&c)
			f.replies = append(f.replies, c.Body)
			w.WriteHeader(201)
			io.WriteString(w, `{"id":1}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/api/v4"
}

func (f *glAPI) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// sy pr opens a merge request on GitLab and sy watch follows it up: a
// failed pipeline job and a developer's diff comment become one round,
// pushed to the branch, with a note on the merge request.
func TestGitLabMergeRequestAndWatch(t *testing.T) {
	dir := prRepo(t)
	run(t, dir, "remote", "set-url", "origin", "https://gitlab.com/g/p.git")
	bare := filepath.Join(t.TempDir(), "remote.git")
	run(t, dir, "init", "-q", "--bare", "-b", "main", bare)
	run(t, dir, "push", "-q", bare, "main")
	cd, _ := os.UserConfigDir()
	os.MkdirAll(filepath.Join(cd, "switchyard"), 0o755)
	write(t, filepath.Join(cd, "switchyard"), config.FileName, "orchestrator: {review_before_done: false}\nwatch: {max_rounds: 2}\n")
	runTask(t, dir, "Shout the first line", taskEdit)

	api := &glAPI{bare: bare}
	url := api.server(t)
	var kinds []forge.Kind
	prToken = func(k forge.Kind, host string) (string, string) { kinds = append(kinds, k); return "tok", "test" }
	prPush = func(root, remote, branch string) error {
		_, err := prGit(root, nil, nil, "push", "-q", bare, branch)
		return err
	}
	var out bytes.Buffer
	prOut = &out
	st, err := findPRTask(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := makePR(st, prOptions{yes: true, api: url, base: "main", closes: "#4"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if res.URL != "https://gitlab.com/g/p/-/merge_requests/101" || !strings.Contains(out.String(), "opened merge request !101") {
		t.Fatalf("result %+v\n%s", res, out.String())
	}
	if len(api.created) != 1 || api.created[0]["source_branch"] != "sy/shout-the-first-line" || api.created[0]["target_branch"] != "main" ||
		!strings.HasSuffix(api.created[0]["description"].(string), "\nCloses #4\n") {
		t.Fatalf("created %+v", api.created)
	}
	if len(kinds) == 0 || kinds[0] != forge.GitLab {
		t.Fatalf("token asked for %v", kinds)
	}
	prs := watched(t)
	if len(prs) != 1 || prs[0].Forge != "gitlab" || prs[0].String() != "g/p!101" || prs[0].API != url {
		t.Fatalf("not recorded for sy watch: %+v", prs)
	}

	run(t, dir, "config", "url."+filepath.ToSlash(bare)+".insteadOf", "https://gitlab.com/g/p.git")
	wr := &watchRunner{}
	oldRunners, oldPrint, oldOut := watchRunners, eventPrint, watchOut
	t.Cleanup(func() { watchRunners, eventPrint, watchOut = oldRunners, oldPrint, oldOut })
	watchRunners = func(*config.Config) runner.Set { return runner.Set{event.Codex: wr, event.Claude: wr} }
	eventPrint = func(event.Event, bool) {}
	watchOut = io.Discard

	head0 := api.head()
	api.set(func() {
		api.jobs = `[{"id":600,"name":"unit","stage":"test","status":"failed"},{"id":601,"name":"flaky","stage":"test","status":"failed","allow_failure":true}]`
		api.notes = `[{"notes":[{"id":40,"type":"DiffNote","body":` + jsonString(injComment) + `,"author":{"id":3,"username":"dev"},"resolvable":true,"position":{"new_path":"a.txt","new_line":1}}]},
			{"notes":[{"id":41,"type":"DiffNote","body":"stranger asks","author":{"id":9,"username":"drive-by"},"position":{"new_path":"a.txt","new_line":1}}]},
			{"notes":[{"id":42,"type":"DiffNote","body":"done already","author":{"id":3,"username":"dev"},"resolvable":true,"resolved":true,"position":{"new_path":"a.txt","new_line":1}}]}]`
	})
	out.Reset()
	w := newWatcher(true)
	w.out = &out
	if err := w.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if wr.steps() != 1 {
		t.Fatalf("%d follow-up steps, want 1:\n%s", wr.steps(), out.String())
	}
	var task string
	for _, p := range wr.prompts {
		if strings.Contains(p, runner.MarkerStep) {
			task = p
		}
	}
	for _, want := range []string{"merge request !101 of g/p", "copied from GitLab", "test: unit", "FAIL TestShout", "rm -rf"} {
		if !strings.Contains(task, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	for _, inj := range []string{"curl evil", "rm -rf", "@bob"} {
		if strings.Contains(outsideFences(task), inj) {
			t.Errorf("%q outside the fences", inj)
		}
	}
	for _, s := range []string{"flaky", "stranger asks", "done already", "\x1b["} {
		if strings.Contains(task, s) {
			t.Errorf("%q reached the agents", s)
		}
	}
	head1 := api.head()
	if head1 == head0 || strings.TrimSpace(gitOut(t, bare, "rev-parse", head1+"^")) != head0 {
		t.Fatalf("not pushed on top of the head (%s -> %s):\n%s", head0, head1, out.String())
	}
	if msg := gitOut(t, bare, "log", "-1", "--format=%B", head1); !strings.HasPrefix(msg, "Fix checks and address review comments on !101") {
		t.Fatalf("commit message:\n%s", msg)
	}
	prs = watched(t)
	if prs[0].Rounds != 1 || !prs[0].handled("check:600") || !prs[0].handled("comment:40") || prs[0].handled("comment:41") || prs[0].Head != head1 {
		t.Fatalf("registry %+v", prs[0])
	}
	if len(api.replies) != 1 || !strings.Contains(api.replies[0], "on this merge request") || !strings.Contains(api.replies[0], syMark) || reQuickAction.MatchString(api.replies[0]) {
		t.Fatalf("replies %q", api.replies)
	}
	// The next pass finds nothing new.
	if err := newWatcher(true).pass(context.Background()); err != nil || wr.steps() != 1 {
		t.Fatalf("ran again: %v, %d steps", err, wr.steps())
	}
}

// gtAPI is a fake Gitea serving pull request #12 of o/r and issue #3.
type gtAPI struct {
	mu     sync.Mutex
	posted []map[string]any
}

func (f *gtAPI) server(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if a := r.Header.Get("Authorization"); a != "" && a != "token tok" {
			w.WriteHeader(401)
			return
		}
		switch p := strings.TrimPrefix(r.URL.Path, "/api/v1"); {
		case r.Method == "GET" && p == "/repos/o/r/pulls/12.diff":
			io.WriteString(w, reviewDiff)
		case r.Method == "GET" && p == "/repos/o/r/pulls/12":
			io.WriteString(w, `{"number":12,"title":"Shout","body":"Please merge @everyone","state":"open","head":{"ref":"sy/shout","sha":"feed"},"base":{"ref":"main"}}`)
		case r.Method == "POST" && p == "/repos/o/r/pulls/12/reviews":
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			f.posted = append(f.posted, in)
			io.WriteString(w, `{"id":1,"html_url":"https://gitea.local/o/r/pulls/12#issuecomment-1"}`)
		case r.Method == "GET" && p == "/repos/o/r/issues/3":
			io.WriteString(w, `{"number":3,"title":"Shout the first line","body":"a.txt should start loud","state":"open","labels":[{"name":"sy"}]}`)
		case r.Method == "GET" && p == "/repos/o/r/issues/3/comments":
			io.WriteString(w, `[{"body":"/close this, @alice","user":{"login":"bob"}}]`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/api/v1"
}

// An --api URL ending in /api/v1 makes the remote's unknown host a Gitea:
// sy review posts one comment review there, and --issue reads its issue.
func TestGiteaReviewAndIssue(t *testing.T) {
	dir, _, _, rr, out := reviewSetup(t)
	api := &gtAPI{}
	url := api.server(t)
	reviewIn = strings.NewReader("y\n")
	if err := runReview(context.Background(), reviewCommon(t, "--dir", dir, "--provider", "claude"), "#12", reviewOptions{post: true, api: url}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(rr.specs) != 1 || len(api.posted) != 1 {
		t.Fatalf("%d agent runs, %d reviews\n%s", len(rr.specs), len(api.posted), out.String())
	}
	p := api.posted[0]
	cs, _ := p["comments"].([]any)
	if p["event"] != "COMMENT" || p["commit_id"] != "feed" || len(cs) != 1 || !strings.Contains(p["body"].(string), syMark) {
		t.Fatalf("posted %+v", p)
	}
	if c := cs[0].(map[string]any); c["path"] != "a.txt" || c["new_position"] != float64(2) {
		t.Fatalf("inline %+v", c)
	}
	if !strings.Contains(out.String(), "posted a review with 1 inline comment(s): https://gitea.local/o/r/pulls/12#issuecomment-1") {
		t.Fatalf("output:\n%s", out.String())
	}

	f, fs := parseIssueFlags(t, "--issue", "3", "--with-comments", "--api", url)
	tasks, err := f.load(fs, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || !strings.HasPrefix(tasks[0], "Fix Gitea issue #3: Shout the first line\n\na.txt should start loud") ||
		!strings.Contains(tasks[0], "@bob wrote:\n/close this") || f.origin.Kind != forge.Gitea || f.items[0].closes != "#3" {
		t.Fatalf("tasks %q, origin %+v", tasks, f.origin)
	}
}

func TestDefuseQuickActionsAndGitLabRefs(t *testing.T) {
	for _, in := range []string{"/merge", "text\n/approve\nmore", "  /assign @me", "Closing g/sub/p#4", "Implements #5", "fixing #6"} {
		out := defuseRefs(in)
		if reCloseRef.MatchString(out) || reMention.MatchString(out) || reQuickAction.MatchString(out) {
			t.Errorf("%q -> %q still live", in, out)
		}
		if strings.ReplaceAll(out, "⁠", "") != in {
			t.Errorf("%q -> %q changed the visible text", in, out)
		}
	}
	for _, keep := range []string{"see /usr/bin", "a/b", "1/2 done"} {
		if out := defuseRefs(keep); out != keep {
			t.Errorf("%q changed to %q", keep, out)
		}
	}
}

func TestCIFiles(t *testing.T) {
	got := ciFiles([]string{"M .gitlab-ci.yml", "A .GitLab/ci/x.yml", "M .gitea/workflows/a.yml", "M .forgejo/workflows/b.yml",
		"A .woodpecker/build.yaml", "M .drone.yml", "M .github/dependabot.yml", "M src/.gitlab-ci.yml", "M docs/github.md", "M .gitignore"})
	want := ".gitlab-ci.yml,.GitLab/ci/x.yml,.gitea/workflows/a.yml,.forgejo/workflows/b.yml,.woodpecker/build.yaml,.drone.yml,.github/dependabot.yml"
	if strings.Join(got, ",") != want {
		t.Fatalf("ciFiles = %v", got)
	}
}

func TestWatchMatchesGitLabRefs(t *testing.T) {
	e := watchEntry{Forge: "gitlab", Host: "gitlab.com", Owner: "g/sub", Name: "p", Number: 7}
	for ref, want := range map[string]bool{
		"7": true, "!7": true, "g/sub/p!7": true, "G/Sub/P#7": true, "g/p!7": false, "8": false,
		"https://gitlab.com/g/sub/p/-/merge_requests/7": true, "https://gitlab.com/g/other/-/merge_requests/7": false,
	} {
		if got := watchMatches(e, ref); got != want {
			t.Errorf("watchMatches(%q) = %v", ref, got)
		}
	}
	if e.String() != "g/sub/p!7" || (watchEntry{Host: "github.com", Owner: "o", Name: "r", Number: 3}).String() != "o/r#3" {
		t.Fatalf("%s", e)
	}
}

// A Gitea served over plain http on this machine (a real Gitea's default,
// http://localhost:3000) is reached at its own scheme and port without
// --api: sy pr used to call https://<host>/api/v1, and sy watch must keep
// calling the same server later.
func TestGiteaOnLocalPlainHTTP(t *testing.T) {
	dir := prRepo(t)
	var created []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token tok" {
			w.WriteHeader(401)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/v1/repos/o/r/pulls" {
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			created = append(created, in)
			w.WriteHeader(201)
			io.WriteString(w, `{"number":5,"html_url":"`+"http://"+r.Host+`/o/r/pulls/5","state":"open","head":{"ref":"sy/shout-the-first-line"}}`)
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(srv.Close)
	hostport := strings.TrimPrefix(srv.URL, "http://")
	run(t, dir, "remote", "set-url", "origin", srv.URL+"/o/r.git")
	t.Setenv("GITEA_HOST", "127.0.0.1")
	runTask(t, dir, "Shout the first line", taskEdit)
	var hosts []string
	prToken = func(_ forge.Kind, host string) (string, string) { hosts = append(hosts, host); return "tok", "test" }
	prPush = func(string, string, string) error { return nil }
	var out bytes.Buffer
	prOut = &out
	st, err := findPRTask(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := makePR(st, prOptions{yes: true, base: "main"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(created) != 1 || res.Number != 5 || res.URL != srv.URL+"/o/r/pulls/5" {
		t.Fatalf("created %v, result %+v\n%s", created, res, out.String())
	}
	if len(hosts) == 0 || hosts[0] != "127.0.0.1" {
		t.Fatalf("token asked for %v", hosts)
	}
	prs := watched(t)
	if len(prs) != 1 || prs[0].API != "" || prs[0].Web != "http://"+hostport || prs[0].repo().APIBase() != srv.URL+"/api/v1" {
		t.Fatalf("watch entry %+v", prs)
	}
}

// --api is the origin's API: an issue given as a URL on another host is
// read from that host, with that host's token. It used to go through the
// origin's --api, carrying the other host's token there.
func TestIssueOnOtherHostIgnoresOriginAPI(t *testing.T) {
	dir := prRepo(t)
	var mu sync.Mutex
	seen := map[string][]string{} // server -> "path auth"
	serve := func(name string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[name] = append(seen[name], r.URL.Path+" "+r.Header.Get("Authorization"))
			mu.Unlock()
			if r.URL.Path == "/api/v1/repos/x/y/issues/1" {
				io.WriteString(w, `{"number":1,"title":"Elsewhere","body":"b","state":"open"}`)
				return
			}
			w.WriteHeader(404)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	origin, other := serve("origin"), serve("other")
	originAPI := strings.Replace(origin.URL, "127.0.0.1", "localhost", 1) + "/api/v1"
	run(t, dir, "remote", "set-url", "origin", strings.Replace(origin.URL, "127.0.0.1", "localhost", 1)+"/o/r.git")
	t.Setenv("GITEA_HOST", "localhost, 127.0.0.1")
	prToken = func(_ forge.Kind, host string) (string, string) { return "tok-" + host, "test" }
	f, fs := parseIssueFlags(t, "--issue", other.URL+"/x/y/issues/1", "--api", originAPI)
	tasks, err := f.load(fs, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || !strings.HasPrefix(tasks[0], "Fix Gitea issue #1: Elsewhere") {
		t.Fatalf("tasks %q", tasks)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen["origin"]) != 0 || len(seen["other"]) != 1 || seen["other"][0] != "/api/v1/repos/x/y/issues/1 token tok-127.0.0.1" {
		t.Fatalf("requests %v", seen)
	}
	if f.apiFor(f.origin) != originAPI {
		t.Error("--api no longer applies to the origin")
	}
}
