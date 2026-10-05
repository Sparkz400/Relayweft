package gh

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// pullsAPI serves pull request #7 of o/r with checks, reviews and a log
// that lives behind a redirect, as GitHub serves job logs.
func pullsAPI(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	logs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("noise\n", 1000)+"FAIL: TestX\nexit 1\n")
	}))
	t.Cleanup(logs.Close)
	var posted []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case r.Method == "GET" && p == "/repos/o/r/pulls/7" && r.Header.Get("Accept") == "application/vnd.github.diff":
			io.WriteString(w, "diff --git a/x b/x\n")
		case r.Method == "GET" && p == "/repos/o/r/pulls/7":
			io.WriteString(w, `{"number":7,"state":"closed","merged":true,"user":{"login":"me"},"head":{"ref":"rw/x","sha":"abc","repo":{"full_name":"o/r"}},"base":{"ref":"main","sha":"def"}}`)
		case r.Method == "GET" && p == "/repos/o/r/commits/abc/check-runs":
			page := r.URL.Query().Get("page")
			var runs []string
			for i := 0; i < 100 && page == "1"; i++ {
				runs = append(runs, fmt.Sprintf(`{"id":%d,"status":"completed","conclusion":"success"}`, i+1))
			}
			if page == "2" {
				runs = append(runs, `{"id":500,"name":"test","status":"completed","conclusion":"failure","app":{"slug":"github-actions"}}`)
			}
			fmt.Fprintf(w, `{"total_count":101,"check_runs":[%s]}`, strings.Join(runs, ","))
		case r.Method == "GET" && p == "/repos/o/r/actions/jobs/500/logs":
			http.Redirect(w, r, logs.URL+"/log.txt", http.StatusFound)
		case r.Method == "GET" && p == "/repos/o/r/pulls/7/reviews":
			io.WriteString(w, `[{"id":1,"state":"CHANGES_REQUESTED","body":"no","user":{"login":"rev"}}]`)
		case r.Method == "GET" && p == "/repos/o/r/pulls/7/comments":
			io.WriteString(w, `[{"id":2,"path":"a.go","line":3,"body":"why?","user":{"login":"rev"}}]`)
		case r.Method == "GET" && p == "/user":
			io.WriteString(w, `{"login":"me"}`)
		case r.Method == "POST" && p == "/repos/o/r/pulls/7/reviews":
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			posted = append(posted, in)
			io.WriteString(w, `{"id":9,"html_url":"https://github.com/o/r/pull/7#pullrequestreview-9"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &posted
}

func TestPullChecksReviewsAndLogs(t *testing.T) {
	srv, posted := pullsAPI(t)
	c := NewClient(srv.URL, "tok")
	p, err := c.Pull(repo, 7)
	if err != nil || !p.Merged || p.Head.SHA != "abc" || p.Head.Repo.FullName != "o/r" || p.Base.Ref != "main" {
		t.Fatalf("%+v %v", p, err)
	}
	if d, err := c.PullDiff(repo, 7, 100); err != nil || d != "diff --git a/x b/x\n" {
		t.Fatalf("diff %q %v", d, err)
	}
	if _, err := c.PullDiff(repo, 7, 5); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over the cap: %v", err)
	}
	runs, err := c.CheckRuns(repo, "abc")
	if err != nil || len(runs) != 101 {
		t.Fatalf("%d runs, %v", len(runs), err)
	}
	var failed []CheckRun
	for _, r := range runs {
		if r.Failed() {
			failed = append(failed, r)
		}
	}
	if len(failed) != 1 || !failed[0].Actions() || failed[0].Name != "test" {
		t.Fatalf("failed = %+v", failed)
	}
	log, err := c.JobLogTail(repo, failed[0].ID, 40)
	if err != nil || len(log) != 40 || !strings.HasSuffix(log, "FAIL: TestX\nexit 1\n") {
		t.Fatalf("log tail %q %v", log, err)
	}
	rs, err := c.Reviews(repo, 7)
	if err != nil || len(rs) != 1 || rs[0].State != "CHANGES_REQUESTED" {
		t.Fatalf("%+v %v", rs, err)
	}
	cs, err := c.ReviewComments(repo, 7)
	if err != nil || len(cs) != 1 || cs[0].Line != 3 || cs[0].Path != "a.go" {
		t.Fatalf("%+v %v", cs, err)
	}
	if v, err := c.Viewer(); err != nil || v != "me" {
		t.Fatalf("viewer %q %v", v, err)
	}
	// A review is always a plain comment, with every inline comment on
	// the new side.
	rv, err := c.CommentReview(repo, 7, "abc", "body", []InlineComment{{Path: "a.go", Line: 3, Side: "LEFT", Body: "x"}})
	if err != nil || rv.ID != 9 || len(*posted) != 1 {
		t.Fatalf("%+v %v", rv, err)
	}
	in := (*posted)[0]
	cm := in["comments"].([]any)[0].(map[string]any)
	if in["event"] != "COMMENT" || in["commit_id"] != "abc" || cm["side"] != "RIGHT" || cm["line"] != float64(3) {
		t.Fatalf("posted %+v", in)
	}
}

func TestCheckRunFailed(t *testing.T) {
	for _, c := range []struct {
		status, conclusion string
		want               bool
	}{
		{"completed", "failure", true}, {"completed", "timed_out", true}, {"completed", "startup_failure", true},
		{"completed", "success", false}, {"completed", "cancelled", false}, {"completed", "skipped", false},
		{"in_progress", "", false}, {"queued", "failure", false},
	} {
		if got := (CheckRun{Status: c.status, Conclusion: c.conclusion}).Failed(); got != c.want {
			t.Errorf("%s/%s: failed = %v", c.status, c.conclusion, got)
		}
	}
}

func TestParsePullRef(t *testing.T) {
	for in, n := range map[string]int{"12": 12, "#7": 7} {
		r, err := ParsePullRef(in, "")
		if err != nil || r.Number != n || !r.Repo.IsZero() {
			t.Errorf("%q = %+v %v", in, r, err)
		}
	}
	r, err := ParsePullRef("https://github.com/o/r/pull/42/files", "")
	if err != nil || r.Number != 42 || r.Repo.String() != "o/r" {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = ParsePullRef("https://ghe.example.com/team/app/pull/5", "ghe.example.com")
	if err != nil || r.Repo.Host != "ghe.example.com" || r.Repo.APIBase() != "https://ghe.example.com/api/v3" {
		t.Fatalf("enterprise: %+v %v", r, err)
	}
	for _, bad := range []string{"0", "x", "https://github.com/o/r/issues/3", "https://github.com/o/r/pull/x", "https://ghe.example.com/team/app/pull/5"} {
		if _, err := ParsePullRef(bad, ""); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}
