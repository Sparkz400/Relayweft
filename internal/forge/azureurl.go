package forge

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Azure DevOps URLs. A repository is <org>/<project>/_git/<repo> on
// dev.azure.com, <project>/_git/<repo> on <org>.visualstudio.com (the old
// name, optionally after DefaultCollection) and
// [<prefix>/]<collection>/<project>/_git/<repo> on Azure DevOps Server
// (AZURE_DEVOPS_HOST=https://tfs.example.com/tfs gives the prefix). SSH
// remotes are v3/<org>/<project>/<repo> on ssh.dev.azure.com and
// vs-ssh.visualstudio.com, and like the web form on a server. When the
// repository is named like its project, the project may be left out
// (<org>/_git/<repo>).
//
// Every form becomes Host dev.azure.com (a server keeps its host) with
// Owner "<org>/<project>", so the token for dev.azure.com is the one used
// for the old *.visualstudio.com names too.

// azureCloud is the host of Azure DevOps Services.
const azureCloud = "dev.azure.com"

// isAzureCloud reports whether host (lower case) is Azure DevOps Services:
// dev.azure.com, ssh.dev.azure.com or a *.visualstudio.com name.
func isAzureCloud(host string) bool {
	return host == azureCloud || host == "ssh."+azureCloud || strings.HasSuffix(host, ".visualstudio.com")
}

// azParts splits an Azure DevOps Owner into organization and project.
func (r Repo) azParts() (org, project string) {
	org, project, _ = strings.Cut(r.Owner, "/")
	return org, project
}

// azWeb is the repository's page on Azure DevOps.
func (r Repo) azWeb() string {
	org, project := r.azParts()
	return r.root() + "/" + url.PathEscape(org) + "/" + url.PathEscape(project) + "/_git/" + url.PathEscape(r.Name)
}

// azSegments splits a URL path into its unescaped segments.
func azSegments(path string) []string {
	var out []string
	for _, p := range strings.Split(strings.Trim(path, "/"), "/") {
		if u, err := url.PathUnescape(p); err == nil {
			p = u
		}
		out = append(out, p)
	}
	return out
}

// azLegacyOrg is the organization of an <org>.visualstudio.com host, ""
// for another host (also vs-ssh.visualstudio.com, whose paths name it).
func azLegacyOrg(host string) string {
	org, ok := strings.CutSuffix(host, ".visualstudio.com")
	if !ok || org == "vs-ssh" || strings.Contains(org, ".") {
		return ""
	}
	return org
}

// azPath reads an Azure DevOps path (segments, after the host's prefix):
// org, project and repository, and what follows the repository (a pull
// request's "pullrequest/12"). On a host given as a URL with a path, pre
// is that path ("tfs"). More segments than collection and project mean a
// path rw was not told about: refused, as the API would be guessed.
func azPath(host string, segs []string, pre string) (org, project, repo string, rest []string, err error) {
	bad := func() (string, string, string, []string, error) {
		return "", "", "", nil, fmt.Errorf("want <org>/<project>/_git/<repo>")
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return bad()
		}
	}
	if pre != "" {
		p := strings.Split(pre, "/")
		if len(segs) > len(p) && strings.EqualFold(strings.Join(segs[:len(p)], "/"), pre) {
			segs = segs[len(p):]
		}
	}
	if len(segs) > 0 && strings.EqualFold(segs[0], "v3") {
		// SSH: v3/<org>/<project>/<repo>.
		if len(segs) != 4 {
			return bad()
		}
		return segs[1], segs[2], strings.TrimSuffix(segs[3], ".git"), nil, nil
	}
	at := -1
	for i, s := range segs {
		if s == "_git" {
			at = i
			break
		}
	}
	if at < 0 || at+1 >= len(segs) {
		return bad()
	}
	repo = strings.TrimSuffix(segs[at+1], ".git")
	before := segs[:at]
	if org = azLegacyOrg(host); org != "" {
		if len(before) > 0 && strings.EqualFold(before[0], "DefaultCollection") {
			before = before[1:]
		}
		switch len(before) {
		case 0:
			project = repo
		case 1:
			project = before[0]
		default:
			return bad()
		}
		return org, project, repo, segs[at+2:], nil
	}
	switch {
	case len(before) == 1:
		org, project = before[0], repo
	case len(before) == 2:
		org, project = before[0], before[1]
	case host != azureCloud:
		return "", "", "", nil, fmt.Errorf("want <collection>/<project>/_git/<repo>; for a server under a path set AZURE_DEVOPS_HOST to its URL (https://%s/%s)", host, strings.Join(before[:len(before)-2], "/"))
	default:
		return bad()
	}
	return org, project, repo, segs[at+2:], nil
}

// parseAzureRemote reads an Azure DevOps remote (host and path as
// remoteParts split them).
func parseAzureRemote(remote, host, path string, hosts Hosts) (Repo, error) {
	host = hostName(host)
	pre := ""
	if !isAzureCloud(host) {
		pre = hosts.prefix(host)
	}
	org, project, name, rest, err := azPath(host, azSegments(path), pre)
	if err == nil && len(rest) > 0 {
		err = fmt.Errorf("want <org>/<project>/_git/<repo>")
	}
	if err != nil {
		return Repo{}, fmt.Errorf("remote %q: %w", remote, err)
	}
	if isAzureCloud(host) {
		host = azureCloud
	}
	return Repo{Kind: Azure, Host: host, Owner: org + "/" + project, Name: name}, nil
}

// parseAzureRef reads a work item URL (.../<org>/<project>/_workitems/edit/12)
// or a pull request URL (.../_git/<repo>/pullrequest/12).
func parseAzureRef(u *url.URL, hosts Hosts, what string) (Ref, error) {
	host := strings.ToLower(u.Hostname())
	segs := azSegments(u.Path)
	pre := ""
	if !isAzureCloud(host) {
		pre = hosts.prefix(host)
		if p := strings.Split(pre, "/"); pre != "" && len(segs) > len(p) && strings.EqualFold(strings.Join(segs[:len(p)], "/"), pre) {
			segs = segs[len(p):]
		}
	}
	number := func(s string) (int, error) {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%s %q: bad number", what, u.String())
		}
		return n, nil
	}
	repo := Repo{Kind: Azure, Host: host}
	if isAzureCloud(host) {
		repo.Host = azureCloud
	} else {
		repo.Web = hosts.web(host)
	}
	if what == "issue" {
		// [<org>/]<project>/_workitems/edit/<n>
		for i, s := range segs {
			if s != "_workitems" {
				continue
			}
			if i+2 >= len(segs) || !strings.EqualFold(segs[i+1], "edit") {
				break
			}
			org, project := azLegacyOrg(host), ""
			before := segs[:i]
			if org != "" && len(before) > 0 && strings.EqualFold(before[0], "DefaultCollection") {
				before = before[1:]
			}
			switch {
			case org != "" && len(before) == 1:
				project = before[0]
			case org == "" && len(before) == 2:
				org, project = before[0], before[1]
			default:
				return Ref{}, fmt.Errorf("issue %q: want https://dev.azure.com/<org>/<project>/_workitems/edit/<n>", u.String())
			}
			n, err := number(segs[i+2])
			if err != nil {
				return Ref{}, err
			}
			repo.Owner = org + "/" + project
			return Ref{Repo: repo, Number: n}, nil
		}
		return Ref{}, fmt.Errorf("issue %q: want a work item URL, https://dev.azure.com/<org>/<project>/_workitems/edit/<n>", u.String())
	}
	org, project, name, rest, err := azPath(host, segs, "")
	if err != nil || len(rest) < 2 || !strings.EqualFold(rest[0], "pullrequest") {
		return Ref{}, fmt.Errorf("%s %q: want https://dev.azure.com/<org>/<project>/_git/<repo>/pullrequest/<n>", what, u.String())
	}
	n, err := number(rest[1])
	if err != nil {
		return Ref{}, err
	}
	repo.Owner, repo.Name = org+"/"+project, name
	return Ref{Repo: repo, Number: n}, nil
}
