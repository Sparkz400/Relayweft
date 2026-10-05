package gh

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestParseRemote(t *testing.T) {
	ok := map[string]string{
		"https://github.com/o/r.git":            "o/r",
		"https://github.com/o/r":                "o/r",
		"https://github.com/o/r/":               "o/r",
		"https://user:tok@github.com/o/r.git":   "o/r",
		"http://www.github.com/Owner/Repo.git":  "Owner/Repo",
		"git@github.com:o/r.git":                "o/r",
		"git@github.com:o/r":                    "o/r",
		"ssh://git@github.com/o/r.git":          "o/r",
		"ssh://git@github.com:22/o/r":           "o/r",
		"git://github.com/o/my.repo.git":        "o/my.repo",
		"  https://github.com/o/r.git\n":        "o/r",
		"org-123@github.com:o/r-with-dash.git":  "o/r-with-dash",
		"https://GitHub.com/o/r.git":            "o/r",
		"ssh://git@ssh.github.com:443/o/r.git?": "",
	}
	for in, want := range ok {
		r, err := ParseRemote(in, "")
		if want == "" {
			if err == nil {
				t.Errorf("%q: want error, got %+v", in, r)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if r.String() != want || r.Host != "github.com" {
			t.Errorf("%q = %+v, want %s on github.com", in, r, want)
		}
		if r.APIBase() != "https://api.github.com" {
			t.Errorf("%q api = %s", in, r.APIBase())
		}
	}
	for _, bad := range []string{"", "C:\\work\\repo", "/srv/git/repo.git", "https://gitlab.com/o/r.git",
		"https://github.com/o", "https://github.com/o/r/extra", "file:///tmp/r", "git@github.com:"} {
		if r, err := ParseRemote(bad, ""); err == nil {
			t.Errorf("%q: want error, got %+v", bad, r)
		}
	}
}

func TestEnterpriseHostIsOptIn(t *testing.T) {
	const u = "git@ghe.example.com:team/app.git"
	_, err := ParseRemote(u, "")
	if err == nil || !strings.Contains(err.Error(), "GH_HOST=ghe.example.com") {
		t.Fatalf("enterprise remote without GH_HOST: %v", err)
	}
	r, err := ParseRemote(u, "GHE.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if r.Host != "ghe.example.com" || r.APIBase() != "https://ghe.example.com/api/v3" {
		t.Fatalf("%+v %s", r, r.APIBase())
	}
	if got := r.CompareURL("main", "rw/fix-it"); got != "https://ghe.example.com/team/app/compare/main...rw/fix-it?expand=1" {
		t.Fatal(got)
	}
	t.Setenv("GH_HOST", "github.com")
	if EnterpriseHost() != "" {
		t.Error("GH_HOST=github.com is not an enterprise host")
	}
	t.Setenv("GH_HOST", "ghe.example.com")
	if EnterpriseHost() != "ghe.example.com" {
		t.Error("GH_HOST ignored")
	}
}

func TestParseIssueRef(t *testing.T) {
	for in, n := range map[string]int{"12": 12, "#7": 7, " 3 ": 3} {
		r, err := ParseIssueRef(in, "")
		if err != nil || r.Number != n || !r.Repo.IsZero() {
			t.Errorf("%q = %+v %v", in, r, err)
		}
	}
	r, err := ParseIssueRef("https://github.com/o/r/issues/42#issuecomment-1", "")
	if err != nil || r.Number != 42 || r.Repo.String() != "o/r" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{"0", "-1", "abc", "https://github.com/o/r/pull/3", "https://gitlab.com/o/r/issues/1", "https://github.com/o/r/issues/x"} {
		if _, err := ParseIssueRef(bad, ""); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

// fakeGitHub is a tiny GitHub API: one repo o/r with issues and pulls.
type fakeGitHub struct {
	mu       sync.Mutex
	token    string // accepted token ("" = any token is rejected)
	private  bool   // unauthenticated reads get 404
	reqs     []string
	auths    []string
	created  []NewPull
	comments []string
}

func (f *fakeGitHub) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.RequestURI())
		auth := r.Header.Get("Authorization")
		f.auths = append(f.auths, auth)
		if auth != "" && auth != "Bearer "+f.token {
			w.WriteHeader(401)
			io.WriteString(w, `{"message":"Bad credentials"}`)
			return
		}
		if auth == "" && (f.private || r.Method != "GET") {
			if r.Method != "GET" {
				w.WriteHeader(401)
				io.WriteString(w, `{"message":"Requires authentication"}`)
				return
			}
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/12":
			io.WriteString(w, `{"number":12,"title":"Crash on empty input","body":"Steps:\r\n1. run it","state":"open","labels":[{"name":"bug"}],"user":{"login":"alice"}}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/12/comments":
			io.WriteString(w, `[{"body":"also on Windows","user":{"login":"bob"}}]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues":
			q := r.URL.Query()
			if q.Get("labels") != "rw" || q.Get("state") != "open" || q.Get("sort") != "created" || q.Get("direction") != "asc" {
				t.Errorf("list query = %s", r.URL.RawQuery)
			}
			io.WriteString(w, `[{"number":3,"title":"old"},{"number":4,"title":"a PR","pull_request":{}},{"number":5,"title":"taken"},{"number":9,"title":"new"}]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls":
			io.WriteString(w, `[{"number":20,"body":"Some text.\n\nCloses #5"},{"number":21,"body":"refs #3 only"}]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r":
			io.WriteString(w, `{"default_branch":"trunk"}`)
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/pulls":
			var p NewPull
			json.NewDecoder(r.Body).Decode(&p)
			if p.Head == "exists" {
				w.WriteHeader(422)
				io.WriteString(w, `{"message":"Validation Failed","errors":[{"message":"A pull request already exists for o:exists."}]}`)
				return
			}
			f.created = append(f.created, p)
			w.WriteHeader(201)
			io.WriteString(w, `{"number":77,"html_url":"https://github.com/o/r/pull/77"}`)
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/12/comments":
			var c struct{ Body string }
			json.NewDecoder(r.Body).Decode(&c)
			f.comments = append(f.comments, c.Body)
			w.WriteHeader(201)
			io.WriteString(w, `{}`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"Not Found"}`)
		}
	})
}

var repo = Repo{Host: "github.com", Owner: "o", Name: "r"}

func TestIssueReadWithoutTokenAndStaleToken(t *testing.T) {
	f := &fakeGitHub{token: "good"}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	is, err := c.Issue(repo, 12)
	if err != nil || is.Title != "Crash on empty input" || strings.Join(is.LabelNames(), ",") != "bug" {
		t.Fatalf("%+v %v", is, err)
	}
	cs, err := c.Comments(repo, 12)
	if err != nil || len(cs) != 1 || cs[0].User.Login != "bob" {
		t.Fatalf("%+v %v", cs, err)
	}

	// A stale token: reads retry without it and say so once.
	var notes strings.Builder
	c = NewClient(srv.URL, "stale")
	c.Notes = &notes
	if _, err := c.Issue(repo, 12); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(notes.String(), "401") || c.HasToken() {
		t.Fatalf("notes %q, has token %v", notes.String(), c.HasToken())
	}
	// Writes then fail clearly instead of silently going anonymous.
	_, err = c.CreatePull(repo, NewPull{Title: "x", Head: "b", Base: "main"})
	if !IsUnauthorized(err) || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("create with rejected token: %v", err)
	}
}

func TestNotFoundHints(t *testing.T) {
	f := &fakeGitHub{token: "good", private: true}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	_, err := NewClient(srv.URL, "").Issue(repo, 12)
	if !IsNotFound(err) || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("anonymous 404: %v", err)
	}
	_, err = NewClient(srv.URL, "good").Issue(repo, 999)
	if !IsNotFound(err) || !strings.Contains(err.Error(), "lack access") {
		t.Fatalf("authenticated 404: %v", err)
	}
	if _, err := NewClient(srv.URL, "good").Issue(repo, 12); err != nil {
		t.Fatalf("private repo with token: %v", err)
	}
}

func TestListIssuesAndPulls(t *testing.T) {
	f := &fakeGitHub{token: "good"}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	c := NewClient(srv.URL, "")
	is, err := c.OpenIssues(repo, "rw", 0)
	if err != nil {
		t.Fatal(err)
	}
	var nums []int
	for _, i := range is {
		nums = append(nums, i.Number)
	}
	if len(nums) != 3 || nums[0] != 3 || nums[1] != 5 || nums[2] != 9 {
		t.Fatalf("issues = %v (pull requests must be skipped, order kept)", nums)
	}
	if is, _ := c.OpenIssues(repo, "rw", 2); len(is) != 2 {
		t.Fatalf("max not applied: %d", len(is))
	}
	if ps, err := c.OpenPulls(repo); err != nil || len(ps) == 0 {
		t.Fatalf("open pulls: %v %v", ps, err)
	}
	if b, err := c.DefaultBranch(repo); err != nil || b != "trunk" {
		t.Fatalf("default branch %q %v", b, err)
	}
}

func TestCreatePullAndComment(t *testing.T) {
	f := &fakeGitHub{token: "good"}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	c := NewClient(srv.URL+"/", "good")
	p, err := c.CreatePull(repo, NewPull{Title: "Fix it", Head: "rw/fix", Base: "main", Body: "Closes #12", Draft: true})
	if err != nil || p.Number != 77 || p.HTMLURL != "https://github.com/o/r/pull/77" {
		t.Fatalf("%+v %v", p, err)
	}
	if len(f.created) != 1 || !f.created[0].Draft || f.created[0].Head != "rw/fix" || f.created[0].Body != "Closes #12" {
		t.Fatalf("sent %+v", f.created)
	}
	if err := c.AddComment(repo, 12, "see #77"); err != nil || len(f.comments) != 1 {
		t.Fatalf("%v %v", err, f.comments)
	}
	_, err = c.CreatePull(repo, NewPull{Title: "x", Head: "exists", Base: "main"})
	if err == nil || !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), "422") {
		t.Fatalf("422: %v", err)
	}
	// Without a token a write is a clear 401.
	_, err = NewClient(srv.URL, "").CreatePull(repo, NewPull{Title: "x", Head: "b", Base: "main"})
	if !IsUnauthorized(err) || !strings.Contains(err.Error(), "needs a token") {
		t.Fatalf("anonymous create: %v", err)
	}
}

func TestTokenSources(t *testing.T) {
	old := GHCLIToken
	defer func() { GHCLIToken = old }()
	var asked string
	GHCLIToken = func(host string) string { asked = host; return "from-gh" }
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GH_ENTERPRISE_TOKEN", "")
	t.Setenv("GITHUB_ENTERPRISE_TOKEN", "")
	if tok, src := Token("github.com"); tok != "from-gh" || src != "gh auth token" || asked != "github.com" {
		t.Fatalf("%q %q %q", tok, src, asked)
	}
	t.Setenv("GH_TOKEN", "b")
	if tok, src := Token("github.com"); tok != "b" || src != "GH_TOKEN" {
		t.Fatalf("%q %q", tok, src)
	}
	t.Setenv("GITHUB_TOKEN", "a")
	if tok, _ := Token("github.com"); tok != "a" {
		t.Fatal(tok)
	}
	t.Setenv("GH_ENTERPRISE_TOKEN", "e")
	if tok, _ := Token("ghe.example.com"); tok != "e" {
		t.Fatal(tok)
	}
	if tok, _ := Token("github.com"); tok != "a" {
		t.Fatal("enterprise token used for github.com")
	}
}

// A github.com token never goes to an Enterprise host.
func TestEnterpriseHostNeverGetsDotComToken(t *testing.T) {
	old := GHCLIToken
	defer func() { GHCLIToken = old }()
	var asked string
	GHCLIToken = func(host string) string { asked = host; return "" }
	t.Setenv("GITHUB_TOKEN", "dotcom")
	t.Setenv("GH_TOKEN", "dotcom2")
	t.Setenv("GH_ENTERPRISE_TOKEN", "")
	t.Setenv("GITHUB_ENTERPRISE_TOKEN", "")
	if tok, src := Token("ghe.example.com"); tok != "" || asked != "ghe.example.com" {
		t.Fatalf("enterprise host got %q from %s (gh asked for %q)", tok, src, asked)
	}
	GHCLIToken = func(host string) string { return "gh-" + host }
	if tok, src := Token("ghe.example.com"); tok != "gh-ghe.example.com" || src != "gh auth token" {
		t.Fatalf("%q %q", tok, src)
	}
	t.Setenv("GITHUB_ENTERPRISE_TOKEN", "ent")
	if tok, src := Token("ghe.example.com"); tok != "ent" || src != "GITHUB_ENTERPRISE_TOKEN" {
		t.Fatalf("%q %q", tok, src)
	}
}
