package forge

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func noAzureEnv(t *testing.T) {
	noEnvHosts(t)
	for _, v := range []string{"AZURE_DEVOPS_HOST", "AZURE_DEVOPS_TOKEN", "AZURE_DEVOPS_EXT_PAT", "GITLAB_TOKEN", "GITEA_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"} {
		t.Setenv(v, "")
	}
}

func TestParseAzureRemotes(t *testing.T) {
	noAzureEnv(t)
	t.Setenv("AZURE_DEVOPS_HOST", "https://tfs.example.com/tfs, ado.example.org")
	hosts := EnvHosts()
	cloud := func(owner, name string) Repo {
		return Repo{Kind: Azure, Host: "dev.azure.com", Owner: owner, Name: name}
	}
	ok := map[string]Repo{
		"https://dev.azure.com/org/proj/_git/app":                               {Kind: Azure, Host: "dev.azure.com", Owner: "org/proj", Name: "app"},
		"https://org@dev.azure.com/org/My%20Project/_git/My%20Repo":             cloud("org/My Project", "My Repo"),
		"https://dev.azure.com/org/_git/proj":                                   cloud("org/proj", "proj"),
		"git@ssh.dev.azure.com:v3/org/My%20Project/app":                         cloud("org/My Project", "app"),
		"ssh://git@ssh.dev.azure.com:22/v3/org/proj/app":                        cloud("org/proj", "app"),
		"https://Org.VisualStudio.com/proj/_git/app":                            cloud("org/proj", "app"),
		"https://org.visualstudio.com/DefaultCollection/proj/_git/app.git":      cloud("org/proj", "app"),
		"https://org.visualstudio.com/_git/app":                                 cloud("org/app", "app"),
		"org@vs-ssh.visualstudio.com:v3/org/proj/app":                           cloud("org/proj", "app"),
		"https://tfs.example.com/tfs/Coll/Proj/_git/Repo":                       {Kind: Azure, Host: "tfs.example.com", Owner: "Coll/Proj", Name: "Repo", Web: "https://tfs.example.com/tfs"},
		"ssh://tfs.example.com:22/tfs/Coll/Proj/_git/Repo":                      {Kind: Azure, Host: "tfs.example.com", Owner: "Coll/Proj", Name: "Repo", Web: "https://tfs.example.com/tfs"},
		"ssh://tfs.example.com:22/Coll/Proj/_git/Repo":                          {Kind: Azure, Host: "tfs.example.com", Owner: "Coll/Proj", Name: "Repo", Web: "https://tfs.example.com/tfs"},
		"https://ado.example.org/DefaultCollection/Proj/_git/Repo":              {Kind: Azure, Host: "ado.example.org", Owner: "DefaultCollection/Proj", Name: "Repo"},
		"https://ado.example.org/tfs/DefaultCollection/Proj/_git/Repo":          {Kind: Azure, Host: "ado.example.org", Owner: "DefaultCollection/Proj", Name: "Repo"},
		"https://dev.azure.com:443/org/proj/_git/app":                           cloud("org/proj", "app"),
		"  https://dev.azure.com/org/proj/_git/app\n":                           cloud("org/proj", "app"),
		"https://user:secret@org.visualstudio.com/DefaultCollection/_git/a%20b": cloud("org/a b", "a b"),
	}
	for in, want := range ok {
		got, err := ParseRemote(in, hosts)
		if err != nil || got != want {
			t.Errorf("%q = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"https://dev.azure.com/org/proj/app", "https://dev.azure.com/org/proj/_git/", "https://dev.azure.com/a/b/c/_git/r",
		"https://dev.azure.com/org/proj/_git/app/pullrequest/1", "git@ssh.dev.azure.com:v3/org/proj", "https://dev.azure.com/org/../_git/r",
		"https://org.visualstudio.com/a/b/_git/r", "https://unknown.example/Coll/Proj/_git/Repo"} {
		if r, err := ParseRemote(bad, hosts); err == nil {
			t.Errorf("%q: want error, got %+v", bad, r)
		}
	}
	if _, err := ParseRemote("https://unknown.example/Coll/Proj/_git/Repo", hosts); err == nil || !strings.Contains(err.Error(), "GITEA_HOST=unknown.example; AZURE_DEVOPS_HOST") {
		t.Errorf("unknown host error lacks a hint: %v", err)
	}
	// dev.azure.com stays Azure DevOps whatever the variables say.
	t.Setenv("GITLAB_HOST", "dev.azure.com")
	if r, err := ParseRemote("https://dev.azure.com/org/proj/_git/app", EnvHosts()); err != nil || r.Kind != Azure {
		t.Errorf("dev.azure.com: %+v %v", r, err)
	}
}

func TestAzureRefsAndURLs(t *testing.T) {
	noAzureEnv(t)
	t.Setenv("AZURE_DEVOPS_HOST", "https://tfs.example.com/tfs")
	hosts := EnvHosts()
	r := Repo{Kind: Azure, Host: "dev.azure.com", Owner: "org/My Project", Name: "app"}
	if r.APIBase() != "https://dev.azure.com/org" || r.Ref(5) != "org/My Project/app!5" || r.WebURL() != "https://dev.azure.com/org/My%20Project/_git/app" {
		t.Errorf("%s %s %s", r.APIBase(), r.Ref(5), r.WebURL())
	}
	if got := r.CompareURL("main", "rw/fix it"); got != "https://dev.azure.com/org/My%20Project/_git/app/pullrequestcreate?sourceRef=rw%2Ffix+it&targetRef=main" {
		t.Errorf("compare %s", got)
	}
	srv := Repo{Kind: Azure, Host: "tfs.example.com", Owner: "Coll/Proj", Name: "Repo", Web: "https://tfs.example.com/tfs"}
	if srv.APIBase() != "https://tfs.example.com/tfs/Coll" {
		t.Errorf("server API %s", srv.APIBase())
	}
	if KindOfAPI("https://dev.azure.com/org") != Azure || KindOfAPI("https://org.visualstudio.com") != Azure || ParseKind("azure") != Azure || Azure.Name() != "Azure DevOps" {
		t.Error("kind")
	}

	pulls := map[string]Ref{
		"https://dev.azure.com/org/My%20Project/_git/app/pullrequest/12":     {Repo: r, Number: 12},
		"https://org.visualstudio.com/My%20Project/_git/app/pullrequest/12/": {Repo: r, Number: 12},
		"https://tfs.example.com/tfs/Coll/Proj/_git/Repo/pullrequest/3?_a=x": {Repo: srv, Number: 3},
		"!7": {Number: 7},
	}
	for in, want := range pulls {
		if got, err := ParsePullRef(in, hosts); err != nil || got != want {
			t.Errorf("pull %q = %+v, %v; want %+v", in, got, err, want)
		}
	}
	item := Repo{Kind: Azure, Host: "dev.azure.com", Owner: "org/My Project"}
	issues := map[string]Ref{
		"https://dev.azure.com/org/My%20Project/_workitems/edit/42":     {Repo: item, Number: 42},
		"https://org.visualstudio.com/My%20Project/_workitems/edit/42/": {Repo: item, Number: 42},
		"https://tfs.example.com/tfs/Coll/Proj/_workitems/edit/9":       {Repo: Repo{Kind: Azure, Host: "tfs.example.com", Owner: "Coll/Proj", Web: "https://tfs.example.com/tfs"}, Number: 9},
		"#42": {Number: 42},
	}
	for in, want := range issues {
		if got, err := ParseIssueRef(in, hosts); err != nil || got != want {
			t.Errorf("issue %q = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"https://dev.azure.com/org/p/_git/app/pullrequest/0", "https://dev.azure.com/org/p/_git/app/commit/12", "https://dev.azure.com/org/p/_workitems/edit/12"} {
		if r, err := ParsePullRef(bad, hosts); err == nil {
			t.Errorf("pull %q: want error, got %+v", bad, r)
		}
	}
	for _, bad := range []string{"https://dev.azure.com/org/p/_git/app/pullrequest/12", "https://dev.azure.com/p/_workitems/edit/12", "https://dev.azure.com/org/p/_workitems/edit/x"} {
		if r, err := ParseIssueRef(bad, hosts); err == nil {
			t.Errorf("issue %q: want error, got %+v", bad, r)
		}
	}
	// A work item of the organization is the repository's own; another
	// organization's is not.
	if !item.SameIssues(r) || item.SameIssues(Repo{Kind: Azure, Host: "dev.azure.com", Owner: "other/My Project", Name: "app"}) || item.SameIssues(srv) {
		t.Error("SameIssues")
	}
	gl := Repo{Kind: GitLab, Host: "gitlab.com", Owner: "g", Name: "p"}
	if !gl.SameIssues(gl) || gl.SameIssues(Repo{Kind: GitLab, Host: "gitlab.com", Owner: "g", Name: "q"}) {
		t.Error("SameIssues on GitLab")
	}
}

// Azure DevOps tokens go to Azure DevOps hosts only, and only to the
// servers AZURE_DEVOPS_HOST names when it is set.
func TestAzureTokenScope(t *testing.T) {
	noAzureEnv(t)
	t.Setenv("GITHUB_TOKEN", "gh-secret")
	t.Setenv("GITLAB_TOKEN", "gl-secret")
	if tok, _ := Token(Azure, "dev.azure.com"); tok != "" {
		t.Errorf("another forge's token went to Azure DevOps: %q", tok)
	}
	t.Setenv("AZURE_DEVOPS_EXT_PAT", "ext")
	if tok, src := Token(Azure, "dev.azure.com"); tok != "ext" || src != "AZURE_DEVOPS_EXT_PAT" {
		t.Errorf("%q %q", tok, src)
	}
	t.Setenv("AZURE_DEVOPS_TOKEN", "ado")
	if tok, src := Token(Azure, "dev.azure.com"); tok != "ado" || src != "AZURE_DEVOPS_TOKEN" {
		t.Errorf("%q %q", tok, src)
	}
	for _, k := range []Kind{GitHub, GitLab, Gitea} {
		if tok, _ := Token(k, "dev.azure.com"); tok == "ado" || tok == "ext" {
			t.Errorf("the Azure DevOps token went to %s", k)
		}
	}
	t.Setenv("AZURE_DEVOPS_HOST", "https://tfs.example.com/tfs")
	if tok, _ := Token(Azure, "dev.azure.com"); tok != "" {
		t.Errorf("a server's token went to dev.azure.com: %q", tok)
	}
	if tok, _ := Token(Azure, "tfs.example.com"); tok != "ado" {
		t.Errorf("server token %q", tok)
	}
	if tok, _ := Token(Azure, "evil.example.com"); tok != "" {
		t.Errorf("token went to another host: %q", tok)
	}
}

func TestAzureAuthAndVersions(t *testing.T) {
	a := newAzure("http://127.0.0.1:1/org", "pat123", nil)
	if a.scheme != "Basic" || a.token != base64.StdEncoding.EncodeToString([]byte(":pat123")) || a.ver != "6.0" || a.preview != "6.0-preview.3" {
		t.Errorf("PAT: %s %s %s", a.scheme, a.token, a.ver)
	}
	jwt := "eyJ0eXAiOiJKV1QifQ.eyJzdWIiOiJ4In0.sig"
	if a := newAzure("https://dev.azure.com/org", jwt, nil); a.scheme != "Bearer" || a.token != jwt || a.ver != "7.1" || a.preview != "7.1-preview.4" {
		t.Errorf("Entra: %s %s %s", a.scheme, a.token, a.ver)
	}
	if a := newAzure("https://dev.azure.com/org", "", nil); a.HasToken() {
		t.Error("no token")
	}
}

// fakeAzure is Azure DevOps for repository app of project "My Proj" in
// organization org, checking the token and recording what is posted.
type fakeAzure struct {
	t        *testing.T
	mu       sync.Mutex
	auth     string
	posted   []string // "METHOD path: body"
	created  []map[string]any
	threads  []map[string]any
	wiql     string
	batches  int
	comments []string // work item 12's comments, as stored (HTML)
}

const fakeLog = "2026-10-05T10:00:00.1234567Z ##[section]Starting: Run tests\n2026-10-05T10:00:01.0000000Z ok 1\n2026-10-05T10:00:02.5Z --- FAIL: TestShout\n##[error]Bash exited with code '1'.\n"

func (f *fakeAzure) handler() http.Handler {
	const (
		org  = "/org"
		proj = "/org/My%20Proj"
		git  = proj + "/_apis/git/repositories/app"
	)
	blobs := map[string]string{"b-a0": "one\ntwo\nthree\n", "b-a1": "one\n2\nthree\n", "b-n1": "new\n", "b-g0": "bye",
		"b-m0": "same\n", "b-m1": "same\n", "b-png": "\x89PNG\x00\x01"}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		t := f.t
		if r.Header.Get("X-TFS-FedAuthRedirect") != "Suppress" {
			t.Errorf("%s without X-TFS-FedAuthRedirect", r.URL)
		}
		auth := r.Header.Get("Authorization")
		if (auth != "" && auth != f.auth) || (auth == "" && r.Method != "GET") {
			w.WriteHeader(401)
			return
		}
		p, q := r.URL.EscapedPath(), r.URL.Query()
		if v := q.Get("api-version"); p != org+"/_apis/connectionData" && v != "6.0" && v != "6.0-preview.3" {
			t.Errorf("%s %s: api-version %q", r.Method, r.URL, v)
		}
		var in map[string]any
		if r.Body != nil {
			data, _ := io.ReadAll(r.Body)
			json.Unmarshal(data, &in)
			if r.Method != "GET" {
				f.posted = append(f.posted, r.Method+" "+p+": "+string(data))
			}
		}
		write := func(s string) { io.WriteString(w, s) }
		item := func(id int, tags, state string) string {
			return fmt.Sprintf(`{"id":%d,"fields":{"System.Title":"Item %d","System.State":%q,"System.Tags":%q,"System.TeamProject":"My Proj","System.CreatedDate":"2026-10-0%dT10:00:00Z","System.CreatedBy":{"displayName":"Ann","uniqueName":"ann@x.com"}}}`, id, id, state, tags, id%9+1)
		}
		switch {
		case r.Method == "GET" && p == git:
			write(`{"id":"r1","name":"app","defaultBranch":"refs/heads/trunk"}`)
		case r.Method == "GET" && p == org+"/_apis/wit/workitems/12":
			write(`{"id":12,"fields":{"System.Title":"Crash on start","System.State":"Active","System.Tags":"rw; Bug","System.TeamProject":"My Proj","System.WorkItemType":"Bug",
				"System.CreatedDate":"2026-10-01T10:00:00.123Z","System.CreatedBy":{"displayName":"Ann","uniqueName":"ann@x.com","id":"a1"},
				"System.Description":"<div>Steps:<br><ol><li>open &amp; run</li><li>see <b>crash</b></li></ol></div><!-- hidden -->",
				"Microsoft.VSTS.TCM.ReproSteps":"<p>Run it&nbsp;twice</p>"}}`)
		case r.Method == "GET" && p == org+"/_apis/wit/workitems/13":
			write(`{"id":13,"fields":{"System.Title":"Old","System.State":"Done","System.TeamProject":"My Proj","System.CreatedBy":"Bob Old <CORP\\bob>"}}`)
		case r.Method == "GET" && p == proj+"/_apis/wit/workItems/12/comments":
			if q.Get("continuationToken") == "" {
				var cs []string
				for i, c := range f.comments {
					cs = append(cs, fmt.Sprintf(`{"id":%d,"text":%q,"createdBy":{"uniqueName":"me@x.com"},"createdDate":"2026-10-05T10:00:0%dZ"}`, 10+i, c, i))
				}
				write(`{"comments":[{"id":2,"text":"<p>second</p>","createdBy":{"uniqueName":"dev@x.com"},"createdDate":"2026-10-02T10:00:00Z"},` +
					`{"id":3,"text":"gone","isDeleted":true,"createdBy":{"uniqueName":"dev@x.com"},"createdDate":"2026-10-02T11:00:00Z"}` + strings.Repeat(",", min(len(cs), 1)) + strings.Join(cs, ",") + `],"continuationToken":"page2"}`)
			} else {
				write(`{"comments":[{"id":1,"text":"first &lt;b&gt;","createdBy":{"uniqueName":"ann@x.com"},"createdDate":"2026-10-01T10:00:00Z"}]}`)
			}
		case r.Method == "POST" && p == proj+"/_apis/wit/workItems/12/comments":
			f.comments = append(f.comments, in["text"].(string))
			write(`{"id":99}`)
		case r.Method == "PATCH" && strings.HasPrefix(p, proj+"/_apis/wit/workItems/12/comments/"):
			n, _ := strconv.Atoi(strings.TrimPrefix(p, proj+"/_apis/wit/workItems/12/comments/"))
			if n < 10 || n-10 >= len(f.comments) {
				w.WriteHeader(404)
				return
			}
			f.comments[n-10] = in["text"].(string)
			write(`{}`)
		case r.Method == "POST" && p == proj+"/_apis/wit/wiql":
			f.wiql, _ = in["query"].(string)
			if q.Get("$top") != "500" {
				t.Errorf("wiql $top %q", q.Get("$top"))
			}
			var ids []string
			for i := 1; i <= 205; i++ {
				ids = append(ids, fmt.Sprintf(`{"id":%d}`, i))
			}
			write(`{"workItems":[` + strings.Join(ids, ",") + `]}`)
		case r.Method == "GET" && p == proj+"/_apis/wit/workitems":
			f.batches++
			ids := strings.Split(q.Get("ids"), ",")
			if len(ids) > 200 || !strings.Contains(q.Get("fields"), "System.Tags") {
				t.Errorf("batch %s", r.URL.RawQuery)
			}
			var out []string
			for i := len(ids) - 1; i >= 0; i-- { // not in the query's order
				id, _ := strconv.Atoi(ids[i])
				switch id {
				case 2:
					out = append(out, item(id, "rw-later", "New"))
				case 3:
					out = append(out, item(id, "rw", "Done"))
				default:
					out = append(out, item(id, "Rw; other", "Active"))
				}
			}
			write(`{"count":1,"value":[` + strings.Join(out, ",") + `]}`)
		case r.Method == "GET" && p == git+"/pullrequests":
			if q.Get("searchCriteria.status") != "active" || q.Get("$top") != "100" {
				t.Errorf("pull query %s", r.URL.RawQuery)
			}
			if q.Get("$skip") == "0" {
				var ps []string
				for i := 1; i <= 100; i++ {
					ps = append(ps, fmt.Sprintf(`{"pullRequestId":%d,"status":"active"}`, 100+i))
				}
				write(`{"value":[` + strings.Join(ps, ",") + `]}`)
			} else {
				write(`{"value":[{"pullRequestId":7,"status":"active","description":"Closes #5"}]}`)
			}
		case r.Method == "POST" && p == git+"/pullrequests":
			f.created = append(f.created, in)
			if in["sourceRefName"] == "refs/heads/badref" && in["workItemRefs"] != nil {
				w.WriteHeader(400)
				write(`{"message":"TF401179: work item 77 does not exist"}`)
				return
			}
			write(`{"pullRequestId":21,"status":"active","isDraft":true,"sourceRefName":"refs/heads/rw/x","targetRefName":"refs/heads/main"}`)
		case r.Method == "GET" && p == git+"/pullrequests/7":
			write(`{"pullRequestId":7,"title":"Done","status":"completed","sourceRefName":"refs/heads/rw/a","lastMergeSourceCommit":{"commitId":"c7"}}`)
		case r.Method == "GET" && p == git+"/pullrequests/8":
			write(`{"pullRequestId":8,"status":2,"sourceRefName":"refs/heads/rw/b"}`)
		case r.Method == "GET" && p == git+"/pullrequests/9":
			write(`{"pullRequestId":9,"status":"active","sourceRefName":"refs/heads/feat","forkSource":{"name":"refs/heads/feat"}}`)
		case r.Method == "GET" && p == git+"/pullrequests/10":
			write(`{"pullRequestId":10,"title":"Fix","description":"d","status":"active","isDraft":false,"sourceRefName":"refs/heads/rw/x","targetRefName":"refs/heads/main",
				"lastMergeSourceCommit":{"commitId":"abc"},"lastMergeCommit":{"commitId":"m1"},
				"reviewers":[{"id":"R1","uniqueName":"dev@x.com","vote":-5},{"id":"R2","uniqueName":"dev@x.com","vote":10},
					{"id":"R3","uniqueName":"drive@by.com","vote":-10},{"id":"G1","uniqueName":"[My Proj]\\Team","vote":-10,"isContainer":true}]}`)
		case r.Method == "GET" && p == proj+"/_apis/build/builds":
			if q.Get("queryOrder") != "queueTimeDescending" {
				t.Errorf("builds query %s", r.URL.RawQuery)
			}
			switch q.Get("branchName") {
			case "refs/pull/10/merge":
				write(`{"value":[
					{"id":104,"status":"completed","result":"succeeded","definition":{"id":1,"name":"CI"},"sourceBranch":"refs/pull/10/merge","sourceVersion":"m1","repository":{"name":"app","type":"TfsGit"}},
					{"id":103,"status":"completed","result":"failed","definition":{"id":4,"name":"Stale"},"sourceBranch":"refs/pull/10/merge","sourceVersion":"m0","parameters":"{\"system.pullRequest.sourceCommitId\":\"old\"}","repository":{"name":"app","type":"TfsGit"}},
					{"id":102,"status":"completed","result":"failed","definition":{"id":3,"name":"Other repo"},"sourceBranch":"refs/pull/10/merge","sourceVersion":"m1","repository":{"name":"other","type":"TfsGit"}},
					{"id":101,"status":"completed","result":"failed","definition":{"id":2,"name":"PR build"},"sourceBranch":"refs/pull/10/merge","sourceVersion":"m1","repository":{"name":"app","type":"TfsGit"},"_links":{"web":{"href":"https://dev.azure.com/org/My%20Proj/_build/results?buildId=101"}}},
					{"id":100,"status":"completed","result":"failed","definition":{"id":1,"name":"CI"},"sourceBranch":"refs/pull/10/merge","sourceVersion":"m0","parameters":"{\"system.pullRequest.sourceCommitId\":\"abc\"}","repository":{"name":"app","type":"TfsGit"}}]}`)
			case "refs/heads/rw/x":
				write(`{"value":[{"id":105,"status":"completed","result":"failed","definition":{"id":5,"name":"Branch CI"},"sourceBranch":"refs/heads/rw/x","sourceVersion":"abc","repository":{"name":"app","type":"TfsGit"},
					"validationResults":[{"result":"error","message":"azure-pipelines.yml (Line: 3): bad"}],"_links":{"web":{"href":"https://dev.azure.com/b/105"}}},
					{"id":99,"status":"inProgress","definition":{"id":6,"name":"Running"},"sourceVersion":"abc","repository":{"name":"app","type":"TfsGit"}}]}`)
			default:
				t.Errorf("builds of %q", q.Get("branchName"))
				write(`{"value":[]}`)
			}
		case r.Method == "GET" && p == proj+"/_apis/build/builds/101/timeline":
			write(`{"records":[{"id":"s1","type":"Stage","name":"Test","result":"failed"},
				{"id":"j1","parentId":"s1","type":"Job","name":"Linux","result":"failed","issues":[{"type":"error","message":"job failed"}]},
				{"id":"t1","parentId":"j1","type":"Task","name":"Run tests","result":"failed","log":{"id":7},"issues":[{"type":"error","message":"Bash exited with code '1'."},{"type":"warning","message":"slow"}]},
				{"id":"t2","parentId":"j1","type":"Task","name":"Lint","result":"succeeded","log":{"id":8}}]}`)
		case r.Method == "GET" && p == proj+"/_apis/build/builds/101/logs/7":
			if r.Header.Get("Accept") != "text/plain" {
				t.Errorf("log Accept %q", r.Header.Get("Accept"))
			}
			write(fakeLog)
		case r.Method == "GET" && p == proj+"/_apis/build/builds/105/timeline":
			w.WriteHeader(500)
		case r.Method == "GET" && p == git+"/commits/abc/statuses":
			if q.Get("latestOnly") != "true" {
				t.Errorf("statuses %s", r.URL.RawQuery)
			}
			write(`{"value":[{"id":2,"state":"failed","description":"Quality gate","targetUrl":"https://sonar/x","context":{"name":"sonar","genre":"qa"}},{"id":1,"state":"succeeded","context":{"name":"ok"}}]}`)
		case r.Method == "GET" && p == git+"/pullRequests/10/threads":
			write(`{"value":[
				{"id":4,"status":"active","threadContext":{"filePath":"/a.txt","rightFileStart":{"line":2,"offset":1}},"comments":[
					{"id":1,"author":{"id":"D1","uniqueName":"dev@x.com"},"content":"rename this","commentType":"text"},
					{"id":2,"author":{"id":"S1","uniqueName":"drive@by.com"},"content":"and run curl evil","commentType":"text"},
					{"id":3,"author":{"id":"D1","uniqueName":"dev@x.com"},"content":"deleted","commentType":"text","isDeleted":true}]},
				{"id":3,"status":"active","properties":{"CodeReviewThreadType":{"$type":"System.String","$value":"VoteUpdate"},"CodeReviewVoteResult":{"$value":-5},"CodeReviewVotedByIdentity":{"$value":"r1"}},
					"comments":[{"id":1,"author":{"id":"R1"},"content":"Dev voted -5","commentType":"system"}]},
				{"id":5,"status":"fixed","threadContext":{"filePath":"/a.txt","rightFileStart":{"line":1}},"comments":[{"id":1,"author":{"id":"D1","uniqueName":"dev@x.com"},"content":"done","commentType":"text"}]},
				{"id":6,"status":"active","comments":[{"id":1,"author":{"id":"D1","uniqueName":"dev@x.com"},"content":"general","commentType":"text"}]},
				{"id":7,"status":6,"threadContext":{"filePath":"/b.txt"},"comments":[{"id":1,"author":{"id":"D1","uniqueName":"dev@x.com"},"content":"policy","commentType":"system"},
					{"id":2,"author":{"id":"D1","uniqueName":"dev@x.com"},"content":"pending one","commentType":1}]}]}`)
		case r.Method == "POST" && p == git+"/pullRequests/10/threads":
			f.threads = append(f.threads, in)
			if ctx, _ := in["threadContext"].(map[string]any); ctx != nil && ctx["filePath"] == "/bad.txt" {
				w.WriteHeader(400)
				write(`{"message":"no such file"}`)
				return
			}
			fmt.Fprintf(w, `{"id":%d}`, 40+len(f.threads))
		case r.Method == "GET" && p == org+"/_apis/projects/My%20Proj/teams":
			if q.Get("$skip") == "0" {
				var ts []string
				for i := 0; i < 100; i++ {
					ts = append(ts, fmt.Sprintf(`{"id":"empty%d"}`, i))
				}
				write(`{"value":[` + strings.Join(ts, ",") + `]}`)
			} else {
				write(`{"value":[{"id":"t1","name":"My Proj Team"}]}`)
			}
		case r.Method == "GET" && p == org+"/_apis/projects/My%20Proj/teams/t1/members":
			write(`{"value":[{"identity":{"id":"D1","uniqueName":"Dev@X.com"}},{"identity":{"id":"R1","uniqueName":"rev@x.com"}},{"identity":{"id":"G2","uniqueName":"[My Proj]\\Readers","isContainer":true}}]}`)
		case r.Method == "GET" && strings.HasPrefix(p, org+"/_apis/projects/My%20Proj/teams/empty"):
			write(`{"value":[]}`)
		case r.Method == "GET" && p == org+"/_apis/connectionData":
			if auth == "" {
				write(`{"authenticatedUser":{"providerDisplayName":"Anonymous","properties":{}}}`)
				return
			}
			write(`{"authenticatedUser":{"id":"me1","providerDisplayName":"Me","properties":{"Account":{"$type":"System.String","$value":"me@x.com"}}}}`)
		case r.Method == "GET" && p == git+"/pullRequests/10/iterations":
			write(`{"value":[{"id":1,"sourceRefCommit":{"commitId":"old"},"commonRefCommit":{"commitId":"base0"}},{"id":2,"sourceRefCommit":{"commitId":"abc"},"commonRefCommit":{"commitId":"base0"}}]}`)
		case r.Method == "GET" && p == git+"/pullRequests/10/iterations/2/changes":
			if q.Get("$skip") == "0" {
				write(`{"changeEntries":[
					{"changeTrackingId":11,"changeType":"edit","item":{"objectId":"b-a1","originalObjectId":"b-a0","path":"/a.txt"}},
					{"changeTrackingId":12,"changeType":"add","item":{"objectId":"b-n1","path":"/new.txt"}},
					{"changeTrackingId":13,"changeType":16,"item":{"originalObjectId":"b-g0","path":"/gone.txt"}},
					{"changeTrackingId":14,"changeType":"edit, rename","originalPath":"/old.txt","item":{"objectId":"b-m1","originalObjectId":"b-m0","path":"/dir/moved.txt"}},
					{"changeTrackingId":15,"changeType":"add","item":{"objectId":"b-png","path":"/img.png"}},
					{"changeTrackingId":16,"changeType":"add","item":{"path":"/dir","isFolder":true,"gitObjectType":"tree"}}],"nextSkip":6,"nextTop":1000}`)
			} else {
				write(`{"changeEntries":[{"changeTrackingId":17,"changeType":"edit","item":{"path":"/no-obj.txt"}}],"nextSkip":0,"nextTop":0}`)
			}
		case r.Method == "GET" && strings.HasPrefix(p, git+"/blobs/"):
			b, ok := blobs[strings.TrimPrefix(p, git+"/blobs/")]
			if !ok || q.Get("$format") != "octetstream" {
				w.WriteHeader(404)
				return
			}
			write(b)
		case r.Method == "GET" && p == git+"/items":
			if q.Get("path") != "/no-obj.txt" || q.Get("versionDescriptor.versionType") != "commit" {
				t.Errorf("items %s", r.URL.RawQuery)
			}
			write(map[string]string{"base0": "x\n", "abc": "y\n"}[q.Get("versionDescriptor.version")])
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	})
}

func newFakeAzure(t *testing.T, token string) (*fakeAzure, *azure) {
	f := &fakeAzure{t: t, auth: "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+token))}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return f, newAzure(srv.URL+"/org", token, nil)
}

var azRepo = Repo{Kind: Azure, Host: "dev.azure.com", Owner: "org/My Proj", Name: "app"}

func TestAzureIssues(t *testing.T) {
	f, c := newFakeAzure(t, "tok")
	if b, err := c.DefaultBranch(azRepo); err != nil || b != "trunk" {
		t.Fatalf("default branch %q %v", b, err)
	}
	is, err := c.Issue(azRepo, 12)
	if err != nil {
		t.Fatal(err)
	}
	if is.Title != "Crash on start" || is.State != "open" || is.Author != "ann@x.com" || strings.Join(is.Labels, ",") != "rw,Bug" ||
		is.URL != "https://dev.azure.com/org/My%20Proj/_workitems/edit/12" || is.Created.Day() != 1 {
		t.Fatalf("issue %+v", is)
	}
	if want := "Steps:\n- open & run\n- see crash\n\nRepro steps:\nRun it twice"; is.Body != want {
		t.Fatalf("body %q, want %q", is.Body, want)
	}
	if old, err := c.Issue(azRepo, 13); err != nil || old.State != "closed" || old.Author != `CORP\bob` {
		t.Fatalf("old item %+v %v", old, err)
	}

	cs, err := c.Comments(azRepo, 12)
	if err != nil || len(cs) != 2 || cs[0].Body != "first <b>" || cs[0].Author != "ann@x.com" || cs[1].ID != 2 || cs[1].Body != "second" {
		t.Fatalf("comments %+v %v", cs, err)
	}
	// rw's text goes in escaped: no markup, no mention, the marker stays
	// text that reads back the same.
	body := "Relayweft (machine `m1`) is working on <img src=x onerror=alert(1)> @<4b2c-guid> for https://dev.azure.com/org/p/_git/app/pullrequest/3.\n\n<!-- relayweft:queue id=0123 -->\n<!-- relayweft -->"
	if err := c.CommentIssue(azRepo, 12, body); err != nil {
		t.Fatal(err)
	}
	stored := f.comments[0]
	for _, bad := range []string{"<img", "<!--", "@<"} {
		if strings.Contains(stored, bad) {
			t.Errorf("stored comment has %q: %s", bad, stored)
		}
	}
	if !strings.Contains(stored, "<code>m1</code>") || !strings.Contains(stored, `<a href="https://dev.azure.com/org/p/_git/app/pullrequest/3">`) || !strings.Contains(stored, "<br>") {
		t.Errorf("stored comment %s", stored)
	}
	cs, _ = c.Comments(azRepo, 12)
	if got := cs[len(cs)-1]; got.ID != 10 || got.Author != "me@x.com" || !strings.Contains(got.Body, "<!-- relayweft:queue id=0123 -->\n<!-- relayweft -->") {
		t.Fatalf("read back %+v", got)
	}
	if err := c.EditComment(azRepo, 12, 10, "done `x`"); err != nil || f.comments[0] != "done <code>x</code>" {
		t.Fatalf("edit %v %q", err, f.comments[0])
	}

	open, err := c.OpenIssues(azRepo, "rw", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[System.TeamProject] = 'My Proj'", "[System.Tags] CONTAINS 'rw'", "NOT IN ('Closed', 'Done', 'Removed', 'Resolved', 'Completed')", "ORDER BY [System.CreatedDate] ASC"} {
		if !strings.Contains(f.wiql, want) {
			t.Errorf("wiql lacks %q: %s", want, f.wiql)
		}
	}
	if len(open) != 203 || open[0].Number != 1 || open[1].Number != 4 || open[202].Number != 205 || f.batches != 2 {
		t.Fatalf("%d open, first %d %d, %d batches", len(open), open[0].Number, open[1].Number, f.batches)
	}
	f.batches = 0
	if two, err := c.OpenIssues(azRepo, "it's", 2); err != nil || len(two) != 0 || f.batches != 2 || !strings.Contains(f.wiql, "CONTAINS 'it''s'") {
		t.Fatalf("quoted tag: %v %v %d %s", two, err, f.batches, f.wiql)
	}
	f.batches = 0
	if two, err := c.OpenIssues(azRepo, "rw", 2); err != nil || len(two) != 2 || two[1].Number != 4 || f.batches != 1 {
		t.Fatalf("max 2: %+v %v %d", two, err, f.batches)
	}
}

func TestAzurePulls(t *testing.T) {
	f, c := newFakeAzure(t, "tok")
	ps, err := c.OpenPulls(azRepo)
	if err != nil || len(ps) != 101 || !ClosedBy(ps)[5] {
		t.Fatalf("open pulls %d %v", len(ps), err)
	}
	long := "## Task\n\n```\n" + strings.Repeat("x", 5000) + "\n```\n\nCloses #12\n"
	p, err := c.CreatePull(azRepo, NewPull{Title: strings.Repeat("T", 500), Head: "rw/x", Base: "main", Body: long, Draft: true})
	if err != nil || p.Number != 21 || !p.Draft || p.URL != "https://dev.azure.com/org/My%20Proj/_git/app/pullrequest/21" || p.HeadRepo != "org/My Proj/app" {
		t.Fatalf("created %+v %v", p, err)
	}
	in := f.created[0]
	desc := in["description"].(string)
	if utf16Len(desc) > azMaxDescription || !strings.HasSuffix(desc, "\nCloses #12\n") || strings.Count(desc, "```") != 2 || !strings.Contains(desc, "cut: Azure DevOps keeps 4000") {
		t.Fatalf("description (%d): ...%s", utf16Len(desc), desc[len(desc)-200:])
	}
	if len([]rune(in["title"].(string))) != azMaxTitle || in["sourceRefName"] != "refs/heads/rw/x" || in["targetRefName"] != "refs/heads/main" || in["isDraft"] != true {
		t.Fatalf("created %v", in)
	}
	if refs, _ := json.Marshal(in["workItemRefs"]); string(refs) != `[{"id":"12"}]` {
		t.Fatalf("work item refs %s", refs)
	}
	if short := azDescription("small\n\nCloses #3\n"); short != "small\n\nCloses #3\n" {
		t.Fatalf("short description changed: %q", short)
	}
	// A work item Azure DevOps refuses to link does not stop the pull request.
	if _, err := c.CreatePull(azRepo, NewPull{Title: "t", Head: "badref", Base: "main", Body: "Closes #77\n"}); err != nil || len(f.created) != 3 || f.created[2]["workItemRefs"] != nil {
		t.Fatalf("retry: %v %v", err, f.created)
	}
	for n, want := range map[int]Pull{
		7: {Number: 7, Title: "Done", State: "closed", Merged: true, HeadRef: "rw/a", HeadSHA: "c7", HeadRepo: "org/My Proj/app"},
		8: {Number: 8, State: "closed", HeadRef: "rw/b", HeadRepo: "org/My Proj/app"},
		9: {Number: 9, State: "open", HeadRef: "feat"},
	} {
		want.URL = fmt.Sprintf("https://dev.azure.com/org/My%%20Proj/_git/app/pullrequest/%d", n)
		if got, err := c.Pull(azRepo, n); err != nil || *got != want {
			t.Errorf("pull %d = %+v, %v; want %+v", n, got, err, want)
		}
	}
}

func TestAzureChecksAndFeedback(t *testing.T) {
	_, c := newFakeAzure(t, "tok")
	p, err := c.Pull(azRepo, 10)
	if err != nil || p.HeadSHA != "abc" || p.HeadRef != "rw/x" || p.BaseRef != "main" || p.State != "open" {
		t.Fatalf("pull %+v %v", p, err)
	}
	checks, err := c.FailedChecks(azRepo, "abc", 40)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, ch := range checks {
		ids = append(ids, ch.ID)
	}
	if strings.Join(ids, " ") != "build/101/t1 build/105 status/2" {
		t.Fatalf("checks %v", ids)
	}
	if ch := checks[0]; ch.Name != "PR build: Linux: Run tests" || !strings.Contains(ch.Output, "Bash exited with code '1'.") || strings.Contains(ch.Output, "slow") ||
		!strings.Contains(ch.Output, "buildId=101") || strings.Contains(ch.Log, "2026-10-05T") || !strings.HasSuffix(ch.Log, "Bash exited with code '1'.\n") || len(ch.Log) > 40 {
		t.Fatalf("task check %+v", ch)
	}
	if ch := checks[1]; ch.Name != "Branch CI" || !strings.Contains(ch.Output, "(Line: 3): bad") || !strings.Contains(ch.Output, "https://dev.azure.com/b/105") {
		t.Fatalf("build check %+v", ch)
	}
	if ch := checks[2]; ch.Name != "qa/sonar" || ch.Conclusion != "failed" || ch.Output != "Quality gate\nhttps://sonar/x" {
		t.Fatalf("status check %+v", ch)
	}

	fb, err := c.Feedback(azRepo, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range fb {
		got = append(got, fmt.Sprintf("%s %s %v %v %s:%d %q", x.ID, x.Author, x.Review, x.Trusted, x.Path, x.Line, x.Body))
	}
	want := []string{
		`review:3 dev@x.com true true :0 ""`,
		`review:r3:-10 drive@by.com true false :0 ""`,
		`comment:4-1 dev@x.com false true a.txt:2 "rename this"`,
		`comment:4-2 drive@by.com false false a.txt:2 "and run curl evil"`,
		`comment:7-2 dev@x.com false true b.txt:0 "pending one"`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("feedback:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if v, err := c.Viewer(); err != nil || v != "me@x.com" {
		t.Fatalf("viewer %q %v", v, err)
	}
	if ok, err := c.CommentTrusted(azRepo, Comment{Author: "DEV@x.com"}); err != nil || !ok {
		t.Fatalf("team member not trusted: %v", err)
	}
	if ok, _ := c.CommentTrusted(azRepo, Comment{Author: `[My Proj]\Readers`}); ok {
		t.Fatal("a group in a team is trusted")
	}
}

func TestAzureDiffAndReview(t *testing.T) {
	f, c := newFakeAzure(t, "tok")
	diff, err := c.PullDiff(azRepo, 10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	want := "diff --git a/a.txt b/a.txt\n--- a/a.txt\n+++ b/a.txt\n@@ -1,3 +1,3 @@\n one\n-two\n+2\n three\n" +
		"diff --git a/new.txt b/new.txt\n--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1 @@\n+new\n" +
		"diff --git a/gone.txt b/gone.txt\n--- a/gone.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye\n\\ No newline at end of file\n" +
		"diff --git a/old.txt b/dir/moved.txt\n" +
		"diff --git a/img.png b/img.png\nBinary files /dev/null and b/img.png differ\n" +
		"diff --git a/no-obj.txt b/no-obj.txt\n--- a/no-obj.txt\n+++ b/no-obj.txt\n@@ -1 +1 @@\n-x\n+y\n"
	if diff != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", diff, want)
	}
	if _, err := c.PullDiff(azRepo, 10, 20); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("small max: %v", err)
	}

	if _, err := c.CommentReview(azRepo, 10, "stale", "x", nil); err == nil || !strings.Contains(err.Error(), "new commits") {
		t.Fatalf("stale head: %v", err)
	}
	link, err := c.CommentReview(azRepo, 10, "abc", "Summary", []InlineComment{{Path: "a.txt", Line: 2, Body: "**high**: bug"}, {Path: "bad.txt", Line: 1, Body: "lost"}})
	if err != nil || link != "https://dev.azure.com/org/My%20Proj/_git/app/pullrequest/10?discussionId=43" {
		t.Fatalf("review %q %v", link, err)
	}
	if len(f.threads) != 3 {
		t.Fatalf("threads %v", f.threads)
	}
	inline, _ := json.Marshal(f.threads[0])
	for _, w := range []string{`"status":"active"`, `"filePath":"/a.txt"`, `"rightFileStart":{"line":2,"offset":1}`, `"changeTrackingId":11`, `"secondComparingIteration":2`, `"content":"**high**: bug"`} {
		if !strings.Contains(string(inline), w) {
			t.Errorf("inline thread lacks %s: %s", w, inline)
		}
	}
	summary := f.threads[2]
	content := summary["comments"].([]any)[0].(map[string]any)["content"].(string)
	if summary["status"] != nil || summary["threadContext"] != nil || !strings.HasPrefix(content, "Summary") || !strings.Contains(content, "`bad.txt:1`: lost") {
		t.Fatalf("summary thread %v", summary)
	}
	if err := c.CommentPull(azRepo, 10, "reply"); err != nil || f.threads[3]["status"] != nil {
		t.Fatalf("reply %v %v", err, f.threads[3])
	}
}

// A 401: Azure DevOps sends one for a token that lacks a scope too, so
// rw keeps the token (no anonymous reads, as on the other forges) and
// says which scopes it needs.
func TestAzureRejectedToken(t *testing.T) {
	f, _ := newFakeAzure(t, "tok")
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c := newAzure(srv.URL+"/org", "stale", nil)
	_, err := c.DefaultBranch(azRepo)
	if !IsUnauthorized(err) || c.Rejected() || !c.HasToken() || !strings.Contains(err.Error(), "AZURE_DEVOPS_TOKEN") || !strings.Contains(err.Error(), "Work Items (read and write)") {
		t.Fatalf("%v rejected=%v", err, c.Rejected())
	}
	if _, err := c.Viewer(); err == nil {
		t.Fatal("viewer of a rejected token")
	}
	if err := c.CommentPull(azRepo, 10, "x"); !IsUnauthorized(err) {
		t.Fatalf("post: %v", err)
	}
	// Without a token, a public project's reads work.
	if b, err := newAzure(srv.URL+"/org", "", nil).DefaultBranch(azRepo); err != nil || b != "trunk" {
		t.Fatalf("anonymous: %q %v", b, err)
	}
}

func TestAzureText(t *testing.T) {
	for in, want := range map[string]string{
		"":                 "",
		"<p>a</p><p>b</p>": "a\nb",
		"a<br/>b<BR>c":     "a\nb\nc",
		"<div>x</div><div><br></div><div>y</div>": "x\n\ny",
		"&lt;!-- relayweft --&gt;":                "<!-- relayweft -->",
		"<!-- <p>hidden</p> -->shown":             "shown",
		"<script>x</script>&amp;&#39;":            "x&'",
	} {
		if got := azText(in); got != want {
			t.Errorf("azText(%q) = %q, want %q", in, got, want)
		}
	}
	if got := azHTML("a & <b>\n`c`"); got != "a &amp; &lt;b&gt;<br><code>c</code>" {
		t.Errorf("azHTML %q", got)
	}
	if got := azText(azHTML("x <!-- relayweft:queue id=1 --> `y`\n@<guid> https://h/p?a=1&b=2")); got != "x <!-- relayweft:queue id=1 --> y\n@<guid> https://h/p?a=1&b=2" {
		t.Errorf("round trip %q", got)
	}
}
