package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/proc"
)

// Client talks to one GitHub API host.
type Client struct {
	// Base is the API root: https://api.github.com, https://<host>/api/v3
	// or a test server.
	Base  string
	Token string
	HTTP  *http.Client
	// Notes receives one-line notices (a rejected token); nil = discard.
	Notes io.Writer
	// rejected is set when the token got a 401: reads go on without it.
	rejected bool
}

// Rejected reports whether the token got a 401, so later reads went on
// without it (and a private repository then looks like a 404).
func (c *Client) Rejected() bool { return c.rejected }

// NewClient returns a client for base with the given token ("" = none).
// The default transport is kept on purpose: it honours HTTPS_PROXY.
func NewClient(base, token string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), Token: token, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// HasToken reports whether requests carry a token that GitHub accepted so far.
func (c *Client) HasToken() bool { return c.Token != "" && !c.rejected }

// APIError is a non-2xx answer.
type APIError struct {
	Status  int
	Method  string
	Path    string
	Message string
	Hint    string
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("GitHub %s %s: %d", e.Method, e.Path, e.Status)
	if e.Message != "" {
		s += " " + e.Message
	}
	if e.Hint != "" {
		s += ". " + e.Hint
	}
	return s
}

// IsNotFound reports a 404 from GitHub.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// IsUnauthorized reports a 401 from GitHub.
func IsUnauthorized(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusUnauthorized
}

func (c *Client) note(format string, args ...any) {
	if c.Notes != nil {
		fmt.Fprintf(c.Notes, format+"\n", args...)
	}
}

// do sends one request; in (if not nil) is sent as JSON and the answer is
// decoded into out (if not nil).
func (c *Client) do(method, path string, in, out any) error {
	resp, err := c.send(method, path, "application/vnd.github+json", in)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("GitHub %s %s: bad response: %w", method, path, err)
	}
	return nil
}

// send sends one request and returns a 2xx response (the caller closes
// its body); any other status is an *APIError.
func (c *Client) send(method, path, accept string, in any) (*http.Response, error) {
	var body []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = b
	}
	req, err := http.NewRequest(method, c.Base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "switchyard")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	authed := c.HasToken()
	if authed {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub %s %s: %w", method, path, err)
	}
	if resp.StatusCode == http.StatusUnauthorized && authed && method == http.MethodGet {
		// A stale token must not block reading a public repository.
		resp.Body.Close()
		c.note("note: GitHub rejected the token (401); continuing without it")
		c.rejected = true
		return c.send(method, path, accept, in)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		ae := &APIError{Status: resp.StatusCode, Method: method, Path: path, Message: apiMessage(data)}
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			if authed || c.rejected {
				ae.Hint = "The token (GITHUB_TOKEN, GH_TOKEN or `gh auth token`) was rejected; create a new one or run `gh auth login`"
			} else {
				ae.Hint = "This needs a token: set GITHUB_TOKEN or GH_TOKEN, or run `gh auth login`"
			}
		case http.StatusNotFound:
			if authed {
				ae.Hint = "The token may lack access to this repository"
			} else {
				ae.Hint = "If the repository is private, set GITHUB_TOKEN or GH_TOKEN (or run `gh auth login`)"
			}
		case http.StatusForbidden:
			if resp.Header.Get("X-RateLimit-Remaining") == "0" {
				ae.Hint = "API rate limit reached; a token raises it (GITHUB_TOKEN, GH_TOKEN or `gh auth login`)"
			} else {
				ae.Hint = "The token may lack the needed permission (pull requests / issues: write)"
			}
		}
		return nil, ae
	}
	return resp, nil
}

// apiMessage pulls "message" and the per-field "errors" out of an error body.
func apiMessage(data []byte) string {
	var e struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Field   string `json:"field"`
		} `json:"errors"`
	}
	if json.Unmarshal(data, &e) != nil {
		return strings.TrimSpace(string(data))
	}
	msg := e.Message
	for _, x := range e.Errors {
		d := x.Message
		if d == "" {
			d = strings.TrimSpace(x.Field + " " + x.Code)
		}
		if d != "" {
			msg += "; " + d
		}
	}
	return msg
}

func repoPath(r Repo) string {
	return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

// Label is an issue label.
type Label struct {
	Name string `json:"name"`
}

// User is a GitHub account.
type User struct {
	Login string `json:"login"`
	Type  string `json:"type"` // User, Bot, Organization
}

// Issue is a GitHub issue (pull requests are issues too; PullRequest is
// set for them).
type Issue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	HTMLURL     string    `json:"html_url"`
	Labels      []Label   `json:"labels"`
	User        User      `json:"user"`
	CreatedAt   time.Time `json:"created_at"`
	PullRequest *struct{} `json:"pull_request,omitempty"`
}

// LabelNames lists the issue's labels.
func (i Issue) LabelNames() []string {
	var out []string
	for _, l := range i.Labels {
		out = append(out, l.Name)
	}
	return out
}

// Comment is an issue comment.
type Comment struct {
	ID          int64     `json:"id"`
	Body        string    `json:"body"`
	User        User      `json:"user"`
	Association string    `json:"author_association"` // see Trusted
	CreatedAt   time.Time `json:"created_at"`
}

// Pull is a pull request.
type Pull struct {
	Number  int     `json:"number"`
	Title   string  `json:"title"`
	Body    string  `json:"body"`
	HTMLURL string  `json:"html_url"`
	Draft   bool    `json:"draft"`
	State   string  `json:"state"`  // open or closed
	Merged  bool    `json:"merged"` // set in single-pull answers only
	User    User    `json:"user"`
	Head    PullEnd `json:"head"`
	Base    PullEnd `json:"base"`
}

// PullEnd is a pull request's head or base: a branch of a repository.
type PullEnd struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo *struct {
		FullName string `json:"full_name"`
	} `json:"repo"` // nil when the head's fork was deleted
}

// NewPull is a pull request to open.
type NewPull struct {
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body"`
	Draft bool   `json:"draft"`
}

// maxPages caps paginated listings.
const maxPages = 5

// Issue reads one issue.
func (c *Client) Issue(r Repo, n int) (*Issue, error) {
	var is Issue
	if err := c.do(http.MethodGet, repoPath(r)+"/issues/"+strconv.Itoa(n), nil, &is); err != nil {
		return nil, err
	}
	return &is, nil
}

// Comments reads an issue's comments, oldest first.
func (c *Client) Comments(r Repo, n int) ([]Comment, error) {
	var all []Comment
	for page := 1; page <= maxPages; page++ {
		var cs []Comment
		if err := c.do(http.MethodGet, fmt.Sprintf("%s/issues/%d/comments?per_page=100&page=%d", repoPath(r), n, page), nil, &cs); err != nil {
			return nil, err
		}
		all = append(all, cs...)
		if len(cs) < 100 {
			break
		}
	}
	return all, nil
}

// OpenIssues lists open issues with the label, oldest first, without pull
// requests; at most max (0 = up to the page cap).
func (c *Client) OpenIssues(r Repo, label string, max int) ([]Issue, error) {
	var out []Issue
	q := url.Values{"state": {"open"}, "sort": {"created"}, "direction": {"asc"}, "per_page": {"100"}}
	if label != "" {
		q.Set("labels", label)
	}
	for page := 1; page <= maxPages; page++ {
		q.Set("page", strconv.Itoa(page))
		var is []Issue
		if err := c.do(http.MethodGet, repoPath(r)+"/issues?"+q.Encode(), nil, &is); err != nil {
			return nil, err
		}
		for _, i := range is {
			if i.PullRequest == nil {
				out = append(out, i)
			}
		}
		if len(is) < 100 || (max > 0 && len(out) >= max) {
			break
		}
	}
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out, nil
}

// OpenPulls lists open pull requests.
func (c *Client) OpenPulls(r Repo) ([]Pull, error) {
	var out []Pull
	for page := 1; page <= maxPages; page++ {
		var ps []Pull
		if err := c.do(http.MethodGet, fmt.Sprintf("%s/pulls?state=open&per_page=100&page=%d", repoPath(r), page), nil, &ps); err != nil {
			return nil, err
		}
		out = append(out, ps...)
		if len(ps) < 100 {
			break
		}
	}
	return out, nil
}

// CreatePull opens a pull request.
func (c *Client) CreatePull(r Repo, p NewPull) (*Pull, error) {
	var out Pull
	if err := c.do(http.MethodPost, repoPath(r)+"/pulls", p, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AddComment comments on an issue or pull request.
func (c *Client) AddComment(r Repo, n int, body string) error {
	return c.do(http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", repoPath(r), n), map[string]string{"body": body}, nil)
}

// EditComment replaces the text of an issue or pull request comment.
func (c *Client) EditComment(r Repo, id int64, body string) error {
	return c.do(http.MethodPatch, fmt.Sprintf("%s/issues/comments/%d", repoPath(r), id), map[string]string{"body": body}, nil)
}

// DefaultBranch is the repository's default branch.
func (c *Client) DefaultBranch(r Repo) (string, error) {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.do(http.MethodGet, repoPath(r), nil, &out); err != nil {
		return "", err
	}
	if out.DefaultBranch == "" {
		return "", errors.New("GitHub reported no default branch")
	}
	return out.DefaultBranch, nil
}

// GHCLIToken asks the gh CLI for its token; tests replace it.
var GHCLIToken = func(host string) string {
	bin, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "auth", "token", "--hostname", host)
	proc.Background(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Token finds a token for host. github.com: GITHUB_TOKEN, GH_TOKEN, then
// `gh auth token`. Any other host (GitHub Enterprise): GH_ENTERPRISE_TOKEN,
// GITHUB_ENTERPRISE_TOKEN, then `gh auth token --hostname <host>`; a
// github.com token is never sent there (as with gh itself). source says
// where it came from ("" when there is none).
func Token(host string) (token, source string) {
	vars := []string{"GITHUB_TOKEN", "GH_TOKEN"}
	if !isDotCom(host) && host != "" {
		vars = []string{"GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}
	}
	for _, v := range vars {
		if t := strings.TrimSpace(os.Getenv(v)); t != "" {
			return t, v
		}
	}
	if host == "" {
		host = "github.com"
	}
	if t := GHCLIToken(host); t != "" {
		return t, "gh auth token"
	}
	return "", ""
}
