package forge

import (
	"encoding/base64"
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

// Account UUIDs of the fake Bitbucket's people.
const (
	bbMe      = "{00000000-0000-4000-8000-000000000001}"
	bbDev     = "{00000000-0000-4000-8000-000000000003}"
	bbVisitor = "{00000000-0000-4000-8000-000000000005}"
)

func bbUserJSON(nick, uuid string) string {
	return fmt.Sprintf(`{"type":"user","nickname":%q,"display_name":"Name of %s","uuid":%q}`, nick, nick, uuid)
}

// fakeBitbucket serves repository w/r on /2.0: issues 4 and 9, pull
// request #7 with head commit abc1234def (shortened as Bitbucket does).
type fakeBitbucket struct {
	mu        sync.Mutex
	url       string
	auth      string // the Authorization header it accepts
	noIssues  bool   // the repository has no issue tracker
	permAdmin bool   // the token may read workspace permissions
	noPipes   bool   // the token may not read pipelines
	created   []map[string]any
	comments  []string
	inline    []map[string]any
	lookups   []string
	elsewhere string // a next link it gives for page 2 of the issues
}

const bbHead = "abc1234def0123456789abc1234def0123456789"

func (f *fakeBitbucket) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		auth := r.Header.Get("Authorization")
		if (auth != "" && auth != f.auth) || (auth == "" && r.Method != "GET") {
			w.WriteHeader(401)
			io.WriteString(w, `{"type":"error","error":{"message":"Access token expired."}}`)
			return
		}
		p := strings.TrimPrefix(r.URL.EscapedPath(), "/2.0")
		q := r.URL.Query()
		write := func(s string) { io.WriteString(w, s) }
		body := func() map[string]any {
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			return in
		}
		raw := func(in map[string]any) string {
			c, _ := in["content"].(map[string]any)
			s, _ := c["raw"].(string)
			return s
		}
		const repo = "/repositories/w/r"
		switch {
		case r.Method == "GET" && p == repo:
			fmt.Fprintf(w, `{"mainbranch":{"name":"trunk"},"has_issues":%v}`, !f.noIssues)
		case strings.HasPrefix(p, repo+"/issues") && f.noIssues:
			w.WriteHeader(404)
			write(`{"type":"error","error":{"message":"Repository has no issue tracker."}}`)
		case r.Method == "GET" && p == repo+"/issues/4":
			write(`{"id":4,"title":"Crash","content":{"raw":"Steps"},"state":"new","kind":"bug","component":{"name":"rw"},
				"reporter":` + bbUserJSON("ann", bbVisitor) + `,"links":{"html":{"href":"https://bitbucket.org/w/r/issues/4"}}}`)
		case r.Method == "GET" && p == repo+"/issues/9":
			write(`{"id":9,"title":"Old","state":"resolved"}`)
		case r.Method == "GET" && p == repo+"/issues/4/comments":
			if q.Get("sort") != "created_on" || q.Get("pagelen") != "100" {
				t.Errorf("comment query %s", r.URL.RawQuery)
			}
			write(`{"values":[{"id":31,"content":{"raw":"me too"},"user":` + bbUserJSON("bob", bbVisitor) + `},
				{"id":32,"content":{"raw":"ok"},"user":` + bbUserJSON("dev", bbDev) + `},
				{"id":33,"content":{"raw":""},"user":` + bbUserJSON("dev", bbDev) + `},
				{"id":34,"content":{"raw":"claim"},"user":{"type":"app_user","display_name":"Some app","uuid":"{00000000-0000-4000-8000-000000000009}"}},
				{"id":35,"content":{"raw":"I am you"},"user":` + bbUserJSON("me "+bbMe, bbVisitor) + `}]}`)
		case r.Method == "PUT" && p == repo+"/issues/4/comments/31":
			f.comments = append(f.comments, "edit 31: "+raw(body()))
			write(`{}`)
		case r.Method == "GET" && p == repo+"/issues":
			if q.Get("page") == "2" {
				write(`{"values":[{"id":5,"created_on":"2026-02-01T00:00:00Z"}]}`)
				return
			}
			if q.Get("q") != `(state="new" OR state="open") AND component.name="r\"w"` || q.Get("sort") != "created_on" || q.Get("pagelen") != "50" {
				t.Errorf("issue query %q", q.Get("q"))
			}
			next := f.url + repo + "/issues?page=2&pagelen=50"
			if f.elsewhere != "" {
				next = f.elsewhere
			}
			fmt.Fprintf(w, `{"values":[{"id":3,"created_on":"2026-01-01T00:00:00Z"},{"id":9,"created_on":"2026-03-01T00:00:00Z"}],"next":%q}`, next)
		case r.Method == "GET" && p == repo+"/pullrequests":
			if q.Get("state") != "OPEN" {
				t.Errorf("pull query %s", r.URL.RawQuery)
			}
			write(`{"values":[{"id":7,"description":"Fixes issue #5"}]}`)
		case r.Method == "POST" && p == repo+"/pullrequests":
			f.created = append(f.created, body())
			write(`{"id":8,"state":"OPEN","links":{"html":{"href":"https://bitbucket.org/w/r/pull-requests/8"}},
				"source":{"branch":{"name":"rw/fix"},"repository":{"full_name":"w/r"}}}`)
		case r.Method == "POST" && strings.HasPrefix(p, repo+"/issues/") && strings.HasSuffix(p, "/comments"):
			f.comments = append(f.comments, strings.TrimSuffix(strings.TrimPrefix(p, repo+"/issues/"), "/comments")+": "+raw(body()))
			w.WriteHeader(201)
			write(`{}`)
		case r.Method == "GET" && p == repo+"/pullrequests/7":
			write(`{"id":7,"title":"Fix","state":"OPEN","links":{"html":{"href":"https://bitbucket.org/w/r/pull-requests/7"}},
				"source":{"branch":{"name":"rw/x"},"commit":{"hash":"abc1234def01"},"repository":{"full_name":"w/r"}},
				"destination":{"branch":{"name":"main"}},
				"participants":[{"user":` + bbUserJSON("dev", bbDev) + `,"state":"changes_requested","participated_on":"2026-10-01T10:00:00Z"},
					{"user":` + bbUserJSON("visitor", bbVisitor) + `,"state":"changes_requested","participated_on":"2026-10-01T11:00:00Z"},
					{"user":` + bbUserJSON("me", bbMe) + `,"state":"approved"}]}`)
		case r.Method == "GET" && p == repo+"/pullrequests/8":
			write(`{"id":8,"state":"MERGED","source":{"branch":{"name":"rw/y"},"commit":{"hash":"abc1234def01"}}}`)
		case r.Method == "GET" && p == repo+"/commit/abc1234def01":
			write(`{"hash":"` + bbHead + `"}`)
		case r.Method == "GET" && p == repo+"/pullrequests/7/diff":
			http.Redirect(w, r, "/2.0"+repo+"/diff/w/r:abc1234def01%0Dw/r:main?from_pullrequest_id=7", http.StatusFound)
		case r.Method == "GET" && strings.HasPrefix(p, repo+"/diff/"):
			if auth == "" {
				w.WriteHeader(404) // private: the redirect must keep the token
				return
			}
			write("diff --git a/a.go b/a.go\n")
		case r.Method == "GET" && p == repo+"/pipelines":
			if f.noPipes {
				w.WriteHeader(403)
				write(`{"type":"error","error":{"message":"Your credentials lack one or more required privilege scopes."}}`)
				return
			}
			if q.Get("target.commit.hash") != bbHead || q.Get("sort") != "-created_on" {
				t.Errorf("pipeline query %s", r.URL.RawQuery)
			}
			// Build 12 re-ran build 10 (same branch); 11 is a custom run
			// that failed before any step; 13 is another commit's.
			write(`{"values":[
				{"uuid":"{p13}","build_number":13,"state":{"name":"COMPLETED","result":{"name":"FAILED"}},"target":{"type":"pipeline_ref_target","ref_name":"rw/x","commit":{"hash":"0000000aaaaa"}}},
				{"uuid":"{p12}","build_number":12,"state":{"name":"COMPLETED","result":{"name":"FAILED"}},"target":{"type":"pipeline_ref_target","ref_name":"rw/x","commit":{"hash":"` + bbHead + `"}}},
				{"uuid":"{p11}","build_number":11,"state":{"name":"COMPLETED","result":{"name":"ERROR"}},"target":{"type":"pipeline_ref_target","ref_name":"rw/x","commit":{"hash":"` + bbHead + `"},"selector":{"type":"custom","pattern":"nightly"}}},
				{"uuid":"{p10}","build_number":10,"state":{"name":"COMPLETED","result":{"name":"FAILED"}},"target":{"type":"pipeline_ref_target","ref_name":"rw/x","commit":{"hash":"` + bbHead + `"}}}]}`)
		case r.Method == "GET" && p == repo+"/pipelines/%7Bp12%7D/steps":
			write(`{"values":[{"uuid":"{s1}","name":"Build","state":{"name":"COMPLETED","result":{"name":"SUCCESSFUL"}}},
				{"uuid":"{s2}","name":"Test","state":{"name":"COMPLETED","result":{"name":"FAILED"}}},
				{"uuid":"{s3}","name":"Deploy","state":{"name":"COMPLETED","result":{"name":"STOPPED"}}}]}`)
		case r.Method == "GET" && p == repo+"/pipelines/%7Bp11%7D/steps":
			write(`{"values":[]}`)
		case r.Method == "GET" && strings.HasPrefix(p, repo+"/pipelines/%7Bp10%7D/"):
			t.Errorf("read the older run: %s", p)
			w.WriteHeader(404)
		case r.Method == "GET" && p == repo+"/pipelines/%7Bp12%7D/steps/%7Bs2%7D/log":
			write(strings.Repeat("noise\n", 100) + "--- FAIL: TestX\n")
		case r.Method == "GET" && p == repo+"/commit/"+bbHead+"/statuses":
			write(`{"values":[{"key":"PIPE","name":"Pipeline #12 for rw/x","state":"FAILED","url":"https://bitbucket.org/w/r/pipelines/results/12","updated_on":"t1"},
				{"key":"lint","name":"Lint","state":"FAILED","description":"2 problems","url":"https://ci.example/9","updated_on":"t2"},
				{"key":"ok","name":"Fine","state":"SUCCESSFUL","updated_on":"t3"}]}`)
		case r.Method == "GET" && p == repo+"/pullrequests/7/comments":
			const thread = `"inline":{"path":"a.go","to":3}`
			write(`{"values":[
				{"id":11,"content":{"raw":"rename"},"user":` + bbUserJSON("dev", bbDev) + `,` + thread + `},
				{"id":12,"content":{"raw":"fixed?"},"user":` + bbUserJSON("dev", bbDev) + `,"inline":{"path":"a.go","to":4},"resolution":{"type":"comment_resolution"}},
				{"id":15,"content":{"raw":"reply in a resolved thread"},"user":` + bbUserJSON("dev", bbDev) + `,"inline":{"path":"a.go","to":4},"parent":{"id":12}},
				{"id":13,"content":{"raw":"gone"},"user":` + bbUserJSON("dev", bbDev) + `,"deleted":true,` + thread + `},
				{"id":14,"content":{"raw":"draft"},"user":` + bbUserJSON("dev", bbDev) + `,"pending":true,` + thread + `},
				{"id":16,"content":{"raw":"general"},"user":` + bbUserJSON("dev", bbDev) + `},
				{"id":17,"content":{"raw":"old side"},"user":` + bbUserJSON("dev", bbDev) + `,"inline":{"path":"b.go","from":2,"to":null}},
				{"id":100,"content":{"raw":"drive-by"},"user":` + bbUserJSON("visitor", bbVisitor) + `,` + thread + `},
				{"id":101,"content":{"raw":"bot"},"user":{"type":"app_user","uuid":"{00000000-0000-4000-8000-000000000009}"},` + thread + `}]}`)
		case r.Method == "GET" && strings.HasPrefix(p, "/workspaces/w/permissions/repositories/r"):
			f.lookups = append(f.lookups, "eff:"+strings.TrimPrefix(q.Get("q"), "user.uuid="))
			if !f.permAdmin {
				w.WriteHeader(403)
				return
			}
			perm := "read"
			if strings.Contains(q.Get("q"), bbDev) {
				perm = "write"
			}
			fmt.Fprintf(w, `{"values":[{"permission":%q}]}`, perm)
		case r.Method == "GET" && strings.HasPrefix(p, repo+"/permissions-config/users/"):
			f.lookups = append(f.lookups, "explicit")
			w.WriteHeader(403)
		case r.Method == "GET" && strings.HasPrefix(p, "/workspaces/w/members/"):
			who := strings.TrimPrefix(p, "/workspaces/w/members/")
			f.lookups = append(f.lookups, "member")
			if who == "%7B00000000-0000-4000-8000-000000000003%7D" {
				write(`{"user":{}}`)
				return
			}
			w.WriteHeader(404)
		case r.Method == "GET" && p == "/user":
			write(bbUserJSON("me", bbMe))
		case r.Method == "POST" && p == repo+"/pullrequests/7/comments":
			in := body()
			if in["inline"] != nil {
				if strings.Contains(raw(in), "unplaceable") {
					w.WriteHeader(400)
					write(`{"type":"error","error":{"message":"Bad request","detail":"line not in diff"}}`)
					return
				}
				f.inline = append(f.inline, in)
				write(`{"id":50}`)
				return
			}
			f.comments = append(f.comments, "pr: "+raw(in))
			write(`{"id":51,"links":{"html":{"href":"https://bitbucket.org/w/r/pull-requests/7/_/diff#comment-51"}}}`)
		default:
			w.WriteHeader(404)
			write(`{"type":"error","error":{"message":"not found"}}`)
		}
	})
}

var bbTestRepo = Repo{Kind: Bitbucket, Host: "bitbucket.org", Owner: "w", Name: "r"}

func bitbucketServer(t *testing.T, token string) (*fakeBitbucket, string) {
	f := &fakeBitbucket{}
	switch {
	case strings.Contains(token, ":"):
		f.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(token))
	default:
		f.auth = "Bearer " + token
	}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	f.url = srv.URL + "/2.0"
	return f, f.url
}

func TestBitbucketIssuesAndPulls(t *testing.T) {
	// An API token goes as Basic with its email.
	f, api := bitbucketServer(t, "me@example.com:ATATT3x")
	c := New(Bitbucket, api, "me@example.com:ATATT3x", nil)
	is, err := c.Issue(bbTestRepo, 4)
	if err != nil || is.Title != "Crash" || is.Body != "Steps" || is.State != "open" || strings.Join(is.Labels, ",") != "rw,bug" ||
		is.Author != "ann "+bbVisitor || is.URL != "https://bitbucket.org/w/r/issues/4" {
		t.Fatalf("%+v %v", is, err)
	}
	if is, err := c.Issue(bbTestRepo, 9); err != nil || is.State != "closed" {
		t.Fatalf("a resolved issue: %+v %v", is, err)
	}
	cs, err := c.Comments(bbTestRepo, 4)
	if err != nil || len(cs) != 4 || cs[0].Author != "bob "+bbVisitor || cs[0].ID != 31 || cs[1].Author != "dev "+bbDev {
		t.Fatalf("empty comments left out: %+v %v", cs, err)
	}
	// The team queue's calls: trust per comment author, and edits.
	if ok, err := c.CommentTrusted(bbTestRepo, cs[0]); err != nil || ok {
		t.Fatalf("a stranger's comment: %v %v", ok, err)
	}
	if ok, err := c.CommentTrusted(bbTestRepo, cs[1]); err != nil || !ok {
		t.Fatalf("a workspace member's comment: %v %v", ok, err)
	}
	if ok, err := c.CommentTrusted(bbTestRepo, cs[2]); err != nil || ok {
		t.Fatalf("an app's comment: %v %v", ok, err)
	}
	// A name cannot make someone the token's owner: the UUID is theirs.
	me, err := c.Viewer()
	if err != nil || me != "me "+bbMe || strings.EqualFold(cs[3].Author, me) {
		t.Fatalf("viewer %q, impostor %q, %v", me, cs[3].Author, err)
	}
	if err := c.EditComment(bbTestRepo, 4, 31, "new text"); err != nil || f.comments[len(f.comments)-1] != "edit 31: new text" {
		t.Fatalf("%v %v", err, f.comments)
	}
	f.comments = nil
	// label r"w is the component r"w; the next page is followed, and
	// the result is oldest first.
	open, err := c.OpenIssues(bbTestRepo, `r"w`, 0)
	if err != nil || len(open) != 3 || open[0].Number != 3 || open[1].Number != 5 || open[2].Number != 9 {
		t.Fatalf("oldest first over two pages: %+v %v", open, err)
	}
	if open, err := c.OpenIssues(bbTestRepo, `r"w`, 2); err != nil || len(open) != 2 || open[0].Number != 3 || open[1].Number != 9 {
		t.Fatalf("at most 2 (from the first page): %+v %v", open, err)
	}
	ps, err := c.OpenPulls(bbTestRepo)
	if err != nil || !ClosedBy(ps)[5] {
		t.Fatalf("Bitbucket's \"fixes issue #5\": %+v %v", ps, err)
	}
	if b, err := c.DefaultBranch(bbTestRepo); err != nil || b != "trunk" {
		t.Fatalf("%q %v", b, err)
	}
	pr, err := c.CreatePull(bbTestRepo, NewPull{Title: "Fix it", Head: "rw/fix", Base: "main", Body: "Closes #4", Draft: true})
	if err != nil || pr.Number != 8 || pr.URL != "https://bitbucket.org/w/r/pull-requests/8" || pr.HeadRepo != "w/r" || pr.State != "open" {
		t.Fatalf("%+v %v", pr, err)
	}
	got, _ := json.Marshal(f.created[0])
	if string(got) != `{"description":"Closes #4","destination":{"branch":{"name":"main"}},"draft":true,"source":{"branch":{"name":"rw/fix"}},"title":"Fix it"}` {
		t.Fatalf("sent %s", got)
	}
	if err := c.CommentIssue(bbTestRepo, 4, "see #8"); err != nil || len(f.comments) != 1 || f.comments[0] != "4: see #8" {
		t.Fatalf("%v %v", err, f.comments)
	}
	err = New(Bitbucket, api, "", nil).CommentPull(bbTestRepo, 4, "x")
	if !IsUnauthorized(err) || !strings.Contains(err.Error(), "BITBUCKET_TOKEN") {
		t.Fatalf("anonymous write: %v", err)
	}
	// A rejected token: Bitbucket's error text comes through.
	err = New(Bitbucket, api, "stale", nil).CommentPull(bbTestRepo, 4, "x")
	if !IsUnauthorized(err) || !strings.Contains(err.Error(), "Access token expired.") {
		t.Fatalf("rejected token: %v", err)
	}
}

// An access token goes as Bearer.
func TestBitbucketAccessToken(t *testing.T) {
	_, api := bitbucketServer(t, "ATCTT3x")
	c := New(Bitbucket, api, "ATCTT3x", nil)
	if err := c.CommentPull(bbTestRepo, 7, "hi"); err != nil || !c.HasToken() {
		t.Fatalf("%v", err)
	}
}

// A repository whose issues live in Jira has no Bitbucket issue tracker:
// rw says so instead of a bare 404.
func TestBitbucketWithoutIssueTracker(t *testing.T) {
	f, api := bitbucketServer(t, "tok")
	f.noIssues = true
	c := New(Bitbucket, api, "tok", nil)
	for name, call := range map[string]func() error{
		"issue":    func() error { _, err := c.Issue(bbTestRepo, 4); return err },
		"issues":   func() error { _, err := c.OpenIssues(bbTestRepo, "rw", 5); return err },
		"comments": func() error { _, err := c.Comments(bbTestRepo, 4); return err },
		"comment":  func() error { return c.CommentIssue(bbTestRepo, 4, "x") },
	} {
		err := call()
		if !IsNotFound(err) || !strings.Contains(err.Error(), "no Bitbucket issue tracker") || !strings.Contains(err.Error(), "Jira") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A next link off the API is never followed: the token would go with it.
func TestBitbucketNextPageStaysOnTheAPI(t *testing.T) {
	var hits []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Header.Get("Authorization"))
		io.WriteString(w, `{"values":[]}`)
	}))
	defer other.Close()
	f, api := bitbucketServer(t, "tok")
	c := New(Bitbucket, api, "tok", nil)
	for _, link := range []string{
		other.URL + "/2.0/repositories/w/r/issues?page=2",
		strings.Replace(api, "http://", "http://user:pw@", 1) + "/repositories/w/r/issues?page=2",
		strings.TrimSuffix(api, "/2.0") + "/other/repositories/w/r/issues?page=2",
		"https" + strings.TrimPrefix(api, "http") + "/repositories/w/r/issues?page=2",
	} {
		f.elsewhere = link
		if _, err := c.OpenIssues(bbTestRepo, `r"w`, 0); err == nil || !strings.Contains(err.Error(), "not on its API") {
			t.Errorf("%s: %v", link, err)
		}
	}
	if len(hits) > 0 {
		t.Fatalf("the other server was called: %q", hits)
	}
}

func TestBitbucketWatchAndReview(t *testing.T) {
	f, api := bitbucketServer(t, "tok")
	c := New(Bitbucket, api, "tok", nil)
	// The head commit's hash comes shortened; rw watch needs all of it.
	p, err := c.Pull(bbTestRepo, 7)
	if err != nil || p.Merged || p.State != "open" || p.HeadRef != "rw/x" || p.HeadSHA != bbHead || p.HeadRepo != "w/r" || p.BaseRef != "main" ||
		p.URL != "https://bitbucket.org/w/r/pull-requests/7" {
		t.Fatalf("%+v %v", p, err)
	}
	if p, err := c.Pull(bbTestRepo, 8); err != nil || !p.Merged || p.State != "closed" || p.URL != "https://bitbucket.org/w/r/pull-requests/8" {
		t.Fatalf("merged: %+v %v", p, err)
	}
	// The diff is a redirect on the same API, which keeps the token.
	if d, err := c.PullDiff(bbTestRepo, 7, 1<<20); err != nil || d != "diff --git a/a.go b/a.go\n" {
		t.Fatalf("%q %v", d, err)
	}
	if _, err := c.PullDiff(bbTestRepo, 7, 5); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too large: %v", err)
	}

	checks := func() string {
		cs, err := c.FailedChecks(bbTestRepo, bbHead, 30)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, x := range cs {
			got = append(got, fmt.Sprintf("%s|%s|%s|%s|%q", x.ID, x.Name, x.Conclusion, x.Output, x.Log))
		}
		return strings.Join(got, "\n")
	}
	// The failed step of the newest run with its log tail, the custom
	// pipeline that failed before any step, and other CI's failed status;
	// not the older run, another commit's run, or the pipeline's own status.
	want := `step:{s2}|pipeline #12: Test|failed|https://bitbucket.org/w/r/pipelines/results/12/steps/%7Bs2%7D|"noise\nnoise\n--- FAIL: TestX\n"
pipeline:{p11}|pipeline #11|error|https://bitbucket.org/w/r/pipelines/results/11|""
status:lint@t2|Lint|failed|2 problems
https://ci.example/9|""`
	if got := checks(); got != want {
		t.Fatalf("checks:\n%s\nwant:\n%s", got, want)
	}
	// Without the pipeline scope the pipeline's own status stands in.
	f.noPipes = true
	if got := checks(); got != "status:PIPE@t1|Pipeline #12 for rw/x|failed|https://bitbucket.org/w/r/pipelines/results/12|\"\"\nstatus:lint@t2|Lint|failed|2 problems\nhttps://ci.example/9|\"\"" {
		t.Fatalf("checks without pipelines:\n%s", got)
	}

	feedback := func() string {
		fb, err := c.Feedback(bbTestRepo, 7)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, x := range fb {
			got = append(got, fmt.Sprintf("%s %s %v %s:%d %s", x.ID, strings.Fields(x.Author)[0], x.Trusted, x.Path, x.Line, x.Body))
		}
		return strings.Join(got, "|")
	}
	// Change requests first, then unresolved inline comments oldest first;
	// resolved threads (with their replies), deleted and draft comments and
	// general comments are left out, and apps are never trusted.
	want = "review:" + bbDev + "@2026-10-01T10:00:00Z dev true :0 |review:" + bbVisitor + "@2026-10-01T11:00:00Z visitor false :0 |" +
		"comment:11 dev true a.go:3 rename|comment:17 dev true b.go:0 old side|comment:100 visitor false a.go:3 drive-by|" +
		"comment:101 {00000000-0000-4000-8000-000000000009} false a.go:3 bot"
	if got := feedback(); got != want {
		t.Fatalf("feedback\n%q\nwant\n%q", got, want)
	}
	// Without workspace permissions: the explicit permission and then
	// workspace membership decide, once per person.
	if strings.Join(f.lookups, ",") != "eff:"+`"`+bbDev+`"`+",explicit,member,eff:"+`"`+bbVisitor+`"`+",explicit,member" {
		t.Fatalf("lookups %v", f.lookups)
	}
	// With them, the effective permission decides.
	f.permAdmin, f.lookups = true, nil
	c = New(Bitbucket, api, "tok", nil)
	if got := feedback(); got != want || strings.Join(f.lookups, ",") != "eff:"+`"`+bbDev+`"`+",eff:"+`"`+bbVisitor+`"` {
		t.Fatalf("feedback %q, lookups %v", got, f.lookups)
	}

	// The review: one comment per inline finding, the one Bitbucket cannot
	// place goes into the text.
	link, err := c.CommentReview(bbTestRepo, 7, bbHead, "body", []InlineComment{{Path: "a.go", Line: 3, OldLine: 2, Body: "x"}, {Path: "b.go", Line: 9, Body: "unplaceable"}})
	if err != nil || link != "https://bitbucket.org/w/r/pull-requests/7/_/diff#comment-51" {
		t.Fatalf("%q %v", link, err)
	}
	in, _ := json.Marshal(f.inline)
	if string(in) != `[{"content":{"raw":"x"},"inline":{"path":"a.go","to":3}}]` {
		t.Fatalf("inline %s", in)
	}
	if len(f.comments) != 1 || f.comments[0] != "pr: body\n\nComments Bitbucket could not place on their line:\n\n- `b.go:9`: unplaceable\n" {
		t.Fatalf("comments %q", f.comments)
	}
	if _, err := c.CommentReview(bbTestRepo, 7, "fff0000", "body", nil); err == nil || !strings.Contains(err.Error(), "new commits") {
		t.Fatalf("a moved head: %v", err)
	}
}

func TestBitbucketTokenAndHosts(t *testing.T) {
	noEnvHosts(t)
	t.Setenv("BITBUCKET_TOKEN", "bb")
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("GITLAB_ACCESS_TOKEN", "")
	t.Setenv("GITEA_TOKEN", "")
	t.Setenv("FORGEJO_TOKEN", "")
	old := GLabToken
	defer func() { GLabToken = old }()
	GLabToken = func(string) string { return "" }
	if tok, src := Token(Bitbucket, "bitbucket.org"); tok != "bb" || src != "BITBUCKET_TOKEN" {
		t.Errorf("bitbucket.org: %q %q", tok, src)
	}
	// Bitbucket Cloud has one host: its token goes nowhere else, also not
	// to a host --api calls Bitbucket, nor to another forge.
	for _, h := range []string{"bitbucket.example.com", "127.0.0.1", "api.bitbucket.org.evil.example"} {
		if tok, _ := Token(Bitbucket, h); tok != "" {
			t.Errorf("%s got the token", h)
		}
	}
	if tok, _ := Token(GitLab, "gitlab.com"); tok != "" {
		t.Errorf("GitLab got %q", tok)
	}
	if tok, _ := Token(Gitea, "codeberg.org"); tok != "" {
		t.Errorf("Gitea got %q", tok)
	}
	for api, want := range map[string]bool{"https://api.bitbucket.org/2.0": true, "https://API.Bitbucket.org/2.0/": true,
		"https://bitbucket.org/2.0": false, "https://api.bitbucket.org.evil.example/2.0": false, "https://evil.example/2.0": false} {
		if got := APIServes(api, "bitbucket.org"); got != want {
			t.Errorf("APIServes(%q, bitbucket.org) = %v", api, got)
		}
	}
	if APIServes("https://api.bitbucket.org/2.0", "github.com") || APIServes("https://api.github.com", "bitbucket.org") {
		t.Error("APIServes across forges")
	}
}

func TestBitbucketRemotesAndRefs(t *testing.T) {
	noEnvHosts(t)
	hosts := EnvHosts()
	want := Repo{Kind: Bitbucket, Host: "bitbucket.org", Owner: "w", Name: "r"}
	for _, in := range []string{"https://bitbucket.org/w/r.git", "git@bitbucket.org:w/r.git", "https://me@bitbucket.org/w/r.git", "ssh://git@bitbucket.org/w/r.git"} {
		if got, err := ParseRemote(in, hosts); err != nil || got != want {
			t.Errorf("%q = %+v, %v", in, got, err)
		}
	}
	if r, err := ParseRemote("https://bitbucket.org/w/r/x.git", hosts); err == nil {
		t.Errorf("three parts: %+v", r)
	}
	if want.APIBase() != "https://api.bitbucket.org/2.0" || want.Ref(5) != "w/r#5" || want.Kind.PullNoun() != "pull request" || want.ForgeName() != "Bitbucket" {
		t.Errorf("%s %s %s", want.APIBase(), want.Ref(5), want.ForgeName())
	}
	if got := want.CompareURL("main", "rw/fix it"); got != "https://bitbucket.org/w/r/pull-requests/new?dest=main&source=rw%2Ffix+it" {
		t.Errorf("compare %s", got)
	}
	if KindOfAPI("https://api.bitbucket.org/2.0") != Bitbucket || KindOfAPI("http://127.0.0.1:9/2.0/") != Bitbucket || ParseKind("Bitbucket") != Bitbucket {
		t.Error("KindOfAPI / ParseKind")
	}
	if r, err := ParsePullRef("https://bitbucket.org/w/r/pull-requests/7/diff", hosts); err != nil || r.Repo != want || r.Number != 7 {
		t.Errorf("pull ref %+v %v", r, err)
	}
	if r, err := ParseIssueRef("https://bitbucket.org/w/r/issues/12/crash-on-start", hosts); err != nil || r.Repo != want || r.Number != 12 {
		t.Errorf("issue ref %+v %v", r, err)
	}
	if _, err := ParsePullRef("https://bitbucket.org/w/r/pull/7", hosts); err == nil || !strings.Contains(err.Error(), "/pull-requests/<n>") {
		t.Errorf("GitHub-style pull URL: %v", err)
	}
	if got := restMessage([]byte(`{"type":"error","error":{"message":"Bad request","detail":"title: required"}}`)); got != "Bad request; title: required" {
		t.Errorf("restMessage %q", got)
	}
}

func TestBitbucketLogin(t *testing.T) {
	for u, want := range map[bbUser]string{
		{Nickname: "ann", UUID: "{0F3C0000-0000-4000-8000-000000000001}"}:          "ann {0f3c0000-0000-4000-8000-000000000001}",
		{DisplayName: "Ann  B\nC", UUID: "{0f3c0000-0000-4000-8000-000000000001}"}: "Ann B C {0f3c0000-0000-4000-8000-000000000001}",
		{Nickname: "ann", UUID: ""}: "",
		{Nickname: "ann {0f3c0000-0000-4000-8000-000000000001}", UUID: "{not a uuid}"}: "",
	} {
		if got := u.login(); got != want {
			t.Errorf("%+v: %q, want %q", u, got, want)
		}
	}
}
