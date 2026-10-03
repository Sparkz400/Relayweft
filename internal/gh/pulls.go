package gh

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// What sy watch and sy review read and write on a pull request: its state,
// diff, check runs (with the Actions job log), reviews and review
// comments, and a review of its own (always a plain COMMENT).

// Pull reads one pull request (Merged is only set here, not in listings).
func (c *Client) Pull(r Repo, n int) (*Pull, error) {
	var p Pull
	if err := c.do(http.MethodGet, fmt.Sprintf("%s/pulls/%d", repoPath(r), n), nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ErrTooLarge means a text answer was longer than the caller allows.
var ErrTooLarge = errors.New("answer too large")

// text GETs path with accept and returns the body, at most max bytes
// (ErrTooLarge beyond that).
func (c *Client) text(path, accept string, max int64) (string, error) {
	resp, err := c.send(http.MethodGet, path, accept, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return "", fmt.Errorf("GitHub GET %s: %w", path, err)
	}
	if int64(len(data)) > max {
		return "", fmt.Errorf("GitHub GET %s: %w (over %d bytes)", path, ErrTooLarge, max)
	}
	return string(data), nil
}

// PullDiff reads a pull request's unified diff (at most max bytes).
func (c *Client) PullDiff(r Repo, n int, max int64) (string, error) {
	return c.text(fmt.Sprintf("%s/pulls/%d", repoPath(r), n), "application/vnd.github.diff", max)
}

// CheckRun is one check (a CI job) on a commit.
type CheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`     // queued, in_progress, completed
	Conclusion string `json:"conclusion"` // success, failure, timed_out, ...
	HTMLURL    string `json:"html_url"`
	DetailsURL string `json:"details_url"`
	Output     struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
		Text    string `json:"text"`
	} `json:"output"`
	App *struct {
		Slug string `json:"slug"`
	} `json:"app"`
}

// Failed reports whether the check finished unsuccessfully in a way a code
// change can fix (cancelled and skipped checks are not failures).
func (cr CheckRun) Failed() bool {
	if cr.Status != "completed" {
		return false
	}
	switch cr.Conclusion {
	case "failure", "timed_out", "startup_failure":
		return true
	}
	return false
}

// Actions reports whether GitHub Actions ran the check: its id is then the
// job id, and the job's log can be read.
func (cr CheckRun) Actions() bool { return cr.App != nil && cr.App.Slug == "github-actions" }

// CheckRuns lists the check runs of a commit.
func (c *Client) CheckRuns(r Repo, sha string) ([]CheckRun, error) {
	var all []CheckRun
	for page := 1; page <= maxPages; page++ {
		var res struct {
			Total int        `json:"total_count"`
			Runs  []CheckRun `json:"check_runs"`
		}
		if err := c.do(http.MethodGet, fmt.Sprintf("%s/commits/%s/check-runs?per_page=100&page=%d", repoPath(r), url.PathEscape(sha), page), nil, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Runs...)
		if len(res.Runs) < 100 || len(all) >= res.Total {
			break
		}
	}
	return all, nil
}

// maxLogRead caps how much of a job log is read to find its tail.
const maxLogRead = 64 << 20

// JobLogTail reads the last max bytes of a GitHub Actions job's log. The
// API answers with a redirect to the log file; Go's client follows it and
// drops the Authorization header for the other host.
func (c *Client) JobLogTail(r Repo, jobID int64, max int) (string, error) {
	path := fmt.Sprintf("%s/actions/jobs/%d/logs", repoPath(r), jobID)
	resp, err := c.send(http.MethodGet, path, "application/vnd.github+json", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	t := &tail{max: max}
	if _, err := io.Copy(t, io.LimitReader(resp.Body, maxLogRead)); err != nil {
		return "", fmt.Errorf("GitHub GET %s: %w", path, err)
	}
	return string(t.buf), nil
}

// tail keeps the last max bytes written to it.
type tail struct {
	buf []byte
	max int
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0:0], t.buf[over:]...)
	}
	return len(p), nil
}

// Review is a submitted pull request review.
type Review struct {
	ID          int64     `json:"id"`
	User        User      `json:"user"`
	Body        string    `json:"body"`
	State       string    `json:"state"` // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED, PENDING
	CommitID    string    `json:"commit_id"`
	HTMLURL     string    `json:"html_url"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// ReviewComment is an inline comment on a pull request's diff.
type ReviewComment struct {
	ID        int64     `json:"id"`
	User      User      `json:"user"`
	Body      string    `json:"body"`
	Path      string    `json:"path"`
	Line      int       `json:"line"`
	DiffHunk  string    `json:"diff_hunk"`
	CommitID  string    `json:"commit_id"`
	InReplyTo int64     `json:"in_reply_to_id"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
}

// Reviews lists a pull request's reviews, oldest first.
func (c *Client) Reviews(r Repo, n int) ([]Review, error) {
	var all []Review
	for page := 1; page <= maxPages; page++ {
		var rs []Review
		if err := c.do(http.MethodGet, fmt.Sprintf("%s/pulls/%d/reviews?per_page=100&page=%d", repoPath(r), n, page), nil, &rs); err != nil {
			return nil, err
		}
		all = append(all, rs...)
		if len(rs) < 100 {
			break
		}
	}
	return all, nil
}

// ReviewComments lists a pull request's inline review comments, oldest
// first.
func (c *Client) ReviewComments(r Repo, n int) ([]ReviewComment, error) {
	var all []ReviewComment
	for page := 1; page <= maxPages; page++ {
		var cs []ReviewComment
		if err := c.do(http.MethodGet, fmt.Sprintf("%s/pulls/%d/comments?per_page=100&page=%d", repoPath(r), n, page), nil, &cs); err != nil {
			return nil, err
		}
		all = append(all, cs...)
		if len(cs) < 100 {
			break
		}
	}
	return all, nil
}

// Viewer is the login of the token's owner.
func (c *Client) Viewer() (string, error) {
	var u User
	if err := c.do(http.MethodGet, "/user", nil, &u); err != nil {
		return "", err
	}
	if u.Login == "" {
		return "", errors.New("GitHub reported no login for the token")
	}
	return u.Login, nil
}

// InlineComment is a review comment on one line of the pull request's new
// version (side RIGHT).
type InlineComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

// CommentReview posts one review with inline comments. Its event is always
// COMMENT: sy never approves or requests changes on anyone's behalf.
func (c *Client) CommentReview(r Repo, n int, commit, body string, comments []InlineComment) (*Review, error) {
	for i := range comments {
		comments[i].Side = "RIGHT"
	}
	in := struct {
		CommitID string          `json:"commit_id,omitempty"`
		Body     string          `json:"body"`
		Event    string          `json:"event"`
		Comments []InlineComment `json:"comments,omitempty"`
	}{commit, body, "COMMENT", comments}
	var out Review
	if err := c.do(http.MethodPost, fmt.Sprintf("%s/pulls/%d/reviews", repoPath(r), n), in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PullRef is a pull request given as "12", "#12" or a pull request URL.
// Repo is zero for a bare number (meaning: the current repository).
type PullRef struct {
	Repo   Repo
	Number int
}

// ParsePullRef reads a pull request number or URL
// (https://github.com/o/r/pull/12, also .../pull/12/files).
func ParsePullRef(s, enterpriseHost string) (PullRef, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(strings.TrimPrefix(s, "#")); err == nil {
		if n <= 0 {
			return PullRef{}, fmt.Errorf("pull request number must be positive: %q", s)
		}
		return PullRef{Number: n}, nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return PullRef{}, fmt.Errorf("pull request %q: want a number or a pull request URL", s)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return PullRef{}, fmt.Errorf("pull request %q: want https://github.com/<owner>/<repo>/pull/<n>", s)
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return PullRef{}, fmt.Errorf("pull request %q: bad number", s)
	}
	r, err := ParseRemote(u.Scheme+"://"+u.Host+"/"+parts[0]+"/"+parts[1], enterpriseHost)
	if err != nil {
		return PullRef{}, err
	}
	return PullRef{Repo: r, Number: n}, nil
}
