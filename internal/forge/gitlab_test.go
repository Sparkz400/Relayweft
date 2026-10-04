package forge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeGitLab serves project g/sub/p and merge request !7 on /api/v4,
// checking the token and recording what is posted.
type fakeGitLab struct {
	mu          sync.Mutex
	token       string
	noDiffs     bool // an old GitLab without /diffs
	created     []map[string]any
	notes       []string // POSTed notes: "<issues|merge_requests>/<n>: body"
	discussions []map[string]any
	memberCalls int
	trace       string // the job log of 501 (default: noise, then FAIL TestX)
}

func (f *fakeGitLab) handler(t *testing.T) http.Handler {
	const proj = "/api/v4/projects/g%2Fsub%2Fp"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		auth := r.Header.Get("Authorization")
		if (auth != "" && auth != "Bearer "+f.token) || (auth == "" && r.Method != "GET") {
			w.WriteHeader(401)
			io.WriteString(w, `{"message":"401 Unauthorized"}`)
			return
		}
		p := r.URL.EscapedPath()
		if !strings.HasPrefix(p, proj) && p != "/api/v4/user" {
			t.Errorf("unexpected path %s", p)
			w.WriteHeader(404)
			return
		}
		p = strings.TrimPrefix(p, proj)
		q := r.URL.Query()
		page := q.Get("page")
		write := func(s string) { io.WriteString(w, s) }
		switch {
		case r.Method == "GET" && p == "":
			write(`{"default_branch":"trunk"}`)
		case r.Method == "GET" && p == "/issues/4":
			write(`{"iid":4,"title":"Crash","description":"Steps","state":"opened","web_url":"https://gitlab.com/g/sub/p/-/issues/4","labels":["bug","sy"],"author":{"username":"ann"}}`)
		case r.Method == "GET" && p == "/issues/4/notes":
			if q.Get("sort") != "asc" {
				t.Errorf("notes not oldest first: %s", r.URL.RawQuery)
			}
			write(`[{"id":1,"body":"changed the label","system":true,"author":{"username":"ann"}},{"id":2,"body":"me too","author":{"id":3,"username":"bob"}}]`)
		case r.Method == "PUT" && p == "/issues/4/notes/2":
			var in struct{ Body string }
			json.NewDecoder(r.Body).Decode(&in)
			f.notes = append(f.notes, "edit issues/4/notes/2: "+in.Body)
			write(`{}`)
		case r.Method == "GET" && p == "/issues":
			if q.Get("state") != "opened" || q.Get("labels") != "sy" || q.Get("sort") != "asc" {
				t.Errorf("issue query %s", r.URL.RawQuery)
			}
			if page == "1" {
				var is []string
				for i := 1; i <= 100; i++ {
					is = append(is, fmt.Sprintf(`{"iid":%d,"title":"t","state":"opened"}`, i))
				}
				write("[" + strings.Join(is, ",") + "]")
			} else {
				write(`[{"iid":101,"title":"last","state":"opened"}]`)
			}
		case r.Method == "GET" && p == "/merge_requests" && q.Get("state") == "opened":
			write(`[{"iid":7,"description":"Closes #5","state":"opened","source_project_id":1,"target_project_id":1}]`)
		case r.Method == "POST" && p == "/merge_requests":
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			f.created = append(f.created, in)
			if in["source_branch"] == "exists" {
				w.WriteHeader(409)
				write(`{"message":["Another open merge request already exists for this source branch: !3"]}`)
				return
			}
			write(`{"iid":8,"web_url":"https://gitlab.com/g/sub/p/-/merge_requests/8","state":"opened","source_project_id":1,"target_project_id":1}`)
		case r.Method == "POST" && (strings.HasSuffix(p, "/notes")):
			var in struct{ Body string }
			json.NewDecoder(r.Body).Decode(&in)
			f.notes = append(f.notes, strings.TrimPrefix(strings.TrimSuffix(p, "/notes"), "/")+": "+in.Body)
			w.WriteHeader(201)
			write(`{"id":99}`)
		case r.Method == "GET" && p == "/merge_requests/7":
			write(`{"iid":7,"title":"Fix","description":"d","state":"merged","web_url":"https://gitlab.com/g/sub/p/-/merge_requests/7","source_branch":"sy/x","target_branch":"main","sha":"abc","source_project_id":1,"target_project_id":1,"diff_refs":{"base_sha":"b0","head_sha":"abc","start_sha":"s0"}}`)
		case r.Method == "GET" && p == "/merge_requests/7/diffs":
			if f.noDiffs {
				w.WriteHeader(404)
				return
			}
			write(`[{"old_path":"a.go","new_path":"a.go","diff":"@@ -1,2 +1,2 @@\n-x\n+y\n z\n"},{"old_path":"img.png","new_path":"img.png","new_file":true,"diff":""}]`)
		case r.Method == "GET" && p == "/merge_requests/7/changes":
			write(`{"changes":[{"old_path":"gone.go","new_path":"gone.go","deleted_file":true,"diff":"@@ -1 +0,0 @@\n-bye"}]}`)
		case r.Method == "GET" && p == "/pipelines":
			if q.Get("sha") != "abc" {
				t.Errorf("pipelines for %s", q.Get("sha"))
			}
			// 10 is older than 12 on the same ref: only 12 counts.
			write(`[{"id":10,"ref":"sy/x"},{"id":12,"ref":"sy/x"},{"id":11,"ref":"refs/merge-requests/7/head"}]`)
		case r.Method == "GET" && p == "/pipelines/12/jobs":
			if q.Get("scope[]") != "failed" {
				t.Errorf("jobs query %s", r.URL.RawQuery)
			}
			write(`[{"id":502,"name":"lint","stage":"test","status":"failed","allow_failure":true},{"id":501,"name":"unit","stage":"test","status":"failed","failure_reason":"script_failure"}]`)
		case r.Method == "GET" && p == "/pipelines/11/jobs":
			write(`[]`)
		case r.Method == "GET" && p == "/pipelines/10/jobs":
			t.Error("read the jobs of a pipeline that was re-run")
			write(`[]`)
		case r.Method == "GET" && p == "/jobs/501/trace":
			if f.trace != "" {
				write(f.trace)
				break
			}
			write(strings.Repeat("noise\n", 1000) + "FAIL TestX\n")
		case r.Method == "GET" && p == "/merge_requests/7/discussions":
			write(`[{"notes":[{"id":30,"type":"DiffNote","body":"rename x","author":{"id":3,"username":"dev"},"resolvable":true,"position":{"new_path":"a.go","new_line":2}},
				{"id":31,"type":"DiffNote","body":"done","author":{"id":3,"username":"dev"},"resolvable":true,"resolved":true,"position":{"new_path":"a.go","new_line":2}}]},
				{"notes":[{"id":20,"type":"DiffNote","body":"guest says","author":{"id":4,"username":"guest"},"position":{"new_path":"a.go","new_line":1}}]},
				{"notes":[{"id":21,"type":null,"body":"LGTM","author":{"id":3,"username":"dev"}}]},
				{"notes":[{"id":22,"type":"DiffNote","body":"bot","author":{"id":5,"username":"project_1_bot_0a1b"},"position":{"new_path":"a.go","new_line":1}}]},
				{"notes":[{"id":23,"type":"DiffNote","body":"assigned","system":true,"author":{"id":3,"username":"dev"}}]}]`)
		case r.Method == "GET" && strings.HasPrefix(p, "/members/all/"):
			f.memberCalls++
			switch strings.TrimPrefix(p, "/members/all/") {
			case "3":
				write(`{"access_level":30}`)
			case "4":
				write(`{"access_level":10}`)
			default:
				w.WriteHeader(404)
			}
		case r.Method == "GET" && r.URL.Path == "/api/v4/user":
			write(`{"username":"me"}`)
		case r.Method == "POST" && p == "/merge_requests/7/discussions":
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			f.discussions = append(f.discussions, in)
			if pos, _ := in["position"].(map[string]any); pos["new_line"] == float64(40) {
				w.WriteHeader(400)
				write(`{"message":"400 Bad request - Note {:line_code=>[\"can't be blank\"]}"}`)
				return
			}
			w.WriteHeader(201)
			write(`{"id":"d1"}`)
		default:
			w.WriteHeader(404)
			write(`{"message":"404 Not Found"}`)
		}
	})
}

var glRepo = Repo{Kind: GitLab, Host: "gitlab.com", Owner: "g/sub", Name: "p"}

func gitlabServer(t *testing.T) (*fakeGitLab, string) {
	f := &fakeGitLab{token: "good"}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return f, srv.URL + "/api/v4"
}

// The team queue's comment calls: trust per comment author, and edits.
func TestGitLabCommentTrustAndEdit(t *testing.T) {
	f, api := gitlabServer(t)
	c := New(GitLab, api, "good", nil)
	cs, err := c.Comments(glRepo, 4)
	if err != nil || len(cs) != 1 || cs[0].ID != 2 {
		t.Fatalf("%+v %v", cs, err)
	}
	if ok, err := c.CommentTrusted(glRepo, cs[0]); err != nil || !ok {
		t.Fatalf("a developer's comment: %v %v", ok, err)
	}
	if ok, err := c.CommentTrusted(glRepo, Comment{ID: 9, Author: "guest", who: commentAuthor{id: 4}}); err != nil || ok {
		t.Fatalf("a guest's comment: %v %v", ok, err)
	}
	if err := c.EditComment(glRepo, 4, 2, "new text"); err != nil || f.notes[len(f.notes)-1] != "edit issues/4/notes/2: new text" {
		t.Fatalf("%v %v", err, f.notes)
	}
}

func TestGitLabIssuesAndMergeRequests(t *testing.T) {
	f, api := gitlabServer(t)
	c := New(GitLab, api, "good", nil)
	if c.Kind() != GitLab || !c.HasToken() {
		t.Fatal("kind or token")
	}
	is, err := c.Issue(glRepo, 4)
	if err != nil || is.Title != "Crash" || is.Body != "Steps" || is.State != "open" || strings.Join(is.Labels, ",") != "bug,sy" || is.IsPull {
		t.Fatalf("%+v %v", is, err)
	}
	cs, err := c.Comments(glRepo, 4)
	if err != nil || len(cs) != 1 || cs[0].Author != "bob" {
		t.Fatalf("system notes must be left out: %+v %v", cs, err)
	}
	open, err := c.OpenIssues(glRepo, "sy", 0)
	if err != nil || len(open) != 101 || open[100].Number != 101 {
		t.Fatalf("paged issues: %d %v", len(open), err)
	}
	if open, _ := c.OpenIssues(glRepo, "sy", 3); len(open) != 3 {
		t.Fatalf("max: %d", len(open))
	}
	ps, err := c.OpenPulls(glRepo)
	if err != nil || !ClosedBy(ps)[5] {
		t.Fatalf("%+v %v", ps, err)
	}
	if b, err := c.DefaultBranch(glRepo); err != nil || b != "trunk" {
		t.Fatalf("%q %v", b, err)
	}
	pr, err := c.CreatePull(glRepo, NewPull{Title: "Fix it", Head: "sy/fix", Base: "main", Body: "Closes #4", Draft: true})
	if err != nil || pr.Number != 8 || pr.URL != "https://gitlab.com/g/sub/p/-/merge_requests/8" || pr.HeadRepo != "g/sub/p" {
		t.Fatalf("%+v %v", pr, err)
	}
	if got := f.created[0]; got["title"] != "Draft: Fix it" || got["source_branch"] != "sy/fix" || got["target_branch"] != "main" || got["description"] != "Closes #4" {
		t.Fatalf("sent %v", got)
	}
	if _, err := c.CreatePull(glRepo, NewPull{Title: "Draft: already", Head: "b", Base: "main", Draft: true}); err != nil || f.created[1]["title"] != "Draft: already" {
		t.Fatalf("double draft prefix: %v %v", f.created[1]["title"], err)
	}
	_, err = c.CreatePull(glRepo, NewPull{Title: "x", Head: "exists", Base: "main"})
	if err == nil || !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), "409") {
		t.Fatalf("409: %v", err)
	}
	if err := c.CommentIssue(glRepo, 4, "see !8"); err != nil {
		t.Fatal(err)
	}
	if err := c.CommentPull(glRepo, 8, "hi"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.notes, "|") != "issues/4: see !8|merge_requests/8: hi" {
		t.Fatalf("notes %q", f.notes)
	}
	// Without a token a write is a clear 401 that says which token.
	err = New(GitLab, api, "", nil).CommentIssue(glRepo, 4, "x")
	if !IsUnauthorized(err) || !strings.Contains(err.Error(), "GITLAB_TOKEN") {
		t.Fatalf("anonymous write: %v", err)
	}
	// A rejected token: reads go on without it.
	var notes strings.Builder
	stale := New(GitLab, api, "stale", &notes)
	if _, err := stale.Issue(glRepo, 4); err != nil || !stale.Rejected() || stale.HasToken() || !strings.Contains(notes.String(), "GitLab rejected the token") {
		t.Fatalf("stale token: %v %q", err, notes.String())
	}
}

func TestGitLabWatchAndReview(t *testing.T) {
	f, api := gitlabServer(t)
	c := New(GitLab, api, "good", nil)
	p, err := c.Pull(glRepo, 7)
	if err != nil || !p.Merged || p.State != "closed" || p.HeadRef != "sy/x" || p.HeadSHA != "abc" || p.HeadRepo != "g/sub/p" || p.BaseRef != "main" {
		t.Fatalf("%+v %v", p, err)
	}
	diff, err := c.PullDiff(glRepo, 7, 1<<20)
	want := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,2 +1,2 @@\n-x\n+y\n z\ndiff --git a/img.png b/img.png\nBinary files /dev/null and b/img.png differ\n"
	if err != nil || diff != want {
		t.Fatalf("diff %q %v", diff, err)
	}
	if _, err := c.PullDiff(glRepo, 7, 10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	f.noDiffs = true
	if diff, err := c.PullDiff(glRepo, 7, 1<<20); err != nil || !strings.Contains(diff, "--- a/gone.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye\n") {
		t.Fatalf("/changes fallback: %q %v", diff, err)
	}

	checks, err := c.FailedChecks(glRepo, "abc", 100)
	if err != nil || len(checks) != 1 {
		t.Fatalf("%+v %v", checks, err)
	}
	if ch := checks[0]; ch.ID != "501" || ch.Name != "test: unit" || ch.Conclusion != "failed (script_failure)" || !strings.HasSuffix(ch.Log, "FAIL TestX\n") || len(ch.Log) > 100 {
		t.Fatalf("check %+v", ch)
	}

	fb, err := c.Feedback(glRepo, 7)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range fb {
		got = append(got, fmt.Sprintf("%s %s %v %s:%d", x.ID, x.Author, x.Trusted, x.Path, x.Line))
	}
	// Oldest first; resolved, non-diff and system notes left out; a guest
	// and a token bot are untrusted.
	if strings.Join(got, "|") != "comment:20 guest false a.go:1|comment:22 project_1_bot_0a1b false a.go:1|comment:30 dev true a.go:2" {
		t.Fatalf("feedback %q", got)
	}
	if f.memberCalls != 2 {
		t.Fatalf("member lookups %d (cached per user; none for bots)", f.memberCalls)
	}
	if v, err := c.Viewer(); err != nil || v != "me" {
		t.Fatalf("%q %v", v, err)
	}

	url, err := c.CommentReview(glRepo, 7, "abc", "### review\n<!-- switchyard -->\n",
		[]InlineComment{{Path: "a.go", Line: 2, OldLine: 2, Body: "context"}, {Path: "a.go", Line: 1, Body: "added"}, {Path: "a.go", Line: 40, Body: "elsewhere"}})
	if err != nil || url != "https://gitlab.com/g/sub/p/-/merge_requests/7#note_99" {
		t.Fatalf("%q %v", url, err)
	}
	if len(f.discussions) != 3 {
		t.Fatalf("discussions %v", f.discussions)
	}
	pos := f.discussions[0]["position"].(map[string]any)
	if pos["base_sha"] != "b0" || pos["start_sha"] != "s0" || pos["head_sha"] != "abc" || pos["new_line"] != float64(2) || pos["old_line"] != float64(2) || pos["position_type"] != "text" {
		t.Fatalf("position %v", pos)
	}
	if _, ok := f.discussions[1]["position"].(map[string]any)["old_line"]; ok {
		t.Fatal("an added line has no old line")
	}
	last := f.notes[len(f.notes)-1]
	if !strings.HasPrefix(last, "merge_requests/7: ### review") || !strings.Contains(last, "could not place") || !strings.Contains(last, "`a.go:40`: elsewhere") || strings.Contains(last, "added") {
		t.Fatalf("review note %q", last)
	}
	if _, err := c.CommentReview(glRepo, 7, "moved", "x", nil); err == nil || !strings.Contains(err.Error(), "new commits") {
		t.Fatalf("head moved: %v", err)
	}
}

// A real GitLab Runner 19 job log: every line starts with a timestamp and
// a stream marker, and sections are framed by section_start/section_end
// markers. Found on GitLab CE 19.4 with Runner 19.4: the prefixes took
// about half of the log tail sy watch passes to the agents.
func TestGitLabTraceTimestampsAndSections(t *testing.T) {
	f, api := gitlabServer(t)
	var b strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "2026-10-04T18:01:47.%06dZ 01O fetching object %d\n", i, i)
	}
	b.WriteString("2026-10-04T18:01:47.651168Z 00O section_end:1791136907:get_sources\r\x1b[0K\n" +
		"2026-10-04T18:01:47.651596Z 00O+section_start:1791136907:step_script[collapsed=true]\r\x1b[0K\x1b[0K\x1b[36;1mExecuting \"step_script\" stage of the job script\x1b[0;m\n" +
		"2026-10-04T18:01:47.925374Z 01O \x1b[32;1m$ sh test.sh\x1b[0;m\n" +
		"2026-10-04T18:01:47.927012Z 01E FAIL value.txt is 43, want 42\n" +
		"2026-10-04T18:01:48.072851Z 00O section_end:1791136908:step_script\r\x1b[0K\n" +
		"2026-10-04T18:01:48.495354Z 00O \x1b[31;1mERROR: Job failed: exit code 1\n")
	f.trace = b.String()
	c := New(GitLab, api, "good", nil)
	checks, err := c.FailedChecks(glRepo, "abc", 400)
	if err != nil || len(checks) != 1 {
		t.Fatalf("%+v %v", checks, err)
	}
	log := checks[0].Log
	for _, junk := range []string{"2026-10-04T", "01O", "00O", "01E", "section_start", "section_end"} {
		if strings.Contains(log, junk) {
			t.Errorf("log tail keeps %q:\n%s", junk, log)
		}
	}
	for _, keep := range []string{"Executing \"step_script\" stage", "$ sh test.sh", "FAIL value.txt is 43, want 42\n", "ERROR: Job failed: exit code 1"} {
		if !strings.Contains(log, keep) {
			t.Errorf("log tail lacks %q:\n%s", keep, log)
		}
	}
	if len(log) > 400 || !strings.Contains(log, "fetching object 199") {
		t.Errorf("log tail is %d bytes (max 400) or lost the lines before the script:\n%s", len(log), log)
	}
}
