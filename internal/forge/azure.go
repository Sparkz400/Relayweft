package forge

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// azure is the Azure DevOps REST API (Services on dev.azure.com, and
// Azure DevOps Server 2020 or later), at https://<host>/<org>:
//
//   - Issues are work items, numbered per organization: their HTML text is
//     made plain, labels are tags, and the open ones with a tag come from
//     a WIQL query (closed means Closed, Done, Removed, Resolved or
//     Completed).
//   - A pull request links the work items its "Closes #N" line names
//     (workItemRefs); a draft is a draft pull request. Azure DevOps keeps
//     4000 characters of a description: a longer one is cut, keeping that
//     line.
//   - Work item comments are HTML: rw's text goes in escaped (code spans,
//     links and line breaks kept), so none of it becomes markup or a
//     mention, and rw's markers show as text.
//   - Failed checks are the failed builds of the head commit (its branch
//     builds and the pull request's build validation, which builds a merge
//     commit), the newest per pipeline, one check per failed task with the
//     tail of its log; plus failed commit statuses other services post.
//   - Feedback is the votes "waiting for author" (-5) and "rejected"
//     (-10), and the active comment threads on a line of a file. An author
//     is trusted as a member of one of the project's teams (a group in a
//     team is not looked into), so service identities never are.
//   - A comment review is one active thread per inline comment, and one
//     thread without a status (it blocks nothing) with the text; a comment
//     Azure DevOps refuses goes into that text.
//
// A personal access token goes as basic auth, a Microsoft Entra token (a
// JWT) as a bearer token. A 401 may mean a scope the token lacks, so
// reads do not go on without the token as on the other forges.
type azure struct {
	*rest
	ver     string                     // api-version
	preview string                     // api-version of work item comments (a preview API)
	members map[string]map[string]bool // org/project -> team members' ids and unique names, lower case
	wiProj  map[int]string             // work item -> its project
	heads   map[string]azHead          // head commit -> the pull request read with it
	ids     map[string]string          // unique name (lower case) -> identity id, as seen
	me      *azSelf                    // the token's owner, once read
}

// azSelf is the token's owner: its identity id, and its name as Viewer
// gives it (the Account property, else the id).
type azSelf struct {
	id, name string
	err      error
}

// azHead is what FailedChecks needs from the pull request of a head
// commit: build validation builds its merge commit.
type azHead struct {
	n           int
	sourceRef   string
	mergeCommit string
}

func newAzure(api, token string, notes io.Writer) *azure {
	scheme := "Basic"
	if isJWT(token) {
		scheme = "Bearer"
	} else if token != "" {
		token = base64.StdEncoding.EncodeToString([]byte(":" + token))
	}
	a := &azure{rest: newRest(Azure, api, token, scheme, notes), ver: "6.0", preview: "6.0-preview.3",
		members: map[string]map[string]bool{}, wiProj: map[int]string{}, heads: map[string]azHead{}, ids: map[string]string{}}
	if u, err := url.Parse(api); err == nil && isAzureCloud(strings.ToLower(u.Hostname())) {
		a.ver, a.preview = "7.1", "7.1-preview.4"
	}
	// A rejected token gets a 401, not a redirect to a sign-in page.
	a.header = http.Header{}
	a.header.Set("X-TFS-FedAuthRedirect", "Suppress")
	a.forbidden = "A personal access token needs the scopes Code (read and write), Work Items (read and write), Build (read) and Project and Team (read)"
	// Azure DevOps answers a scope the token lacks with a 401 too.
	a.keep = true
	return a
}

// isJWT reports whether a token is a Microsoft Entra access token (a
// JWT) rather than a personal access token.
func isJWT(t string) bool { return strings.HasPrefix(t, "eyJ") && strings.Count(t, ".") == 2 }

func azProject(r Repo) string {
	_, p := r.azParts()
	return "/" + url.PathEscape(p)
}

func azGit(r Repo) string { return azProject(r) + "/_apis/git/repositories/" + url.PathEscape(r.Name) }

// v adds the api-version to path.
func (a *azure) v(path string) string {
	if strings.Contains(path, "?") {
		return path + "&api-version=" + a.ver
	}
	return path + "?api-version=" + a.ver
}

// azEnum is an enum Azure DevOps writes as a name or a number.
type azEnum string

func (e *azEnum) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*e = azEnum(strings.ToLower(s))
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*e = azEnum(strconv.FormatInt(n, 10))
	return nil
}

func (e azEnum) is(name string, num int) bool {
	return string(e) == strings.ToLower(name) || string(e) == strconv.Itoa(num)
}

// azProp is a property value ({"$value": ...}) as text.
type azProp struct {
	Value json.RawMessage `json:"$value"`
}

func (p azProp) String() string {
	var s string
	if json.Unmarshal(p.Value, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(p.Value))
}

type azIdentity struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	UniqueName  string `json:"uniqueName"`
	IsContainer bool   `json:"isContainer"`
}

// login is how rw names an identity: its unique name (an email or
// DOMAIN\user), else its id; never the display name, which anyone may
// share.
func (u azIdentity) login() string {
	if u.UniqueName != "" {
		return u.UniqueName
	}
	return u.ID
}

// author names an identity: the token's owner by the name Viewer gives
// (on a server the Account is "jdoe" where comments say "CONTOSO\jdoe"),
// matched by identity id; anyone else by login. It remembers the id, for
// trusted.
func (a *azure) author(u azIdentity) string {
	if u.ID == "" {
		return u.login()
	}
	if a.HasToken() {
		if me, err := a.self(); err == nil && strings.EqualFold(me.id, u.ID) {
			return me.name
		}
	}
	if l := u.login(); l != "" {
		a.ids[strings.ToLower(l)] = u.ID
		return l
	}
	return u.ID
}

func (a *azure) DefaultBranch(r Repo) (string, error) {
	var out struct {
		DefaultBranch string `json:"defaultBranch"`
	}
	if err := a.do(http.MethodGet, a.v(azGit(r)), nil, &out); err != nil {
		return "", err
	}
	b := strings.TrimPrefix(out.DefaultBranch, "refs/heads/")
	if b == "" {
		return "", errors.New("no default branch on Azure DevOps (is the repository empty?)")
	}
	return b, nil
}

// Work items.

type azWorkItem struct {
	ID     int `json:"id"`
	Fields struct {
		Title       string          `json:"System.Title"`
		State       string          `json:"System.State"`
		Tags        string          `json:"System.Tags"`
		Project     string          `json:"System.TeamProject"`
		Created     time.Time       `json:"System.CreatedDate"`
		CreatedBy   json.RawMessage `json:"System.CreatedBy"`
		Description string          `json:"System.Description"`
		ReproSteps  string          `json:"Microsoft.VSTS.TCM.ReproSteps"`
		Acceptance  string          `json:"Microsoft.VSTS.Common.AcceptanceCriteria"`
	} `json:"fields"`
	// Formats is "markdown" for a long text field written in Markdown
	// (else it is HTML).
	Formats map[string]string `json:"multilineFieldsFormat"`
}

// azClosed are the states of the default processes that mean done.
var azClosed = []string{"Closed", "Done", "Removed", "Resolved", "Completed"}

func (a *azure) issue(r Repo, w azWorkItem) Issue {
	f := w.Fields
	org, project := r.azParts()
	if f.Project != "" {
		project = f.Project
	}
	is := Issue{Number: w.ID, Title: f.Title, State: "open", Created: f.Created,
		URL: fmt.Sprintf("%s/%s/%s/_workitems/edit/%d", r.root(), url.PathEscape(org), url.PathEscape(project), w.ID)}
	for _, s := range azClosed {
		if strings.EqualFold(f.State, s) {
			is.State = "closed"
		}
	}
	for _, t := range strings.Split(f.Tags, ";") {
		if t = strings.TrimSpace(t); t != "" {
			is.Labels = append(is.Labels, t)
		}
	}
	var by azIdentity
	if json.Unmarshal(f.CreatedBy, &by) == nil {
		is.Author = by.login()
	} else {
		// Older servers: "Name <DOMAIN\user>".
		var s string
		_ = json.Unmarshal(f.CreatedBy, &s) // "" when it is neither
		if i, j := strings.LastIndex(s, "<"), strings.LastIndex(s, ">"); i >= 0 && j > i {
			s = s[i+1 : j]
		}
		is.Author = s
	}
	var parts []string
	for _, p := range []struct{ head, field, text string }{{"", "System.Description", f.Description},
		{"Repro steps:\n", "Microsoft.VSTS.TCM.ReproSteps", f.ReproSteps}, {"Acceptance criteria:\n", "Microsoft.VSTS.Common.AcceptanceCriteria", f.Acceptance}} {
		t := strings.TrimSpace(p.text)
		if !strings.EqualFold(w.Formats[p.field], "markdown") {
			t = azText(p.text)
		}
		if t != "" {
			parts = append(parts, p.head+t)
		}
	}
	is.Body = strings.Join(parts, "\n\n")
	return is
}

func (a *azure) Issue(r Repo, n int) (*Issue, error) {
	var w azWorkItem
	if err := a.do(http.MethodGet, a.v(fmt.Sprintf("/_apis/wit/workitems/%d", n)), nil, &w); err != nil {
		return nil, err
	}
	if w.Fields.Project != "" {
		a.wiProj[n] = w.Fields.Project
	}
	out := a.issue(r, w)
	return &out, nil
}

// itemProject is the path of work item n's project, which its comments
// live under (it may not be the repository's).
func (a *azure) itemProject(r Repo, n int) (string, error) {
	p, ok := a.wiProj[n]
	if !ok {
		var w azWorkItem
		if err := a.do(http.MethodGet, a.v(fmt.Sprintf("/_apis/wit/workitems/%d?fields=System.TeamProject", n)), nil, &w); err != nil {
			return "", err
		}
		p = w.Fields.Project
		a.wiProj[n] = p
	}
	if p == "" {
		return azProject(r), nil
	}
	return "/" + url.PathEscape(p), nil
}

func (a *azure) Comments(r Repo, n int) ([]Comment, error) {
	proj, err := a.itemProject(r, n)
	if err != nil {
		return nil, err
	}
	var out []Comment
	cont := ""
	for page := 0; page < maxPages; page++ {
		path := fmt.Sprintf("%s/_apis/wit/workItems/%d/comments?$top=200&order=asc&api-version=%s", proj, n, a.preview)
		if cont != "" {
			path += "&continuationToken=" + url.QueryEscape(cont)
		}
		var res struct {
			Comments []struct {
				ID          int64      `json:"id"`
				Text        string     `json:"text"`
				CreatedBy   azIdentity `json:"createdBy"`
				CreatedDate time.Time  `json:"createdDate"`
				IsDeleted   bool       `json:"isDeleted"`
				Format      azEnum     `json:"format"` // html, or markdown (7.1)
			} `json:"comments"`
			ContinuationToken string `json:"continuationToken"`
		}
		if err := a.do(http.MethodGet, path, nil, &res); err != nil {
			return nil, err
		}
		for _, c := range res.Comments {
			if c.IsDeleted {
				continue
			}
			body := strings.TrimSpace(c.Text)
			if !c.Format.is("markdown", 0) {
				body = azText(c.Text)
			}
			out = append(out, Comment{ID: c.ID, Author: a.author(c.CreatedBy), Body: body, Created: c.CreatedDate})
		}
		if res.ContinuationToken == "" || res.ContinuationToken == cont || len(res.Comments) == 0 {
			break
		}
		cont = res.ContinuationToken
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (a *azure) CommentTrusted(r Repo, c Comment) (bool, error) {
	return a.trusted(r, a.ids[strings.ToLower(c.Author)], c.Author)
}

// wiqlString is s as a WIQL string literal.
func wiqlString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// OpenIssues runs a WIQL query for the project's open work items with the
// tag, oldest first, and reads them 200 at a time.
func (a *azure) OpenIssues(r Repo, label string, max int) ([]Issue, error) {
	_, project := r.azParts()
	q := "SELECT [System.Id] FROM WorkItems WHERE [System.TeamProject] = " + wiqlString(project)
	if label != "" {
		q += " AND [System.Tags] CONTAINS " + wiqlString(label)
	}
	var closed []string
	for _, s := range azClosed {
		closed = append(closed, wiqlString(s))
	}
	q += " AND [System.State] NOT IN (" + strings.Join(closed, ", ") + ") ORDER BY [System.CreatedDate] ASC, [System.Id] ASC"
	var res struct {
		WorkItems []struct {
			ID int `json:"id"`
		} `json:"workItems"`
	}
	if err := a.do(http.MethodPost, fmt.Sprintf("%s/_apis/wit/wiql?$top=%d&api-version=%s", azProject(r), maxPages*100, a.ver), map[string]string{"query": q}, &res); err != nil {
		return nil, err
	}
	var out []Issue
	for i := 0; i < len(res.WorkItems); i += 200 {
		chunk := res.WorkItems[i:min(i+200, len(res.WorkItems))]
		ids := make([]string, len(chunk))
		for j, w := range chunk {
			ids[j] = strconv.Itoa(w.ID)
		}
		var batch struct {
			Value []azWorkItem `json:"value"`
		}
		path := fmt.Sprintf("%s/_apis/wit/workitems?ids=%s&fields=System.Id,System.Title,System.State,System.Tags,System.CreatedDate,System.CreatedBy,System.TeamProject&errorPolicy=omit", azProject(r), strings.Join(ids, ","))
		if err := a.do(http.MethodGet, a.v(path), nil, &batch); err != nil {
			return nil, err
		}
		byID := map[int]Issue{}
		for _, w := range batch.Value {
			if w.ID != 0 { // a work item gone since the query is null
				byID[w.ID] = a.issue(r, w)
			}
		}
		// In the query's order; CONTAINS may match part of a tag.
		for _, w := range chunk {
			is, ok := byID[w.ID]
			if !ok || is.State != "open" || (label != "" && !hasLabel(is.Labels, label)) {
				continue
			}
			out = append(out, is)
		}
		if max > 0 && len(out) >= max {
			return out[:max], nil
		}
	}
	return out, nil
}

func hasLabel(labels []string, l string) bool {
	for _, x := range labels {
		if strings.EqualFold(x, l) {
			return true
		}
	}
	return false
}

// Pull requests.

type azCommitRef struct {
	CommitID string `json:"commitId"`
}

type azPR struct {
	ID          int          `json:"pullRequestId"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	Status      azEnum       `json:"status"` // active, abandoned, completed
	IsDraft     bool         `json:"isDraft"`
	SourceRef   string       `json:"sourceRefName"`
	TargetRef   string       `json:"targetRefName"`
	LastSource  *azCommitRef `json:"lastMergeSourceCommit"`
	LastMerge   *azCommitRef `json:"lastMergeCommit"`
	ForkSource  *struct {
		Name string `json:"name"`
	} `json:"forkSource"`
	Reviewers []struct {
		azIdentity
		Vote int `json:"vote"`
	} `json:"reviewers"`
}

func (p azPR) pull(r Repo) Pull {
	out := Pull{Number: p.ID, Title: p.Title, Body: p.Description, URL: fmt.Sprintf("%s/pullrequest/%d", r.WebURL(), p.ID),
		State: "open", Draft: p.IsDraft, HeadRef: strings.TrimPrefix(p.SourceRef, "refs/heads/"), BaseRef: strings.TrimPrefix(p.TargetRef, "refs/heads/")}
	switch {
	case p.Status.is("completed", 3):
		out.State, out.Merged = "closed", true
	case p.Status.is("abandoned", 2):
		out.State = "closed"
	}
	if p.LastSource != nil {
		out.HeadSHA = p.LastSource.CommitID
	}
	if p.ForkSource == nil {
		out.HeadRepo = r.String()
	}
	return out
}

func (a *azure) OpenPulls(r Repo) ([]Pull, error) {
	var out []Pull
	for page := 0; page < maxPages; page++ {
		var res struct {
			Value []azPR `json:"value"`
		}
		if err := a.do(http.MethodGet, a.v(fmt.Sprintf("%s/pullrequests?searchCriteria.status=active&$top=100&$skip=%d", azGit(r), page*100)), nil, &res); err != nil {
			return nil, err
		}
		for _, p := range res.Value {
			if utf16Len(p.Description) >= azListDescription {
				// The list cuts descriptions, and the "Closes #N" line
				// is at the end: read the whole one.
				full, err := a.pr(r, p.ID)
				if err != nil {
					return nil, err
				}
				p = *full
			}
			out = append(out, p.pull(r))
		}
		if len(res.Value) < 100 {
			break
		}
	}
	return out, nil
}

// Azure DevOps' limits on a pull request's title and description, and
// the length a list of pull requests cuts descriptions to.
const (
	azMaxTitle        = 400
	azMaxDescription  = 4000
	azListDescription = 400
)

func (a *azure) CreatePull(r Repo, p NewPull) (*Pull, error) {
	in := map[string]any{"sourceRefName": "refs/heads/" + p.Head, "targetRefName": "refs/heads/" + p.Base,
		"title": cutUTF16(p.Title, azMaxTitle), "description": azDescription(p.Body), "isDraft": p.Draft}
	var ns []int
	for n := range ClosedBy([]Pull{{Body: p.Body}}) {
		ns = append(ns, n)
	}
	sort.Ints(ns)
	var refs []map[string]string
	for _, n := range ns {
		refs = append(refs, map[string]string{"id": strconv.Itoa(n)})
	}
	if len(refs) > 0 {
		in["workItemRefs"] = refs
	}
	var pr azPR
	err := a.do(http.MethodPost, a.v(azGit(r)+"/pullrequests"), in, &pr)
	if status(err) == http.StatusBadRequest && len(refs) > 0 {
		// A work item the token cannot see (or that is gone) fails the
		// whole request: open it without the link.
		a.note(fmt.Sprintf("note: Azure DevOps refused to link work item(s) %v (%v); opening the pull request without the link", ns, err))
		delete(in, "workItemRefs")
		err = a.do(http.MethodPost, a.v(azGit(r)+"/pullrequests"), in, &pr)
	}
	if err != nil {
		return nil, err
	}
	out := pr.pull(r)
	return &out, nil
}

// azDescription fits a pull request description into Azure DevOps' limit:
// the end is cut (an open code block closed), and the closing line rw
// adds at the end ("Closes #12") stays.
func azDescription(s string) string {
	if utf16Len(s) <= azMaxDescription {
		return s
	}
	tail := ""
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if last := lines[len(lines)-1]; reCloses.MatchString(last) && len(last) < 200 {
		tail = "\n" + last + "\n"
		s = strings.Join(lines[:len(lines)-1], "\n")
	}
	const note = "\n\n_(cut: Azure DevOps keeps 4000 characters of a description)_\n"
	room := azMaxDescription - utf16Len(tail) - utf16Len(note)
	for {
		head := cutUTF16(s, room)
		if i := strings.LastIndexByte(head, '\n'); i > 0 {
			head = head[:i]
		}
		head = closeFence(head)
		out := head + note + tail
		if over := utf16Len(out) - azMaxDescription; over > 0 && room > over {
			room -= over
			continue
		}
		return out
	}
}

var reFenceLine = regexp.MustCompile("^[ \t]{0,3}(`{3,}|~{3,})")

// closeFence closes a fenced code block that s leaves open.
func closeFence(s string) string {
	open := ""
	for _, l := range strings.Split(s, "\n") {
		m := reFenceLine.FindStringSubmatch(l)
		switch {
		case m == nil:
		case open == "":
			open = m[1]
		case m[1][0] == open[0] && len(m[1]) >= len(open) && strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), m[1][:1])) == "":
			open = ""
		}
	}
	if open != "" {
		s += "\n" + open
	}
	return s
}

// utf16Len is the length of s in UTF-16 units, as Azure DevOps counts.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// cutUTF16 keeps the start of s that fits in n UTF-16 units.
func cutUTF16(s string, n int) string {
	used := 0
	for i, r := range s {
		w := utf16.RuneLen(r)
		if used+w > n {
			return s[:i]
		}
		used += w
	}
	return s
}

// CommentIssue comments on work item n.
func (a *azure) CommentIssue(r Repo, n int, body string) error {
	proj, err := a.itemProject(r, n)
	if err != nil {
		return err
	}
	return a.do(http.MethodPost, fmt.Sprintf("%s/_apis/wit/workItems/%d/comments?api-version=%s", proj, n, a.preview), map[string]string{"text": azHTML(body)}, nil)
}

func (a *azure) EditComment(r Repo, n int, id int64, body string) error {
	proj, err := a.itemProject(r, n)
	if err != nil {
		return err
	}
	return a.do(http.MethodPatch, fmt.Sprintf("%s/_apis/wit/workItems/%d/comments/%d?api-version=%s", proj, n, id, a.preview), map[string]string{"text": azHTML(body)}, nil)
}

// thread posts a comment thread on pull request n; status "" leaves it
// without one, so it blocks nothing.
func (a *azure) thread(r Repo, n int, body, status string, ctx, prCtx map[string]any) (int64, error) {
	in := map[string]any{"comments": []map[string]any{{"parentCommentId": 0, "content": body, "commentType": "text"}}}
	if status != "" {
		in["status"] = status
	}
	if ctx != nil {
		in["threadContext"] = ctx
	}
	if prCtx != nil {
		in["pullRequestThreadContext"] = prCtx
	}
	var out struct {
		ID int64 `json:"id"`
	}
	err := a.do(http.MethodPost, a.v(fmt.Sprintf("%s/pullRequests/%d/threads", azGit(r), n)), in, &out)
	return out.ID, err
}

func (a *azure) CommentPull(r Repo, n int, body string) error {
	_, err := a.thread(r, n, body, "", nil, nil)
	return err
}

func (a *azure) pr(r Repo, n int) (*azPR, error) {
	var p azPR
	if err := a.do(http.MethodGet, a.v(fmt.Sprintf("%s/pullrequests/%d", azGit(r), n)), nil, &p); err != nil {
		return nil, err
	}
	if p.LastSource != nil && p.LastSource.CommitID != "" {
		h := azHead{n: n, sourceRef: p.SourceRef}
		if p.LastMerge != nil {
			h.mergeCommit = p.LastMerge.CommitID
		}
		a.heads[p.LastSource.CommitID] = h
	}
	return &p, nil
}

func (a *azure) Pull(r Repo, n int) (*Pull, error) {
	p, err := a.pr(r, n)
	if err != nil {
		return nil, err
	}
	out := p.pull(r)
	return &out, nil
}

// Checks.

type azBuild struct {
	ID         int64  `json:"id"`
	Status     azEnum `json:"status"`
	Result     azEnum `json:"result"`
	Definition struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"definition"`
	SourceBranch  string `json:"sourceBranch"`
	SourceVersion string `json:"sourceVersion"`
	Repository    struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"repository"`
	TriggerInfo map[string]string `json:"triggerInfo"`
	Parameters  string            `json:"parameters"`
	Links       struct {
		Web struct {
			Href string `json:"href"`
		} `json:"web"`
	} `json:"_links"`
	ValidationResults []struct {
		Message string `json:"message"`
	} `json:"validationResults"`
}

// of reports whether b built sha: a branch build of it, or a pull request
// build of it (build validation builds the merge commit).
func (b azBuild) of(sha string, h azHead, known bool) bool {
	if b.SourceVersion == sha || b.TriggerInfo["pr.sourceSha"] == sha {
		return true
	}
	var params map[string]any
	if json.Unmarshal([]byte(b.Parameters), &params) == nil {
		if s, ok := params["system.pullRequest.sourceCommitId"].(string); ok && strings.EqualFold(s, sha) {
			return true
		}
	}
	return known && h.mergeCommit != "" && b.SourceVersion == h.mergeCommit && b.SourceBranch == fmt.Sprintf("refs/pull/%d/merge", h.n)
}

// builds are the newest build per pipeline of the head commit sha. With
// its pull request read (Pull), they are looked up by its branches, else
// among the project's newest builds.
func (a *azure) builds(r Repo, sha string) ([]azBuild, error) {
	h, known := a.heads[sha]
	branches := []string{""}
	if known {
		branches = []string{fmt.Sprintf("refs/pull/%d/merge", h.n), h.sourceRef}
	}
	var all []azBuild
	for _, br := range branches {
		path := azProject(r) + "/_apis/build/builds?queryOrder=queueTimeDescending&$top=50"
		if br != "" {
			path += "&branchName=" + url.QueryEscape(br)
		}
		var res struct {
			Value []azBuild `json:"value"`
		}
		if err := a.do(http.MethodGet, a.v(path), nil, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Value...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].ID > all[j].ID })
	seen := map[int]bool{}
	var out []azBuild
	for _, b := range all {
		if !strings.EqualFold(b.Repository.Name, r.Name) || (b.Repository.Type != "" && !strings.EqualFold(b.Repository.Type, "TfsGit")) || !b.of(sha, h, known) {
			continue
		}
		if seen[b.Definition.ID] {
			continue // re-run since
		}
		seen[b.Definition.ID] = true
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// reAzLogStamp matches the time an Azure Pipelines log line starts with.
var reAzLogStamp = regexp.MustCompile(`(?m)^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z ?`)

func (a *azure) FailedChecks(r Repo, sha string, logTail int) ([]Check, error) {
	builds, err := a.builds(r, sha)
	if err != nil {
		return nil, err
	}
	var out []Check
	for _, b := range builds {
		if !b.Status.is("completed", 2) || !b.Result.is("failed", 8) {
			continue
		}
		out = append(out, a.buildChecks(r, b, logTail)...)
	}
	// Statuses other services post on the commit, and on the pull
	// request (the newest per context).
	paths := map[string]string{"status": a.v(fmt.Sprintf("%s/commits/%s/statuses?latestOnly=true", azGit(r), url.PathEscape(sha)))}
	if h, ok := a.heads[sha]; ok {
		paths["prstatus"] = a.v(fmt.Sprintf("%s/pullRequests/%d/statuses", azGit(r), h.n))
	}
	for _, kind := range []string{"status", "prstatus"} {
		path, ok := paths[kind]
		if !ok {
			continue
		}
		var st struct {
			Value []struct {
				ID          int64  `json:"id"`
				State       azEnum `json:"state"` // notSet, pending, succeeded, failed, error, notApplicable
				Description string `json:"description"`
				TargetURL   string `json:"targetUrl"`
				Context     struct {
					Name  string `json:"name"`
					Genre string `json:"genre"`
				} `json:"context"`
			} `json:"value"`
		}
		if err := a.do(http.MethodGet, path, nil, &st); err != nil {
			return nil, err
		}
		sort.Slice(st.Value, func(i, j int) bool { return st.Value[i].ID > st.Value[j].ID })
		seen := map[string]bool{}
		var cs []Check
		for _, s := range st.Value {
			name := s.Context.Name
			if s.Context.Genre != "" {
				name = s.Context.Genre + "/" + name
			}
			if seen[name] {
				continue // set again since
			}
			seen[name] = true
			if !s.State.is("failed", 3) && !s.State.is("error", 4) {
				continue
			}
			cs = append(cs, Check{ID: fmt.Sprintf("%s/%d", kind, s.ID), Name: name, Conclusion: string(s.State),
				Output: strings.TrimSpace(s.Description + "\n" + s.TargetURL)})
		}
		for i := len(cs) - 1; i >= 0; i-- {
			out = append(out, cs[i])
		}
	}
	return out, nil
}

// buildChecks are a failed build's failed tasks, each with its log tail;
// the build itself when its timeline names none (a YAML error, a
// cancelled agent).
func (a *azure) buildChecks(r Repo, b azBuild, logTail int) []Check {
	whole := Check{ID: fmt.Sprintf("build/%d", b.ID), Name: b.Definition.Name, Conclusion: "failed"}
	var why []string
	for _, v := range b.ValidationResults {
		why = append(why, v.Message)
	}
	type record struct {
		ID       string `json:"id"`
		ParentID string `json:"parentId"`
		Type     string `json:"type"`
		Name     string `json:"name"`
		Result   azEnum `json:"result"`
		Log      *struct {
			ID int `json:"id"`
		} `json:"log"`
		Issues []struct {
			Type    azEnum `json:"type"`
			Message string `json:"message"`
		} `json:"issues"`
	}
	var tl struct {
		Records []record `json:"records"`
	}
	if err := a.do(http.MethodGet, a.v(fmt.Sprintf("%s/_apis/build/builds/%d/timeline", azProject(r), b.ID)), nil, &tl); err != nil {
		// The log is a bonus: the build is still reported.
		whole.Output = strings.TrimSpace(strings.Join(why, "\n") + "\n" + b.Links.Web.Href)
		return []Check{whole}
	}
	byID := map[string]record{}
	for _, rec := range tl.Records {
		byID[rec.ID] = rec
	}
	var out []Check
	for _, rec := range tl.Records {
		var errs []string
		for _, is := range rec.Issues {
			if is.Type.is("error", 1) {
				errs = append(errs, is.Message)
			}
		}
		if !rec.Result.is("failed", 2) {
			continue
		}
		if !strings.EqualFold(rec.Type, "Task") {
			why = append(why, errs...)
			continue
		}
		c := Check{ID: fmt.Sprintf("build/%d/%s", b.ID, rec.ID), Name: b.Definition.Name + ": " + rec.Name, Conclusion: "failed",
			Output: strings.TrimSpace(strings.Join(errs, "\n") + "\n" + b.Links.Web.Href)}
		if job, ok := byID[rec.ParentID]; ok && job.Name != "" && job.Name != "Job" && job.Name != "__default" {
			c.Name = b.Definition.Name + ": " + job.Name + ": " + rec.Name
		}
		if rec.Log != nil {
			// Read more than the tail: the times are dropped first.
			if log, err := a.tailOf(a.v(fmt.Sprintf("%s/_apis/build/builds/%d/logs/%d", azProject(r), b.ID, rec.Log.ID)), 2*logTail); err == nil {
				c.Log = lineTail(reAzLogStamp.ReplaceAllString(log, ""), logTail)
			}
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		whole.Output = strings.TrimSpace(strings.Join(why, "\n") + "\n" + b.Links.Web.Href)
		out = append(out, whole)
	}
	return out
}

// Feedback and trust.

type azThread struct {
	ID            int64  `json:"id"`
	Status        azEnum `json:"status"` // active, fixed, wontFix, closed, byDesign, pending
	IsDeleted     bool   `json:"isDeleted"`
	ThreadContext *struct {
		FilePath       string `json:"filePath"`
		RightFileStart *struct {
			Line int `json:"line"`
		} `json:"rightFileStart"`
	} `json:"threadContext"`
	Comments []struct {
		ID          int64      `json:"id"`
		Author      azIdentity `json:"author"`
		Content     string     `json:"content"`
		CommentType azEnum     `json:"commentType"` // text, codeChange, system
		IsDeleted   bool       `json:"isDeleted"`
	} `json:"comments"`
	Properties map[string]azProp `json:"properties"`
}

// trusted: a member of one of the project's teams, by id or unique name.
func (a *azure) trusted(r Repo, id, login string) (bool, error) {
	if id == "" && login == "" {
		return false, nil
	}
	key := strings.ToLower(r.Host + "\x00" + r.Owner)
	m, ok := a.members[key]
	if !ok {
		var err error
		if m, err = a.teamMembers(r); err != nil {
			return false, fmt.Errorf("read the access of %s (the members of the project's teams): %w", login, err)
		}
		a.members[key] = m
	}
	return (id != "" && m[strings.ToLower(id)]) || (login != "" && m[strings.ToLower(login)]), nil
}

// teamMembers are the ids and unique names (lower case) of the people in
// the project's teams.
func (a *azure) teamMembers(r Repo) (map[string]bool, error) {
	_, project := r.azParts()
	base := "/_apis/projects/" + url.PathEscape(project) + "/teams"
	var teams []string
	for page := 0; page < maxPages; page++ {
		var res struct {
			Value []struct {
				ID string `json:"id"`
			} `json:"value"`
		}
		if err := a.do(http.MethodGet, a.v(fmt.Sprintf("%s?$top=100&$skip=%d", base, page*100)), nil, &res); err != nil {
			return nil, err
		}
		for _, t := range res.Value {
			teams = append(teams, t.ID)
		}
		if len(res.Value) < 100 {
			break
		}
	}
	m := map[string]bool{}
	for _, t := range teams {
		for page := 0; page < maxPages; page++ {
			var res struct {
				Value []struct {
					Identity azIdentity `json:"identity"`
				} `json:"value"`
			}
			if err := a.do(http.MethodGet, a.v(fmt.Sprintf("%s/%s/members?$top=100&$skip=%d", base, url.PathEscape(t), page*100)), nil, &res); err != nil {
				return nil, err
			}
			for _, mb := range res.Value {
				if u := mb.Identity; !u.IsContainer {
					for _, k := range []string{u.ID, u.UniqueName} {
						if k != "" {
							m[strings.ToLower(k)] = true
						}
					}
				}
			}
			if len(res.Value) < 100 {
				break
			}
		}
	}
	return m, nil
}

func (a *azure) Feedback(r Repo, n int) ([]Feedback, error) {
	p, err := a.pr(r, n)
	if err != nil {
		return nil, err
	}
	var res struct {
		Value []azThread `json:"value"`
	}
	if err := a.do(http.MethodGet, a.v(fmt.Sprintf("%s/pullRequests/%d/threads", azGit(r), n)), nil, &res); err != nil {
		return nil, fmt.Errorf("read threads: %w", err)
	}
	threads := res.Value
	sort.Slice(threads, func(i, j int) bool { return threads[i].ID < threads[j].ID })
	var out []Feedback
	for _, rv := range p.Reviewers {
		if rv.Vote > -5 || rv.IsContainer {
			continue
		}
		who := a.author(rv.azIdentity)
		ok, err := a.trusted(r, rv.ID, rv.login())
		if err != nil {
			return nil, err
		}
		// The vote's own thread makes a new vote a new item.
		id := fmt.Sprintf("review:%s:%d", strings.ToLower(rv.ID), rv.Vote)
		for _, t := range threads {
			if t.Properties["CodeReviewThreadType"].String() == "VoteUpdate" && strings.EqualFold(t.Properties["CodeReviewVotedByIdentity"].String(), rv.ID) {
				id = fmt.Sprintf("review:%d", t.ID)
			}
		}
		out = append(out, Feedback{ID: id, Review: true, Author: who, Trusted: ok})
	}
	for _, t := range threads {
		if t.IsDeleted || t.ThreadContext == nil || t.ThreadContext.FilePath == "" || !(t.Status.is("active", 1) || t.Status.is("pending", 6)) {
			continue
		}
		line := 0
		if s := t.ThreadContext.RightFileStart; s != nil {
			line = s.Line
		}
		for _, c := range t.Comments {
			if c.IsDeleted || !c.CommentType.is("text", 1) {
				continue
			}
			ok, err := a.trusted(r, c.Author.ID, c.Author.login())
			if err != nil {
				return nil, err
			}
			out = append(out, Feedback{ID: fmt.Sprintf("comment:%d-%d", t.ID, c.ID), Author: a.author(c.Author), Trusted: ok,
				Body: c.Content, Path: strings.TrimPrefix(t.ThreadContext.FilePath, "/"), Line: line})
		}
	}
	return out, nil
}

// Viewer is the token owner's Account (an email, or a server's user
// name), else its identity id. Comments and votes of that identity carry
// the same name (author).
func (a *azure) Viewer() (string, error) {
	me, err := a.self()
	return me.name, err
}

// self reads the token's owner once.
func (a *azure) self() (azSelf, error) {
	if a.me != nil {
		return *a.me, a.me.err
	}
	var cd struct {
		AuthenticatedUser struct {
			ID         string            `json:"id"`
			Properties map[string]azProp `json:"properties"`
		} `json:"authenticatedUser"`
	}
	var me azSelf
	switch err := a.do(http.MethodGet, "/_apis/connectionData", nil, &cd); {
	case err != nil:
		me.err = err
	case a.Rejected() || !a.HasToken():
		me.err = errors.New("the token was rejected by Azure DevOps")
	case cd.AuthenticatedUser.ID == "":
		me.err = errors.New("no user for the token on Azure DevOps")
	default:
		me.id = cd.AuthenticatedUser.ID
		me.name = cd.AuthenticatedUser.Properties["Account"].String()
		if me.name == "" {
			me.name = me.id
		}
	}
	a.me = &me
	return me, me.err
}

// Diffs and reviews.

type azChange struct {
	ChangeTrackingID int    `json:"changeTrackingId"`
	ChangeType       azEnum `json:"changeType"` // "edit", "add", "delete", "rename", "edit, rename", ... or flags
	OriginalPath     string `json:"originalPath"`
	SourceServerItem string `json:"sourceServerItem"`
	Item             struct {
		ObjectID         string `json:"objectId"`
		OriginalObjectID string `json:"originalObjectId"`
		Path             string `json:"path"`
		IsFolder         bool   `json:"isFolder"`
		GitObjectType    azEnum `json:"gitObjectType"`
	} `json:"item"`
}

// kinds reads the change type: a list of names or a number of flags
// (add 1, rename 8, delete 16).
func (c azChange) kinds() (add, del, rename bool) {
	if n, err := strconv.Atoi(string(c.ChangeType)); err == nil {
		return n&1 != 0, n&16 != 0, n&8 != 0
	}
	for _, k := range strings.Split(string(c.ChangeType), ",") {
		switch strings.TrimSpace(k) {
		case "add":
			add = true
		case "delete":
			del = true
		case "rename":
			rename = true
		}
	}
	return add, del, rename
}

// maxDiffFiles caps the files of a pull request rw diffs, maxDiffFile the
// size of a file version it diffs.
const (
	maxDiffFiles = 3000
	maxDiffFile  = 1 << 20
)

// iteration is the pull request's latest iteration (push) and its files,
// compared with the common commit of the two branches.
func (a *azure) iteration(r Repo, n int) (id int, base, head string, files []azChange, err error) {
	var its struct {
		Value []struct {
			ID              int          `json:"id"`
			SourceRefCommit *azCommitRef `json:"sourceRefCommit"`
			CommonRefCommit *azCommitRef `json:"commonRefCommit"`
		} `json:"value"`
	}
	if err := a.do(http.MethodGet, a.v(fmt.Sprintf("%s/pullRequests/%d/iterations", azGit(r), n)), nil, &its); err != nil {
		return 0, "", "", nil, err
	}
	for _, it := range its.Value {
		if it.ID > id {
			id = it.ID
			base, head = "", ""
			if it.SourceRefCommit != nil {
				head = it.SourceRefCommit.CommitID
			}
			if it.CommonRefCommit != nil {
				base = it.CommonRefCommit.CommitID
			}
		}
	}
	if id == 0 {
		return 0, "", "", nil, fmt.Errorf("no iterations of pull request %d on Azure DevOps", n)
	}
	for skip := 0; ; {
		var res struct {
			ChangeEntries []azChange `json:"changeEntries"`
			NextSkip      int        `json:"nextSkip"`
		}
		if err := a.do(http.MethodGet, a.v(fmt.Sprintf("%s/pullRequests/%d/iterations/%d/changes?$top=1000&$skip=%d", azGit(r), n, id, skip)), nil, &res); err != nil {
			return 0, "", "", nil, err
		}
		files = append(files, res.ChangeEntries...)
		if len(files) > maxDiffFiles {
			return id, base, head, files, fmt.Errorf("pull request %d: %w (over %d files)", n, ErrTooLarge, maxDiffFiles)
		}
		if res.NextSkip <= skip || len(res.ChangeEntries) == 0 {
			break
		}
		skip = res.NextSkip
	}
	return id, base, head, files, nil
}

// raw GETs path and returns at most max bytes (ErrTooLarge beyond).
func (a *azure) raw(path string, max int64) ([]byte, error) {
	resp, err := a.send(http.MethodGet, path, "application/octet-stream", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("%s GET %s: %w", a.name(), path, err)
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s GET %s: %w (over %d bytes)", a.name(), path, ErrTooLarge, max)
	}
	return data, nil
}

// file reads a version of a file: by its blob id, else by its path in a
// commit.
func (a *azure) file(r Repo, blob, path, commit string, max int64) ([]byte, error) {
	if blob != "" {
		return a.raw(a.v(fmt.Sprintf("%s/blobs/%s?$format=octetstream", azGit(r), url.PathEscape(blob))), max)
	}
	q := url.Values{"path": {path}, "versionDescriptor.versionType": {"commit"}, "versionDescriptor.version": {commit}, "download": {"true"}}
	return a.raw(a.v(azGit(r)+"/items?"+q.Encode()+"&$format=octetstream"), max)
}

// PullDiff builds a unified diff: Azure DevOps serves the changed files
// and their versions, not a diff.
func (a *azure) PullDiff(r Repo, n int, max int64) (string, error) {
	_, base, head, files, err := a.iteration(r, n)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, f := range files {
		if f.Item.IsFolder || f.Item.GitObjectType.is("tree", 2) || f.Item.GitObjectType.is("commit", 1) {
			continue
		}
		add, del, rename := f.kinds()
		newPath := strings.TrimPrefix(f.Item.Path, "/")
		oldPath := newPath
		if rename {
			if o := f.OriginalPath; o != "" {
				oldPath = strings.TrimPrefix(o, "/")
			} else if o := f.SourceServerItem; o != "" {
				oldPath = strings.TrimPrefix(o, "/")
			}
		}
		// A file too large to diff is shown like a binary one: only the
		// diff as a whole counts against max.
		var old, cur []byte
		big := false
		if !add {
			old, err = a.file(r, f.Item.OriginalObjectID, "/"+oldPath, base, min(max, maxDiffFile))
			big = big || errors.Is(err, ErrTooLarge)
			if err != nil && !big {
				return "", err
			}
		}
		if !del {
			cur, err = a.file(r, f.Item.ObjectID, "/"+newPath, head, min(max, maxDiffFile))
			big = big || errors.Is(err, ErrTooLarge)
			if err != nil && !errors.Is(err, ErrTooLarge) {
				return "", err
			}
		}
		from, to := "a/"+oldPath, "b/"+newPath
		fmt.Fprintf(&b, "diff --git %s %s\n", from, to)
		if add {
			from = "/dev/null"
		}
		if del {
			to = "/dev/null"
		}
		if big || isBinary(old) || isBinary(cur) {
			fmt.Fprintf(&b, "Binary files %s and %s differ\n", from, to)
		} else if h := unifiedHunks(string(old), string(cur)); h != "" {
			fmt.Fprintf(&b, "--- %s\n+++ %s\n%s", from, to, h)
		}
		if int64(b.Len()) > max {
			return "", fmt.Errorf("pull request %d: %w (over %d bytes)", n, ErrTooLarge, max)
		}
	}
	return b.String(), nil
}

// isBinary: git's test, a NUL byte in the first 8000.
func isBinary(b []byte) bool {
	return strings.IndexByte(string(b[:min(len(b), 8000)]), 0) >= 0
}

func (a *azure) CommentReview(r Repo, n int, headSHA, body string, comments []InlineComment) (string, error) {
	p, err := a.pr(r, n)
	if err != nil {
		return "", err
	}
	if p.LastSource == nil || p.LastSource.CommitID == "" {
		return "", fmt.Errorf("no head commit for %s on Azure DevOps", r.Ref(n))
	}
	if headSHA != "" && p.LastSource.CommitID != headSHA {
		return "", fmt.Errorf("%s has new commits since its diff was read; review it again", r.Ref(n))
	}
	tracking := map[string]int{}
	iter := 0
	if len(comments) > 0 {
		id, _, _, files, err := a.iteration(r, n)
		if err != nil && !errors.Is(err, ErrTooLarge) {
			return "", err
		}
		iter = id
		for _, f := range files {
			tracking[strings.TrimPrefix(f.Item.Path, "/")] = f.ChangeTrackingID
		}
	}
	var missed []InlineComment
	for _, c := range comments {
		pos := map[string]any{"line": c.Line, "offset": 1}
		ctx := map[string]any{"filePath": "/" + c.Path, "rightFileStart": pos, "rightFileEnd": pos}
		var prCtx map[string]any
		if id, ok := tracking[c.Path]; ok && iter > 0 {
			prCtx = map[string]any{"changeTrackingId": id, "iterationContext": map[string]int{"firstComparingIteration": 1, "secondComparingIteration": iter}}
		}
		_, err := a.thread(r, n, c.Body, "active", ctx, prCtx)
		if s := status(err); s == 401 || s == 403 || (err != nil && s == 0) {
			return "", err
		}
		if err != nil {
			missed = append(missed, c) // Azure DevOps could not place it on the line
		}
	}
	if len(missed) > 0 {
		var b strings.Builder
		b.WriteString(body)
		b.WriteString("\n\nComments Azure DevOps could not place on their line:\n\n")
		for _, c := range missed {
			fmt.Fprintf(&b, "- `%s:%d`: %s\n", strings.ReplaceAll(c.Path, "`", "'"), c.Line, strings.ReplaceAll(c.Body, "\n", "\n  "))
		}
		body = b.String()
	}
	id, err := a.thread(r, n, body, "", nil, nil)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s?discussionId=%d", p.pull(r).URL, id), nil
}

// Work item comment text.

var (
	reAzCode  = regexp.MustCompile("`([^`\n]+)`")
	reAzLink  = regexp.MustCompile("https?://[^\\s<>\"'&`]+")
	reAzBreak = regexp.MustCompile(`(?i)<br\s*/?>|</(?:p|div|li|h[1-6]|tr|pre|blockquote)\s*>`)
	reAzItem  = regexp.MustCompile(`(?i)<li(?:\s[^>]*)?>`)
	reAzTag   = regexp.MustCompile(`(?s)<!--.*?-->|<[^>]*>`)
	reAzBlank = regexp.MustCompile(`\n[ \t]*\n(?:[ \t]*\n)+`)
)

// azHTML is rw's text (Markdown) as the HTML of a work item comment:
// everything is escaped, so nothing in it is markup, a mention or a hidden
// comment; code spans, http(s) links and line breaks are kept.
func azHTML(s string) string {
	s = html.EscapeString(s)
	s = reAzLink.ReplaceAllStringFunc(s, func(u string) string {
		link := strings.TrimRight(u, ".,;:!?)")
		return `<a href="` + link + `">` + link + `</a>` + u[len(link):]
	})
	s = reAzCode.ReplaceAllString(s, "<code>$1</code>")
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "<br>")
}

// azText is HTML from Azure DevOps (a work item's text, a comment) as
// plain text: line breaks kept, tags and HTML comments dropped, entities
// decoded.
func azText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = reAzBreak.ReplaceAllString(s, "\n")
	s = reAzItem.ReplaceAllString(s, "- ")
	s = reAzTag.ReplaceAllString(s, "")
	s = strings.ReplaceAll(html.UnescapeString(s), "\u00a0", " ")
	s = reAzBlank.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
