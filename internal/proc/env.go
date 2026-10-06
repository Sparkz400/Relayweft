package proc

import (
	"runtime"
	"strings"
)

// childSecrets are variables that agents and the commands rw runs for
// them (verify commands, hooks, bench checks) never get, though rw itself
// has them: the forge tokens rw uses to read issues and open pull requests,
// and the CI runner's own tokens. In CI (docs/ci.md) the job holds a token
// that can push and comment, while the agents read untrusted issue text and
// the verify commands run code the agents wrote; both need only the model's
// key. This keeps the tokens out of their environment, transcripts and
// test runs. It is not a sandbox: a process of the same user can still read
// rw's own environment on some systems, so the token's permissions and the
// agents' sandboxes (Claude's allowed tools, Codex's sandbox) still matter.
//
// An MCP server that needs one names it explicitly (${GITHUB_TOKEN}); the
// runner adds that value back after filtering (runner.MCPRun.ChildEnv).
var childSecrets = []string{
	// GitHub (forge.Token, gh.Token)
	"GITHUB_TOKEN", "GH_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
	// GitLab
	"GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN",
	// Gitea / Forgejo
	"GITEA_TOKEN", "FORGEJO_TOKEN",
	// Azure DevOps, and an Azure Pipelines job's token (when a job maps
	// System.AccessToken into the environment)
	"AZURE_DEVOPS_TOKEN", "AZURE_DEVOPS_EXT_PAT", "SYSTEM_ACCESSTOKEN",
	// Bitbucket, and Bitbucket Pipelines' OpenID Connect token
	"BITBUCKET_TOKEN", "BITBUCKET_STEP_OIDC_TOKEN",
	// GitHub Actions runner tokens; Forgejo and Gitea runners set the same
	// ones, plus GITHUB_TOKEN and FORGEJO_TOKEN (GITEA_TOKEN) to the job's
	// token. A registration token lets anyone register a runner and take
	// jobs with their secrets: Gitea's act_runner image reads it from the
	// environment, which a host-executor job can inherit.
	"ACTIONS_RUNTIME_TOKEN", "ACTIONS_ID_TOKEN_REQUEST_TOKEN", "ACTIONS_ID_TOKEN_REQUEST_URL",
	"GITEA_RUNNER_REGISTRATION_TOKEN",
	// GitLab CI job credentials (CI_REPOSITORY_URL embeds the job token)
	"CI_JOB_TOKEN", "CI_JOB_JWT", "CI_JOB_JWT_V1", "CI_JOB_JWT_V2", "CI_REPOSITORY_URL",
	"CI_REGISTRY_PASSWORD", "CI_DEPLOY_PASSWORD", "CI_DEPENDENCY_PROXY_PASSWORD",
}

// WithoutSecrets is env without childSecrets. Windows variable names
// ignore case.
func WithoutSecrets(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !IsChildSecret(name) {
			out = append(out, kv)
		}
	}
	return out
}

// IsChildSecret reports whether name is one of rw's forge or CI tokens,
// which no agent gets.
func IsChildSecret(name string) bool {
	for _, s := range childSecrets {
		if name == s || runtime.GOOS == "windows" && strings.EqualFold(name, s) {
			return true
		}
	}
	return false
}
