// Package gh is the small part of the GitHub REST API Relayweft uses
// (through package forge, next to GitLab and Gitea):
// read issues, list open pull requests, open a pull request and comment,
// and watch and review pull requests (pulls.go).
// It needs no gh CLI; a token comes from GITHUB_TOKEN, GH_TOKEN or, when
// installed, `gh auth token`.
package gh

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Repo is a GitHub repository.
type Repo struct {
	Host  string // github.com or a GitHub Enterprise host
	Owner string
	Name  string
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// IsZero reports whether r is unset.
func (r Repo) IsZero() bool { return r.Owner == "" && r.Name == "" }

// Same reports whether r and o name the same repository.
func (r Repo) Same(o Repo) bool {
	return strings.EqualFold(r.Host, o.Host) && strings.EqualFold(r.Owner, o.Owner) && strings.EqualFold(r.Name, o.Name)
}

// WebURL is the repository's page.
func (r Repo) WebURL() string { return "https://" + r.Host + "/" + r.Owner + "/" + r.Name }

// CompareURL opens GitHub's "new pull request" page for branch against base.
func (r Repo) CompareURL(base, branch string) string {
	return r.WebURL() + "/compare/" + escapeRef(base) + "..." + escapeRef(branch) + "?expand=1"
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
	if isDotCom(r.Host) {
		return "https://api.github.com"
	}
	return "https://" + r.Host + "/api/v3"
}

// APIServes reports whether the API base URL api belongs to host: the
// same host name (any port), or api.github.com for github.com. A token is
// for one host, so a client for api only gets the token of a host it
// serves.
func APIServes(api, host string) bool {
	u, err := url.Parse(api)
	if err != nil || u.Hostname() == "" || host == "" {
		return false
	}
	h := u.Hostname()
	if isDotCom(host) {
		return strings.EqualFold(h, "api.github.com") || isDotCom(h)
	}
	return strings.EqualFold(h, host)
}

func isDotCom(host string) bool {
	h := strings.ToLower(host)
	return h == "github.com" || h == "www.github.com"
}

// ParseRemote reads owner and name from a git remote URL: https
// (https://github.com/o/r[.git]), scp-style ssh (git@github.com:o/r.git)
// and ssh:// or git:// URLs. Only github.com is accepted, plus
// enterpriseHost (GH_HOST) when it is set.
func ParseRemote(remote, enterpriseHost string) (Repo, error) {
	s := strings.TrimSpace(remote)
	if s == "" {
		return Repo{}, errors.New("empty remote URL")
	}
	var host, path string
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return Repo{}, fmt.Errorf("remote %q: %w", remote, err)
		}
		switch strings.ToLower(u.Scheme) {
		case "https", "http", "ssh", "git", "git+ssh", "ssh+git":
		default:
			return Repo{}, fmt.Errorf("remote %q: unsupported scheme %s", remote, u.Scheme)
		}
		host, path = u.Hostname(), u.Path
	} else if at := strings.Index(s, ":"); at > 1 && !strings.ContainsAny(s[:at], `/\`) {
		// scp-like: [user@]host:owner/repo.git (at > 1 keeps C:\ paths out)
		host, path = s[:at], s[at+1:]
		if j := strings.LastIndex(host, "@"); j >= 0 {
			host = host[j+1:]
		}
	} else {
		return Repo{}, fmt.Errorf("remote %q is not a GitHub URL", remote)
	}
	if host == "" {
		return Repo{}, fmt.Errorf("remote %q has no host", remote)
	}
	if !isDotCom(host) && !strings.EqualFold(host, enterpriseHost) {
		hint := ""
		if enterpriseHost == "" {
			hint = " (for GitHub Enterprise set GH_HOST=" + host + ")"
		}
		return Repo{}, fmt.Errorf("remote %q is not on github.com%s", remote, hint)
	}
	if isDotCom(host) {
		host = "github.com"
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Repo{}, fmt.Errorf("remote %q: want <host>/<owner>/<repo>", remote)
	}
	return Repo{Host: strings.ToLower(host), Owner: parts[0], Name: parts[1]}, nil
}

// EnterpriseHost is GH_HOST unless it is empty or names github.com.
func EnterpriseHost() string {
	h := strings.TrimSpace(os.Getenv("GH_HOST"))
	if isDotCom(h) {
		return ""
	}
	return h
}

// IssueRef is an issue given as "12", "#12" or an issue URL. Repo is zero
// for a bare number (meaning: the current repository).
type IssueRef struct {
	Repo   Repo
	Number int
}

// ParseIssueRef reads an issue number or URL
// (https://github.com/o/r/issues/12).
func ParseIssueRef(s, enterpriseHost string) (IssueRef, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(strings.TrimPrefix(s, "#")); err == nil {
		if n <= 0 {
			return IssueRef{}, fmt.Errorf("issue number must be positive: %q", s)
		}
		return IssueRef{Number: n}, nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return IssueRef{}, fmt.Errorf("issue %q: want a number or an issue URL", s)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "issues" {
		return IssueRef{}, fmt.Errorf("issue %q: want https://github.com/<owner>/<repo>/issues/<n>", s)
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return IssueRef{}, fmt.Errorf("issue %q: bad issue number", s)
	}
	r, err := ParseRemote(u.Scheme+"://"+u.Host+"/"+parts[0]+"/"+parts[1], enterpriseHost)
	if err != nil {
		return IssueRef{}, err
	}
	return IssueRef{Repo: r, Number: n}, nil
}
