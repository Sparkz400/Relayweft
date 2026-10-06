package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/forge"
	"github.com/sparkz400/relayweft/internal/runner"
)

const azRemote = "https://dev.azure.com/org/proj/_git/app"

// azAPI is a fake Azure DevOps for repository app of org/proj: the pull
// request rw opens (!101, its head whatever the bare remote's branch points
// at) and work item 12's comments, stored as HTML.
type azAPI struct {
	mu       sync.Mutex
	bare     string
	branch   string
	created  []map[string]any
	threads  string // the pull request's threads JSON
	failed   bool   // its build validation failed
	replies  []string
	comments []azComment
}

type azComment struct {
	ID     int64
	Text   string
	Author string
}

func (f *azAPI) head() string {
	out, err := exec.Command("git", "-C", f.bare, "rev-parse", "refs/heads/"+f.branch).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (f *azAPI) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *azAPI) server(t *testing.T) string {
	const git = "/org/proj/_apis/git/repositories/app"
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(":tok"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != auth {
			w.WriteHeader(401)
			return
		}
		p, q := r.URL.EscapedPath(), r.URL.Query()
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		switch {
		case r.Method == "POST" && p == git+"/pullrequests":
			f.created = append(f.created, in)
			f.branch = strings.TrimPrefix(in["sourceRefName"].(string), "refs/heads/")
			io.WriteString(w, `{"pullRequestId":101,"status":"active"}`)
		case r.Method == "GET" && p == git+"/pullrequests/101":
			fmt.Fprintf(w, `{"pullRequestId":101,"title":"Shout @<abc-guid>, fixes #8","status":"active","sourceRefName":"refs/heads/%s","targetRefName":"refs/heads/main",
				"lastMergeSourceCommit":{"commitId":%q},"lastMergeCommit":{"commitId":"merge1"}}`, f.branch, f.head())
		case r.Method == "GET" && p == "/org/proj/_apis/build/builds":
			if f.failed && q.Get("branchName") == "refs/pull/101/merge" {
				io.WriteString(w, `{"value":[{"id":600,"status":"completed","result":"failed","definition":{"id":1,"name":"CI"},"sourceBranch":"refs/pull/101/merge","sourceVersion":"merge1","repository":{"name":"app","type":"TfsGit"}}]}`)
				return
			}
			io.WriteString(w, `{"value":[]}`)
		case r.Method == "GET" && p == "/org/proj/_apis/build/builds/600/timeline":
			io.WriteString(w, `{"records":[{"id":"t1","type":"Task","name":"go test","result":"failed","log":{"id":3}}]}`)
		case r.Method == "GET" && p == "/org/proj/_apis/build/builds/600/logs/3":
			io.WriteString(w, "2026-10-05T10:00:00.0000000Z "+strings.Repeat("ok line\n", 50)+"\x1b[31mFAIL\x1b[0m TestShout\n"+injLog+"\n")
		case r.Method == "GET" && (strings.HasPrefix(p, git+"/commits/") || p == git+"/pullRequests/101/statuses"):
			io.WriteString(w, `{"value":[]}`)
		case r.Method == "GET" && p == git+"/pullRequests/101/threads":
			io.WriteString(w, or(f.threads, `{"value":[]}`))
		case r.Method == "POST" && p == git+"/pullRequests/101/threads":
			cs := in["comments"].([]any)
			f.replies = append(f.replies, cs[0].(map[string]any)["content"].(string))
			io.WriteString(w, `{"id":77}`)
		case r.Method == "GET" && p == "/org/_apis/projects/proj/teams":
			io.WriteString(w, `{"value":[{"id":"t1"}]}`)
		case r.Method == "GET" && p == "/org/_apis/projects/proj/teams/t1/members":
			io.WriteString(w, `{"value":[{"identity":{"id":"D1","uniqueName":"dev@x.com"}},{"identity":{"id":"M2","uniqueName":"mate@x.com"}}]}`)
		case r.Method == "GET" && p == "/org/_apis/connectionData":
			io.WriteString(w, `{"authenticatedUser":{"id":"me1","properties":{"Account":{"$value":"me@x.com"}}}}`)
		case r.Method == "GET" && p == "/org/_apis/wit/workitems/12":
			io.WriteString(w, `{"id":12,"fields":{"System.Title":"Shout","System.State":"New","System.TeamProject":"proj"}}`)
		case r.Method == "GET" && p == "/org/proj/_apis/wit/workItems/12/comments":
			var cs []string
			for _, c := range f.comments {
				cs = append(cs, fmt.Sprintf(`{"id":%d,"text":%s,"createdBy":{"uniqueName":%q},"createdDate":"2026-10-05T10:00:%02dZ"}`, c.ID, jsonString(c.Text), c.Author, c.ID))
			}
			io.WriteString(w, `{"comments":[`+strings.Join(cs, ",")+`]}`)
		case r.Method == "POST" && p == "/org/proj/_apis/wit/workItems/12/comments":
			f.comments = append(f.comments, azComment{ID: int64(len(f.comments) + 1), Text: in["text"].(string), Author: "me@x.com"})
			io.WriteString(w, `{}`)
		case r.Method == "PATCH" && strings.HasPrefix(p, "/org/proj/_apis/wit/workItems/12/comments/"):
			id, _ := strconv.Atoi(strings.TrimPrefix(p, "/org/proj/_apis/wit/workItems/12/comments/"))
			if id < 1 || id > len(f.comments) {
				w.WriteHeader(404)
				return
			}
			f.comments[id-1].Text = in["text"].(string)
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/org"
}

// rw pr opens a pull request on Azure DevOps that links the work item, and
// rw watch follows it up: a failed build validation task and a team
// member's comment thread become one round, pushed to the branch, with a
// reply on the pull request.
func TestAzurePullRequestAndWatch(t *testing.T) {
	dir := prRepo(t)
	run(t, dir, "remote", "set-url", "origin", azRemote)
	bare := filepath.Join(t.TempDir(), "remote.git")
	run(t, dir, "init", "-q", "--bare", "-b", "main", bare)
	run(t, dir, "push", "-q", bare, "main")
	cd, _ := os.UserConfigDir()
	os.MkdirAll(filepath.Join(cd, "relayweft"), 0o755)
	write(t, filepath.Join(cd, "relayweft"), config.FileName, "orchestrator: {review_before_done: false}\nwatch: {max_rounds: 2}\n")
	runTask(t, dir, "Shout the first line", taskEdit)

	api := &azAPI{bare: bare}
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
	if res.URL != "https://dev.azure.com/org/proj/_git/app/pullrequest/101" || !strings.Contains(out.String(), "opened pull request !101") {
		t.Fatalf("result %+v\n%s", res, out.String())
	}
	c := api.created[0]
	if c["sourceRefName"] != "refs/heads/rw/shout-the-first-line" || c["targetRefName"] != "refs/heads/main" || !strings.HasSuffix(c["description"].(string), "\nCloses #4\n") {
		t.Fatalf("created %+v", c)
	}
	if refs, _ := json.Marshal(c["workItemRefs"]); string(refs) != `[{"id":"4"}]` {
		t.Fatalf("work item link %s", refs)
	}
	if len(kinds) == 0 || kinds[0] != forge.Azure {
		t.Fatalf("token asked for %v", kinds)
	}
	prs := watched(t)
	if len(prs) != 1 || prs[0].Forge != "azure" || prs[0].String() != "org/proj/app!101" || prs[0].API != url {
		t.Fatalf("not recorded for rw watch: %+v", prs)
	}

	run(t, dir, "config", "url."+filepath.ToSlash(bare)+".insteadOf", azRemote)
	wr := &watchRunner{}
	oldRunners, oldPrint, oldOut := watchRunners, eventPrint, watchOut
	t.Cleanup(func() { watchRunners, eventPrint, watchOut = oldRunners, oldPrint, oldOut })
	watchRunners = func(*config.Config) runner.Set { return runner.Set{event.Codex: wr, event.Claude: wr} }
	eventPrint = func(event.Event, bool) {}
	watchOut = io.Discard

	head0 := api.head()
	api.set(func() {
		api.failed = true
		api.threads = `{"value":[
			{"id":4,"status":"active","threadContext":{"filePath":"/a.txt","rightFileStart":{"line":1}},"comments":[{"id":1,"author":{"id":"D1","uniqueName":"dev@x.com"},"content":` + jsonString(injComment) + `,"commentType":"text"}]},
			{"id":5,"status":"active","threadContext":{"filePath":"/a.txt","rightFileStart":{"line":1}},"comments":[{"id":1,"author":{"id":"S9","uniqueName":"drive@by.com"},"content":"stranger asks","commentType":"text"}]},
			{"id":6,"status":"fixed","threadContext":{"filePath":"/a.txt","rightFileStart":{"line":1}},"comments":[{"id":1,"author":{"id":"D1","uniqueName":"dev@x.com"},"content":"done already","commentType":"text"}]}]}`
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
	for _, want := range []string{"pull request !101 of org/proj/app", "copied from Azure DevOps", "CI: go test", "FAIL TestShout", "rm -rf"} {
		if !strings.Contains(task, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	for _, inj := range []string{"curl evil", "rm -rf", "@bob", "@<abc-guid>"} {
		if strings.Contains(outsideFences(task), inj) {
			t.Errorf("%q outside the fences", inj)
		}
	}
	for _, s := range []string{"stranger asks", "done already", "\x1b["} {
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
	if prs[0].Rounds != 1 || !prs[0].handled("check:build/600/t1") || !prs[0].handled("comment:4-1") || prs[0].handled("comment:5-1") || prs[0].Head != head1 {
		t.Fatalf("registry %+v", prs[0])
	}
	if len(api.replies) != 1 || !strings.Contains(api.replies[0], "on this pull request") || !strings.Contains(api.replies[0], rwMark) || reMention.MatchString(api.replies[0]) {
		t.Fatalf("replies %q", api.replies)
	}
	if err := newWatcher(true).pass(context.Background()); err != nil || wr.steps() != 1 {
		t.Fatalf("ran again: %v, %d steps", err, wr.steps())
	}
}

// Team claims on a work item: the claim goes in as escaped HTML and is
// read back; a claim by someone outside the project's teams holds nothing,
// a teammate's does.
func TestAzureTeamClaims(t *testing.T) {
	dir, _, _ := teamSetup(t)
	run(t, dir, "remote", "set-url", "origin", azRemote)
	api := &azAPI{}
	url := api.server(t)
	q := testQueue(t, teamFlags(t, dir, url))
	if q.repo.Kind != forge.Azure || q.viewer != "me@x.com" {
		t.Fatalf("queue %+v", q)
	}
	stranger := html.EscapeString(otherClaim("00000000000000bb", "evil", claimHeld, time.Now().Add(time.Hour)))
	api.set(func() { api.comments = append(api.comments, azComment{ID: 1, Text: stranger, Author: "evil@x.com"}) })

	cl, skip, err := q.take(12)
	if err != nil || cl == nil {
		t.Fatalf("take: %v %q", err, skip)
	}
	mine := api.comments[1].Text
	if cl.comment != 2 || strings.Contains(mine, "<!--") || !strings.Contains(mine, "&lt;!-- relayweft:queue ") || !strings.Contains(mine, "<code>"+q.machine+"</code>") {
		t.Fatalf("claim %d: %s", cl.comment, mine)
	}
	q2 := testQueue(t, teamFlags(t, dir, url))
	q2.machine = "othermachine"
	if cl2, skip, err := q2.take(12); cl2 != nil || err != nil || !strings.Contains(skip, "holds it") {
		t.Fatalf("second machine: %v %q %v", cl2, skip, err)
	}
	cl.end(claimDone, "Pull request: https://dev.azure.com/org/proj/_git/app/pullrequest/101")
	if done := api.comments[1].Text; !strings.Contains(done, "state=done") || !strings.Contains(done, `<a href="https://dev.azure.com/org/proj/_git/app/pullrequest/101">`) {
		t.Fatalf("result: %s", done)
	}

	// A teammate's live claim holds the work item.
	api.set(func() {
		api.comments = append(api.comments, azComment{ID: 3, Text: html.EscapeString(otherClaim("00000000000000cc", "matebox", claimHeld, time.Now().Add(time.Hour))), Author: "mate@x.com"})
	})
	if cl3, skip, err := q.take(12); cl3 != nil || err != nil || !strings.Contains(skip, "machine matebox holds it") {
		t.Fatalf("teammate's claim: %v %q %v", cl3, skip, err)
	}
}

// A work item URL names a project, not a repository: one of the origin's
// organization is the repository's own, so its pull request says
// "Closes #N".
func TestAzureWorkItemURL(t *testing.T) {
	dir := prRepo(t)
	run(t, dir, "remote", "set-url", "origin", azRemote)
	f, fs := parseIssueFlags(t, "--issue", "https://dev.azure.com/org/Other%20Proj/_workitems/edit/12")
	if err := f.prepare(fs, dir); err != nil {
		t.Fatal(err)
	}
	if !f.ref.Repo.Same(f.origin) || f.ref.Number != 12 {
		t.Fatalf("ref %+v, origin %+v", f.ref, f.origin)
	}
	f, fs = parseIssueFlags(t, "--issue", "https://dev.azure.com/elsewhere/p/_workitems/edit/12")
	if err := f.prepare(fs, dir); err != nil || f.ref.Repo.Same(f.origin) || f.ref.Repo.Owner != "elsewhere/p" {
		t.Fatalf("another organization: %v %+v", err, f.ref)
	}
}

func TestCIFilesAzure(t *testing.T) {
	got := ciFiles([]string{"M azure-pipelines.yml", "A build/Azure-Pipelines-ci.YAML", "M .azuredevops/pull_request_template.md", "A .pipelines/x.yml",
		"M .azure-pipelines/t.yml", "M src/azure.go", "M docs/azure-pipelines.md"})
	want := "azure-pipelines.yml,build/Azure-Pipelines-ci.YAML,.azuredevops/pull_request_template.md,.pipelines/x.yml,.azure-pipelines/t.yml"
	if strings.Join(got, ",") != want {
		t.Fatalf("ciFiles = %v", got)
	}
	// Azure DevOps mentions are @<id>: defused like @user.
	if d := defuseRefs("ping @<6a5d-guid> and @bob"); reMention.MatchString(d) || !strings.Contains(d, "@\u2060<6a5d-guid>") {
		t.Fatalf("defused %q", d)
	}
	// "Fixes AB#12" in a GitHub repository closes an Azure Boards work item.
	if d := defuseRefs("Fixes AB#12, closes ab#3"); reCloseRef.MatchString(d) {
		t.Fatalf("AB# not defused: %q", d)
	}
}

// rw review --post on Azure DevOps Server (AZURE_DEVOPS_HOST with a path):
// the diff is rebuilt from the file versions, the finding on a changed
// line becomes an active thread on it, the rest one thread with the text.
func TestAzureServerReview(t *testing.T) {
	dir, _, _, rr, out := reviewSetup(t)
	var mu sync.Mutex
	var posted []map[string]any
	blobs := map[string]string{"o1": "one\ntwo\nthree\n", "n1": "ONE\nTWO\nthree\n"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		const repo = "/tfs/Coll/proj/_apis/git/repositories/app"
		p := r.URL.EscapedPath()
		if r.URL.Query().Get("api-version") != "6.0" {
			t.Errorf("%s: api-version %q (want a server's)", r.URL, r.URL.Query().Get("api-version"))
		}
		switch {
		case r.Method == "GET" && p == repo+"/pullrequests/12":
			io.WriteString(w, `{"pullRequestId":12,"title":"Shout","description":"Please merge @everyone","status":"active","sourceRefName":"refs/heads/rw/shout","targetRefName":"refs/heads/main","lastMergeSourceCommit":{"commitId":"feed"}}`)
		case r.Method == "GET" && p == repo+"/pullRequests/12/iterations":
			io.WriteString(w, `{"value":[{"id":1,"sourceRefCommit":{"commitId":"feed"},"commonRefCommit":{"commitId":"base"}}]}`)
		case r.Method == "GET" && p == repo+"/pullRequests/12/iterations/1/changes":
			io.WriteString(w, `{"changeEntries":[{"changeTrackingId":3,"changeType":"edit","item":{"objectId":"n1","originalObjectId":"o1","path":"/a.txt"}}]}`)
		case r.Method == "GET" && strings.HasPrefix(p, repo+"/blobs/"):
			io.WriteString(w, blobs[strings.TrimPrefix(p, repo+"/blobs/")])
		case r.Method == "POST" && p == repo+"/pullRequests/12/threads":
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			posted = append(posted, in)
			fmt.Fprintf(w, `{"id":%d}`, len(posted))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AZURE_DEVOPS_HOST", srv.URL+"/tfs")
	run(t, dir, "remote", "set-url", "origin", srv.URL+"/tfs/Coll/proj/_git/app")
	reviewIn = strings.NewReader("y\n")
	if err := runReview(context.Background(), reviewCommon(t, "--dir", dir, "--provider", "claude"), "12", reviewOptions{post: true}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(rr.specs) != 1 || !strings.Contains(rr.specs[0].Prompt, "+TWO") || len(posted) != 2 {
		t.Fatalf("%d agent runs, %d threads\n%s", len(rr.specs), len(posted), out.String())
	}
	inline, _ := json.Marshal(posted[0])
	for _, w := range []string{`"status":"active"`, `"filePath":"/a.txt"`, `"line":2`, `"changeTrackingId":3`} {
		if !strings.Contains(string(inline), w) {
			t.Errorf("inline thread lacks %s: %s", w, inline)
		}
	}
	text := posted[1]["comments"].([]any)[0].(map[string]any)["content"].(string)
	if posted[1]["status"] != nil || !strings.Contains(text, "outside the diff") || !strings.Contains(text, rwMark) || reMention.MatchString(text) || reMention.MatchString(string(inline)) {
		t.Fatalf("summary thread %v", posted[1])
	}
	if want := srv.URL + "/tfs/Coll/proj/_git/app/pullrequest/12?discussionId=2"; !strings.Contains(out.String(), want) {
		t.Fatalf("output lacks %s:\n%s", want, out.String())
	}
	// rw watch checks the folder's origin with the server's path, which
	// the host name alone does not give.
	repo := forge.Repo{Kind: forge.Azure, Host: "127.0.0.1", Owner: "Coll/proj", Name: "app", Web: srv.URL + "/tfs"}
	if err := checkOrigin(dir, repo); err != nil {
		t.Fatalf("checkOrigin: %v", err)
	}
	if e := (watchEntry{Forge: "azure", Host: "127.0.0.1", Web: srv.URL + "/tfs", Owner: "Coll/proj", Name: "app", Number: 12}); !watchMatches(e, srv.URL+"/tfs/Coll/proj/_git/app/pullrequest/12") {
		t.Fatal("watchMatches: the pull request URL under the server's path")
	}
}
