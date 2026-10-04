package forge

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/sparkz400/switchyard/internal/gh"
)

// githubClient is GitHub through package gh.
type githubClient struct{ c *gh.Client }

func (g *githubClient) Kind() Kind     { return GitHub }
func (g *githubClient) HasToken() bool { return g.c.HasToken() }
func (g *githubClient) Rejected() bool { return g.c.Rejected() }

func (g *githubClient) DefaultBranch(r Repo) (string, error) { return g.c.DefaultBranch(r.gh()) }

func ghIssue(i gh.Issue) Issue {
	return Issue{Number: i.Number, Title: i.Title, Body: i.Body, State: i.State, URL: i.HTMLURL, Labels: i.LabelNames(),
		Author: i.User.Login, Created: i.CreatedAt, IsPull: i.PullRequest != nil}
}

func ghPull(p gh.Pull) Pull {
	out := Pull{Number: p.Number, Title: p.Title, Body: p.Body, URL: p.HTMLURL, State: p.State, Merged: p.Merged, Draft: p.Draft,
		HeadRef: p.Head.Ref, HeadSHA: p.Head.SHA, BaseRef: p.Base.Ref}
	if p.Head.Repo != nil {
		out.HeadRepo = p.Head.Repo.FullName
	}
	return out
}

func (g *githubClient) Issue(r Repo, n int) (*Issue, error) {
	i, err := g.c.Issue(r.gh(), n)
	if err != nil {
		return nil, err
	}
	out := ghIssue(*i)
	return &out, nil
}

func (g *githubClient) Comments(r Repo, n int) ([]Comment, error) {
	cs, err := g.c.Comments(r.gh(), n)
	if err != nil {
		return nil, err
	}
	var out []Comment
	for _, c := range cs {
		out = append(out, Comment{Author: c.User.Login, Body: c.Body})
	}
	return out, nil
}

func (g *githubClient) OpenIssues(r Repo, label string, max int) ([]Issue, error) {
	is, err := g.c.OpenIssues(r.gh(), label, max)
	if err != nil {
		return nil, err
	}
	var out []Issue
	for _, i := range is {
		out = append(out, ghIssue(i))
	}
	return out, nil
}

func (g *githubClient) OpenPulls(r Repo) ([]Pull, error) {
	ps, err := g.c.OpenPulls(r.gh())
	if err != nil {
		return nil, err
	}
	var out []Pull
	for _, p := range ps {
		out = append(out, ghPull(p))
	}
	return out, nil
}

func (g *githubClient) CreatePull(r Repo, p NewPull) (*Pull, error) {
	pr, err := g.c.CreatePull(r.gh(), gh.NewPull{Title: p.Title, Head: p.Head, Base: p.Base, Body: p.Body, Draft: p.Draft})
	if err != nil {
		return nil, err
	}
	out := ghPull(*pr)
	return &out, nil
}

func (g *githubClient) CommentIssue(r Repo, n int, body string) error {
	return g.c.AddComment(r.gh(), n, body)
}

func (g *githubClient) CommentPull(r Repo, n int, body string) error {
	return g.c.AddComment(r.gh(), n, body)
}

func (g *githubClient) Pull(r Repo, n int) (*Pull, error) {
	p, err := g.c.Pull(r.gh(), n)
	if err != nil {
		return nil, err
	}
	out := ghPull(*p)
	return &out, nil
}

func (g *githubClient) PullDiff(r Repo, n int, max int64) (string, error) {
	return g.c.PullDiff(r.gh(), n, max)
}

func (g *githubClient) FailedChecks(r Repo, sha string, logTail int) ([]Check, error) {
	runs, err := g.c.CheckRuns(r.gh(), sha)
	if err != nil {
		return nil, err
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].ID < runs[j].ID })
	var out []Check
	for _, cr := range runs {
		if !cr.Failed() || cr.HeadSHA != "" && cr.HeadSHA != sha {
			continue
		}
		c := Check{ID: strconv.FormatInt(cr.ID, 10), Name: cr.Name, Conclusion: cr.Conclusion,
			Output: strings.TrimSpace(cr.Output.Title + "\n" + cr.Output.Summary + "\n" + cr.Output.Text)}
		if cr.Actions() {
			// The log is a bonus: a check without one is still reported.
			if log, err := g.c.JobLogTail(r.gh(), cr.ID, logTail+1); err == nil {
				c.Log = lineTail(log, logTail)
			}
		}
		out = append(out, c)
	}
	return out, nil
}

func (g *githubClient) Feedback(r Repo, n int) ([]Feedback, error) {
	var out []Feedback
	reviews, err := g.c.Reviews(r.gh(), n)
	if err != nil {
		return nil, fmt.Errorf("read reviews: %w", err)
	}
	for _, rv := range reviews {
		if rv.State != "CHANGES_REQUESTED" {
			continue
		}
		out = append(out, Feedback{ID: fmt.Sprintf("review:%d", rv.ID), Review: true, Author: rv.User.Login,
			Trusted: gh.Trusted(rv.User, rv.Association), Body: rv.Body})
	}
	comments, err := g.c.ReviewComments(r.gh(), n)
	if err != nil {
		return nil, fmt.Errorf("read review comments: %w", err)
	}
	for _, cm := range comments {
		out = append(out, Feedback{ID: fmt.Sprintf("comment:%d", cm.ID), Author: cm.User.Login,
			Trusted: gh.Trusted(cm.User, cm.Association), Body: cm.Body, Path: cm.Path, Line: cm.Line, DiffHunk: cm.DiffHunk})
	}
	return out, nil
}

func (g *githubClient) Viewer() (string, error) { return g.c.Viewer() }

func (g *githubClient) CommentReview(r Repo, n int, headSHA, body string, comments []InlineComment) (string, error) {
	var ics []gh.InlineComment
	for _, c := range comments {
		ics = append(ics, gh.InlineComment{Path: c.Path, Line: c.Line, Body: c.Body})
	}
	rv, err := g.c.CommentReview(r.gh(), n, headSHA, body, ics)
	if err != nil {
		return "", err
	}
	return rv.HTMLURL, nil
}
