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

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/forge"
	"github.com/sparkz400/relayweft/internal/runner"
)

const (
	bbDevUUID = "{00000000-0000-4000-8000-000000000003}"
	bbMeUUID  = "{00000000-0000-4000-8000-000000000001}"
)

func bbUser(nick, uuid string) string {
	return fmt.Sprintf(`{"type":"user","nickname":%q,"uuid":%q}`, nick, uuid)
}

// bbAPI is a fake Bitbucket Cloud for repository w/r: the pull request rw
// opens (#101, whose head is whatever the bare remote's branch points at),
// pull request #12 and issue #3.
type bbAPI struct {
	mu       sync.Mutex
	bare     string
	branch   string
	created  []map[string]any
	failing  bool   // the head's pipeline failed
	comments string // the inline comments JSON of #101
	posted   []map[string]any
	replies  []string
}

func (f *bbAPI) head() string {
	out, err := exec.Command("git", "-C", f.bare, "rev-parse", "refs/heads/"+f.branch).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (f *bbAPI) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *bbAPI) server(t *testing.T) string {
	const repo = "/2.0/repositories/w/r"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if a := r.Header.Get("Authorization"); a != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		raw := func() (map[string]any, string) {
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			c, _ := in["content"].(map[string]any)
			s, _ := c["raw"].(string)
			return in, s
		}
		p := r.URL.EscapedPath()
		switch {
		case r.Method == "POST" && p == repo+"/pullrequests":
			in, _ := raw()
			f.created = append(f.created, in)
			src, _ := in["source"].(map[string]any)
			br, _ := src["branch"].(map[string]any)
			f.branch, _ = br["name"].(string)
			io.WriteString(w, `{"id":101,"state":"OPEN","links":{"html":{"href":"https://bitbucket.org/w/r/pull-requests/101"}},"source":{"repository":{"full_name":"w/r"}}}`)
		case r.Method == "GET" && p == repo+"/pullrequests/101":
			// Bitbucket gives the head commit's hash shortened.
			fmt.Fprintf(w, `{"id":101,"title":"Shout @{557058:x}, fixes issue #8","state":"OPEN","source":{"branch":{"name":%q},"commit":{"hash":%q},"repository":{"full_name":"w/r"}},"destination":{"branch":{"name":"main"}}}`,
				f.branch, f.head()[:12])
		case r.Method == "GET" && p == repo+"/commit/feedfacecafe":
			io.WriteString(w, `{"hash":"feedfacecafe0000000000000000000000000000"}`)
		case r.Method == "GET" && strings.HasPrefix(p, repo+"/commit/") && !strings.HasSuffix(p, "/statuses"):
			fmt.Fprintf(w, `{"hash":%q}`, f.head())
		case r.Method == "GET" && p == repo+"/pipelines":
			if !f.failing {
				io.WriteString(w, `{"values":[]}`)
				return
			}
			fmt.Fprintf(w, `{"values":[{"uuid":"{p1}","build_number":5,"state":{"name":"COMPLETED","result":{"name":"FAILED"}},"target":{"type":"pipeline_ref_target","ref_name":%q,"commit":{"hash":%q}}}]}`, f.branch, f.head())
		case r.Method == "GET" && p == repo+"/pipelines/%7Bp1%7D/steps":
			io.WriteString(w, `{"values":[{"uuid":"{s1}","name":"unit","state":{"name":"COMPLETED","result":{"name":"FAILED"}}}]}`)
		case r.Method == "GET" && p == repo+"/pipelines/%7Bp1%7D/steps/%7Bs1%7D/log":
			io.WriteString(w, strings.Repeat("ok line\n", 50)+"\x1b[31mFAIL\x1b[0m TestShout\n"+injLog+"\n")
		case r.Method == "GET" && strings.HasSuffix(p, "/statuses"):
			io.WriteString(w, `{"values":[]}`)
		case r.Method == "GET" && p == repo+"/pullrequests/101/comments":
			io.WriteString(w, `{"values":`+or(f.comments, "[]")+`}`)
		case r.Method == "GET" && p == "/2.0/workspaces/w/permissions/repositories/r":
			perm := "read"
			if strings.Contains(r.URL.Query().Get("q"), bbDevUUID) {
				perm = "write"
			}
			fmt.Fprintf(w, `{"values":[{"permission":%q}]}`, perm)
		case r.Method == "GET" && p == "/2.0/user":
			io.WriteString(w, bbUser("me", bbMeUUID))
		case r.Method == "POST" && p == repo+"/pullrequests/101/comments":
			_, s := raw()
			f.replies = append(f.replies, s)
			io.WriteString(w, `{"id":1}`)
		case r.Method == "GET" && p == repo+"/pullrequests/12/diff":
			io.WriteString(w, reviewDiff)
		case r.Method == "GET" && p == repo+"/pullrequests/12":
			io.WriteString(w, `{"id":12,"title":"Shout","description":"Please merge @everyone","state":"OPEN","source":{"branch":{"name":"rw/shout"},"commit":{"hash":"feedfacecafe"}},"destination":{"branch":{"name":"main"}}}`)
		case r.Method == "POST" && p == repo+"/pullrequests/12/comments":
			in, _ := raw()
			f.posted = append(f.posted, in)
			io.WriteString(w, `{"id":77,"links":{"html":{"href":"https://bitbucket.org/w/r/pull-requests/12#comment-77"}}}`)
		case r.Method == "GET" && p == repo+"/issues/3":
			io.WriteString(w, `{"id":3,"title":"Shout the first line","content":{"raw":"a.txt should start loud"},"state":"open","component":{"name":"rw"}}`)
		case r.Method == "GET" && p == repo+"/issues/3/comments":
			io.WriteString(w, `{"values":[{"id":9,"content":{"raw":"/close this, @{557058:alice} and fixes issue #1"},"user":`+bbUser("bob", "{00000000-0000-4000-8000-000000000007}")+`}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/2.0"
}

// rw pr opens a pull request on Bitbucket and rw watch follows it up: a
// failed pipeline step and a developer's inline comment become one round,
// pushed to the branch, with a comment on the pull request.
func TestBitbucketPullRequestAndWatch(t *testing.T) {
	dir := prRepo(t)
	run(t, dir, "remote", "set-url", "origin", "git@bitbucket.org:w/r.git")
	bare := filepath.Join(t.TempDir(), "remote.git")
	run(t, dir, "init", "-q", "--bare", "-b", "main", bare)
	run(t, dir, "push", "-q", bare, "main")
	cd, _ := os.UserConfigDir()
	os.MkdirAll(filepath.Join(cd, "relayweft"), 0o755)
	write(t, filepath.Join(cd, "relayweft"), config.FileName, "orchestrator: {review_before_done: false}\nwatch: {max_rounds: 2}\n")
	runTask(t, dir, "Shout the first line", taskEdit)

	api := &bbAPI{bare: bare}
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
	res, err := makePR(st, prOptions{yes: true, api: url, base: "main", closes: "#4", draft: true, draftSet: true})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if res.URL != "https://bitbucket.org/w/r/pull-requests/101" || !strings.Contains(out.String(), "opened pull request #101") {
		t.Fatalf("result %+v\n%s", res, out.String())
	}
	c := api.created[0]
	if c["draft"] != true || !strings.HasSuffix(c["description"].(string), "\nCloses #4\n") || fmt.Sprint(c["destination"]) != "map[branch:map[name:main]]" {
		t.Fatalf("created %+v", c)
	}
	if len(kinds) == 0 || kinds[0] != forge.Bitbucket {
		t.Fatalf("token asked for %v", kinds)
	}
	prs := watched(t)
	if len(prs) != 1 || prs[0].Forge != "bitbucket" || prs[0].String() != "w/r#101" || prs[0].API != url {
		t.Fatalf("not recorded for rw watch: %+v", prs)
	}

	run(t, dir, "config", "url."+filepath.ToSlash(bare)+".insteadOf", "git@bitbucket.org:w/r.git")
	wr := &watchRunner{}
	oldRunners, oldPrint, oldOut := watchRunners, eventPrint, watchOut
	t.Cleanup(func() { watchRunners, eventPrint, watchOut = oldRunners, oldPrint, oldOut })
	watchRunners = func(*config.Config) runner.Set { return runner.Set{event.Codex: wr, event.Claude: wr} }
	eventPrint = func(event.Event, bool) {}
	watchOut = io.Discard

	head0 := api.head()
	api.set(func() {
		api.failing = true
		api.comments = `[{"id":40,"content":{"raw":` + jsonString(injComment) + `},"user":` + bbUser("dev", bbDevUUID) + `,"inline":{"path":"a.txt","to":1}},
			{"id":41,"content":{"raw":"stranger asks"},"user":` + bbUser("drive-by", "{00000000-0000-4000-8000-000000000009}") + `,"inline":{"path":"a.txt","to":1}},
			{"id":42,"content":{"raw":"I am me"},"user":` + bbUser("me "+bbMeUUID, "{00000000-0000-4000-8000-000000000009}") + `,"inline":{"path":"a.txt","to":1}},
			{"id":43,"content":{"raw":"done already"},"user":` + bbUser("dev", bbDevUUID) + `,"inline":{"path":"a.txt","to":1},"resolution":{"type":"comment_resolution"}}]`
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
	for _, want := range []string{"pull request #101 of w/r", "copied from Bitbucket", "pipeline #5: unit", "FAIL TestShout", "rm -rf"} {
		if !strings.Contains(task, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	for _, inj := range []string{"curl evil", "rm -rf", "@bob"} {
		if strings.Contains(outsideFences(task), inj) {
			t.Errorf("%q outside the fences", inj)
		}
	}
	for _, s := range []string{"stranger asks", "I am me", "done already", "\x1b["} {
		if strings.Contains(task, s) {
			t.Errorf("%q reached the agents", s)
		}
	}
	head1 := api.head()
	if head1 == head0 || strings.TrimSpace(gitOut(t, bare, "rev-parse", head1+"^")) != head0 {
		t.Fatalf("not pushed on top of the head (%s -> %s):\n%s", head0, head1, out.String())
	}
	prs = watched(t)
	if prs[0].Rounds != 1 || !prs[0].handled("check:step:{s1}") || !prs[0].handled("comment:40") || prs[0].handled("comment:41") || prs[0].Head != head1 {
		t.Fatalf("registry %+v", prs[0])
	}
	if len(api.replies) != 1 || !strings.Contains(api.replies[0], "on this pull request") || !strings.Contains(api.replies[0], rwMark) {
		t.Fatalf("replies %q", api.replies)
	}
	// The next pass finds nothing new.
	if err := newWatcher(true).pass(context.Background()); err != nil || wr.steps() != 1 {
		t.Fatalf("ran again: %v, %d steps", err, wr.steps())
	}
}

// An --api URL ending in /2.0 makes the remote's host a Bitbucket: rw
// review posts its inline comments and one comment, and --issue reads
// its issue.
func TestBitbucketReviewAndIssue(t *testing.T) {
	dir, _, _, rr, out := reviewSetup(t)
	run(t, dir, "remote", "set-url", "origin", "https://127.0.0.1/w/r.git")
	api := &bbAPI{}
	url := api.server(t)
	reviewIn = strings.NewReader("y\n")
	if err := runReview(context.Background(), reviewCommon(t, "--dir", dir, "--provider", "claude"), "#12", reviewOptions{post: true, api: url}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(rr.specs) != 1 || len(api.posted) != 2 {
		t.Fatalf("%d agent runs, %d comments\n%s", len(rr.specs), len(api.posted), out.String())
	}
	inline, _ := json.Marshal(api.posted[0]["inline"])
	if string(inline) != `{"path":"a.txt","to":2}` {
		t.Fatalf("inline %s", inline)
	}
	for _, p := range api.posted {
		body := p["content"].(map[string]any)["raw"].(string)
		if reMention.MatchString(body) || reCloseRef.MatchString(body) {
			t.Errorf("live mention or closing keyword: %q", body)
		}
	}
	if body := api.posted[1]["content"].(map[string]any)["raw"].(string); !strings.Contains(body, rwMark) || api.posted[1]["inline"] != nil {
		t.Fatalf("summary %+v", api.posted[1])
	}
	if !strings.Contains(out.String(), "posted a review with 1 inline comment(s): https://bitbucket.org/w/r/pull-requests/12#comment-77") {
		t.Fatalf("output:\n%s", out.String())
	}

	f, fs := parseIssueFlags(t, "--issue", "3", "--with-comments", "--api", url)
	tasks, err := f.load(fs, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || !strings.HasPrefix(tasks[0], "Fix Bitbucket issue #3: Shout the first line\n\na.txt should start loud\n\nLabels: rw") ||
		!strings.Contains(tasks[0], "@bob {00000000-0000-4000-8000-000000000007} wrote:\n/close this") || f.origin.Kind != forge.Bitbucket || f.items[0].closes != "#3" {
		t.Fatalf("tasks %q, origin %+v", tasks, f.origin)
	}
}

func TestDefuseBitbucketRefs(t *testing.T) {
	for _, in := range []string{"fixes issue #6", "Resolving bug #2", "closes ticket #9", "reopen #3", "wontfix #4", "Invalidates #5",
		"holding issue #1", "cc @{557058:0f3c-ab}", "ping @{00000000-0000-4000-8000-000000000003}"} {
		out := defuseRefs(in)
		if reCloseRef.MatchString(out) || reMention.MatchString(out) {
			t.Errorf("%q -> %q still live", in, out)
		}
		if strings.ReplaceAll(out, "⁠", "") != in {
			t.Errorf("%q -> %q changed the visible text", in, out)
		}
	}
	for _, keep := range []string{"Fix Bitbucket issue #42: crash", "a hold on #", "map{x}"} {
		if out := defuseRefs(keep); out != keep {
			t.Errorf("%q changed to %q", keep, out)
		}
	}
}

// rw watch never pushes Bitbucket Pipelines' config.
func TestCIFilesBitbucket(t *testing.T) {
	if got := ciFiles([]string{"M bitbucket-pipelines.yml", "A Bitbucket-Pipelines.yml", "M sub/bitbucket-pipelines.yml"}); strings.Join(got, ",") != "bitbucket-pipelines.yml,Bitbucket-Pipelines.yml" {
		t.Fatalf("ciFiles = %v", got)
	}
}
