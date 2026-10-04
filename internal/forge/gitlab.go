package forge

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// gitlab is the GitLab REST API v4 (gitlab.com and self-managed):
//
//   - A merge request is opened from a branch of the same project; a draft
//     is a title starting with "Draft: ".
//   - Failed checks are the failed jobs (not allowed to fail) of the newest
//     pipeline per ref on the commit, with the tail of each job's log.
//   - Feedback is the unresolved diff comments (GitLab has no API for a
//     review's "request changes" state). An author is trusted with at least
//     Developer access to the project, and project or group access token
//     bots never are.
//   - A comment review is one discussion per inline comment plus one note
//     with the text; a comment GitLab cannot place on its line goes into
//     the note.
type gitlab struct {
	*rest
	access map[string]int // project + user id -> access level
}

func newGitLab(api, token string, notes io.Writer) *gitlab {
	return &gitlab{rest: newRest(GitLab, api, token, "Bearer", notes), access: map[string]int{}}
}

func glProject(r Repo) string { return "/projects/" + url.PathEscape(r.Owner+"/"+r.Name) }

type glUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

type glIssue struct {
	IID         int       `json:"iid"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	State       string    `json:"state"` // opened, closed
	WebURL      string    `json:"web_url"`
	Labels      []string  `json:"labels"`
	Author      glUser    `json:"author"`
	CreatedAt   time.Time `json:"created_at"`
}

func (i glIssue) issue() Issue {
	return Issue{Number: i.IID, Title: i.Title, Body: i.Description, State: glState(i.State), URL: i.WebURL,
		Labels: i.Labels, Author: i.Author.Username, Created: i.CreatedAt}
}

// glState maps GitLab's states onto open and closed.
func glState(s string) string {
	switch s {
	case "opened", "locked":
		return "open"
	}
	return "closed"
}

type glMR struct {
	IID             int    `json:"iid"`
	Title           string `json:"title"`
	Description     string `json:"description"`
	State           string `json:"state"` // opened, closed, merged, locked
	WebURL          string `json:"web_url"`
	Draft           bool   `json:"draft"`
	SourceBranch    string `json:"source_branch"`
	TargetBranch    string `json:"target_branch"`
	SHA             string `json:"sha"`
	SourceProjectID int64  `json:"source_project_id"`
	TargetProjectID int64  `json:"target_project_id"`
	DiffRefs        *struct {
		BaseSHA  string `json:"base_sha"`
		HeadSHA  string `json:"head_sha"`
		StartSHA string `json:"start_sha"`
	} `json:"diff_refs"`
}

func (m glMR) pull(r Repo) Pull {
	p := Pull{Number: m.IID, Title: m.Title, Body: m.Description, URL: m.WebURL, State: glState(m.State),
		Merged: m.State == "merged", Draft: m.Draft, HeadRef: m.SourceBranch, HeadSHA: m.SHA, BaseRef: m.TargetBranch}
	if m.SourceProjectID != 0 && m.SourceProjectID == m.TargetProjectID {
		p.HeadRepo = r.String()
	}
	return p
}

func (g *gitlab) DefaultBranch(r Repo) (string, error) {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.do(http.MethodGet, glProject(r), nil, &out); err != nil {
		return "", err
	}
	if out.DefaultBranch == "" {
		return "", errors.New("GitLab reported no default branch")
	}
	return out.DefaultBranch, nil
}

func (g *gitlab) Issue(r Repo, n int) (*Issue, error) {
	var is glIssue
	if err := g.do(http.MethodGet, fmt.Sprintf("%s/issues/%d", glProject(r), n), nil, &is); err != nil {
		return nil, err
	}
	out := is.issue()
	return &out, nil
}

type glNote struct {
	ID         int64     `json:"id"`
	Type       string    `json:"type"` // DiffNote, DiscussionNote or empty
	Body       string    `json:"body"`
	Author     glUser    `json:"author"`
	System     bool      `json:"system"`
	Resolvable bool      `json:"resolvable"`
	Resolved   bool      `json:"resolved"`
	CreatedAt  time.Time `json:"created_at"`
	Position   *struct {
		NewPath string `json:"new_path"`
		OldPath string `json:"old_path"`
		NewLine int    `json:"new_line"`
	} `json:"position"`
}

func (g *gitlab) Comments(r Repo, n int) ([]Comment, error) {
	notes, err := pages[glNote](g.rest, fmt.Sprintf("%s/issues/%d/notes?sort=asc&order_by=created_at", glProject(r), n), "per_page", 100)
	if err != nil {
		return nil, err
	}
	var out []Comment
	for _, nt := range notes {
		if !nt.System {
			out = append(out, Comment{ID: nt.ID, Author: nt.Author.Username, Body: nt.Body, Created: nt.CreatedAt,
				who: commentAuthor{id: nt.Author.ID, bot: nt.Author.Bot}})
		}
	}
	return out, nil
}

func (g *gitlab) CommentTrusted(r Repo, c Comment) (bool, error) {
	return g.trusted(r, glUser{ID: c.who.id, Username: c.Author, Bot: c.who.bot})
}

func (g *gitlab) OpenIssues(r Repo, label string, max int) ([]Issue, error) {
	q := url.Values{"state": {"opened"}, "order_by": {"created_at"}, "sort": {"asc"}}
	if label != "" {
		q.Set("labels", label)
	}
	var out []Issue
	for page := 1; page <= maxPages; page++ {
		var is []glIssue
		if err := g.do(http.MethodGet, fmt.Sprintf("%s/issues?%s&per_page=100&page=%d", glProject(r), q.Encode(), page), nil, &is); err != nil {
			return nil, err
		}
		for _, i := range is {
			out = append(out, i.issue())
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

func (g *gitlab) OpenPulls(r Repo) ([]Pull, error) {
	ms, err := pages[glMR](g.rest, glProject(r)+"/merge_requests?state=opened", "per_page", 100)
	if err != nil {
		return nil, err
	}
	var out []Pull
	for _, m := range ms {
		out = append(out, m.pull(r))
	}
	return out, nil
}

// reDraft matches the title prefixes GitLab reads as "draft".
var reDraft = regexp.MustCompile(`(?i)^\s*(\[draft\]|\(draft\)|draft:|draft\s-|\[wip\]|wip:)`)

func (g *gitlab) CreatePull(r Repo, p NewPull) (*Pull, error) {
	title := p.Title
	if p.Draft && !reDraft.MatchString(title) {
		title = "Draft: " + title
	}
	in := map[string]any{"source_branch": p.Head, "target_branch": p.Base, "title": title, "description": p.Body}
	var m glMR
	if err := g.do(http.MethodPost, glProject(r)+"/merge_requests", in, &m); err != nil {
		return nil, err
	}
	out := m.pull(r)
	return &out, nil
}

func (g *gitlab) CommentIssue(r Repo, n int, body string) error {
	return g.do(http.MethodPost, fmt.Sprintf("%s/issues/%d/notes", glProject(r), n), map[string]string{"body": body}, nil)
}

func (g *gitlab) EditComment(r Repo, n int, id int64, body string) error {
	return g.do(http.MethodPut, fmt.Sprintf("%s/issues/%d/notes/%d", glProject(r), n, id), map[string]string{"body": body}, nil)
}

func (g *gitlab) CommentPull(r Repo, n int, body string) error {
	return g.do(http.MethodPost, fmt.Sprintf("%s/merge_requests/%d/notes", glProject(r), n), map[string]string{"body": body}, nil)
}

func (g *gitlab) mr(r Repo, n int) (*glMR, error) {
	var m glMR
	if err := g.do(http.MethodGet, fmt.Sprintf("%s/merge_requests/%d", glProject(r), n), nil, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (g *gitlab) Pull(r Repo, n int) (*Pull, error) {
	m, err := g.mr(r, n)
	if err != nil {
		return nil, err
	}
	out := m.pull(r)
	return &out, nil
}

type glDiff struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	Diff        string `json:"diff"`
	NewFile     bool   `json:"new_file"`
	DeletedFile bool   `json:"deleted_file"`
}

// PullDiff rebuilds a unified diff from the merge request's per-file
// diffs (/diffs since GitLab 15.7, else /changes).
func (g *gitlab) PullDiff(r Repo, n int, max int64) (string, error) {
	base := fmt.Sprintf("%s/merge_requests/%d", glProject(r), n)
	var files []glDiff
	var size int64
	for page := 1; ; page++ {
		var xs []glDiff
		err := g.do(http.MethodGet, fmt.Sprintf("%s/diffs?per_page=100&page=%d", base, page), nil, &xs)
		if IsNotFound(err) && page == 1 {
			var ch struct {
				Changes []glDiff `json:"changes"`
			}
			if err := g.do(http.MethodGet, base+"/changes", nil, &ch); err != nil {
				return "", err
			}
			files = ch.Changes
			break
		}
		if err != nil {
			return "", err
		}
		files = append(files, xs...)
		for _, f := range xs {
			size += int64(len(f.Diff))
		}
		if size > max {
			// Stop reading early: the diff is too large either way.
			return "", fmt.Errorf("GitLab GET %s/diffs: %w (over %d bytes)", base, ErrTooLarge, max)
		}
		if len(xs) < 100 {
			break
		}
		if page == 100 {
			return "", fmt.Errorf("GitLab GET %s/diffs: %w (over 10000 files)", base, ErrTooLarge)
		}
	}
	var b strings.Builder
	for _, f := range files {
		from, to := "a/"+f.OldPath, "b/"+f.NewPath
		fmt.Fprintf(&b, "diff --git %s %s\n", from, to)
		if f.NewFile {
			from = "/dev/null"
		}
		if f.DeletedFile {
			to = "/dev/null"
		}
		if f.Diff == "" {
			fmt.Fprintf(&b, "Binary files %s and %s differ\n", from, to)
		} else {
			fmt.Fprintf(&b, "--- %s\n+++ %s\n%s", from, to, f.Diff)
			if !strings.HasSuffix(f.Diff, "\n") {
				b.WriteString("\n")
			}
		}
		if int64(b.Len()) > max {
			return "", fmt.Errorf("GitLab GET %s/diffs: %w (over %d bytes)", base, ErrTooLarge, max)
		}
	}
	return b.String(), nil
}

func (g *gitlab) FailedChecks(r Repo, sha string, logTail int) ([]Check, error) {
	var pipes []struct {
		ID  int64  `json:"id"`
		Ref string `json:"ref"`
	}
	if err := g.do(http.MethodGet, fmt.Sprintf("%s/pipelines?sha=%s&per_page=100", glProject(r), url.QueryEscape(sha)), nil, &pipes); err != nil {
		return nil, err
	}
	// The newest pipeline of each ref (a branch pipeline and a merge
	// request pipeline can both run the commit); older ones were re-run.
	sort.Slice(pipes, func(i, j int) bool { return pipes[i].ID > pipes[j].ID })
	seen := map[string]bool{}
	var out []Check
	for _, p := range pipes {
		if seen[p.Ref] {
			continue
		}
		seen[p.Ref] = true
		jobs, err := pages[struct {
			ID            int64  `json:"id"`
			Name          string `json:"name"`
			Stage         string `json:"stage"`
			Status        string `json:"status"`
			AllowFailure  bool   `json:"allow_failure"`
			FailureReason string `json:"failure_reason"`
			WebURL        string `json:"web_url"`
		}](g.rest, fmt.Sprintf("%s/pipelines/%d/jobs?scope%%5B%%5D=failed", glProject(r), p.ID), "per_page", 100)
		if err != nil {
			return nil, err
		}
		sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
		for _, j := range jobs {
			if j.Status != "failed" || j.AllowFailure {
				continue
			}
			c := Check{ID: strconv.FormatInt(j.ID, 10), Name: j.Name, Conclusion: "failed"}
			if j.Stage != "" {
				c.Name = j.Stage + ": " + j.Name
			}
			if j.FailureReason != "" {
				c.Conclusion += " (" + j.FailureReason + ")"
			}
			// Read more than the tail: the trace's markup is dropped first.
			if log, err := g.tailOf(fmt.Sprintf("%s/jobs/%d/trace", glProject(r), j.ID), 4*logTail); err == nil {
				c.Log = lineTail(cleanGLTrace(log), logTail)
			}
			out = append(out, c)
		}
	}
	return out, nil
}

// GitLab job trace markup: GitLab Runner (with timestamps, the default in
// Runner 19) starts every line with "<UTC time> <stream><O|E>[+] ", and
// collapsible sections are framed by section_start:<time>:<name>[options]
// and section_end:<time>:<name>, each followed by "\r\x1b[0K".
var (
	reGLTraceStamp = regexp.MustCompile(`(?m)^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z [0-9a-fA-F]{2}[OE]\+? ?`)
	reGLSection    = regexp.MustCompile(`section_(?:start|end):\d+:[A-Za-z0-9_.-]+(?:\[[^\]\r\n]*\])?\r?(?:\x1b\[0K)*`)
)

// cleanGLTrace drops the timestamps and section markers from a job trace,
// and the lines that held only a marker, so the log tail is the job's own
// output.
func cleanGLTrace(s string) string {
	s = reGLTraceStamp.ReplaceAllString(s, "")
	lines := strings.SplitAfter(s, "\n")
	out := lines[:0]
	for _, l := range lines {
		if c := reGLSection.ReplaceAllString(l, ""); c != l {
			if strings.TrimSpace(c) == "" {
				continue
			}
			l = c
		}
		out = append(out, l)
	}
	return strings.Join(out, "")
}

// reGLBot matches the users of project and group access tokens.
var reGLBot = regexp.MustCompile(`^(project|group)_\d+_bot(_[0-9a-f]+)?$`)

// trusted: at least Developer access (30) to the project, not a bot.
func (g *gitlab) trusted(r Repo, u glUser) (bool, error) {
	if u.Bot || reGLBot.MatchString(u.Username) || u.ID == 0 {
		return false, nil
	}
	key := r.String() + "\x00" + strconv.FormatInt(u.ID, 10)
	lvl, ok := g.access[key]
	if !ok {
		var m struct {
			AccessLevel int  `json:"access_level"`
			Bot         bool `json:"bot"`
		}
		err := g.do(http.MethodGet, fmt.Sprintf("%s/members/all/%d", glProject(r), u.ID), nil, &m)
		switch {
		case IsNotFound(err):
			lvl = 0
		case err != nil:
			return false, fmt.Errorf("read the access of %s: %w", u.Username, err)
		case m.Bot:
			lvl = 0
		default:
			lvl = m.AccessLevel
		}
		g.access[key] = lvl
	}
	return lvl >= 30, nil
}

func (g *gitlab) Feedback(r Repo, n int) ([]Feedback, error) {
	ds, err := pages[struct {
		Notes []glNote `json:"notes"`
	}](g.rest, fmt.Sprintf("%s/merge_requests/%d/discussions", glProject(r), n), "per_page", 100)
	if err != nil {
		return nil, fmt.Errorf("read discussions: %w", err)
	}
	var notes []glNote
	for _, d := range ds {
		for _, nt := range d.Notes {
			if nt.Type == "DiffNote" && !nt.System && !(nt.Resolvable && nt.Resolved) {
				notes = append(notes, nt)
			}
		}
	}
	sort.Slice(notes, func(i, j int) bool { return notes[i].ID < notes[j].ID })
	var out []Feedback
	for _, nt := range notes {
		ok, err := g.trusted(r, nt.Author)
		if err != nil {
			return nil, err
		}
		f := Feedback{ID: fmt.Sprintf("comment:%d", nt.ID), Author: nt.Author.Username, Trusted: ok, Body: nt.Body}
		if p := nt.Position; p != nil {
			f.Path, f.Line = p.NewPath, p.NewLine
			if f.Path == "" {
				f.Path = p.OldPath
			}
		}
		out = append(out, f)
	}
	return out, nil
}

func (g *gitlab) Viewer() (string, error) {
	var u glUser
	if err := g.do(http.MethodGet, "/user", nil, &u); err != nil {
		return "", err
	}
	if u.Username == "" {
		return "", errors.New("GitLab reported no user for the token")
	}
	return u.Username, nil
}

func (g *gitlab) CommentReview(r Repo, n int, headSHA, body string, comments []InlineComment) (string, error) {
	m, err := g.mr(r, n)
	if err != nil {
		return "", err
	}
	if m.DiffRefs == nil || m.DiffRefs.HeadSHA == "" {
		return "", fmt.Errorf("GitLab reported no diff refs for %s", r.Ref(n))
	}
	if headSHA != "" && m.DiffRefs.HeadSHA != headSHA {
		return "", fmt.Errorf("%s has new commits since its diff was read; review it again", r.Ref(n))
	}
	var missed []InlineComment
	for _, c := range comments {
		pos := map[string]any{"position_type": "text", "base_sha": m.DiffRefs.BaseSHA, "start_sha": m.DiffRefs.StartSHA,
			"head_sha": m.DiffRefs.HeadSHA, "new_path": c.Path, "old_path": c.Path, "new_line": c.Line}
		if c.OldLine > 0 {
			pos["old_line"] = c.OldLine
		}
		err := g.do(http.MethodPost, fmt.Sprintf("%s/merge_requests/%d/discussions", glProject(r), n), map[string]any{"body": c.Body, "position": pos}, nil)
		if s := status(err); s == 401 || s == 403 || (err != nil && s == 0) {
			return "", err
		}
		if err != nil {
			missed = append(missed, c) // GitLab could not place it on the line
		}
	}
	if len(missed) > 0 {
		var b strings.Builder
		b.WriteString(body)
		b.WriteString("\n\nComments GitLab could not place on their line:\n\n")
		for _, c := range missed {
			fmt.Fprintf(&b, "- `%s:%d`: %s\n", strings.ReplaceAll(c.Path, "`", "'"), c.Line, strings.ReplaceAll(c.Body, "\n", "\n  "))
		}
		body = b.String()
	}
	var note struct {
		ID int64 `json:"id"`
	}
	if err := g.do(http.MethodPost, fmt.Sprintf("%s/merge_requests/%d/notes", glProject(r), n), map[string]string{"body": body}, &note); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s#note_%d", m.WebURL, note.ID), nil
}
