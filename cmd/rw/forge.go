package main

import (
	"io"
	"strings"

	"github.com/sparkz400/relayweft/internal/forge"
)

// The forge (GitHub, GitLab or Gitea) of rw pr, issues as tasks, rw watch
// and rw review is the origin remote's host (package forge).

// originURL is dir's origin remote URL ("" and an error without one).
func originURL(dir string) (string, error) {
	u, err := prGit(dir, nil, nil, "remote", "get-url", "origin")
	return strings.TrimSpace(u), err
}

// forgeHosts are the configured forge hosts. An explicit API URL means:
// trust the remote's host, as the forge the URL looks like (/api/v4 is
// GitLab, /api/v1 Gitea, else GitHub).
func forgeHosts(remote, api string) forge.Hosts {
	h := forge.EnvHosts()
	if api != "" && remote != "" {
		h = h.With(hostOf(remote), forge.KindOfAPI(api))
	}
	return h
}

// forgeClient is a client for repo's forge with its token; api overrides
// the host's API base URL.
func forgeClient(repo forge.Repo, api string, notes io.Writer) forge.Client {
	if api == "" {
		api = repo.APIBase()
	}
	tok, _ := prToken(repo.Kind, repo.Host)
	return forge.New(repo.Kind, api, tok, notes)
}

// noTokenText says that a forge's token is missing and where it comes from.
func noTokenText(k forge.Kind) string {
	return "no " + k.Name() + " token (" + k.TokenHint() + ")"
}
