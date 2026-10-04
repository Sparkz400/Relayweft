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
	"sync/atomic"
	"testing"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/forge"
	"github.com/sparkz400/switchyard/internal/gh"
	"github.com/sparkz400/switchyard/internal/limits"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/runner"
	"github.com/sparkz400/switchyard/internal/sysload"
)

// stepRunner edits the working tree on the worker step and approves
// everything else.
type stepRunner struct{ edit func(dir string) []string }

func (x stepRunner) Run(_ context.Context, s runner.Spec, _ func(event.Event)) runner.Result {
	if strings.Contains(s.Prompt, runner.MarkerStep) {
		return runner.Result{Final: "edited", Files: x.edit(s.Dir)}
	}
	return runner.Result{Final: `{"approve": true, "advice": "ok"}`}
}

var taskSeq atomic.Int64

// runTask runs a short task (no planner) in dir with a fake agent that
// calls edit, through the real orchestrator: snapshots, undo refs and
// task state are recorded as in a real run.
func runTask(t *testing.T, dir, text string, edit func(dir string) []string) orchestrator.TaskResult {
	t.Helper()
	cfg := config.Default()
	cfg.Orchestrator.ReviewBeforeDone = false
	cfg.Orchestrator.ApprovePlan = false
	cfg.Verify.Commands = nil
	ch := make(chan event.Event, 256)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range ch {
		}
	}()
	defer func() { close(ch); <-done }()
	r := stepRunner{edit}
	o := orchestrator.New(orchestrator.Options{
		Dir: dir, Store: config.NewStore(cfg, filepath.Join(t.TempDir(), "sy.yaml")),
		Runners: func(*config.Config) runner.Set { return runner.Set{event.Codex: r, event.Claude: r} },
		Tracker: limits.NewTracker(), Events: ch,
		Load:         func() sysload.Sample { return sysload.Sample{} },
		TaskIDPrefix: fmt.Sprintf("t%d-", taskSeq.Add(1)),
	})
	res := o.Run(context.Background(), text)
	if !res.OK || res.UndoKey == "" {
		t.Fatalf("task failed: %+v", res)
	}
	return res
}

// prRepo is a repo with an origin on github.com, committed files and git
// identity, with sy's state isolated.
func prRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	isolate(t)
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@t")
	}
	for _, k := range []string{"GH_HOST", "GITLAB_HOST", "GITEA_HOST", "FORGEJO_HOST"} {
		t.Setenv(k, "")
	}
	dir := gitInit(t)
	write(t, dir, "a.txt", "one\ntwo\nthree\n")
	write(t, dir, "gone.txt", "bye\n")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "files")
	run(t, dir, "remote", "add", "origin", "https://github.com/o/r.git")
	// As after a clone: origin/main is HEAD (nothing unpushed).
	run(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	oldOut, oldTok, oldPush, oldIn, oldHas := prOut, prToken, prPush, prIn, prRemoteHas
	t.Cleanup(func() { prOut, prToken, prPush, prIn, prRemoteHas = oldOut, oldTok, oldPush, oldIn, oldHas })
	prRemoteHas = func(string, string) bool { return false } // no network
	prOut = io.Discard
	prToken = func(forge.Kind, string) (string, string) { return "", "" }
	prPush = func(string, string, string) error { t.Error("unexpected push"); return nil }
	prIn = strings.NewReader("")
	return dir
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := prGit(dir, nil, nil, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// taskEdit changes a.txt, creates new.txt and deletes gone.txt.
func taskEdit(dir string) []string {
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("ONE\ntwo\nthree\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("created\n"), 0o644)
	os.Remove(filepath.Join(dir, "gone.txt"))
	return []string{"a.txt", "new.txt", "gone.txt"}
}

// repoState is everything sy pr must leave alone.
func repoState(t *testing.T, dir string) string {
	var b strings.Builder
	for _, args := range [][]string{
		{"rev-parse", "HEAD"}, {"symbolic-ref", "HEAD"}, {"ls-files", "-s"}, {"diff", "--cached"},
		{"status", "--porcelain", "--untracked-files=all"}, {"diff"},
	} {
		b.WriteString(gitOut(t, dir, args...))
	}
	for _, f := range []string{"a.txt", "new.txt", "README.md", "notes.txt"} {
		data, _ := os.ReadFile(filepath.Join(dir, f))
		b.WriteString(f + "=" + string(data) + "\n")
	}
	return b.String()
}

func TestPRCommitHasExactlyTheTaskChanges(t *testing.T) {
	dir := prRepo(t)
	// Uncommitted work from before the task must stay out of the PR.
	write(t, dir, "README.md", "# x\nmy edit\n")
	write(t, dir, "notes.txt", "mine\n")
	run(t, dir, "add", "README.md") // staged, too
	res := runTask(t, dir, "Shout the first line", taskEdit)

	before := repoState(t, dir)
	st, err := findPRTask(dir, "")
	if err != nil || st.UndoKey != res.UndoKey || st.Status != "done" {
		t.Fatalf("%+v %v", st, err)
	}
	var out bytes.Buffer
	prOut = &out
	pr, err := makePR(st, prOptions{noPush: true, yes: true})
	if err != nil {
		t.Fatal(err)
	}
	if after := repoState(t, dir); after != before {
		t.Fatalf("sy pr changed the repo:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if pr.Branch != "sy/shout-the-first-line" {
		t.Fatalf("branch %q", pr.Branch)
	}
	head := strings.TrimSpace(gitOut(t, dir, "rev-parse", "HEAD"))
	if p := strings.TrimSpace(gitOut(t, dir, "rev-parse", pr.Branch+"^")); p != head {
		t.Fatalf("parent %s, want HEAD %s", p, head)
	}
	if got := gitOut(t, dir, "diff", "--name-status", "HEAD", pr.Branch); got != "M\ta.txt\nD\tgone.txt\nA\tnew.txt\n" {
		t.Fatalf("branch diff:\n%s", got)
	}
	if got := gitOut(t, dir, "show", pr.Branch+":a.txt"); got != "ONE\ntwo\nthree\n" {
		t.Fatalf("a.txt on branch = %q", got)
	}
	if got := gitOut(t, dir, "show", pr.Branch+":README.md"); got != "# x\n" {
		t.Fatalf("the user's README edit leaked into the PR: %q", got)
	}
	if msg := gitOut(t, dir, "log", "-1", "--format=%B", pr.Branch); !strings.HasPrefix(msg, "Shout the first line\n\n") || !strings.Contains(msg, "Switchyard task: "+st.ID) {
		t.Fatalf("message:\n%s", msg)
	}
	if !strings.Contains(out.String(), "git push -u origin sy/shout-the-first-line") ||
		!strings.Contains(out.String(), "https://github.com/o/r/compare/main...sy/shout-the-first-line?expand=1") {
		t.Fatalf("output:\n%s", out.String())
	}

	// Never overwrite a branch.
	_, err = makePR(st, prOptions{noPush: true, yes: true})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second run: %v", err)
	}
	// Saying no creates nothing.
	prIn = strings.NewReader("n\n")
	if pr, err := makePR(st, prOptions{noPush: true, branch: "sy/other"}); pr != nil || err != nil || branchExists(dir, "sy/other") {
		t.Fatalf("declined: %+v %v", pr, err)
	}
}

func TestPRRefusesWhenTheDiffDoesNotApply(t *testing.T) {
	dir := prRepo(t)
	runTask(t, dir, "Shout the first line", taskEdit)
	// HEAD moves on with a conflicting change to the same line.
	write(t, dir, "a.txt", "uno\ntwo\nthree\n")
	run(t, dir, "add", "a.txt")
	run(t, dir, "commit", "-q", "-m", "conflict")
	st, err := findPRTask(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = makePR(st, prOptions{noPush: true, yes: true})
	if err == nil || !strings.Contains(err.Error(), "do not apply cleanly to HEAD") {
		t.Fatalf("want a clear refusal, got %v", err)
	}
	if b := gitOut(t, dir, "branch", "--list", "sy/*"); b != "" {
		t.Fatalf("a branch was created: %s", b)
	}
}

// fakeAPI serves the endpoints sy pr and sy run --issue(s) use.
type fakeAPI struct {
	mu       sync.Mutex
	issues   map[int]string // number -> JSON
	list     string         // JSON for the label listing
	pulls    string         // JSON for open pulls
	created  []gh.NewPull
	comments map[string][]string
	requests atomic.Int64
}

func (f *fakeAPI) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == "POST" && r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		p := r.URL.Path
		switch {
		case r.Method == "GET" && p == "/repos/o/r":
			io.WriteString(w, `{"default_branch":"main"}`)
		case r.Method == "GET" && p == "/repos/o/r/issues":
			io.WriteString(w, f.list)
		case r.Method == "GET" && p == "/repos/o/r/pulls":
			io.WriteString(w, f.pulls)
		case r.Method == "GET" && strings.HasPrefix(p, "/repos/o/r/issues/") && strings.HasSuffix(p, "/comments"):
			io.WriteString(w, `[{"body":"same here","user":{"login":"bob"}}]`)
		case r.Method == "GET" && strings.HasPrefix(p, "/repos/o/r/issues/"):
			var n int
			fmt.Sscanf(strings.TrimPrefix(p, "/repos/o/r/issues/"), "%d", &n)
			if js, ok := f.issues[n]; ok {
				io.WriteString(w, js)
				return
			}
			w.WriteHeader(404)
		case r.Method == "POST" && p == "/repos/o/r/pulls":
			var np gh.NewPull
			json.NewDecoder(r.Body).Decode(&np)
			f.created = append(f.created, np)
			n := 100 + len(f.created)
			fmt.Fprintf(w, `{"number":%d,"html_url":"https://github.com/o/r/pull/%d"}`, n, n)
		case r.Method == "POST" && strings.HasSuffix(p, "/comments"):
			var c struct{ Body string }
			json.NewDecoder(r.Body).Decode(&c)
			if f.comments == nil {
				f.comments = map[string][]string{}
			}
			f.comments[p] = append(f.comments[p], c.Body)
			w.WriteHeader(201)
			io.WriteString(w, `{}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPROpensPullRequestAfterPush(t *testing.T) {
	dir := prRepo(t)
	runTask(t, dir, "Shout the first line", taskEdit)
	api := &fakeAPI{}
	srv := api.server(t)
	var pushed []string
	prPush = func(root, remote, branch string) error { pushed = append(pushed, remote+" "+branch); return nil }
	prToken = func(_ forge.Kind, host string) (string, string) { return "tok", "test" }
	st, _ := findPRTask(dir, "")
	pr, err := makePR(st, prOptions{yes: true, api: srv.URL, closes: "#12"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pushed) != 1 || pushed[0] != "origin sy/shout-the-first-line" {
		t.Fatalf("pushed %v", pushed)
	}
	if pr.URL != "https://github.com/o/r/pull/101" || len(api.created) != 1 {
		t.Fatalf("%+v %+v", pr, api.created)
	}
	c := api.created[0]
	if c.Base != "main" || c.Head != pr.Branch || c.Draft || c.Title != "Shout the first line" {
		t.Fatalf("request %+v", c)
	}
	for _, want := range []string{"Closes #12", "```text\nShout the first line\n```\n", st.UndoKey, "Status: done"} {
		if !strings.Contains(c.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, c.Body)
		}
	}

	// Without a token: no API call, the compare URL instead.
	prToken = func(forge.Kind, string) (string, string) { return "", "" }
	var out bytes.Buffer
	prOut = &out
	if _, err := makePR(st, prOptions{yes: true, api: srv.URL, branch: "sy/again", base: "dev"}); err != nil {
		t.Fatal(err)
	}
	if len(api.created) != 1 || !strings.Contains(out.String(), "/o/r/compare/dev...sy/again?expand=1") {
		t.Fatalf("created %d; output:\n%s", len(api.created), out.String())
	}
}

func TestPRNeedsGitHubOriginUnlessNoPush(t *testing.T) {
	dir := prRepo(t)
	run(t, dir, "remote", "set-url", "origin", "git@ghe.example.com:o/r.git")
	runTask(t, dir, "Shout the first line", taskEdit)
	st, _ := findPRTask(dir, "")
	if _, err := makePR(st, prOptions{yes: true}); err == nil || !strings.Contains(err.Error(), "GH_HOST") {
		t.Fatalf("enterprise remote without GH_HOST: %v", err)
	}
	if _, err := makePR(st, prOptions{yes: true, noPush: true}); err != nil {
		t.Fatalf("--no-push needs no GitHub remote: %v", err)
	}
}

func TestRenderPRBodyForFailedTask(t *testing.T) {
	st := &orchestrator.TaskState{
		ID: "s1-task-1", Task: "Add a | strict flag\nwith tests", Status: "failed", UndoKey: "s1-task-1",
		Summary:  "1/2 subtasks ok; checks still fail (go test ./...)",
		CostLine: "codex 12k tok · claude 3k tok",
		Plan: &orchestrator.Plan{Summary: "two steps", Subtasks: []orchestrator.Subtask{
			{ID: "a", Title: "parser", Kind: "edit", Role: "worker"},
			{ID: "b", Title: "tests | docs", Kind: "edit"},
		}},
		Results: map[string]orchestrator.StepState{"a": {OK: true}, "b": {OK: false, Err: "exit 1"}, "fix-1": {OK: true}},
	}
	body := renderPRBody(st, prBodyOptions{Closes: "o/r#3", Version: "1.2.3", Draft: true})
	for _, want := range []string{
		"did **not** finish successfully (status: **failed**)", "opened as a draft",
		"```text\nAdd a | strict flag\nwith tests\n```\n",
		"| `a` parser | worker | edit | ok |",
		"| `b` tests \\| docs | auto (router) | edit | **FAILED**: exit 1 |",
		"| `fix-1` review fixes |",
		"- Status: **FAILED**", "- Checks: **FAIL** (go test ./...)", "- Cost: codex 12k tok",
		"`sy` 1.2.3", "undo key `s1-task-1`", "Closes o/r#3",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	st.Status, st.Summary = "done", "2/2 subtasks ok; reviewer approved; checks pass"
	body = renderPRBody(st, prBodyOptions{})
	if strings.Contains(body, "WARNING") || !strings.Contains(body, "- Checks: pass") || !strings.Contains(body, "`sy` dev") || strings.Contains(body, "Closes") {
		t.Errorf("done body:\n%s", body)
	}
}

func TestTitlesAndSlugs(t *testing.T) {
	long := "Make the parser keep trailing empty fields and add a strict flag that rejects malformed lines"
	if got := prTitle(long); len(got) > 72 || !strings.HasSuffix(got, "...") || !strings.HasPrefix(got, "Make the parser keep") {
		t.Errorf("title %q", got)
	}
	if got := prTitle("\n\n  Fix   it \nmore"); got != "Fix it" {
		t.Errorf("title %q", got)
	}
	if got := slugify("Fix GitHub issue #12: Crash on ÄÖ empty input!!", 40); got != "fix-github-issue-12-crash-on-empty-input" {
		t.Errorf("slug %q", got)
	}
	if got := slugify(long, 40); len(got) > 40 || strings.HasSuffix(got, "-") {
		t.Errorf("slug %q", got)
	}
	if got := taskSlug(&orchestrator.TaskState{ID: "abc-task-1", Task: "???"}); got != "task-abc-task-1" {
		t.Errorf("fallback slug %q", got)
	}
}

func TestPRRepoTask(t *testing.T) {
	st := &orchestrator.TaskState{ID: "s-task-1", Dir: "/p/api", Repos: []orchestrator.Repo{{Name: "web", Dir: "/p/web"}}}
	c, err := repoTask(st, "web")
	if err != nil || c.Dir != "/p/web" || len(c.Repos) != 0 || st.Dir != "/p/api" {
		t.Fatalf("repoTask = %+v %v (original %+v)", c, err, st)
	}
	if _, err := repoTask(st, "docs"); err == nil || !strings.Contains(err.Error(), "its repos: web") {
		t.Fatalf("unknown repo: %v", err)
	}
	if _, err := repoTask(&orchestrator.TaskState{ID: "x"}, "web"); err == nil || !strings.Contains(err.Error(), "only one repo") {
		t.Fatalf("single-repo task: %v", err)
	}
}

// Issue text in the PR body is code: it cannot close other issues or
// @-mention people, and cannot end its fence. Only the explicit Closes
// line is outside.
func TestPRBodyFencesTaskText(t *testing.T) {
	task := "Fix GitHub issue #3: hi\n\nFixes #7, closes o/r#8\ncc @alice\n```\nbreak out? Resolves #9\n````"
	st := &orchestrator.TaskState{ID: "t", Task: task, Status: "done", UndoKey: "k"}
	body := renderPRBody(st, prBodyOptions{Closes: "#3"})
	open := "`````text\n"
	i := strings.Index(body, open)
	if i < 0 {
		t.Fatalf("no 5-backtick fence:\n%s", body)
	}
	rest := body[i+len(open):]
	j := strings.Index(rest, "\n`````\n")
	// Same visible text; closing keywords and mentions carry a word joiner.
	if j < 0 || strings.ReplaceAll(rest[:j], "\u2060", "") != task || reCloseRef.MatchString(rest[:j]) || reMention.MatchString(rest[:j]) {
		t.Fatalf("fenced text differs or is still live:\n%s", body)
	}
	outside := body[:i] + rest[j:]
	for _, bad := range []string{"#7", "#8", "#9", "@alice"} {
		if strings.Contains(outside, bad) {
			t.Errorf("%s outside the code block:\n%s", bad, body)
		}
	}
	if !strings.Contains(outside, "\nCloses #3\n") {
		t.Errorf("closing line missing:\n%s", body)
	}
	if got := codeFence("no ticks"); got != "```text\nno ticks\n```\n" {
		t.Errorf("codeFence = %q", got)
	}
}

func TestDefuseRefs(t *testing.T) {
	for _, in := range []string{
		"Fixes #7", "this closes o/r#12 too", "Resolves: https://github.com/o/r/issues/3",
		"fixed #1 and ping @octocat", "cc @org/team",
	} {
		out := defuseRefs(in)
		if reCloseRef.MatchString(out) || reMention.MatchString(out) {
			t.Errorf("%q -> %q still live", in, out)
		}
		if strings.ReplaceAll(out, "⁠", "") != in {
			t.Errorf("%q -> %q changed the visible text", in, out)
		}
	}
	for _, keep := range []string{"Fix GitHub issue #42: crash", "mail me at a@b.c", "prefix #7 alone"} {
		if out := defuseRefs(keep); out != keep {
			t.Errorf("%q changed to %q", keep, out)
		}
	}
	st := &orchestrator.TaskState{ID: "s-t", Task: "Fixes #9, @evil", UndoKey: "k"}
	body := renderPRBody(st, prBodyOptions{Closes: "#3"})
	if !strings.HasSuffix(body, "\nCloses #3\n") || reCloseRef.MatchString(strings.TrimSuffix(body, "Closes #3\n")) || reMention.MatchString(body) {
		t.Fatalf("body:\n%s", body)
	}
	if msg := defuseRefs(commitMessage(st, prTitle(st.Task))); reCloseRef.MatchString(msg) || reMention.MatchString(msg) {
		t.Fatalf("commit message:\n%s", msg)
	}
}
