package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sparkz400/switchyard/internal/gh"
)

func parseIssueFlags(t *testing.T, args ...string) (*issueFlags, *flag.FlagSet) {
	t.Helper()
	fs := flag.NewFlagSet("sy run", flag.ContinueOnError)
	f := registerIssueFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return f, fs
}

func TestIssueTaskText(t *testing.T) {
	is := gh.Issue{Number: 12, Title: " Crash on empty input ", Body: "Steps:\r\n1. run it\r\n", Labels: []gh.Label{{Name: "bug"}, {Name: "sy"}}}
	got := issueTask(is, []gh.Comment{{Body: "also on Windows", User: gh.User{Login: "bob"}}})
	want := "Fix GitHub issue #12: Crash on empty input\n\nSteps:\n1. run it\n\nLabels: bug, sy\n\nComments:\n\n@bob wrote:\nalso on Windows"
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
	if got := issueTask(gh.Issue{Number: 1, Title: "T"}, nil); got != "Fix GitHub issue #1: T" {
		t.Fatalf("%q", got)
	}
}

func issueAPI(t *testing.T) (*fakeAPI, string) {
	api := &fakeAPI{
		issues: map[int]string{
			3:  `{"number":3,"title":"Shout the first line","body":"a.txt should start loud","state":"open","labels":[{"name":"sy"}]}`,
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
	if _, err := f.load(fs, dir); !gh.IsNotFound(err) {
		t.Fatalf("missing issue: %v", err)
	}

	// Batch: oldest first, PRs and issues an open PR closes are skipped.
	f, fs = parseIssueFlags(t, "--issues", "label:sy", "--api", url)
	tasks, err = f.load(fs, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || f.items[0].issue.Number != 3 || f.items[1].issue.Number != 9 {
		t.Fatalf("batch = %+v", f.items)
	}
	f, fs = parseIssueFlags(t, "--issues", "label:sy", "--limit", "1", "--api", url)
	if tasks, _ := f.load(fs, dir); len(tasks) != 1 {
		t.Fatalf("limit: %d", len(tasks))
	}
	for _, bad := range []string{"sy", "label:", "milestone:x"} {
		f, fs = parseIssueFlags(t, "--issues", bad, "--api", url)
		if _, err := f.load(fs, dir); err == nil {
			t.Errorf("--issues %q accepted", bad)
		}
	}

	// A batch never starts on top of uncommitted work.
	write(t, dir, "notes.txt", "mine\n")
	f, fs = parseIssueFlags(t, "--issues", "label:sy", "--api", url)
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
	prToken = func(string) (string, string) { return "tok", "test" }

	f, fs := parseIssueFlags(t, "--issues", "label:sy", "--pr", "--api", url)
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
	if strings.Join(pushed, ",") != "sy/issue-3-shout-the-first-line,sy/issue-9-add-a-second-file" {
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
	if got := gitOut(t, dir, "diff", "--name-status", "HEAD", "sy/issue-9-add-a-second-file"); got != "A\tsecond.txt\n" {
		t.Fatalf("second PR branch:\n%s", got)
	}
	if f.pulls != 2 {
		t.Fatalf("pulls %d", f.pulls)
	}
}

// A task that leaves changes but no PR stops the batch instead of letting
// the next issue start on top of them.
func TestIssueBatchStopsWithoutPR(t *testing.T) {
	dir := prRepo(t)
	_, url := issueAPI(t)
	prPush = func(string, string, string) error { return os.ErrPermission }
	f, fs := parseIssueFlags(t, "--issues", "label:sy", "--pr", "--api", url)
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
