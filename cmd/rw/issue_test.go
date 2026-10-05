package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/forge"
	"github.com/sparkz400/relayweft/internal/orchestrator"
)

func parseIssueFlags(t *testing.T, args ...string) (*issueFlags, *flag.FlagSet) {
	t.Helper()
	fs := flag.NewFlagSet("rw run", flag.ContinueOnError)
	f := registerIssueFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return f, fs
}

func TestIssueTaskText(t *testing.T) {
	is := forge.Issue{Number: 12, Title: " Crash on empty input ", Body: "Steps:\r\n1. run it\r\n", Labels: []string{"bug", "rw"}}
	ghRepo := forge.Repo{Kind: forge.GitHub, Host: "github.com", Owner: "o", Name: "r"}
	got := issueTask(ghRepo, is, []forge.Comment{{Body: "also on Windows", Author: "bob"}})
	want := "Fix GitHub issue #12: Crash on empty input\n\nSteps:\n1. run it\n\nLabels: bug, rw\n\nComments:\n\n@bob wrote:\nalso on Windows"
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
	if got := issueTask(ghRepo, forge.Issue{Number: 1, Title: "T"}, nil); got != "Fix GitHub issue #1: T" {
		t.Fatalf("%q", got)
	}
}

// An issue on Forgejo (Codeberg, or a host in FORGEJO_HOST) is a Forgejo
// issue, not a Gitea one.
func TestIssueTaskNamesForgejo(t *testing.T) {
	t.Setenv("FORGEJO_HOST", "https://git.example.org:3000")
	t.Setenv("GITEA_HOST", "gitea.lan")
	for host, want := range map[string]string{"codeberg.org": "Forgejo", "git.example.org": "Forgejo", "gitea.lan": "Gitea"} {
		repo := forge.Repo{Kind: forge.Gitea, Host: host, Owner: "o", Name: "r"}
		if got := issueTask(repo, forge.Issue{Number: 4, Title: "T"}, nil); got != "Fix "+want+" issue #4: T" {
			t.Errorf("%s: %q", host, got)
		}
	}
}

func issueAPI(t *testing.T) (*fakeAPI, string) {
	api := &fakeAPI{
		issues: map[int]string{
			3:  `{"number":3,"title":"Shout the first line","body":"a.txt should start loud","state":"open","labels":[{"name":"rw"}]}`,
			5:  `{"number":5,"title":"taken","state":"open"}`,
			9:  `{"number":9,"title":"Add a second file","body":"please","state":"open"}`,
			12: `{"number":12,"title":"A PR","pull_request":{}}`,
		},
		list:  `[{"number":3,"title":"Shout the first line"},{"number":12,"title":"A PR","pull_request":{}},{"number":5,"title":"taken"},{"number":9,"title":"Add a second file"}]`,
		pulls: `[{"number":40,"body":"Closes #5"}]`,
	}
	return api, api.server(t).URL
}

func TestIssueLoad(t *testing.T) {
	dir := prRepo(t)
	_, url := issueAPI(t)

	f, fs := parseIssueFlags(t, "--issue", "#3", "--with-comments", "--api", url)
	tasks, err := f.load(fs, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || !strings.HasPrefix(tasks[0], "Fix GitHub issue #3: Shout the first line\n\na.txt should start loud") ||
		!strings.Contains(tasks[0], "@bob wrote:\nsame here") || f.items[0].closes != "#3" {
		t.Fatalf("%q %+v", tasks, f.items)
	}

	f, fs = parseIssueFlags(t, "--issue", "12", "--api", url)
	if _, err := f.load(fs, dir); err == nil || !strings.Contains(err.Error(), "is a pull request") {
		t.Fatalf("PR as issue: %v", err)
	}
	f, fs = parseIssueFlags(t, "--issue", "77", "--api", url)
	if _, err := f.load(fs, dir); !forge.IsNotFound(err) {
		t.Fatalf("missing issue: %v", err)
	}

	// Batch: oldest first, PRs and issues an open PR closes are skipped.
	f, fs = parseIssueFlags(t, "--issues", "label:rw", "--pr", "--api", url)
	tasks, err = f.load(fs, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || f.items[0].issue.Number != 3 || f.items[1].issue.Number != 9 {
		t.Fatalf("batch = %+v", f.items)
	}
	f, fs = parseIssueFlags(t, "--issues", "label:rw", "--pr", "--limit", "1", "--api", url)
	if tasks, _ := f.load(fs, dir); len(tasks) != 1 {
		t.Fatalf("limit: %d", len(tasks))
	}
	for _, bad := range []string{"rw", "label:", "milestone:x"} {
		f, fs = parseIssueFlags(t, "--issues", bad, "--pr", "--api", url)
		if _, err := f.load(fs, dir); err == nil {
			t.Errorf("--issues %q accepted", bad)
		}
	}

	// A batch never starts on top of uncommitted work.
	write(t, dir, "notes.txt", "mine\n")
	f, fs = parseIssueFlags(t, "--issues", "label:rw", "--pr", "--api", url)
	if _, err := f.load(fs, dir); err == nil || !strings.Contains(err.Error(), "clean working tree") {
		t.Fatalf("dirty tree: %v", err)
	}
}

// The unattended batch with --pr: each issue's task becomes a PR that
// closes it, the issue gets a comment, and the working tree is back at
// HEAD before the next issue starts.
func TestIssueBatchOpensPRsAndRestoresTree(t *testing.T) {
	dir := prRepo(t)
	api, url := issueAPI(t)
	var pushed []string
	prPush = func(root, remote, branch string) error { pushed = append(pushed, branch); return nil }
	prToken = func(forge.Kind, string) (string, string) { return "tok", "test" }

	f, fs := parseIssueFlags(t, "--issues", "label:rw", "--pr", "--api", url)
	tasks, err := f.load(fs, dir)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("%v %v", tasks, err)
	}
	edits := []func(string) []string{
		taskEdit,
		func(d string) []string {
			// The second issue must start from HEAD, not on top of the first.
			if b, _ := os.ReadFile(filepath.Join(d, "a.txt")); string(b) != "one\ntwo\nthree\n" {
				t.Errorf("issue 2 started on a dirty tree: a.txt = %q", b)
			}
			os.WriteFile(filepath.Join(d, "second.txt"), []byte("2\n"), 0o644)
			return []string{"second.txt"}
		},
	}
	for i, task := range tasks {
		res := runTask(t, dir, task, edits[i])
		if stop := f.afterTask(i, res); stop {
			t.Fatalf("batch stopped after %d", i)
		}
		if s := gitOut(t, dir, "status", "--porcelain"); s != "" {
			t.Fatalf("tree not clean after issue %d:\n%s", i, s)
		}
	}
	if strings.Join(pushed, ",") != "rw/issue-3-shout-the-first-line,rw/issue-9-add-a-second-file" {
		t.Fatalf("pushed %v", pushed)
	}
	if len(api.created) != 2 || !strings.Contains(api.created[0].Body, "Closes #3") || !strings.Contains(api.created[1].Body, "Closes #9") {
		t.Fatalf("created %+v", api.created)
	}
	if api.created[0].Draft {
		t.Error("a done task should not be a draft")
	}
	if got := api.comments["/repos/o/r/issues/3/comments"]; len(got) != 1 || !strings.Contains(got[0], "https://github.com/o/r/pull/101") {
		t.Fatalf("comments %v", api.comments)
	}
	if got := gitOut(t, dir, "diff", "--name-status", "HEAD", "rw/issue-9-add-a-second-file"); got != "A\tsecond.txt\n" {
		t.Fatalf("second PR branch:\n%s", got)
	}
	if f.pulls != 2 {
		t.Fatalf("pulls %d", f.pulls)
	}
}

// Running an issue again (its first PR closed, the branch still there,
// here or on origin) gets a new branch: the push was rejected before.
func TestIssueRerunGetsFreshBranch(t *testing.T) {
	dir := prRepo(t)
	_, url := issueAPI(t)
	var pushed []string
	prPush = func(root, remote, branch string) error { pushed = append(pushed, branch); return nil }
	prToken = func(forge.Kind, string) (string, string) { return "tok", "test" }
	run(t, dir, "branch", "rw/issue-3-shout-the-first-line")
	prRemoteHas = func(_, b string) bool { return b == "rw/issue-3-shout-the-first-line-2" }

	f, fs := parseIssueFlags(t, "--issues", "label:rw", "--pr", "--api", url)
	if _, err := f.load(fs, dir); err != nil {
		t.Fatal(err)
	}
	f.afterTask(0, runTask(t, dir, f.items[0].task, taskEdit))
	if len(pushed) != 1 || pushed[0] != "rw/issue-3-shout-the-first-line-3" {
		t.Fatalf("pushed %v", pushed)
	}
	if f.noPR != 0 {
		t.Fatalf("noPR = %d", f.noPR)
	}
}

// --pr without a pull request is a failed run (exit status 1): a CI job
// stayed green when the push or the PR failed.
func TestIssueWithoutPRCounts(t *testing.T) {
	dir := prRepo(t)
	_, url := issueAPI(t)
	prPush = func(string, string, string) error { return os.ErrPermission }
	f, fs := parseIssueFlags(t, "--issues", "label:rw", "--pr", "--api", url)
	if _, err := f.load(fs, dir); err != nil {
		t.Fatal(err)
	}
	f.afterTask(0, runTask(t, dir, f.items[0].task, taskEdit))
	if f.noPR != 1 {
		t.Fatalf("noPR = %d, want 1", f.noPR)
	}
}

// A task that leaves changes but no PR stops the batch instead of letting
// the next issue start on top of them.
func TestIssueBatchStopsWithoutPR(t *testing.T) {
	dir := prRepo(t)
	_, url := issueAPI(t)
	prPush = func(string, string, string) error { return os.ErrPermission }
	f, fs := parseIssueFlags(t, "--issues", "label:rw", "--pr", "--api", url)
	if _, err := f.load(fs, dir); err != nil {
		t.Fatal(err)
	}
	res := runTask(t, dir, f.items[0].task, taskEdit)
	if !f.afterTask(0, res) {
		t.Fatal("batch went on with a dirty tree")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "ONE\ntwo\nthree\n" {
		t.Fatalf("the task's work was discarded: %q", b)
	}
}

// taskEditPlusUser is taskEdit plus a file nobody reported: the user's
// own edit (or a secrets file) made while the task ran.
func taskEditPlusUser(dir string) []string {
	os.WriteFile(filepath.Join(dir, "secrets.env"), []byte("TOKEN=x\n"), 0o644)
	return taskEdit(dir)
}

// Nobody reviews an unattended PR: files no agent reported must not be
// committed and pushed (and then undone from the tree).
func TestUnattendedPRRefusesUnreportedFiles(t *testing.T) {
	dir := prRepo(t)
	runTask(t, dir, "Shout the first line", taskEditPlusUser)
	st, _ := findPRTask(dir, "")
	_, err := makePR(st, prOptions{yes: true, unattended: true, api: "http://127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "secrets.env") || !strings.Contains(err.Error(), "no agent reported") {
		t.Fatalf("err = %v", err)
	}
	if branchExists(dir, "rw/shout-the-first-line") {
		t.Error("a branch was created")
	}

	// rw pr with a person: shown in the preview as a warning.
	var out bytes.Buffer
	prOut, prIn = &out, strings.NewReader("n\n")
	if pr, err := makePR(st, prOptions{noPush: true}); pr != nil || err != nil {
		t.Fatalf("%+v %v", pr, err)
	}
	if !strings.Contains(out.String(), "WARNING: 1 file(s) changed while the task ran that no agent reported") || !strings.Contains(out.String(), "  ! secrets.env") {
		t.Errorf("preview:\n%s", out.String())
	}
}

// The batch stops and keeps the work in place when the PR is refused.
func TestIssueBatchStopsOnUnreportedFiles(t *testing.T) {
	dir := prRepo(t)
	api, url := issueAPI(t)
	prPush = func(string, string, string) error { t.Error("pushed"); return nil }
	prToken = func(forge.Kind, string) (string, string) { return "tok", "test" }
	f, fs := parseIssueFlags(t, "--issues", "label:rw", "--pr", "--api", url)
	if _, err := f.load(fs, dir); err != nil {
		t.Fatal(err)
	}
	res := runTask(t, dir, f.items[0].task, taskEditPlusUser)
	if !f.afterTask(0, res) {
		t.Fatal("batch went on")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "secrets.env")); err != nil || string(b) != "TOKEN=x\n" {
		t.Fatalf("the user's file was touched: %q %v", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "ONE\ntwo\nthree\n" {
		t.Fatalf("the task's work was undone: %q", b)
	}
	if len(api.created) != 0 {
		t.Fatalf("created %+v", api.created)
	}
}

// Commits on HEAD that are not on origin/<base> would go to every
// unattended PR: refused, also when origin/<base> is unknown.
func TestUnattendedPRRefusesUnpushedCommits(t *testing.T) {
	dir := prRepo(t)
	write(t, dir, "local.txt", "not pushed\n")
	run(t, dir, "add", "local.txt")
	run(t, dir, "commit", "-q", "-m", "local only")
	runTask(t, dir, "Shout the first line", taskEdit)
	st, _ := findPRTask(dir, "")
	_, err := makePR(st, prOptions{yes: true, unattended: true, base: "main", api: "http://127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "1 commit(s) that are not on origin/main") {
		t.Fatalf("err = %v", err)
	}
	run(t, dir, "update-ref", "-d", "refs/remotes/origin/main")
	_, err = makePR(st, prOptions{yes: true, unattended: true, base: "main", api: "http://127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "cannot tell whether HEAD has unpushed commits") {
		t.Fatalf("unknown origin/main: %v", err)
	}
	// With a person: a warning in the preview.
	run(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD~1")
	var out bytes.Buffer
	prOut, prIn = &out, strings.NewReader("n\n")
	if _, err := makePR(st, prOptions{base: "main", api: "http://127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "WARNING: HEAD has 1 commit(s) that are not on origin/main") {
		t.Errorf("preview:\n%s", out.String())
	}
}

// One PR cannot hold a multi-repo task, while rw undo would take its
// changes out of every repo.
func TestIssuePRRefusesMultiRepo(t *testing.T) {
	f, _ := parseIssueFlags(t, "--issue", "3", "--pr")
	repos := []orchestrator.Repo{{Name: "api", Dir: t.TempDir()}}
	if err := f.checkWorkspace(repos); err == nil || !strings.Contains(err.Error(), "multi-repo") {
		t.Fatalf("err = %v", err)
	}
	if err := f.checkWorkspace(nil); err != nil {
		t.Fatal(err)
	}
	if f, _ := parseIssueFlags(t, "--issue", "3"); f.checkWorkspace(repos) != nil {
		t.Error("without --pr nothing is pushed: no need to refuse")
	}
	// And makePR itself, for a task that recorded extra repos.
	st := &orchestrator.TaskState{ID: "x", Repos: repos}
	if err := unattendedPRCheck(st, "main", nil, nil, 0, nil, false); err == nil || !strings.Contains(err.Error(), "several repos") {
		t.Fatalf("err = %v", err)
	}
}

func TestIssuesNeedPR(t *testing.T) {
	dir := prRepo(t)
	f, fs := parseIssueFlags(t, "--issues", "label:rw")
	if err := f.prepare(fs, dir); err == nil || !strings.Contains(err.Error(), "--issues needs --pr") {
		t.Fatalf("err = %v", err)
	}
}

// Config that hides untracked files or submodule changes does not make a
// dirty tree look clean.
func TestCleanTreeIgnoresHidingConfig(t *testing.T) {
	dir := prRepo(t)
	run(t, dir, "config", "status.showUntrackedFiles", "no")
	if err := cleanTree(dir); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "untracked.txt", "x\n")
	if err := cleanTree(dir); err == nil {
		t.Fatal("untracked file hidden by status.showUntrackedFiles=no")
	}
}

// rw's own git in the project folder (where agents wrote) overrides
// core.fsmonitor and submodule recursion (proc.GitGuard), so a config
// there cannot decide what git runs; the value git sees is the guard's.
func TestPrGitAndRunGitGuard(t *testing.T) {
	dir := prRepo(t)
	run(t, dir, "config", "core.fsmonitor", "rw-test-not-a-command")
	run(t, dir, "config", "submodule.recurse", "true")
	for name, get := range map[string]func(string) (string, error){
		"prGit":  func(k string) (string, error) { return prGit(dir, nil, nil, "config", k) },
		"runGit": func(k string) (string, error) { return runGit(dir, "config", k) },
	} {
		for _, k := range []string{"core.fsmonitor", "submodule.recurse"} {
			if v, _ := get(k); strings.TrimSpace(v) != "false" {
				t.Errorf("%s: %s = %q, want false", name, k, v)
			}
		}
	}
}

// A scheduled batch reads the issues and checks the tree when it starts:
// prepare (before the wait) reads nothing and does not look at the tree.
func TestIssuePrepareReadsNothing(t *testing.T) {
	dir := prRepo(t)
	api, url := issueAPI(t)
	write(t, dir, "notes.txt", "mine\n") // committed before the run starts
	f, fs := parseIssueFlags(t, "--issues", "label:rw", "--pr", "--api", url)
	if err := f.prepare(fs, dir); err != nil {
		t.Fatal(err)
	}
	if n := api.requests.Load(); n != 0 {
		t.Fatalf("prepare made %d API requests", n)
	}
	run(t, dir, "add", "notes.txt")
	run(t, dir, "commit", "-q", "-m", "notes")
	tasks, err := f.fetch()
	if err != nil || len(tasks) != 2 || api.requests.Load() == 0 {
		t.Fatalf("%v %v", tasks, err)
	}
	write(t, dir, "late.txt", "x\n")
	f, fs = parseIssueFlags(t, "--issues", "label:rw", "--pr", "--api", url)
	if err := f.prepare(fs, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := f.fetch(); err == nil || !strings.Contains(err.Error(), "clean working tree") {
		t.Fatalf("dirty at start: %v", err)
	}
}
