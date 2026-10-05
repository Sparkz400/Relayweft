// Package forge is what rw pr, issues as tasks, rw watch and rw review
// need from a code host, for GitHub (and GitHub Enterprise), GitLab
// (gitlab.com and self-managed), Gitea or Forgejo (Codeberg and
// self-hosted) and Azure DevOps (dev.azure.com, *.visualstudio.com and
// Azure DevOps Server): read issues (work items on Azure DevOps), open a
// pull request (a merge request on GitLab), comment, read a pull request's
// state, diff, failed CI jobs and review comments, and post a comment-only
// review.
//
// The host of the origin remote decides the forge: github.com, gitlab.com,
// codeberg.org and dev.azure.com are known; a self-hosted one is named in
// GH_HOST, GITLAB_HOST, GITEA_HOST or AZURE_DEVOPS_HOST (or by an explicit
// --api URL). A token is only ever sent to a host of its own kind.
//
// A forge's web and API live at https://<host> unless a URL says
// otherwise: GITEA_HOST=http://localhost:3000 (scheme, port or a path
// prefix), an https remote with a port (https://git.example.com:8443/o/r),
// or a plain-http remote on this machine (http://localhost:3000/o/r).
// Plain http to another machine is used only when a variable names it.
package forge

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/gh"
)

// Kind is a forge's API family.
type Kind string

const (
	GitHub Kind = "github"
	GitLab Kind = "gitlab"
	Gitea  Kind = "gitea" // also Forgejo (Codeberg): the same API
	Azure  Kind = "azure" // Azure DevOps Services and Server
)

// ParseKind reads a stored kind; "" (entries from before GitLab and Gitea
// support) is GitHub.
func ParseKind(s string) Kind {
	switch Kind(strings.ToLower(s)) {
	case GitLab:
		return GitLab
	case Gitea, "forgejo":
		return Gitea
	case Azure:
		return Azure
	}
	return GitHub
}

// Name is the forge's name for people.
func (k Kind) Name() string {
	switch k {
	case GitLab:
		return "GitLab"
	case Gitea:
		return "Gitea"
	case Azure:
		return "Azure DevOps"
	}
	return "GitHub"
}

// ForgeName is the name of r's forge for people: Forgejo for a Gitea-API
// forge that is one (codeberg.org, or a host in FORGEJO_HOST), else the
// kind's name.
func (r Repo) ForgeName() string {
	if r.Kind != Gitea {
		return r.Kind.Name()
	}
	host := hostName(r.Host)
	if host == "codeberg.org" || slices.Contains(envHostList("FORGEJO_HOST"), host) {
		return "Forgejo"
	}
	if server, forgejo := actionsServer(); forgejo && hostName(server) == host {
		return "Forgejo"
	}
	return r.Kind.Name()
}

// PullNoun is what the forge calls a pull request.
func (k Kind) PullNoun() string {
	if k == GitLab {
		return "merge request"
	}
	return "pull request"
}

// TokenHint says where a token for this forge comes from.
func (k Kind) TokenHint() string {
	switch k {
	case GitLab:
		return "GITLAB_TOKEN, GITLAB_ACCESS_TOKEN or `glab auth login`"
	case Gitea:
		return "GITEA_TOKEN or FORGEJO_TOKEN"
	case Azure:
		return "AZURE_DEVOPS_TOKEN (a personal access token or a Microsoft Entra token) or AZURE_DEVOPS_EXT_PAT"
	}
	return "GITHUB_TOKEN, GH_TOKEN or `gh auth login`"
}

// Repo is a repository on a forge. On GitLab, Owner is the whole
// namespace ("group/subgroup"); on Azure DevOps it is the organization
// (the collection on a server) and the project ("org/project").
type Repo struct {
	Kind  Kind
	Host  string
	Owner string
	Name  string
	// Web is the forge's root URL when it is not https://<Host>: another
	// scheme or port, or a path prefix (http://localhost:3000,
	// https://example.com/gitlab). "" means https://<Host>.
	Web string
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// IsZero reports whether r is unset.
func (r Repo) IsZero() bool { return r.Owner == "" && r.Name == "" }

// Same reports whether r and o name the same repository.
func (r Repo) Same(o Repo) bool {
	return strings.EqualFold(r.Host, o.Host) && strings.EqualFold(r.Owner, o.Owner) && strings.EqualFold(r.Name, o.Name)
}

// Ref is how the forge writes pull request n of r: o/r#12, or g/p!12 on
// GitLab.
func (r Repo) Ref(n int) string { return r.String() + r.PullSign() + strconv.Itoa(n) }

// PullSign is the sign before a pull request's number: ! on GitLab and
// Azure DevOps, else #.
func (r Repo) PullSign() string {
	if r.Kind == GitLab || r.Kind == Azure {
		return "!"
	}
	return "#"
}

// root is the forge's root URL, without a trailing slash.
func (r Repo) root() string {
	if r.Web != "" {
		return r.Web
	}
	return "https://" + r.Host
}

// WebURL is the repository's page.
func (r Repo) WebURL() string {
	if r.Kind == Azure {
		return r.azWeb()
	}
	return r.root() + "/" + r.Owner + "/" + r.Name
}

// CompareURL opens the forge's "new pull request" page for branch against
// base.
func (r Repo) CompareURL(base, branch string) string {
	switch r.Kind {
	case GitLab:
		q := url.Values{"merge_request[source_branch]": {branch}, "merge_request[target_branch]": {base}}
		return r.WebURL() + "/-/merge_requests/new?" + q.Encode()
	case Gitea:
		return r.WebURL() + "/compare/" + escapeRef(base) + "..." + escapeRef(branch)
	case Azure:
		q := url.Values{"sourceRef": {branch}, "targetRef": {base}}
		return r.WebURL() + "/pullrequestcreate?" + q.Encode()
	}
	if r.Web != "" {
		return r.WebURL() + "/compare/" + escapeRef(base) + "..." + escapeRef(branch) + "?expand=1"
	}
	return r.gh().CompareURL(base, branch)
}

func escapeRef(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// APIBase is the REST endpoint for the repository's host.
func (r Repo) APIBase() string {
	switch r.Kind {
	case GitLab:
		return r.root() + "/api/v4"
	case Gitea:
		return r.root() + "/api/v1"
	case Azure:
		org, _ := r.azParts()
		return r.root() + "/" + url.PathEscape(org)
	}
	if r.Web != "" {
		return r.Web + "/api/v3" // GitHub Enterprise
	}
	return r.gh().APIBase()
}

func (r Repo) gh() gh.Repo { return gh.Repo{Host: r.Host, Owner: r.Owner, Name: r.Name} }

// APIServes reports whether the API base URL api belongs to host: the
// same host name (any port), or api.github.com for github.com. A token is
// for one host, so a client for api only gets the token of a host it
// serves.
func APIServes(api, host string) bool { return gh.APIServes(api, host) }

// KindOfAPI guesses the forge from an API base URL: /api/v4 is GitLab,
// /api/v1 Gitea, dev.azure.com Azure DevOps, anything else GitHub.
func KindOfAPI(api string) Kind {
	u, err := url.Parse(api)
	if err != nil {
		return GitHub
	}
	if isAzureCloud(strings.ToLower(u.Hostname())) {
		return Azure
	}
	p := strings.TrimRight(u.Path, "/")
	switch {
	case strings.HasSuffix(p, "/api/v4"):
		return GitLab
	case strings.HasSuffix(p, "/api/v1"):
		return Gitea
	}
	return GitHub
}

// Hosts maps self-hosted forge host names (lower case, no port) to their
// kind and, when given as a URL, their root URL. github.com, gitlab.com
// and codeberg.org need no entry.
type Hosts map[string]HostInfo

// HostInfo is what is known about a configured forge host.
type HostInfo struct {
	Kind Kind
	// Web is the root URL a variable gave (http://localhost:3000), "" for
	// https://<host>.
	Web string
}

// EnvHosts reads GH_HOST (GitHub Enterprise), GITLAB_HOST, GITEA_HOST
// (or FORGEJO_HOST) and AZURE_DEVOPS_HOST (Azure DevOps Server). Each may
// be a host name or a URL (which also gives the scheme, port and path
// prefix: http://localhost:3000, https://tfs.example.com/tfs); all but
// GH_HOST may list several, separated by commas. In a Forgejo or Gitea
// Actions job, the job's server counts as named in GITEA_HOST.
func EnvHosts() Hosts {
	h := Hosts{}
	if e := gh.EnterpriseHost(); e != "" {
		h.add(e, GitHub)
	}
	for _, x := range envHostValues("GITLAB_HOST") {
		h.add(x, GitLab)
	}
	for _, x := range giteaHostValues() {
		h.add(x, Gitea)
	}
	for _, x := range envHostValues("AZURE_DEVOPS_HOST") {
		h.add(x, Azure)
	}
	return h
}

// actionsServer is the root URL of the forge a Forgejo or Gitea Actions
// job runs on, "" outside one. Their runners set GITEA_ACTIONS (Forgejo's
// also FORGEJO_ACTIONS) and the server in GITHUB_SERVER_URL (Forgejo's
// also FORGEJO_SERVER_URL); GitHub Actions sets neither flag.
func actionsServer() (server string, forgejo bool) {
	forgejo = os.Getenv("FORGEJO_ACTIONS") == "true"
	if !forgejo && os.Getenv("GITEA_ACTIONS") != "true" {
		return "", false
	}
	for _, v := range []string{"FORGEJO_SERVER_URL", "GITEA_SERVER_URL", "GITHUB_SERVER_URL"} {
		if s := strings.TrimSpace(os.Getenv(v)); s != "" {
			return s, forgejo
		}
	}
	return "", false
}

// giteaHostValues are the Gitea and Forgejo hosts: GITEA_HOST,
// FORGEJO_HOST and the server of a Forgejo or Gitea Actions job.
func giteaHostValues() []string {
	out := append(envHostValues("GITEA_HOST"), envHostValues("FORGEJO_HOST")...)
	if s, _ := actionsServer(); s != "" {
		out = append(out, s)
	}
	return out
}

// envHostValues are the entries of a host variable, as written.
func envHostValues(v string) []string {
	return strings.FieldsFunc(os.Getenv(v), func(r rune) bool { return r == ',' || r == ' ' || r == ';' })
}

// envHostList are the host names of a host variable.
func envHostList(v string) []string {
	var out []string
	for _, s := range envHostValues(v) {
		if n := hostName(s); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// webOf is the root URL a host entry given as a URL names, "" when it is a
// bare host name or https://<host> itself.
func webOf(s string) string {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "://") {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	return canonWeb(scheme, u.Host, u.Path)
}

// canonWeb writes a root URL, "" when it is https://<host>.
func canonWeb(scheme, hostport, path string) string {
	hostport = strings.ToLower(hostport)
	if scheme == "https" {
		hostport = strings.TrimSuffix(hostport, ":443")
	} else {
		hostport = strings.TrimSuffix(hostport, ":80")
	}
	w := scheme + "://" + hostport + strings.TrimRight(path, "/")
	if w == "https://"+hostName(hostport) {
		return ""
	}
	return w
}

// remoteWeb is the root URL a remote's own URL implies: an https remote
// with a port serves the web there too, and so does a plain-http remote on
// this machine (a token sent there never leaves it). "" means
// https://<host>: for ssh remotes, and for plain http to another machine
// (a token goes out in clear text only where a variable says so).
func remoteWeb(remote string) string {
	s := strings.TrimSpace(remote)
	if !strings.Contains(s, "://") {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return canonWeb("https", u.Host, "")
	case "http":
		if isLoopback(u.Hostname()) {
			return canonWeb("http", u.Host, "")
		}
	}
	return ""
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hostName is the host of "host", "host:port" or a URL, lower case.
func hostName(s string) string {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil {
			return strings.ToLower(u.Hostname())
		}
		return ""
	}
	s, _, _ = strings.Cut(s, "/")
	if h, _, ok := strings.Cut(s, ":"); ok {
		s = h
	}
	return strings.ToLower(s)
}

// add records entry (a host name or a URL) as kind k, unless its host is
// there already.
func (h Hosts) add(entry string, k Kind) {
	host := hostName(entry)
	if _, ok := h[host]; !ok && host != "" {
		h[host] = HostInfo{Kind: k, Web: webOf(entry)}
	}
}

// With returns h plus host (a host name or a URL) as kind k, unless the
// host is known already.
func (h Hosts) With(host string, k Kind) Hosts {
	out := Hosts{}
	for x, y := range h {
		out[x] = y
	}
	if _, known := out.Kind(host); !known {
		out.add(host, k)
	}
	return out
}

// web is the root URL configured for host ("" = none).
func (h Hosts) web(host string) string {
	host = hostName(host)
	if isDotComHost(host) {
		return ""
	}
	return h[host].Web
}

// prefix is the path a forge on host lives under ("gitlab" for
// https://example.com/gitlab), "" for none.
func (h Hosts) prefix(host string) string {
	w := h.web(host)
	if w == "" {
		return ""
	}
	u, err := url.Parse(w)
	if err != nil {
		return ""
	}
	return strings.Trim(u.Path, "/")
}

func isDotComHost(host string) bool { return host == "github.com" || host == "www.github.com" }

// Kind is the forge of host. github.com is always GitHub and
// dev.azure.com always Azure DevOps; then come the configured hosts, then
// gitlab.com and codeberg.org.
func (h Hosts) Kind(host string) (Kind, bool) {
	host = hostName(host)
	if isDotComHost(host) {
		return GitHub, true
	}
	if isAzureCloud(host) {
		return Azure, true
	}
	if i, ok := h[host]; ok {
		return i.Kind, true
	}
	switch host {
	case "gitlab.com", "www.gitlab.com":
		return GitLab, true
	case "codeberg.org":
		return Gitea, true
	}
	return "", false
}

// remoteParts splits a git remote URL into host and path: https
// (https://host/o/r[.git]), scp-style ssh (git@host:o/r.git) and ssh:// or
// git:// URLs.
func remoteParts(remote string) (host, path string, err error) {
	s := strings.TrimSpace(remote)
	if s == "" {
		return "", "", errors.New("empty remote URL")
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", "", fmt.Errorf("remote %q: %w", remote, err)
		}
		switch strings.ToLower(u.Scheme) {
		case "https", "http", "ssh", "git", "git+ssh", "ssh+git":
		default:
			return "", "", fmt.Errorf("remote %q: unsupported scheme %s", remote, u.Scheme)
		}
		host, path = u.Hostname(), u.Path
	} else if at := strings.Index(s, ":"); at > 1 && !strings.ContainsAny(s[:at], `/\`) {
		// scp-like: [user@]host:owner/repo.git (at > 1 keeps C:\ paths out)
		host, path = s[:at], s[at+1:]
		if j := strings.LastIndex(host, "@"); j >= 0 {
			host = host[j+1:]
		}
	} else {
		return "", "", fmt.Errorf("remote %q is not a URL of a GitHub, GitLab, Gitea or Azure DevOps repository", remote)
	}
	if host == "" {
		return "", "", fmt.Errorf("remote %q has no host", remote)
	}
	return host, path, nil
}

// ParseRemote reads the forge, owner and name from a git remote URL.
// Hosts other than github.com, gitlab.com and codeberg.org must be in
// hosts. Web is the host's configured URL, else what the remote implies
// (remoteWeb).
func ParseRemote(remote string, hosts Hosts) (Repo, error) {
	r, err := parseRemote(remote, hosts)
	if err != nil || isDotComHost(r.Host) || isAzureCloud(r.Host) {
		return r, err
	}
	if r.Web = hosts.web(r.Host); r.Web == "" {
		r.Web = remoteWeb(remote)
	}
	return r, nil
}

func parseRemote(remote string, hosts Hosts) (Repo, error) {
	host, path, err := remoteParts(remote)
	if err != nil {
		return Repo{}, err
	}
	kind, ok := hosts.Kind(host)
	if !ok {
		return Repo{}, fmt.Errorf("remote %q is not on github.com, gitlab.com or codeberg.org (for a self-hosted forge set GH_HOST, GITLAB_HOST or GITEA_HOST=%s; AZURE_DEVOPS_HOST for Azure DevOps Server)", remote, strings.ToLower(host))
	}
	if kind == Azure {
		return parseAzureRemote(remote, host, path, hosts)
	}
	if kind == GitHub {
		r, err := gh.ParseRemote(remote, host)
		if err != nil {
			return Repo{}, err
		}
		return Repo{Kind: GitHub, Host: r.Host, Owner: r.Owner, Name: r.Name}, nil
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if pre := hosts.prefix(host); pre != "" {
		// A forge under a path (https://example.com/gitlab/g/p.git).
		path = strings.TrimPrefix(path, pre+"/")
	}
	parts := strings.Split(path, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || p == "-" {
			return Repo{}, fmt.Errorf("remote %q: want <host>/<owner>/<repo>", remote)
		}
	}
	// GitLab namespaces nest (group/subgroup/project); the others do not.
	if len(parts) < 2 || (kind != GitLab && len(parts) != 2) {
		return Repo{}, fmt.Errorf("remote %q: want <host>/<owner>/<repo>", remote)
	}
	n := len(parts) - 1
	return Repo{Kind: kind, Host: hostName(host), Owner: strings.Join(parts[:n], "/"), Name: parts[n]}, nil
}

// Ref is an issue or pull request given as a number ("12", "#12", "!12")
// or a URL. Repo is zero for a bare number (meaning: the current
// repository).
type Ref struct {
	Repo   Repo
	Number int
}

// ParseIssueRef reads an issue number or URL: https://github.com/o/r/issues/12,
// https://gitlab.com/g/p/-/issues/12, https://codeberg.org/o/r/issues/12,
// or an Azure DevOps work item, https://dev.azure.com/org/p/_workitems/edit/12
// (its Repo has no Name: a work item belongs to a project).
func ParseIssueRef(s string, hosts Hosts) (Ref, error) {
	return parseRef(s, hosts, "issue", func(k Kind) string { return "issues" })
}

// ParsePullRef reads a pull request number or URL: .../o/r/pull/12 on
// GitHub (also .../pull/12/files), .../g/p/-/merge_requests/12 on GitLab,
// .../o/r/pulls/12 on Gitea, .../org/p/_git/r/pullrequest/12 on Azure
// DevOps.
func ParsePullRef(s string, hosts Hosts) (Ref, error) {
	return parseRef(s, hosts, "pull request", func(k Kind) string {
		switch k {
		case GitLab:
			return "merge_requests"
		case Gitea:
			return "pulls"
		}
		return "pull"
	})
}

func parseRef(s string, hosts Hosts, what string, segment func(Kind) string) (Ref, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(strings.TrimLeft(s, "#!")); err == nil && len(s)-len(strings.TrimLeft(s, "#!")) <= 1 {
		if n <= 0 {
			return Ref{}, fmt.Errorf("%s number must be positive: %q", what, s)
		}
		return Ref{Number: n}, nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return Ref{}, fmt.Errorf("%s %q: want a number or a %s URL", what, s, what)
	}
	kind, ok := hosts.Kind(u.Hostname())
	if !ok {
		return Ref{}, fmt.Errorf("%s %q is not on github.com, gitlab.com or codeberg.org (for a self-hosted forge set GH_HOST, GITLAB_HOST or GITEA_HOST=%s; AZURE_DEVOPS_HOST for Azure DevOps Server)", what, s, strings.ToLower(u.Hostname()))
	}
	if kind == Azure {
		return parseAzureRef(u, hosts, what)
	}
	path := strings.Trim(u.Path, "/")
	pre := hosts.prefix(u.Hostname())
	if rest, ok := strings.CutPrefix(path, pre+"/"); pre != "" && ok {
		path = rest
	} else {
		pre = ""
	}
	parts := strings.Split(path, "/")
	seg := segment(kind)
	// The repository's path ends where the kind's segment starts: GitLab
	// puts a "-" before it, and its namespaces nest.
	at := 2
	if kind == GitLab {
		at = -1
		for i, p := range parts {
			if p == "-" {
				at = i
				break
			}
		}
	}
	idx := at
	if kind == GitLab && at >= 0 {
		idx = at + 1
	}
	if at < 2 || idx+1 >= len(parts) || parts[idx] != seg {
		return Ref{}, fmt.Errorf("%s %q: want a %s URL", what, s, exampleURL(kind, seg))
	}
	n, err := strconv.Atoi(parts[idx+1])
	if err != nil || n <= 0 {
		return Ref{}, fmt.Errorf("%s %q: bad number", what, s)
	}
	if pre != "" {
		pre += "/"
	}
	r, err := ParseRemote(u.Scheme+"://"+u.Host+"/"+pre+strings.Join(parts[:at], "/"), hosts)
	if err != nil {
		return Ref{}, err
	}
	return Ref{Repo: r, Number: n}, nil
}

func exampleURL(k Kind, seg string) string {
	switch k {
	case GitLab:
		return "https://<host>/<group>/<project>/-/" + seg + "/<n>"
	case Gitea:
		return "https://<host>/<owner>/<repo>/" + seg + "/<n>"
	}
	return "https://github.com/<owner>/<repo>/" + seg + "/<n>"
}

// Issue is an issue.
type Issue struct {
	Number  int
	Title   string
	Body    string
	State   string // open or closed
	URL     string
	Labels  []string
	Author  string
	Created time.Time
	IsPull  bool // GitHub and Gitea list pull requests as issues too
}

// SameIssues reports whether r and o share one issue tracker: the same
// repository, or on Azure DevOps the same organization (work item numbers
// count per organization, not per repository).
func (r Repo) SameIssues(o Repo) bool {
	if r.Kind == Azure && o.Kind == Azure && strings.EqualFold(r.Host, o.Host) {
		a, _ := r.azParts()
		b, _ := o.azParts()
		return a != "" && strings.EqualFold(a, b)
	}
	return r.Same(o)
}

// Comment is a comment on an issue.
type Comment struct {
	ID      int64
	Author  string
	Body    string
	Created time.Time
	// who is the forge's view of the author, for Client.CommentTrusted.
	who commentAuthor
}

// commentAuthor is what a forge needs to tell whether a comment's author
// is trusted.
type commentAuthor struct {
	id    int64
	assoc string // GitHub's author_association
	typ   string // GitHub's user type (Bot)
	bot   bool
}

// Pull is a pull request (a merge request on GitLab).
type Pull struct {
	Number int
	Title  string
	Body   string
	URL    string
	State  string // open or closed (merged ones are closed too)
	Merged bool
	Draft  bool
	// HeadRef and HeadSHA are the branch and its commit; HeadRepo is the
	// head branch's repository as owner/name ("" when unknown: a fork that
	// was deleted, or another GitLab project).
	HeadRef  string
	HeadSHA  string
	HeadRepo string
	BaseRef  string
}

// NewPull is a pull request to open; Head is a branch of the repository.
type NewPull struct {
	Title string
	Head  string
	Base  string
	Body  string
	Draft bool
}

// Check is a CI check or job on a commit that failed in a way a code
// change can fix.
type Check struct {
	ID         string // the forge's id, stable for this run of the check
	Name       string
	Conclusion string
	Output     string // the check's own summary, if any
	Log        string // the tail of its log, when the forge serves it
}

// Feedback is something a reviewer wrote on a pull request: a review that
// requests changes, or an inline comment.
type Feedback struct {
	ID     string // review:<id> or comment:<id>
	Review bool   // a review requesting changes (else an inline comment)
	Author string
	// Trusted: the author may direct work on the repository (owner,
	// member, collaborator or developer; never a bot).
	Trusted  bool
	Body     string
	Path     string
	Line     int // in the new version; 0 = not about one line
	DiffHunk string
}

// InlineComment is a review comment on one line of the pull request's new
// version.
type InlineComment struct {
	Path string
	Line int
	// OldLine is the same line in the old version when the diff shows it
	// as context, 0 for an added line (GitLab needs both for context).
	OldLine int
	Body    string
}

// Client talks to one forge's API.
type Client interface {
	Kind() Kind
	// HasToken reports whether requests carry a token the forge accepted
	// so far.
	HasToken() bool
	// Rejected reports whether the token got a 401, so later reads went
	// on without it (and a private repository then looks like a 404).
	Rejected() bool

	DefaultBranch(r Repo) (string, error)
	Issue(r Repo, n int) (*Issue, error)
	// Comments are an issue's comments, oldest first.
	Comments(r Repo, n int) ([]Comment, error)
	// CommentTrusted reports whether a comment's author may direct work on
	// the repository, as Feedback.Trusted (GitLab and Gitea ask the forge,
	// once per author).
	CommentTrusted(r Repo, c Comment) (bool, error)
	// OpenIssues are the open issues with the label, oldest first, without
	// pull requests; at most max (0 = up to the page cap).
	OpenIssues(r Repo, label string, max int) ([]Issue, error)
	OpenPulls(r Repo) ([]Pull, error)
	CreatePull(r Repo, p NewPull) (*Pull, error)
	CommentIssue(r Repo, n int, body string) error
	// EditComment replaces the text of comment id (Comment.ID) on issue n.
	EditComment(r Repo, n int, id int64, body string) error

	// Pull reads one pull request.
	Pull(r Repo, n int) (*Pull, error)
	// PullDiff is its unified diff, at most max bytes (ErrTooLarge).
	PullDiff(r Repo, n int, max int64) (string, error)
	// FailedChecks are the failed checks of a commit, with up to logTail
	// bytes of each one's log where the forge serves it.
	FailedChecks(r Repo, sha string, logTail int) ([]Check, error)
	// Feedback is what reviewers asked for on a pull request, oldest
	// first: reviews requesting changes and unresolved inline comments.
	Feedback(r Repo, n int) ([]Feedback, error)
	CommentPull(r Repo, n int, body string) error
	// Viewer is the login of the token's owner.
	Viewer() (string, error)
	// CommentReview posts a review that only comments (never approves or
	// requests changes), with inline comments, on the head commit. It
	// returns a link to it.
	CommentReview(r Repo, n int, headSHA, body string, comments []InlineComment) (string, error)
}

// New returns a client for the API base URL api. notes receives one-line
// notices (a rejected token); nil discards them.
func New(kind Kind, api, token string, notes io.Writer) Client {
	switch kind {
	case GitLab:
		return newGitLab(api, token, notes)
	case Gitea:
		return newGitea(api, token, notes)
	case Azure:
		return newAzure(api, token, notes)
	}
	c := gh.NewClient(api, token)
	c.Notes = notes
	return &githubClient{c: c}
}

// maxPages caps paginated listings.
const maxPages = 5

// ErrTooLarge means a text answer was longer than the caller allows.
var ErrTooLarge = gh.ErrTooLarge

// APIError is a non-2xx answer from GitLab, Gitea or Azure DevOps
// (GitHub's are *gh.APIError).
type APIError struct {
	Forge   Kind
	Status  int
	Method  string
	Path    string
	Message string
	Hint    string
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("%s %s %s: %d", e.Forge.Name(), e.Method, e.Path, e.Status)
	if e.Message != "" {
		s += " " + e.Message
	}
	if e.Hint != "" {
		s += ". " + e.Hint
	}
	return s
}

func status(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	var ge *gh.APIError
	if errors.As(err, &ge) {
		return ge.Status
	}
	return 0
}

// IsNotFound reports a 404 from the forge.
func IsNotFound(err error) bool { return status(err) == 404 }

// IsUnauthorized reports a 401 from the forge.
func IsUnauthorized(err error) bool { return status(err) == 401 }
