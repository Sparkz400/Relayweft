package forge

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/gh"
	"github.com/sparkz400/relayweft/internal/proc"
)

// Token finds a token for a host of kind k; source says where it came
// from ("" when there is none).
//
//   - GitHub: GITHUB_TOKEN, GH_TOKEN or `gh auth token` (GitHub
//     Enterprise: GH_ENTERPRISE_TOKEN, GITHUB_ENTERPRISE_TOKEN or `gh auth
//     token --hostname`), see gh.Token.
//   - GitLab: GITLAB_TOKEN or GITLAB_ACCESS_TOKEN, then `glab config get
//     token --host <host>`.
//   - Gitea: GITEA_TOKEN or FORGEJO_TOKEN.
//
// When GITLAB_HOST (GITEA_HOST, FORGEJO_HOST) is set, the GitLab (Gitea)
// variables are for the hosts it names only, as with glab: a token for a
// company's server is never sent to gitlab.com or codeberg.org. In a
// Forgejo or Gitea Actions job the job's server counts as named, so the
// job's token stays with its server.
func Token(k Kind, host string) (token, source string) {
	switch k {
	case GitLab:
		if envTokenFor(host, envHostValues("GITLAB_HOST")) {
			if t, v := firstEnv("GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN"); t != "" {
				return t, v
			}
		}
		if t := GLabToken(host); t != "" {
			return t, "glab config"
		}
		return "", ""
	case Gitea:
		if envTokenFor(host, giteaHostValues()) {
			if t, v := firstEnv("GITEA_TOKEN", "FORGEJO_TOKEN"); t != "" {
				return t, v
			}
		}
		return "", ""
	}
	t, src := gh.Token(host)
	if src == "GITHUB_TOKEN" {
		if s, _ := actionsServer(); s != "" {
			// A Forgejo or Gitea runner sets GITHUB_TOKEN to the job's
			// token for its own server: it never goes to GitHub.
			if t, v := firstEnv("GH_TOKEN"); t != "" {
				return t, v
			}
			if t := gh.GHCLIToken("github.com"); t != "" {
				return t, "gh auth token"
			}
			return "", ""
		}
	}
	return t, src
}

// envTokenFor reports whether the token variables of a forge may go to
// host: when no hosts are configured for the forge (entries, as
// envHostValues gives them), or one of them is host.
func envTokenFor(host string, entries []string) bool {
	set := false
	for _, e := range entries {
		if h := hostName(e); h != "" {
			set = true
			if h == hostName(host) {
				return true
			}
		}
	}
	return !set
}

func firstEnv(vars ...string) (string, string) {
	for _, v := range vars {
		if t := strings.TrimSpace(os.Getenv(v)); t != "" {
			return t, v
		}
	}
	return "", ""
}

// GLabToken asks the glab CLI for its token for host; tests replace it.
var GLabToken = func(host string) string {
	bin, err := exec.LookPath("glab")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "config", "get", "token", "--host", host)
	proc.Background(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// reCloses matches closing keywords followed by a same-repository issue
// number: "Closes #12", "fixes #3", "Resolved #7", and GitLab's "Fixing",
// "Implements" and so on.
var reCloses = regexp.MustCompile(`(?i)\b(?:clos(?:e[sd]?|ing)|fix(?:e[sd]|ing)?|resolv(?:e[sd]?|ing)|implement(?:s|ed|ing)?)\s*:?\s+#(\d+)\b`)

// ClosedBy returns the issue numbers the pull requests' bodies close.
func ClosedBy(pulls []Pull) map[int]bool {
	out := map[int]bool{}
	for _, p := range pulls {
		for _, m := range reCloses.FindAllStringSubmatch(p.Body, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil {
				out[n] = true
			}
		}
	}
	return out
}
