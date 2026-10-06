package proc

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

func TestWithoutSecrets(t *testing.T) {
	in := []string{
		"PATH=/usr/bin", "GITHUB_TOKEN=ghs_x", "GH_TOKEN=y", "GITLAB_TOKEN=glpat",
		"CI_JOB_TOKEN=job", "CI_REPOSITORY_URL=https://gitlab-ci-token:job@gitlab.com/a/b.git",
		"ANTHROPIC_API_KEY=keep", "CODEX_API_KEY=keep", "CLAUDE_CODE_OAUTH_TOKEN=keep",
		"GITHUB_TOKENS=keep", "MY_GITHUB_TOKEN=keep", "CI=true",
	}
	got := strings.Join(WithoutSecrets(in), " ")
	for _, gone := range []string{"ghs_x", "GH_TOKEN", "glpat", "CI_JOB_TOKEN", "gitlab-ci-token"} {
		if strings.Contains(got, gone) {
			t.Errorf("%s reached the agent: %s", gone, got)
		}
	}
	for _, kept := range []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=keep", "CODEX_API_KEY=keep", "CLAUDE_CODE_OAUTH_TOKEN=keep", "GITHUB_TOKENS=keep", "MY_GITHUB_TOKEN=keep", "CI=true"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%s missing: %s", kept, got)
		}
	}
	if runtime.GOOS == "windows" {
		if out := WithoutSecrets([]string{"github_token=x", "Gitlab_Token=y"}); len(out) != 0 {
			t.Errorf("Windows names ignore case, got %v", out)
		}
	}
}

// The environment of a real Forgejo Actions job step (forgejo-runner
// v12.13.2 on Forgejo 16): the runner sets the job token as GITHUB_TOKEN
// and FORGEJO_TOKEN, and the GitHub-style runtime tokens. The flags and
// URLs stay: verify commands may look at them.
func TestWithoutSecretsForgejoActions(t *testing.T) {
	in := []string{
		"GITHUB_TOKEN=job1", "FORGEJO_TOKEN=job2", "GITEA_TOKEN=job3",
		"ACTIONS_RUNTIME_TOKEN=rt", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=idt",
		"ACTIONS_ID_TOKEN_REQUEST_URL=http://forgejo:3000/api/actions/_apis/idtoken?x=idu",
		"GITEA_RUNNER_REGISTRATION_TOKEN=reg",
		"FORGEJO_ACTIONS=true", "GITEA_ACTIONS=true", "GITHUB_ACTIONS=true", "CI=true",
		"FORGEJO_SERVER_URL=http://forgejo:3000", "GITHUB_SERVER_URL=http://forgejo:3000",
		"FORGEJO_REPOSITORY=alice/demo", "ACTIONS_RUNTIME_URL=http://forgejo:3000/api/actions_pipeline/",
		"ANTHROPIC_API_KEY=keep",
	}
	got := WithoutSecrets(in)
	if want := in[7:]; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

// A Bitbucket Pipelines step: rw's token (a secured variable) and the
// step's OpenID Connect token go; the build's own variables stay.
func TestWithoutSecretsBitbucketPipelines(t *testing.T) {
	in := []string{
		"BITBUCKET_TOKEN=me@example.com:ATATT", "BITBUCKET_STEP_OIDC_TOKEN=eyJ",
		"BITBUCKET_WORKSPACE=w", "BITBUCKET_REPO_SLUG=r", "BITBUCKET_COMMIT=abc", "CI=true", "ANTHROPIC_API_KEY=keep",
	}
	got := WithoutSecrets(in)
	if want := in[2:]; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

// Verify commands, hooks and bench checks run code the agents wrote: the
// shell they run in must not see rw's tokens either.
func TestShellWithoutSecrets(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghs_secret")
	t.Setenv("RW_TEST_KEEP", "kept")
	line := "env"
	if runtime.GOOS == "windows" {
		line = "set"
	}
	out, err := Shell(context.Background(), line).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "ghs_secret") {
		t.Errorf("the shell saw GITHUB_TOKEN:\n%s", out)
	}
	if !strings.Contains(string(out), "RW_TEST_KEEP=kept") {
		t.Errorf("the rest of the environment is missing:\n%s", out)
	}
}
