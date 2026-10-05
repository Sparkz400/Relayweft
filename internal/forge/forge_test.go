package forge

import (
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/gh"
)

func noEnvHosts(t *testing.T) {
	for _, v := range []string{"GH_HOST", "GITLAB_HOST", "GITEA_HOST", "FORGEJO_HOST",
		"FORGEJO_ACTIONS", "GITEA_ACTIONS", "FORGEJO_SERVER_URL", "GITEA_SERVER_URL", "GITHUB_SERVER_URL"} {
		t.Setenv(v, "")
	}
}

func TestParseRemoteKinds(t *testing.T) {
	noEnvHosts(t)
	t.Setenv("GITLAB_HOST", "https://GitLab.Example.com:8443/, gl2.example.com")
	t.Setenv("GITEA_HOST", "git.example.org")
	hosts := EnvHosts()
	ok := map[string]Repo{
		"https://github.com/o/r.git":                      {Kind: GitHub, Host: "github.com", Owner: "o", Name: "r"},
		"git@gitlab.com:group/sub/proj.git":               {Kind: GitLab, Host: "gitlab.com", Owner: "group/sub", Name: "proj"},
		"https://gitlab.com/g/p":                          {Kind: GitLab, Host: "gitlab.com", Owner: "g", Name: "p"},
		"ssh://git@gitlab.example.com:2222/team/app.git":  {Kind: GitLab, Host: "gitlab.example.com", Owner: "team", Name: "app", Web: "https://gitlab.example.com:8443"},
		"https://gl2.example.com/a/b/c/d.git":             {Kind: GitLab, Host: "gl2.example.com", Owner: "a/b/c", Name: "d"},
		"https://codeberg.org/forgejo/forgejo.git":        {Kind: Gitea, Host: "codeberg.org", Owner: "forgejo", Name: "forgejo"},
		"git@git.example.org:me/tool.git":                 {Kind: Gitea, Host: "git.example.org", Owner: "me", Name: "tool"},
		"https://user:secret@git.example.org/me/tool.git": {Kind: Gitea, Host: "git.example.org", Owner: "me", Name: "tool"},
		"  https://Codeberg.org/O/R.git\n":                {Kind: Gitea, Host: "codeberg.org", Owner: "O", Name: "R"},
	}
	for in, want := range ok {
		got, err := ParseRemote(in, hosts)
		if err != nil || got != want {
			t.Errorf("%q = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"https://unknown.example/o/r.git", "https://codeberg.org/o/r/x.git", "https://gitlab.com/onlyone",
		"https://gitlab.com/g/-/p.git", "https://gitlab.com/g/../p.git", "C:\\work\\repo", "file:///srv/r.git"} {
		if r, err := ParseRemote(bad, hosts); err == nil {
			t.Errorf("%q: want error, got %+v", bad, r)
		}
	}
	_, err := ParseRemote("https://unknown.example/o/r.git", hosts)
	if err == nil || !strings.Contains(err.Error(), "GITLAB_HOST or GITEA_HOST=unknown.example") {
		t.Errorf("unknown host error lacks a hint: %v", err)
	}
	// github.com stays GitHub whatever the variables say.
	t.Setenv("GITLAB_HOST", "github.com")
	if r, err := ParseRemote("https://github.com/o/r", EnvHosts()); err != nil || r.Kind != GitHub {
		t.Errorf("github.com: %+v %v", r, err)
	}
}

func TestRepoURLs(t *testing.T) {
	gl := Repo{Kind: GitLab, Host: "gitlab.example.com", Owner: "g/sub", Name: "p"}
	if gl.APIBase() != "https://gitlab.example.com/api/v4" || gl.Ref(5) != "g/sub/p!5" {
		t.Errorf("%s %s", gl.APIBase(), gl.Ref(5))
	}
	if got := gl.CompareURL("main", "rw/fix it"); got != "https://gitlab.example.com/g/sub/p/-/merge_requests/new?merge_request%5Bsource_branch%5D=rw%2Ffix+it&merge_request%5Btarget_branch%5D=main" {
		t.Errorf("compare %s", got)
	}
	gt := Repo{Kind: Gitea, Host: "codeberg.org", Owner: "o", Name: "r"}
	if gt.APIBase() != "https://codeberg.org/api/v1" || gt.Ref(5) != "o/r#5" || gt.CompareURL("main", "rw/x") != "https://codeberg.org/o/r/compare/main...rw/x" {
		t.Errorf("%s %s %s", gt.APIBase(), gt.Ref(5), gt.CompareURL("main", "rw/x"))
	}
	hub := Repo{Kind: GitHub, Host: "github.com", Owner: "o", Name: "r"}
	if hub.APIBase() != "https://api.github.com" || hub.CompareURL("main", "b") != "https://github.com/o/r/compare/main...b?expand=1" {
		t.Errorf("%s %s", hub.APIBase(), hub.CompareURL("main", "b"))
	}
	for api, want := range map[string]Kind{"https://h/api/v4": GitLab, "https://h/gitlab/api/v4/": GitLab, "http://127.0.0.1:9/api/v1": Gitea,
		"https://h/api/v3": GitHub, "http://127.0.0.1:9": GitHub, "::": GitHub} {
		if got := KindOfAPI(api); got != want {
			t.Errorf("KindOfAPI(%q) = %s, want %s", api, got, want)
		}
	}
	if ParseKind("") != GitHub || ParseKind("GitLab") != GitLab || ParseKind("forgejo") != Gitea {
		t.Error("ParseKind")
	}
}

func TestParseRefs(t *testing.T) {
	noEnvHosts(t)
	hosts := Hosts{}.With("gitea.local", Gitea)
	pulls := map[string]Ref{
		"12":                                  {Number: 12},
		"#12":                                 {Number: 12},
		"!12":                                 {Number: 12},
		"https://github.com/o/r/pull/7/files": {Repo{Kind: GitHub, Host: "github.com", Owner: "o", Name: "r"}, 7},
		"https://gitlab.com/g/sub/p/-/merge_requests/9":     {Repo{Kind: GitLab, Host: "gitlab.com", Owner: "g/sub", Name: "p"}, 9},
		"https://gitlab.com/g/p/-/merge_requests/9/diffs#x": {Repo{Kind: GitLab, Host: "gitlab.com", Owner: "g", Name: "p"}, 9},
		"https://gitea.local/o/r/pulls/3":                   {Repo{Kind: Gitea, Host: "gitea.local", Owner: "o", Name: "r"}, 3},
	}
	for in, want := range pulls {
		got, err := ParsePullRef(in, hosts)
		if err != nil || got != want {
			t.Errorf("pull %q = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"0", "#-1", "##3", "https://github.com/o/r/pulls/3", "https://gitlab.com/g/p/merge_requests/9",
		"https://gitlab.com/g/p/-/issues/9", "https://gitea.local/o/r/pull/3", "https://other.example/o/r/pull/1", "https://gitlab.com/-/merge_requests/1"} {
		if r, err := ParsePullRef(bad, hosts); err == nil {
			t.Errorf("pull %q: want error, got %+v", bad, r)
		}
	}
	issues := map[string]Ref{
		"https://github.com/o/r/issues/4":     {Repo{Kind: GitHub, Host: "github.com", Owner: "o", Name: "r"}, 4},
		"https://gitlab.com/a/b/c/-/issues/4": {Repo{Kind: GitLab, Host: "gitlab.com", Owner: "a/b", Name: "c"}, 4},
		"https://codeberg.org/o/r/issues/4":   {Repo{Kind: Gitea, Host: "codeberg.org", Owner: "o", Name: "r"}, 4},
	}
	for in, want := range issues {
		got, err := ParseIssueRef(in, hosts)
		if err != nil || got != want {
			t.Errorf("issue %q = %+v, %v; want %+v", in, got, err, want)
		}
	}
	if _, err := ParseIssueRef("https://gitlab.com/g/p/-/merge_requests/1", hosts); err == nil || !strings.Contains(err.Error(), "/-/issues/<n>") {
		t.Errorf("issue from a merge request URL: %v", err)
	}
}

func TestTokenStaysWithItsHosts(t *testing.T) {
	noEnvHosts(t)
	old, oldGH := GLabToken, gh.GHCLIToken
	defer func() { GLabToken, gh.GHCLIToken = old, oldGH }()
	gh.GHCLIToken = func(string) string { return "" }
	var asked []string
	GLabToken = func(host string) string { asked = append(asked, host); return "" }
	t.Setenv("GITLAB_TOKEN", "gl")
	t.Setenv("GITEA_TOKEN", "gt")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	if tok, src := Token(GitLab, "gitlab.com"); tok != "gl" || src != "GITLAB_TOKEN" {
		t.Errorf("gitlab.com without GITLAB_HOST: %q %q", tok, src)
	}
	if tok, _ := Token(Gitea, "codeberg.org"); tok != "gt" {
		t.Errorf("codeberg without GITEA_HOST: %q", tok)
	}
	// With GITLAB_HOST set, GITLAB_TOKEN is that server's only.
	t.Setenv("GITLAB_HOST", "gitlab.corp")
	if tok, _ := Token(GitLab, "gitlab.corp"); tok != "gl" {
		t.Errorf("GITLAB_HOST's own host: %q", tok)
	}
	if tok, _ := Token(GitLab, "gitlab.com"); tok != "" {
		t.Errorf("a gitlab.corp token was sent to gitlab.com: %q", tok)
	}
	if len(asked) != 1 || asked[0] != "gitlab.com" {
		t.Errorf("glab asked for %v", asked)
	}
	t.Setenv("FORGEJO_HOST", "git.corp")
	if tok, _ := Token(Gitea, "codeberg.org"); tok != "" {
		t.Errorf("a git.corp token was sent to codeberg.org: %q", tok)
	}
	if tok, _ := Token(Gitea, "GIT.corp"); tok != "gt" {
		t.Errorf("FORGEJO_HOST's own host: %q", tok)
	}
	// GitLab and Gitea variables never reach GitHub.
	if tok, src := Token(GitHub, "github.com"); tok != "" {
		t.Errorf("GitHub got %q from %s", tok, src)
	}
}

func TestClosedByKeywords(t *testing.T) {
	got := ClosedBy([]Pull{{Body: "fixes #1, Resolves #2\nclosed #3 and Closes: #4"}, {Body: "Fixing #8; implements #9\nResolving #10, closing #11"},
		{Body: "see #5; prefix#6; closes #7x"}})
	for _, n := range []int{1, 2, 3, 4, 8, 9, 10, 11} {
		if !got[n] {
			t.Errorf("#%d not found in %v", n, got)
		}
	}
	for _, n := range []int{5, 6, 7} {
		if got[n] {
			t.Errorf("#%d wrongly matched", n)
		}
	}
}

// A log tail drops a partial first line only when the log was cut.
func TestLineTail(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"first\nsecond\n", 100, "first\nsecond\n"},       // fits: all of it
		{"first\nsecond\n", 13, "first\nsecond\n"},        // exactly fits
		{"first\nsecond\nthird\n", 13, "second\nthird\n"}, // cut at a line start
		{"first\nsecond\nthird\n", 12, "third\n"},         // cut mid-line
		{"0123456789", 4, "6789"},                         // one long line
	} {
		if got := lineTail(c.in, c.n); got != c.want {
			t.Errorf("lineTail(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestRestMessage(t *testing.T) {
	for in, want := range map[string]string{
		`{"message":"404 Project Not Found"}`:                         "404 Project Not Found",
		`{"message":{"title":["is too long"],"base":["is missing"]}}`: "base is missing; title is too long",
		`{"message":["Another open merge request already exists"]}`:   "Another open merge request already exists",
		`{"error":"insufficient_scope"}`:                              "insufficient_scope",
		`{"message":"Validation Failed","errors":["head: missing"]}`:  "Validation Failed; head: missing",
		`plain text`: "plain text",
	} {
		if got := restMessage([]byte(in)); got != want {
			t.Errorf("restMessage(%s) = %q, want %q", in, got, want)
		}
	}
}

// A self-hosted forge is reached where it lives: the scheme and port of a
// URL in GITEA_HOST / GITLAB_HOST, the port of an https remote, or a
// plain-http remote on this machine. Real Gitea on http://localhost:3300
// used to be called at https://localhost/api/v1.
func TestSelfHostedRootURLs(t *testing.T) {
	noEnvHosts(t)
	t.Setenv("GITEA_HOST", "localhost, git.lan, https://git.example.org:8443/, http://plain.lan:3000")
	t.Setenv("GITLAB_HOST", "https://example.com/gitlab/")
	hosts := EnvHosts()
	cases := []struct{ remote, web, api, page string }{
		{"http://localhost:3300/syadmin/demo.git", "http://localhost:3300", "http://localhost:3300/api/v1", "http://localhost:3300/syadmin/demo"},
		{"https://git.example.org:8443/o/r.git", "https://git.example.org:8443", "https://git.example.org:8443/api/v1", ""},
		{"git@git.example.org:o/r.git", "https://git.example.org:8443", "https://git.example.org:8443/api/v1", ""},
		{"git@plain.lan:o/r.git", "http://plain.lan:3000", "http://plain.lan:3000/api/v1", ""},
		{"https://git.lan:3001/o/r", "https://git.lan:3001", "https://git.lan:3001/api/v1", ""},
		// Plain http to another machine only where a variable says so.
		{"http://git.lan:3000/o/r.git", "", "https://git.lan/api/v1", "https://git.lan/o/r"},
		{"https://git.lan:443/o/r.git", "", "https://git.lan/api/v1", ""},
		{"https://example.com/gitlab/g/sub/p.git", "https://example.com/gitlab", "https://example.com/gitlab/api/v4", "https://example.com/gitlab/g/sub/p"},
	}
	for _, c := range cases {
		r, err := ParseRemote(c.remote, hosts)
		if err != nil || r.Web != c.web || r.APIBase() != c.api || (c.page != "" && r.WebURL() != c.page) {
			t.Errorf("%s: %+v %v: web %q api %q page %q", c.remote, r, err, r.Web, r.APIBase(), r.WebURL())
		}
	}
	r, _ := ParseRemote("http://localhost:3300/syadmin/demo.git", hosts)
	if got := r.CompareURL("main", "rw/x"); got != "http://localhost:3300/syadmin/demo/compare/main...rw/x" {
		t.Errorf("compare %s", got)
	}
	if gl, _ := ParseRemote("https://example.com/gitlab/g/sub/p.git", hosts); gl.Owner != "g/sub" || gl.Name != "p" {
		t.Errorf("path prefix: %+v", gl)
	}
	// Pull request and issue URLs too.
	pr, err := ParsePullRef("http://localhost:3300/syadmin/demo/pulls/4", hosts)
	if err != nil || pr.Number != 4 || pr.Repo.APIBase() != "http://localhost:3300/api/v1" || pr.Repo.Owner != "syadmin" {
		t.Errorf("pull URL: %+v %v", pr, err)
	}
	mr, err := ParsePullRef("https://example.com/gitlab/g/p/-/merge_requests/2", hosts)
	if err != nil || mr.Repo.Owner != "g" || mr.Repo.Name != "p" || mr.Repo.APIBase() != "https://example.com/gitlab/api/v4" {
		t.Errorf("merge request URL under a path: %+v %v", mr, err)
	}
	// github.com is never redirected.
	if hub, _ := ParseRemote("http://github.com/o/r", hosts.With("http://github.com:8080", GitHub)); hub.Web != "" || hub.APIBase() != "https://api.github.com" {
		t.Errorf("github.com: %+v", hub)
	}
	// A GitHub Enterprise URL with a port.
	t.Setenv("GH_HOST", "https://ghe.corp:8443")
	if ghe, err := ParseRemote("git@ghe.corp:o/r.git", EnvHosts()); err != nil || ghe.APIBase() != "https://ghe.corp:8443/api/v3" || ghe.CompareURL("main", "b") != "https://ghe.corp:8443/o/r/compare/main...b?expand=1" {
		t.Errorf("enterprise: %+v %v %s", ghe, err, ghe.APIBase())
	}
}

// In a Forgejo Actions job (what forgejo-runner v12 sets, seen in a real
// job), the job's server is a Forgejo host without GITEA_HOST, its token
// stays there, and GITHUB_TOKEN, which the runner sets to that same job
// token, never goes to GitHub.
func TestForgejoActionsJob(t *testing.T) {
	noEnvHosts(t)
	old := gh.GHCLIToken
	defer func() { gh.GHCLIToken = old }()
	gh.GHCLIToken = func(string) string { return "" }
	t.Setenv("FORGEJO_ACTIONS", "true")
	t.Setenv("GITEA_ACTIONS", "true")
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("FORGEJO_SERVER_URL", "http://forgejo:3000")
	t.Setenv("GITHUB_SERVER_URL", "http://forgejo:3000")
	t.Setenv("FORGEJO_TOKEN", "job")
	t.Setenv("GITEA_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "job")
	t.Setenv("GH_TOKEN", "")

	r, err := ParseRemote("http://forgejo:3000/alice/demo.git", EnvHosts())
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != Gitea || r.Web != "http://forgejo:3000" || r.String() != "alice/demo" {
		t.Errorf("got %+v", r)
	}
	if got := r.APIBase(); got != "http://forgejo:3000/api/v1" {
		t.Errorf("API %s", got)
	}
	if got := r.ForgeName(); got != "Forgejo" {
		t.Errorf("forge name %s", got)
	}
	if tok, src := Token(Gitea, "forgejo"); tok != "job" || src != "FORGEJO_TOKEN" {
		t.Errorf("the job's server: %q %q", tok, src)
	}
	if tok, _ := Token(Gitea, "codeberg.org"); tok != "" {
		t.Errorf("the job token was sent to codeberg.org: %q", tok)
	}
	if tok, src := Token(GitHub, "github.com"); tok != "" {
		t.Errorf("the job token was sent to GitHub from %s", src)
	}
	t.Setenv("GH_TOKEN", "ghp")
	if tok, src := Token(GitHub, "github.com"); tok != "ghp" || src != "GH_TOKEN" {
		t.Errorf("GH_TOKEN is for GitHub: %q %q", tok, src)
	}

	// Gitea's act_runner: GITEA_ACTIONS and GITHUB_SERVER_URL only.
	t.Setenv("FORGEJO_ACTIONS", "")
	t.Setenv("FORGEJO_SERVER_URL", "")
	t.Setenv("GITHUB_SERVER_URL", "https://gitea.example.com")
	r, err = ParseRemote("https://gitea.example.com/o/r.git", EnvHosts())
	if err != nil || r.Kind != Gitea || r.ForgeName() != "Gitea" {
		t.Errorf("Gitea Actions: %+v %v", r, err)
	}

	// GitHub Actions sets neither flag: nothing changes there.
	t.Setenv("GITEA_ACTIONS", "")
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
	if h := EnvHosts(); len(h) != 0 {
		t.Errorf("GitHub Actions: hosts %v", h)
	}
	if tok, src := Token(GitHub, "github.com"); tok != "job" || src != "GITHUB_TOKEN" {
		t.Errorf("GitHub Actions: %q %q", tok, src)
	}
}
