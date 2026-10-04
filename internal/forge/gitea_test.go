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

// fakeGitea serves repository o/r and pull request #7 on /api/v1.
type fakeGitea struct {
	mu        sync.Mutex
	token     string
	permAdmin bool // whether the token may read other users' permissions
	created   []map[string]string
	comments  []string
	reviews   []map[string]any
	lookups   []string
}

func (f *fakeGitea) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		auth := r.Header.Get("Authorization")
		if (auth != "" && auth != "token "+f.token) || (auth == "" && r.Method != "GET") {
			w.WriteHeader(401)
			io.WriteString(w, `{"message":"token is required"}`)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/api/v1")
		q := r.URL.Query()
		write := func(s string) { io.WriteString(w, s) }
		switch {
		case r.Method == "GET" && p == "/repos/o/r":
			write(`{"default_branch":"trunk"}`)
		case r.Method == "GET" && p == "/repos/o/r/issues/4":
			write(`{"number":4,"title":"Crash","body":"Steps","state":"open","labels":[{"name":"bug"}],"user":{"login":"ann"}}`)
		case r.Method == "GET" && p == "/repos/o/r/issues/7":
			write(`{"number":7,"title":"PR","state":"open","pull_request":{"merged":false}}`)
		case r.Method == "GET" && p == "/repos/o/r/issues/4/comments":
			write(`[{"body":"me too","user":{"login":"bob"}}]`)
		case r.Method == "GET" && p == "/repos/o/r/issues":
			if q.Get("type") != "issues" || q.Get("labels") != "sy" || q.Get("limit") != "50" {
				t.Errorf("issue query %s", r.URL.RawQuery)
			}
			// Newest first, as Gitea lists them.
			write(`[{"number":9,"created_at":"2026-03-01T00:00:00Z"},{"number":5,"created_at":"2026-02-01T00:00:00Z"},{"number":3,"created_at":"2026-01-01T00:00:00Z"}]`)
		case r.Method == "GET" && p == "/repos/o/r/pulls" && q.Get("state") == "open":
			write(`[{"number":7,"body":"Fixes #5"}]`)
		case r.Method == "POST" && p == "/repos/o/r/pulls":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			f.created = append(f.created, in)
			write(`{"number":8,"html_url":"https://codeberg.org/o/r/pulls/8","head":{"ref":"sy/fix","repo":{"full_name":"o/r"}}}`)
		case r.Method == "POST" && strings.HasPrefix(p, "/repos/o/r/issues/") && strings.HasSuffix(p, "/comments"):
			var in struct{ Body string }
			json.NewDecoder(r.Body).Decode(&in)
			f.comments = append(f.comments, strings.TrimSuffix(strings.TrimPrefix(p, "/repos/o/r/issues/"), "/comments")+": "+in.Body)
			w.WriteHeader(201)
			write(`{}`)
		case r.Method == "GET" && p == "/repos/o/r/pulls/7":
			write(`{"number":7,"title":"Fix","state":"closed","merged":true,"head":{"ref":"sy/x","sha":"abc","repo":{"full_name":"o/r"}},"base":{"ref":"main"}}`)
		case r.Method == "GET" && p == "/repos/o/r/pulls/7.diff":
			write("diff --git a/a.go b/a.go\n")
		case r.Method == "GET" && p == "/repos/o/r/commits/abc/status":
			write(`{"total_count":3,"statuses":[{"id":3,"status":"success","context":"lint"},{"id":2,"status":"failure","context":"ci / test","description":"Failing after 1m","target_url":"https://codeberg.org/o/r/actions/runs/1/jobs/0"},{"id":4,"status":"pending","context":"slow"}]}`)
		case r.Method == "GET" && p == "/repos/o/r/pulls/7/reviews":
			write(`[{"id":1,"state":"REQUEST_CHANGES","body":"no","user":{"id":3,"login":"dev"},"comments_count":1},
				{"id":2,"state":"REQUEST_CHANGES","body":"old","dismissed":true,"user":{"id":3,"login":"dev"}},
				{"id":3,"state":"COMMENT","body":"","user":{"id":5,"login":"visitor"},"comments_count":1},
				{"id":4,"state":"PENDING","user":{"id":3,"login":"dev"},"comments_count":1},
				{"id":5,"state":"REQUEST_CHANGES","body":"bot","user":{"id":-2,"login":"gitea-actions"}}]`)
		case r.Method == "GET" && p == "/repos/o/r/pulls/7/reviews/1/comments":
			write(`[{"id":11,"body":"rename","path":"a.go","position":3,"diff_hunk":"@@ -1 +1 @@","user":{"id":3,"login":"dev"}},
				{"id":12,"body":"fixed","path":"a.go","position":4,"user":{"id":3,"login":"dev"},"resolver":{"id":3,"login":"dev"}}]`)
		case r.Method == "GET" && p == "/repos/o/r/pulls/7/reviews/3/comments":
			write(`[{"id":100,"body":"drive-by","path":"a.go","position":1,"user":{"id":5,"login":"visitor"}}]`)
		case r.Method == "GET" && p == "/repos/o/r/pulls/7/reviews/4/comments":
			t.Error("read the comments of a pending review")
			write(`[]`)
		case r.Method == "GET" && strings.HasPrefix(p, "/repos/o/r/collaborators/"):
			who := strings.TrimPrefix(p, "/repos/o/r/collaborators/")
			f.lookups = append(f.lookups, who)
			switch {
			case strings.HasSuffix(who, "/permission") && !f.permAdmin:
				w.WriteHeader(403)
			case who == "dev/permission":
				write(`{"permission":"write"}`)
			case who == "visitor/permission":
				write(`{"permission":"read"}`)
			case who == "dev":
				w.WriteHeader(204)
			default:
				w.WriteHeader(404)
			}
		case r.Method == "GET" && strings.HasPrefix(p, "/orgs/o/members/"):
			f.lookups = append(f.lookups, "org:"+strings.TrimPrefix(p, "/orgs/o/members/"))
			w.WriteHeader(404)
		case r.Method == "GET" && p == "/user":
			write(`{"id":1,"login":"me"}`)
		case r.Method == "POST" && p == "/repos/o/r/pulls/7/reviews":
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			f.reviews = append(f.reviews, in)
			write(`{"id":9,"html_url":"https://codeberg.org/o/r/pulls/7#issuecomment-9"}`)
		default:
			w.WriteHeader(404)
			write(`{"message":"not found"}`)
		}
	})
}

var gtTestRepo = Repo{Kind: Gitea, Host: "codeberg.org", Owner: "o", Name: "r"}

func giteaServer(t *testing.T) (*fakeGitea, string) {
	f := &fakeGitea{token: "good"}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return f, srv.URL + "/api/v1"
}

func TestGiteaIssuesAndPulls(t *testing.T) {
	f, api := giteaServer(t)
	c := New(Gitea, api, "good", nil)
	is, err := c.Issue(gtTestRepo, 4)
	if err != nil || is.Title != "Crash" || is.State != "open" || strings.Join(is.Labels, ",") != "bug" || is.IsPull {
		t.Fatalf("%+v %v", is, err)
	}
	if is, err := c.Issue(gtTestRepo, 7); err != nil || !is.IsPull {
		t.Fatalf("a pull request read as an issue: %+v %v", is, err)
	}
	if cs, err := c.Comments(gtTestRepo, 4); err != nil || len(cs) != 1 || cs[0].Author != "bob" {
		t.Fatalf("%+v %v", cs, err)
	}
	open, err := c.OpenIssues(gtTestRepo, "sy", 2)
	if err != nil || len(open) != 2 || open[0].Number != 3 || open[1].Number != 5 {
		t.Fatalf("oldest first, at most 2: %+v %v", open, err)
	}
	ps, err := c.OpenPulls(gtTestRepo)
	if err != nil || !ClosedBy(ps)[5] {
		t.Fatalf("%+v %v", ps, err)
	}
	if b, err := c.DefaultBranch(gtTestRepo); err != nil || b != "trunk" {
		t.Fatalf("%q %v", b, err)
	}
	pr, err := c.CreatePull(gtTestRepo, NewPull{Title: "Fix it", Head: "sy/fix", Base: "main", Body: "Closes #4", Draft: true})
	if err != nil || pr.Number != 8 || pr.URL != "https://codeberg.org/o/r/pulls/8" {
		t.Fatalf("%+v %v", pr, err)
	}
	if got := f.created[0]; got["title"] != "WIP: Fix it" || got["head"] != "sy/fix" || got["base"] != "main" || got["body"] != "Closes #4" {
		t.Fatalf("sent %v", got)
	}
	if err := c.CommentIssue(gtTestRepo, 4, "see #8"); err != nil || len(f.comments) != 1 || f.comments[0] != "4: see #8" {
		t.Fatalf("%v %v", err, f.comments)
	}
	err = New(Gitea, api, "", nil).CommentPull(gtTestRepo, 4, "x")
	if !IsUnauthorized(err) || !strings.Contains(err.Error(), "GITEA_TOKEN") {
		t.Fatalf("anonymous write: %v", err)
	}
}

func TestGiteaWatchAndReview(t *testing.T) {
	f, api := giteaServer(t)
	c := New(Gitea, api, "good", nil)
	p, err := c.Pull(gtTestRepo, 7)
	if err != nil || !p.Merged || p.HeadRef != "sy/x" || p.HeadSHA != "abc" || p.HeadRepo != "o/r" || p.BaseRef != "main" {
		t.Fatalf("%+v %v", p, err)
	}
	if d, err := c.PullDiff(gtTestRepo, 7, 1<<20); err != nil || d != "diff --git a/a.go b/a.go\n" {
		t.Fatalf("%q %v", d, err)
	}
	if _, err := c.PullDiff(gtTestRepo, 7, 5); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	checks, err := c.FailedChecks(gtTestRepo, "abc", 100)
	if err != nil || len(checks) != 1 || checks[0].ID != "2" || checks[0].Name != "ci / test" || !strings.Contains(checks[0].Output, "actions/runs/1") {
		t.Fatalf("%+v %v", checks, err)
	}

	feedback := func() string {
		fb, err := c.Feedback(gtTestRepo, 7)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, x := range fb {
			got = append(got, fmt.Sprintf("%s %s %v %s:%d", x.ID, x.Author, x.Trusted, x.Path, x.Line))
		}
		return strings.Join(got, "|")
	}
	// Reviews requesting changes first, then unresolved comments oldest
	// first (by number: 11 before 100); dismissed and pending reviews and
	// resolved comments are left out, and the Actions bot is never trusted.
	const want = "review:1 dev true :0|review:5 gitea-actions false :0|comment:11 dev true a.go:3|comment:100 visitor false a.go:1"
	// Without permission to read permissions: the collaborator and org
	// member checks decide.
	if got := feedback(); got != want {
		t.Fatalf("feedback %q", got)
	}
	if strings.Join(f.lookups, ",") != "dev/permission,dev,visitor/permission,visitor,org:visitor" {
		t.Fatalf("lookups %v (cached per user)", f.lookups)
	}
	// With it, the permission decides.
	f.permAdmin, f.lookups = true, nil
	c = New(Gitea, api, "good", nil)
	if got := feedback(); got != want || strings.Join(f.lookups, ",") != "dev/permission,visitor/permission" {
		t.Fatalf("feedback %q, lookups %v", got, f.lookups)
	}
	if v, err := c.Viewer(); err != nil || v != "me" {
		t.Fatalf("%q %v", v, err)
	}
	url, err := c.CommentReview(gtTestRepo, 7, "abc", "body", []InlineComment{{Path: "a.go", Line: 3, OldLine: 2, Body: "x"}})
	if err != nil || url != "https://codeberg.org/o/r/pulls/7#issuecomment-9" {
		t.Fatalf("%q %v", url, err)
	}
	rv := f.reviews[0]
	cs := rv["comments"].([]any)[0].(map[string]any)
	if rv["event"] != "COMMENT" || rv["commit_id"] != "abc" || rv["body"] != "body" || cs["path"] != "a.go" || cs["new_position"] != float64(3) {
		t.Fatalf("posted %v", rv)
	}
}
