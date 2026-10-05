package forge

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// bitbucket is Bitbucket Cloud's REST API 2.0 (bitbucket.org, served at
// api.bitbucket.org/2.0). Bitbucket Data Center and Server have another
// API and are not supported.
//
//   - The token is an access token (sent as Bearer) or "<email>:<API
//     token>" (sent as Basic), see Token.
//   - People have no stable login name, so an author is their account
//     UUID ("{0f3c...}"): nobody can pass for the token's owner or a
//     reviewer by choosing a name.
//   - The issue tracker is optional per repository and has no labels:
//     label x is the component x, and open issues are those in state new
//     or open. As anyone who files an issue may be able to set its
//     component, an issue counts only when its reporter, or whoever set
//     the component last, may direct work on the repository. A repository
//     without a tracker (Jira instead) gets a clear error.
//   - A draft is Bitbucket's draft flag.
//   - Failed checks are the failed steps of the newest pipeline per target
//     on the commit, with each step's log tail, plus the failed commit
//     statuses other CI posted.
//   - Feedback is reviewers requesting changes (one item per reviewer and
//     pull request) and unresolved inline comments. An author is trusted
//     with write access to the repository, or, when the token may read no
//     permissions, as a member of the workspace; apps never are.
//   - A comment review is one comment per inline finding plus one comment
//     with the text; a finding Bitbucket cannot place on its line goes into
//     the text.
type bitbucket struct {
	*rest
	trust map[string]bool // repo + account UUID -> trusted
}

// bitbucketAPI is Bitbucket Cloud's API.
const bitbucketAPI = "https://api.bitbucket.org/2.0"

// isBitbucketCloud reports whether host is bitbucket.org.
func isBitbucketCloud(host string) bool {
	h := hostName(host)
	return h == "bitbucket.org" || h == "www.bitbucket.org"
}

func newBitbucket(api, token string, notes io.Writer) *bitbucket {
	scheme := "Bearer"
	if strings.Contains(token, ":") {
		// <email>:<API token> (or a username and an app password).
		scheme, token = "Basic", base64.StdEncoding.EncodeToString([]byte(token))
	}
	return &bitbucket{rest: newRest(Bitbucket, api, token, scheme, notes), trust: map[string]bool{}}
}

func bbRepo(r Repo) string {
	return "/repositories/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

type bbUser struct {
	Type string `json:"type"` // user, app_user, team
	UUID string `json:"uuid"`
}

// reBBUUID matches an account UUID as Bitbucket writes it.
var reBBUUID = regexp.MustCompile(`^\{[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\}$`)

// login is the account UUID, in lower case: the one thing that tells
// accounts apart (names change, and anyone may pick anyone's). "" without
// a valid UUID.
func (u bbUser) login() string {
	if !reBBUUID.MatchString(u.UUID) {
		return ""
	}
	return strings.ToLower(u.UUID)
}

type bbLinks struct {
	HTML struct {
		Href string `json:"href"`
	} `json:"html"`
}

type bbText struct {
	Raw string `json:"raw"`
}

type bbName struct {
	Name string `json:"name"`
}

type bbIssue struct {
	ID        int       `json:"id"`
	Title     string    `json:"title"`
	Content   bbText    `json:"content"`
	State     string    `json:"state"` // new, open, on hold, submitted, resolved, invalid, duplicate, wontfix, closed
	Kind      string    `json:"kind"`  // bug, enhancement, proposal, task
	Component *bbName   `json:"component"`
	Reporter  *bbUser   `json:"reporter"`
	CreatedOn time.Time `json:"created_on"`
	Links     bbLinks   `json:"links"`
}

func (i bbIssue) issue() Issue {
	out := Issue{Number: i.ID, Title: i.Title, Body: i.Content.Raw, State: "closed", URL: i.Links.HTML.Href, Created: i.CreatedOn}
	switch i.State {
	case "new", "open", "on hold", "submitted":
		out.State = "open"
	}
	if i.Component != nil && i.Component.Name != "" {
		out.Labels = append(out.Labels, i.Component.Name)
	}
	if i.Kind != "" {
		out.Labels = append(out.Labels, i.Kind)
	}
	if i.Reporter != nil {
		out.Author = i.Reporter.login()
	}
	return out
}

type bbRef struct {
	Branch bbName `json:"branch"`
	Commit *struct {
		Hash string `json:"hash"`
	} `json:"commit"`
	Repository *struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type bbPR struct {
	ID           int     `json:"id"`
	Title        string  `json:"title"`
	Description  string  `json:"description"`
	State        string  `json:"state"` // OPEN, MERGED, DECLINED, SUPERSEDED
	Draft        bool    `json:"draft"`
	Links        bbLinks `json:"links"`
	Source       bbRef   `json:"source"`
	Destination  bbRef   `json:"destination"`
	Participants []struct {
		User           bbUser `json:"user"`
		State          string `json:"state"` // approved, changes_requested or null
		ParticipatedOn string `json:"participated_on"`
	} `json:"participants"`
}

func (p bbPR) pull(r Repo) Pull {
	out := Pull{Number: p.ID, Title: p.Title, Body: p.Description, URL: p.Links.HTML.Href, State: "closed",
		Merged: p.State == "MERGED", Draft: p.Draft, HeadRef: p.Source.Branch.Name, BaseRef: p.Destination.Branch.Name}
	if p.State == "OPEN" {
		out.State = "open"
	}
	if out.URL == "" {
		out.URL = fmt.Sprintf("%s/pull-requests/%d", r.WebURL(), p.ID)
	}
	if p.Source.Commit != nil {
		out.HeadSHA = p.Source.Commit.Hash
	}
	if p.Source.Repository != nil {
		out.HeadRepo = p.Source.Repository.FullName
	}
	return out
}

type bbComment struct {
	ID        int64     `json:"id"`
	Content   bbText    `json:"content"`
	User      bbUser    `json:"user"`
	CreatedOn time.Time `json:"created_on"`
	Deleted   bool      `json:"deleted"`
	Pending   bool      `json:"pending"` // a draft nobody else sees yet
	Inline    *struct {
		Path string `json:"path"`
		To   *int   `json:"to"` // the line in the new version; null on an old line
	} `json:"inline"`
	Parent *struct {
		ID int64 `json:"id"`
	} `json:"parent"`
	Resolution *struct {
		Type string `json:"type"`
	} `json:"resolution"` // set on the first comment of a resolved thread
}

// bbPages GETs a paged listing (per values a page), following Bitbucket's
// next links on this API only, for up to maxPages pages; stop > 0 ends it
// once that many values are in.
func bbPages[T any](b *bitbucket, path string, per, stop int) ([]T, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	next := fmt.Sprintf("%s%spagelen=%d", path, sep, per)
	var all []T
	for page := 1; page <= maxPages; page++ {
		var res struct {
			Values []T    `json:"values"`
			Next   string `json:"next"`
		}
		if err := b.do(http.MethodGet, next, nil, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Values...)
		if res.Next == "" || (stop > 0 && len(all) >= stop) {
			break
		}
		p, err := b.local(res.Next)
		if err != nil {
			return nil, err
		}
		next = p
	}
	return all, nil
}

// local turns a link Bitbucket gave (a next page) into a path on this
// client's API. A link anywhere else is refused, so the token never
// follows one off the API.
func (b *bitbucket) local(link string) (string, error) {
	u, err := url.Parse(link)
	base, berr := url.Parse(b.base)
	if err != nil || berr != nil || u.User != nil || !strings.EqualFold(u.Scheme, base.Scheme) || !strings.EqualFold(u.Host, base.Host) ||
		!strings.HasPrefix(u.EscapedPath(), base.EscapedPath()+"/") {
		return "", fmt.Errorf("the next page Bitbucket named is not on its API (%s): %q", b.base, link)
	}
	p := strings.TrimPrefix(u.EscapedPath(), base.EscapedPath())
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p, nil
}

func (b *bitbucket) DefaultBranch(r Repo) (string, error) {
	var out struct {
		MainBranch *bbName `json:"mainbranch"`
	}
	if err := b.do(http.MethodGet, bbRepo(r), nil, &out); err != nil {
		return "", err
	}
	if out.MainBranch == nil || out.MainBranch.Name == "" {
		return "", fmt.Errorf("no main branch for %s on Bitbucket", r)
	}
	return out.MainBranch.Name, nil
}

// issues wraps a 404 from the issue tracker: a repository without one
// (it may use Jira) says so.
func (b *bitbucket) issues(r Repo, err error) error {
	if !IsNotFound(err) {
		return err
	}
	var repo struct {
		HasIssues *bool `json:"has_issues"`
	}
	if b.do(http.MethodGet, bbRepo(r), nil, &repo) == nil && repo.HasIssues != nil && !*repo.HasIssues {
		return fmt.Errorf("%s has no Bitbucket issue tracker (it may use Jira instead); rw reads issues from Bitbucket's own tracker only, which a repository admin turns on under Repository settings > Features: %w", r, err)
	}
	return err
}

func (b *bitbucket) Issue(r Repo, n int) (*Issue, error) {
	var is bbIssue
	if err := b.do(http.MethodGet, fmt.Sprintf("%s/issues/%d", bbRepo(r), n), nil, &is); err != nil {
		return nil, b.issues(r, err)
	}
	out := is.issue()
	return &out, nil
}

// Comments are the issue's newest comments (up to the page cap), oldest
// first: a flood of old comments cannot hide a new team claim.
func (b *bitbucket) Comments(r Repo, n int) ([]Comment, error) {
	cs, err := bbPages[bbComment](b, fmt.Sprintf("%s/issues/%d/comments?sort=-created_on", bbRepo(r), n), 100, 0)
	if err != nil {
		return nil, b.issues(r, err)
	}
	var out []Comment
	for _, c := range cs {
		// A state change without text is listed as an empty comment.
		if strings.TrimSpace(c.Content.Raw) == "" {
			continue
		}
		out = append(out, Comment{ID: c.ID, Author: c.User.login(), Body: c.Content.Raw, Created: c.CreatedOn,
			who: commentAuthor{typ: c.User.Type, uuid: c.User.UUID}})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (b *bitbucket) CommentTrusted(r Repo, c Comment) (bool, error) {
	return b.trusted(r, bbUser{Type: c.who.typ, UUID: c.who.uuid})
}

// bbqlString quotes s for a Bitbucket query (BBQL).
func bbqlString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// OpenIssues are the issues in state new or open whose component is label
// (Bitbucket issues have no labels), oldest first. With a label, an issue
// counts only when queued says so.
func (b *bitbucket) OpenIssues(r Repo, label string, max int) ([]Issue, error) {
	q := `(state="new" OR state="open")`
	if label != "" {
		q += ` AND component.name=` + bbqlString(label)
	}
	path := bbRepo(r) + "/issues?" + url.Values{"q": {q}, "sort": {"created_on"}}.Encode()
	is, err := bbPages[bbIssue](b, path, 50, 0)
	if err != nil {
		return nil, b.issues(r, err)
	}
	sort.SliceStable(is, func(i, j int) bool {
		if !is[i].CreatedOn.Equal(is[j].CreatedOn) {
			return is[i].CreatedOn.Before(is[j].CreatedOn)
		}
		return is[i].ID < is[j].ID
	})
	var out []Issue
	for _, i := range is {
		if max > 0 && len(out) == max {
			break
		}
		if label != "" {
			ok, err := b.queued(r, i, label)
			if err != nil {
				return nil, fmt.Errorf("issue #%d: %w", i.ID, err)
			}
			if !ok {
				b.note(fmt.Sprintf("note: skipping #%d: neither its reporter nor whoever set its component %q has write access to %s", i.ID, label, r))
				continue
			}
		}
		out = append(out, i.issue())
	}
	return out, nil
}

// queued reports whether issue i is in the queue of component label: its
// reporter may direct work on the repository, or the component was last
// set (to label) by someone who may. Bitbucket may let anyone who files an
// issue pick its component, unlike a label on the other forges.
func (b *bitbucket) queued(r Repo, i bbIssue, label string) (bool, error) {
	if i.Reporter != nil {
		if ok, err := b.trusted(r, *i.Reporter); err != nil || ok {
			return ok, err
		}
	}
	changes, err := bbPages[struct {
		User      bbUser    `json:"user"`
		CreatedOn time.Time `json:"created_on"`
		Changes   struct {
			Component *struct {
				New string `json:"new"`
			} `json:"component"`
		} `json:"changes"`
	}](b, fmt.Sprintf("%s/issues/%d/changes?sort=-created_on", bbRepo(r), i.ID), 50, 0)
	if s := status(err); s == 401 || s == 403 || s == 404 {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	sort.SliceStable(changes, func(x, y int) bool { return changes[x].CreatedOn.After(changes[y].CreatedOn) })
	for _, c := range changes {
		if c.Changes.Component == nil {
			continue
		}
		if c.Changes.Component.New != label {
			return false, nil
		}
		return b.trusted(r, c.User)
	}
	return false, nil
}

func (b *bitbucket) OpenPulls(r Repo) ([]Pull, error) {
	ps, err := bbPages[bbPR](b, bbRepo(r)+"/pullrequests?state=OPEN", 50, 0)
	if err != nil {
		return nil, err
	}
	var out []Pull
	for _, p := range ps {
		out = append(out, p.pull(r))
	}
	return out, nil
}

func (b *bitbucket) CreatePull(r Repo, p NewPull) (*Pull, error) {
	in := map[string]any{
		"title":       p.Title,
		"description": p.Body,
		"source":      map[string]any{"branch": map[string]string{"name": p.Head}},
		"destination": map[string]any{"branch": map[string]string{"name": p.Base}},
		"draft":       p.Draft,
	}
	var pr bbPR
	if err := b.do(http.MethodPost, bbRepo(r)+"/pullrequests", in, &pr); err != nil {
		return nil, err
	}
	out := pr.pull(r)
	return &out, nil
}

func bbBody(s string) map[string]any { return map[string]any{"content": bbText{Raw: s}} }

func (b *bitbucket) CommentIssue(r Repo, n int, body string) error {
	if err := b.do(http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", bbRepo(r), n), bbBody(body), nil); err != nil {
		return b.issues(r, err)
	}
	return nil
}

func (b *bitbucket) EditComment(r Repo, n int, id int64, body string) error {
	return b.do(http.MethodPut, fmt.Sprintf("%s/issues/%d/comments/%d", bbRepo(r), n, id), bbBody(body), nil)
}

func (b *bitbucket) CommentPull(r Repo, n int, body string) error {
	return b.do(http.MethodPost, fmt.Sprintf("%s/pullrequests/%d/comments", bbRepo(r), n), bbBody(body), nil)
}

func (b *bitbucket) pr(r Repo, n int) (*bbPR, error) {
	var p bbPR
	if err := b.do(http.MethodGet, fmt.Sprintf("%s/pullrequests/%d", bbRepo(r), n), nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Pull reads a pull request. Bitbucket gives its head commit's hash
// shortened, so the full one is looked up, in the source repository (a
// fork's commit may not be in r) and then in r: rw watch compares it with
// the branch it fetched. A failed lookup is no 404 of the pull request,
// which would make rw watch stop watching it.
func (b *bitbucket) Pull(r Repo, n int) (*Pull, error) {
	p, err := b.pr(r, n)
	if err != nil {
		return nil, err
	}
	out := p.pull(r)
	if out.HeadSHA == "" || len(out.HeadSHA) >= 40 {
		return &out, nil
	}
	repos := []string{bbRepo(r)}
	if src := p.Source.Repository; src != nil {
		if w, s, ok := strings.Cut(src.FullName, "/"); ok && w != "" && s != "" && !strings.Contains(s, "/") {
			repos = []string{"/repositories/" + url.PathEscape(w) + "/" + url.PathEscape(s), bbRepo(r)}
		}
	}
	var last error
	for _, repo := range repos {
		var c struct {
			Hash string `json:"hash"`
		}
		if last = b.do(http.MethodGet, repo+"/commit/"+url.PathEscape(out.HeadSHA), nil, &c); last != nil {
			continue
		}
		if !sameCommit(c.Hash, out.HeadSHA) {
			return nil, fmt.Errorf("the head %q of %s is commit %q on Bitbucket", out.HeadSHA, r.Ref(n), c.Hash)
		}
		out.HeadSHA = c.Hash
		return &out, nil
	}
	return nil, fmt.Errorf("read the head commit %s of %s: %v", out.HeadSHA, r.Ref(n), last)
}

// sameCommit reports whether two hashes, either maybe shortened (at least
// 7 digits), name the same commit.
func sameCommit(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if len(a) > len(b) {
		a, b = b, a
	}
	return len(a) >= 7 && strings.HasPrefix(b, a)
}

func (b *bitbucket) PullDiff(r Repo, n int, max int64) (string, error) {
	return b.text(fmt.Sprintf("%s/pullrequests/%d/diff", bbRepo(r), n), max)
}

// bbState is a pipeline's or step's state.
type bbState struct {
	Name   string  `json:"name"`   // PENDING, IN_PROGRESS, COMPLETED, ...
	Result *bbName `json:"result"` // SUCCESSFUL, FAILED, ERROR, STOPPED, EXPIRED
}

// failed is the conclusion of a run a code change may fix ("" for any
// other): failed or error. Stopped and expired runs were stopped by
// someone or by time.
func (s bbState) failed() string {
	if s.Result != nil && (s.Result.Name == "FAILED" || s.Result.Name == "ERROR") {
		return strings.ToLower(s.Result.Name)
	}
	return ""
}

func (b *bitbucket) FailedChecks(r Repo, sha string, logTail int) ([]Check, error) {
	out, piped, err := b.pipelineChecks(r, sha, logTail)
	if err != nil {
		return nil, err
	}
	statuses, err := bbPages[struct {
		Key         string `json:"key"`
		Name        string `json:"name"`
		State       string `json:"state"` // SUCCESSFUL, FAILED, INPROGRESS, STOPPED
		Description string `json:"description"`
		URL         string `json:"url"`
		UpdatedOn   string `json:"updated_on"`
	}](b, bbRepo(r)+"/commit/"+url.PathEscape(sha)+"/statuses", 100, 0)
	if err != nil {
		return nil, err
	}
	for _, s := range statuses {
		if s.State != "FAILED" {
			continue
		}
		if piped && (strings.Contains(s.URL, "/pipelines/results/") || strings.Contains(s.URL, "/addon/pipelines/")) {
			continue // a pipeline's own status: its steps are in already
		}
		name := s.Name
		if name == "" {
			name = s.Key
		}
		// A status is updated in place: a new run of the same check fails
		// at a new time.
		out = append(out, Check{ID: "status:" + s.Key + "@" + s.UpdatedOn, Name: name, Conclusion: "failed",
			Output: strings.TrimSpace(s.Description + "\n" + s.URL)})
	}
	return out, nil
}

// pipelineChecks are the failed steps of the newest pipeline per target
// (a branch, a pull request, a custom run) on the commit. piped is false
// when Bitbucket would not list pipelines (Pipelines off, or the token
// lacks the scope).
func (b *bitbucket) pipelineChecks(r Repo, sha string, logTail int) (out []Check, piped bool, err error) {
	type target struct {
		Type    string `json:"type"`
		RefName string `json:"ref_name"`
		Commit  *struct {
			Hash string `json:"hash"`
		} `json:"commit"`
		Selector *struct {
			Type    string `json:"type"`
			Pattern string `json:"pattern"`
		} `json:"selector"`
	}
	q := url.Values{"target.commit.hash": {sha}, "sort": {"-created_on"}}
	pipes, err := bbPages[struct {
		UUID        string  `json:"uuid"`
		BuildNumber int     `json:"build_number"`
		State       bbState `json:"state"`
		Target      target  `json:"target"`
	}](b, bbRepo(r)+"/pipelines?"+q.Encode(), 50, 0)
	if s := status(err); s == 403 || s == 404 {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	sort.SliceStable(pipes, func(i, j int) bool { return pipes[i].BuildNumber > pipes[j].BuildNumber })
	seen := map[string]bool{}
	for _, p := range pipes {
		// The filter is checked here too: the commit must be this one.
		if p.Target.Commit == nil || !sameCommit(p.Target.Commit.Hash, sha) {
			continue
		}
		key := p.Target.Type + "\x00" + p.Target.RefName
		if s := p.Target.Selector; s != nil {
			key += "\x00" + s.Type + "\x00" + s.Pattern
		}
		if seen[key] {
			continue // an older run of the same pipeline
		}
		seen[key] = true
		concl := p.State.failed()
		if concl == "" {
			continue
		}
		base := fmt.Sprintf("%s/pipelines/%s", bbRepo(r), url.PathEscape(p.UUID))
		steps, err := bbPages[struct {
			UUID  string  `json:"uuid"`
			Name  string  `json:"name"`
			State bbState `json:"state"`
		}](b, base+"/steps", 100, 0)
		if err != nil {
			return nil, false, fmt.Errorf("read the steps of pipeline #%d: %w", p.BuildNumber, err)
		}
		web := fmt.Sprintf("%s/pipelines/results/%d", r.WebURL(), p.BuildNumber)
		n := len(out)
		for _, s := range steps {
			c := s.State.failed()
			if c == "" {
				continue
			}
			ch := Check{ID: "step:" + s.UUID, Name: fmt.Sprintf("pipeline #%d: %s", p.BuildNumber, s.Name), Conclusion: c,
				Output: web + "/steps/" + url.PathEscape(s.UUID)}
			// The log is a bonus: a step without one is still reported.
			if log, err := b.tailOf(base+"/steps/"+url.PathEscape(s.UUID)+"/log", logTail); err == nil {
				ch.Log = log
			}
			out = append(out, ch)
		}
		if len(out) == n {
			// Failed before any step did (a broken bitbucket-pipelines.yml).
			out = append(out, Check{ID: "pipeline:" + p.UUID, Name: fmt.Sprintf("pipeline #%d", p.BuildNumber), Conclusion: concl, Output: web})
		}
	}
	return out, true, nil
}

// trusted: write access to the repository, else (when the token may not
// read permissions) a member of the workspace; never an app or a team.
func (b *bitbucket) trusted(r Repo, u bbUser) (bool, error) {
	if u.Type != "user" || !reBBUUID.MatchString(u.UUID) {
		return false, nil
	}
	key := r.String() + "\x00" + strings.ToLower(u.UUID)
	if t, ok := b.trust[key]; ok {
		return t, nil
	}
	t, err := b.lookupTrust(r, u.UUID)
	if err != nil {
		return false, fmt.Errorf("read the access of %s: %w", u.login(), err)
	}
	b.trust[key] = t
	return t, nil
}

func writes(perm string) bool { return perm == "write" || perm == "admin" }

// unreadable reports an answer that says the token may not ask: 401 (an
// access token on a workspace endpoint), 403 or 404.
func unreadable(err error) bool {
	s := status(err)
	return s == 401 || s == 403 || s == 404
}

func (b *bitbucket) lookupTrust(r Repo, uuid string) (bool, error) {
	// A 401 here means the token may not use the endpoint: it must not
	// make the client go on without the token.
	probe := *b.rest
	probe.keep = true
	ws := url.PathEscape(r.Owner)
	// The user's effective permission (needs a workspace admin's token).
	// Only the user's own row counts, should the filter not apply.
	var eff struct {
		Values []struct {
			Permission string `json:"permission"`
			User       bbUser `json:"user"`
		} `json:"values"`
	}
	q := url.Values{"q": {"user.uuid=" + bbqlString(uuid)}}
	err := probe.do(http.MethodGet, "/workspaces/"+ws+"/permissions/repositories/"+url.PathEscape(r.Name)+"?"+q.Encode(), nil, &eff)
	if err == nil {
		for _, v := range eff.Values {
			if strings.EqualFold(v.User.UUID, uuid) && writes(v.Permission) {
				return true, nil
			}
		}
		return false, nil
	}
	if !unreadable(err) {
		return false, err
	}
	// The permission given to the user on the repository (needs admin on
	// it): when it can be read, it decides.
	var perm struct {
		Permission string `json:"permission"`
	}
	err = probe.do(http.MethodGet, bbRepo(r)+"/permissions-config/users/"+url.PathEscape(uuid), nil, &perm)
	if err == nil {
		return writes(perm.Permission), nil
	}
	if !unreadable(err) {
		return false, err
	}
	// A member of the workspace: 200 means yes, 404 no.
	err = probe.do(http.MethodGet, "/workspaces/"+ws+"/members/"+url.PathEscape(uuid), nil, nil)
	if err == nil {
		return true, nil
	}
	if !unreadable(err) {
		return false, err
	}
	return false, nil
}

func (b *bitbucket) Feedback(r Repo, n int) ([]Feedback, error) {
	p, err := b.pr(r, n)
	if err != nil {
		return nil, err
	}
	var out []Feedback
	for _, pt := range p.Participants {
		if pt.State != "changes_requested" || pt.User.login() == "" {
			continue
		}
		ok, err := b.trusted(r, pt.User)
		if err != nil {
			return nil, err
		}
		// One item per reviewer: Bitbucket's change request has no id of
		// its own, and its time changes whenever the reviewer comments.
		// What they ask for is in their inline comments.
		out = append(out, Feedback{ID: "review:" + pt.User.login(), Review: true, Author: pt.User.login(), Trusted: ok})
	}
	// The newest comments (up to the page cap): old ones cannot hide new
	// feedback.
	cs, err := bbPages[bbComment](b, fmt.Sprintf("%s/pullrequests/%d/comments?sort=-created_on", bbRepo(r), n), 100, 0)
	if err != nil {
		return nil, fmt.Errorf("read comments: %w", err)
	}
	byID := map[int64]bbComment{}
	for _, c := range cs {
		byID[c.ID] = c
	}
	// A thread is resolved on its first comment; replies follow it.
	resolved := func(c bbComment) bool {
		for i := 0; i < 100; i++ {
			if c.Resolution != nil {
				return true
			}
			if c.Parent == nil {
				return false
			}
			parent, ok := byID[c.Parent.ID]
			if !ok {
				return false
			}
			c = parent
		}
		return false
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].ID < cs[j].ID })
	for _, c := range cs {
		if c.Inline == nil || c.Deleted || c.Pending || strings.TrimSpace(c.Content.Raw) == "" || resolved(c) {
			continue
		}
		ok, err := b.trusted(r, c.User)
		if err != nil {
			return nil, err
		}
		f := Feedback{ID: fmt.Sprintf("comment:%d", c.ID), Author: c.User.login(), Trusted: ok, Body: c.Content.Raw, Path: c.Inline.Path}
		if c.Inline.To != nil {
			f.Line = *c.Inline.To
		}
		out = append(out, f)
	}
	return out, nil
}

func (b *bitbucket) Viewer() (string, error) {
	var u bbUser
	if err := b.do(http.MethodGet, "/user", nil, &u); err != nil {
		return "", err
	}
	if u.login() == "" {
		return "", fmt.Errorf("no account for the Bitbucket token (GET /user gave no UUID)")
	}
	return u.login(), nil
}

func (b *bitbucket) CommentReview(r Repo, n int, headSHA, body string, comments []InlineComment) (string, error) {
	p, err := b.pr(r, n)
	if err != nil {
		return "", err
	}
	if headSHA != "" && (p.Source.Commit == nil || !sameCommit(p.Source.Commit.Hash, headSHA)) {
		return "", fmt.Errorf("%s has new commits since its diff was read; review it again", r.Ref(n))
	}
	path := fmt.Sprintf("%s/pullrequests/%d/comments", bbRepo(r), n)
	var missed []InlineComment
	for _, c := range comments {
		in := bbBody(c.Body)
		in["inline"] = map[string]any{"path": c.Path, "to": c.Line}
		err := b.do(http.MethodPost, path, in, nil)
		if s := status(err); s == 401 || s == 403 || (err != nil && s == 0) {
			return "", err
		}
		if err != nil {
			missed = append(missed, c) // Bitbucket could not place it on the line
		}
	}
	body = withMissed(body, "Bitbucket", missed)
	var cm struct {
		ID    int64   `json:"id"`
		Links bbLinks `json:"links"`
	}
	if err := b.do(http.MethodPost, path, bbBody(body), &cm); err != nil {
		return "", err
	}
	if cm.Links.HTML.Href != "" {
		return cm.Links.HTML.Href, nil
	}
	return fmt.Sprintf("%s#comment-%d", p.pull(r).URL, cm.ID), nil
}

// withMissed adds the inline comments a forge could not place on their
// line to a review's text.
func withMissed(body, forge string, missed []InlineComment) string {
	if len(missed) == 0 {
		return body
	}
	var b strings.Builder
	b.WriteString(body)
	b.WriteString("\n\nComments " + forge + " could not place on their line:\n\n")
	for _, c := range missed {
		fmt.Fprintf(&b, "- `%s:%d`: %s\n", strings.ReplaceAll(c.Path, "`", "'"), c.Line, strings.ReplaceAll(c.Body, "\n", "\n  "))
	}
	return b.String()
}
