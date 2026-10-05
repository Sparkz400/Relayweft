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

	"github.com/sparkz400/relayweft/internal/gh"
)

// gitea is the Gitea REST API v1, which Forgejo (Codeberg) serves too. Its
// issues and pull requests look like GitHub's, so gh's types read them.
//
//   - A draft is a title starting with "WIP: ".
//   - Failed checks are the commit's failed statuses (the newest per
//     context); Gitea serves no job logs, so the status's text and link
//     stand in for them.
//   - Feedback is reviews requesting changes (not dismissed) and the
//     unresolved inline comments of all reviews. An author is trusted with
//     write access, or, when the token may not read permissions, as a
//     collaborator or member of the owning organization; Gitea's system
//     users (negative ids, like the Actions bot) never are.
type gitea struct {
	*rest
	trust map[string]bool // repo + login -> trusted
}

func newGitea(api, token string, notes io.Writer) *gitea {
	return &gitea{rest: newRest(Gitea, api, token, "token", notes), trust: map[string]bool{}}
}

func gtRepo(r Repo) string { return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name) }

type gtUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

func (g *gitea) DefaultBranch(r Repo) (string, error) {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.do(http.MethodGet, gtRepo(r), nil, &out); err != nil {
		return "", err
	}
	if out.DefaultBranch == "" {
		return "", errors.New("Gitea reported no default branch")
	}
	return out.DefaultBranch, nil
}

func (g *gitea) Issue(r Repo, n int) (*Issue, error) {
	var is gh.Issue
	if err := g.do(http.MethodGet, fmt.Sprintf("%s/issues/%d", gtRepo(r), n), nil, &is); err != nil {
		return nil, err
	}
	out := ghIssue(is)
	return &out, nil
}

func (g *gitea) Comments(r Repo, n int) ([]Comment, error) {
	var cs []struct {
		ID        int64     `json:"id"`
		Body      string    `json:"body"`
		User      gtUser    `json:"user"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := g.do(http.MethodGet, fmt.Sprintf("%s/issues/%d/comments", gtRepo(r), n), nil, &cs); err != nil {
		return nil, err
	}
	var out []Comment
	for _, c := range cs {
		out = append(out, Comment{ID: c.ID, Author: c.User.Login, Body: c.Body, Created: c.CreatedAt, who: commentAuthor{id: c.User.ID}})
	}
	return out, nil
}

func (g *gitea) CommentTrusted(r Repo, c Comment) (bool, error) {
	return g.trusted(r, gtUser{ID: c.who.id, Login: c.Author})
}

// OpenIssues reads up to the page cap and sorts oldest first: Gitea lists
// newest first and cannot sort otherwise.
func (g *gitea) OpenIssues(r Repo, label string, max int) ([]Issue, error) {
	q := url.Values{"state": {"open"}, "type": {"issues"}}
	if label != "" {
		q.Set("labels", label)
	}
	is, err := pages[gh.Issue](g.rest, gtRepo(r)+"/issues?"+q.Encode(), "limit", 50)
	if err != nil {
		return nil, err
	}
	var out []Issue
	for _, i := range is {
		if i.PullRequest == nil {
			out = append(out, ghIssue(i))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].Number < out[j].Number
	})
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out, nil
}

func (g *gitea) OpenPulls(r Repo) ([]Pull, error) {
	ps, err := pages[gh.Pull](g.rest, gtRepo(r)+"/pulls?state=open", "limit", 50)
	if err != nil {
		return nil, err
	}
	var out []Pull
	for _, p := range ps {
		out = append(out, ghPull(p))
	}
	return out, nil
}

// reWIP matches Gitea's default work-in-progress prefixes.
var reWIP = regexp.MustCompile(`(?i)^\s*(wip:|\[wip\])`)

func (g *gitea) CreatePull(r Repo, p NewPull) (*Pull, error) {
	title := p.Title
	if p.Draft && !reWIP.MatchString(title) {
		title = "WIP: " + title
	}
	in := map[string]string{"head": p.Head, "base": p.Base, "title": title, "body": p.Body}
	var pr gh.Pull
	if err := g.do(http.MethodPost, gtRepo(r)+"/pulls", in, &pr); err != nil {
		return nil, err
	}
	out := ghPull(pr)
	return &out, nil
}

func (g *gitea) CommentIssue(r Repo, n int, body string) error {
	return g.do(http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", gtRepo(r), n), map[string]string{"body": body}, nil)
}

func (g *gitea) EditComment(r Repo, _ int, id int64, body string) error {
	return g.do(http.MethodPatch, fmt.Sprintf("%s/issues/comments/%d", gtRepo(r), id), map[string]string{"body": body}, nil)
}

func (g *gitea) CommentPull(r Repo, n int, body string) error { return g.CommentIssue(r, n, body) }

func (g *gitea) Pull(r Repo, n int) (*Pull, error) {
	var p gh.Pull
	if err := g.do(http.MethodGet, fmt.Sprintf("%s/pulls/%d", gtRepo(r), n), nil, &p); err != nil {
		return nil, err
	}
	out := ghPull(p)
	return &out, nil
}

func (g *gitea) PullDiff(r Repo, n int, max int64) (string, error) {
	return g.text(fmt.Sprintf("%s/pulls/%d.diff", gtRepo(r), n), max)
}

func (g *gitea) FailedChecks(r Repo, sha string, _ int) ([]Check, error) {
	type status struct {
		ID          int64  `json:"id"`
		Status      string `json:"status"` // pending, success, error, failure, warning
		Context     string `json:"context"`
		Description string `json:"description"`
		TargetURL   string `json:"target_url"`
	}
	var all []status
	for page := 1; page <= maxPages; page++ {
		var res struct {
			Statuses []status `json:"statuses"`
			Total    int      `json:"total_count"`
		}
		if err := g.do(http.MethodGet, fmt.Sprintf("%s/commits/%s/status?limit=50&page=%d", gtRepo(r), url.PathEscape(sha), page), nil, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Statuses...)
		if len(res.Statuses) < 50 || len(all) >= res.Total {
			break
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	var out []Check
	for _, s := range all {
		if s.Status != "failure" && s.Status != "error" {
			continue
		}
		out = append(out, Check{ID: strconv.FormatInt(s.ID, 10), Name: s.Context, Conclusion: s.Status,
			Output: strings.TrimSpace(s.Description + "\n" + s.TargetURL)})
	}
	return out, nil
}

// trusted: write access, else (when the token may not read permissions) a
// collaborator or a member of the owning organization; never a system user.
func (g *gitea) trusted(r Repo, u gtUser) (bool, error) {
	if u.ID < 0 || u.Login == "" {
		return false, nil
	}
	key := r.String() + "\x00" + strings.ToLower(u.Login)
	if t, ok := g.trust[key]; ok {
		return t, nil
	}
	t, err := g.lookupTrust(r, u.Login)
	if err != nil {
		return false, fmt.Errorf("read the access of %s: %w", u.Login, err)
	}
	g.trust[key] = t
	return t, nil
}

func (g *gitea) lookupTrust(r Repo, login string) (bool, error) {
	if strings.EqualFold(login, r.Owner) {
		return true, nil
	}
	var perm struct {
		Permission string `json:"permission"`
	}
	err := g.do(http.MethodGet, fmt.Sprintf("%s/collaborators/%s/permission", gtRepo(r), url.PathEscape(login)), nil, &perm)
	if err == nil {
		switch perm.Permission {
		case "write", "admin", "owner":
			return true, nil
		}
		return false, nil
	}
	if s := status(err); s != 403 && s != 404 {
		return false, err
	}
	// 204 means yes, 404 no.
	for _, p := range []string{
		fmt.Sprintf("%s/collaborators/%s", gtRepo(r), url.PathEscape(login)),
		fmt.Sprintf("/orgs/%s/members/%s", url.PathEscape(r.Owner), url.PathEscape(login)),
	} {
		err := g.do(http.MethodGet, p, nil, nil)
		if err == nil {
			return true, nil
		}
		if s := status(err); s != 403 && s != 404 {
			return false, err
		}
	}
	return false, nil
}

func (g *gitea) Feedback(r Repo, n int) ([]Feedback, error) {
	reviews, err := pages[struct {
		ID            int64  `json:"id"`
		User          gtUser `json:"user"`
		Body          string `json:"body"`
		State         string `json:"state"` // APPROVED, REQUEST_CHANGES, COMMENT, PENDING, REQUEST_REVIEW
		Dismissed     bool   `json:"dismissed"`
		CommentsCount int    `json:"comments_count"`
	}](g.rest, fmt.Sprintf("%s/pulls/%d/reviews", gtRepo(r), n), "limit", 50)
	if err != nil {
		return nil, fmt.Errorf("read reviews: %w", err)
	}
	type numbered struct {
		id int64
		f  Feedback
	}
	var out []Feedback
	var comments []numbered
	for _, rv := range reviews {
		if rv.State == "PENDING" || rv.Dismissed {
			continue
		}
		if rv.State == "REQUEST_CHANGES" {
			ok, err := g.trusted(r, rv.User)
			if err != nil {
				return nil, err
			}
			out = append(out, Feedback{ID: fmt.Sprintf("review:%d", rv.ID), Review: true, Author: rv.User.Login, Trusted: ok, Body: rv.Body})
		}
		if rv.CommentsCount == 0 {
			continue
		}
		var cs []struct {
			ID       int64   `json:"id"`
			User     gtUser  `json:"user"`
			Body     string  `json:"body"`
			Path     string  `json:"path"`
			Position int     `json:"position"`
			DiffHunk string  `json:"diff_hunk"`
			Resolver *gtUser `json:"resolver"` // set once resolved
		}
		if err := g.do(http.MethodGet, fmt.Sprintf("%s/pulls/%d/reviews/%d/comments", gtRepo(r), n, rv.ID), nil, &cs); err != nil {
			return nil, fmt.Errorf("read review comments: %w", err)
		}
		for _, c := range cs {
			if c.Resolver != nil {
				continue
			}
			ok, err := g.trusted(r, c.User)
			if err != nil {
				return nil, err
			}
			comments = append(comments, numbered{c.ID, Feedback{ID: fmt.Sprintf("comment:%d", c.ID), Author: c.User.Login, Trusted: ok,
				Body: c.Body, Path: c.Path, Line: c.Position, DiffHunk: c.DiffHunk}})
		}
	}
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].id < comments[j].id })
	for _, c := range comments {
		out = append(out, c.f)
	}
	return out, nil
}

func (g *gitea) Viewer() (string, error) {
	var u gtUser
	if err := g.do(http.MethodGet, "/user", nil, &u); err != nil {
		return "", err
	}
	if u.Login == "" {
		return "", errors.New("Gitea reported no login for the token")
	}
	return u.Login, nil
}

func (g *gitea) CommentReview(r Repo, n int, headSHA, body string, comments []InlineComment) (string, error) {
	type comment struct {
		Path        string `json:"path"`
		Body        string `json:"body"`
		NewPosition int    `json:"new_position"`
	}
	in := struct {
		Body     string    `json:"body"`
		Event    string    `json:"event"`
		CommitID string    `json:"commit_id,omitempty"`
		Comments []comment `json:"comments,omitempty"`
	}{Body: body, Event: "COMMENT", CommitID: headSHA}
	for _, c := range comments {
		in.Comments = append(in.Comments, comment{Path: c.Path, Body: c.Body, NewPosition: c.Line})
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if err := g.do(http.MethodPost, fmt.Sprintf("%s/pulls/%d/reviews", gtRepo(r), n), in, &out); err != nil {
		return "", err
	}
	if out.HTMLURL == "" {
		out.HTMLURL = fmt.Sprintf("%s/pulls/%d", r.WebURL(), n)
	}
	return out.HTMLURL, nil
}
