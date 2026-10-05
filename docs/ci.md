# Relayweft in CI: issues without your PC

`rw run --issue N --pr` and `rw run --issues label:rw --pr` also run in CI. A GitHub Action, a GitLab job or a Forgejo/Gitea workflow installs `rw` and the agent CLIs, runs the issue, and opens a pull request (`Closes #N`) for each task that finished ok. Your PC can stay off overnight.

- **GitHub Actions:** the action in this repository (`uses: Sparkz400/relayweft@…`) with the example workflow [`ci/github-workflow.yml`](../ci/github-workflow.yml).
- **GitLab CI:** the job template [`ci/relayweft.gitlab-ci.yml`](../ci/relayweft.gitlab-ci.yml), for gitlab.com and self-managed GitLab.
- **Forgejo / Gitea Actions:** the workflow [`ci/forgejo-workflow.yml`](../ci/forgejo-workflow.yml), for Codeberg and self-hosted Forgejo or Gitea.

`rw watch` then follows up on those pull requests from your PC, as before. In CI, each run starts with no history and no learned routes.

> **Status:** the Forgejo workflow ran on a real Forgejo 16 with forgejo-runner v12 (with a scripted stand-in for Claude Code). The GitHub Action and the GitLab job are checked locally (the shell logic, and token scrubbing with tests) but have not yet run on real GitHub Actions or GitLab runners. Try it on a test repository first, and send the job log after any problem.

---

## GitHub Actions

1. **Agent sign-in, as repository secrets** (Settings > Secrets and variables > Actions). Use one or both providers:
   - Claude Code: `CLAUDE_CODE_OAUTH_TOKEN` (your Claude subscription; create it with `claude setup-token` on your PC) or `ANTHROPIC_API_KEY`.
   - Codex: `OPENAI_API_KEY`, or `CODEX_AUTH_JSON` for a ChatGPT plan (see [Agent sign-in](#agent-sign-in)).
2. **Let Actions open pull requests:** Settings > Actions > General > Workflow permissions > "Allow GitHub Actions to create and approve pull requests".
3. **Create the label** `rw` (Issues > Labels).
4. **Copy** [`ci/github-workflow.yml`](../ci/github-workflow.yml) to `.github/workflows/relayweft.yml` and keep the secrets you set.

Then:

- **Label an issue `rw`:** a job runs it and opens a pull request.
- **Every night at 02:30 UTC:** the open issues labelled `rw` that no open pull request closes yet, at most 5 (`limit`), oldest first.
- **Actions > relayweft > Run workflow:** one issue by number, or the whole batch.

Each run's job summary has every task's report (plan, checks, cost, diff). The artifact `relayweft-…` holds the reports as HTML and Markdown, the run log and the session log.

### Pull requests that start your CI

GitHub does not start workflows for a pull request opened with the job's own `GITHUB_TOKEN`. To have your checks run on Relayweft's pull requests (and so `rw watch` has checks to follow up on), pass another token as `github-token`:

- a fine-grained personal access token for this repository with **Contents**, **Pull requests** and **Issues** set to read and write, or
- a GitHub App installation token (for example from `actions/create-github-app-token`).

### Inputs

| Input | Default | |
|---|---|---|
| `issue` | | Issue number or URL. Give this or `issues-label`. |
| `issues-label` | | Run the open issues with this label one after another. |
| `limit` | `5` | With `issues-label`: at most this many issues per run. |
| `pr` | `true` | Open a pull request per successful task (always on with `issues-label`). |
| `draft` | `false` | Open the pull requests as drafts. |
| `comment` | `true` | Comment the pull request link on the issue. |
| `with-comments` | `false` | Include the issue's comments in the task. |
| `base` | the default branch | Branch the pull requests merge into. Check this branch out before the action. |
| `provider` | the one with credentials | `codex` or `claude` puts every role on one provider. With both signed in, Relayweft routes between them. |
| `budget-task-usd` | `10` | Stop a task at this API-equivalent $. `0` means no limit. |
| `args` | | More `rw run` flags, separated by spaces. |
| `config` | built-in defaults | A rw config file in the repository. `.relayweft.yaml` is layered on top either way. |
| `trust-repo-config` | `true` | Trust `.relayweft.yaml`, so its verify commands and hooks run. It comes from the same branch as the workflow, so whoever can change it can change the workflow too. |
| `anthropic-api-key`, `claude-code-oauth-token` | | Claude Code sign-in. |
| `openai-api-key`, `codex-auth-json` | | Codex sign-in. |
| `github-token` | `github.token` | Reads the issue, pushes the branch, opens the pull request and comments. |
| `version` | see below | `vX.Y.Z`, `latest` or `source`. |
| `claude-code-version`, `codex-version` | `latest` | npm versions of the agent CLIs. Pin them to the versions `rw doctor` was tested with if a new release breaks a run. |
| `upload-artifacts` | `true` | Upload the reports and logs. On a public repository anyone can download them. |
| `artifact-retention-days` | `14` | |

**Outputs:** `pull-requests` (URLs, one per line), `task-ids` and `report-dir`.

**Which `rw`:** with `uses: Sparkz400/relayweft@v0.2.0` the action installs that release's binary and checks it against the release's `checksums.txt`. With a branch or a commit SHA (`@main`) it builds `rw` from that source with Go. `version` overrides both.

The action works on Linux, macOS and Windows runners. It needs `jq` for the reports, which GitHub's hosted runners have.

---

## GitLab CI

1. **A project access token** (Settings > Access tokens): role **Developer**, scopes **api** and **write_repository**. Add it as the CI/CD variable `GITLAB_TOKEN` (Settings > CI/CD > Variables; masked and protected). The job's own `CI_JOB_TOKEN` cannot read issues or open merge requests.
2. **Agent sign-in, as CI/CD variables** (masked and protected): `CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY`, and/or `OPENAI_API_KEY` or `CODEX_AUTH_JSON` (a variable of type File).
3. **Include the template** in `.gitlab-ci.yml`:
   ```yaml
   include:
     - remote: https://raw.githubusercontent.com/Sparkz400/relayweft/main/ci/relayweft.gitlab-ci.yml
   ```
   Or copy the file into your repository and include it with `local:`, so a change upstream cannot change your pipeline.
4. **A pipeline schedule** (Build > Pipeline schedules) on the default branch with the variable `RELAYWEFT=1`. Each run takes the open issues labelled `rw` that no open merge request closes yet.

For one issue, run a pipeline with `RELAYWEFT=1` and `RW_ISSUE=<number>`: from Build > Pipelines > Run pipeline, or with a pipeline trigger token:

```sh
curl -X POST --fail \
  -F token=$TRIGGER_TOKEN -F ref=main \
  -F "variables[RELAYWEFT]=1" -F "variables[RW_ISSUE]=42" \
  https://gitlab.com/api/v4/projects/<project id>/trigger/pipeline
```

GitLab has no "issue labelled" pipeline event. A project webhook on issue events can call that trigger through a small relay, or the schedule picks the issue up on its next run.

The job's variables (`RW_VERSION`, `RW_ISSUES_LABEL`, `RW_LIMIT`, `RW_PROVIDER`, `RW_BUDGET_TASK_USD`, `RW_DRAFT`, `RW_BASE`, `RW_ARGS`, `RW_TRUST_REPO_CONFIG`, `CLAUDE_CODE_VERSION`, `CODEX_VERSION`) mean the same as the action's inputs; override them in your `.gitlab-ci.yml` or on the schedule. The job runs in `node:22-bookworm` on a Linux runner, one run at a time per project (`resource_group`). The reports, the run log and the session log are job artifacts in `relayweft-report/`.

On self-managed GitLab, the job sets `GITLAB_HOST` to your server, so `GITLAB_TOKEN` only goes there.

---

## Forgejo and Gitea Actions

1. **Enable Actions** for the repository (Settings > Units, or Settings > Advanced settings > Actions) and have a runner. The job runs in the image `node:22-bookworm`, so the runner needs a label that runs jobs in Docker. The workflow asks for `runs-on: docker`, the label Forgejo's documentation uses; change it to one of your runner's labels (Gitea's act_runner registers `ubuntu-latest` and the like; Codeberg's runners have their own labels).
2. **Agent sign-in, as repository secrets** (Settings > Actions > Secrets): `CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY`, and/or `OPENAI_API_KEY` or `CODEX_AUTH_JSON` (see [Agent sign-in](#agent-sign-in)).
3. **Create the label** `rw` (Issues > Labels).
4. **Copy** [`ci/forgejo-workflow.yml`](../ci/forgejo-workflow.yml) to `.forgejo/workflows/relayweft.yml` (Gitea: `.gitea/workflows/relayweft.yml`).

Then, as on GitHub:

- **Label an issue `rw`:** a job runs it and opens a pull request.
- **Every night at 02:30 UTC:** the open issues labelled `rw` that no open pull request closes yet, at most 5 (`RW_LIMIT`).
- **Actions > relayweft > Run workflow:** one issue by number, or the whole batch.

The job's own token reads the issue, pushes the branch, opens the pull request and comments, and goes to the job's own server only (the workflow sets `GITEA_HOST` to it; rw after v0.2.0 finds it by itself). A secret `RELAYWEFT_TOKEN` (an access token with the scopes `write:repository` and `write:issue`) is used instead when set. You need it:

- **on a private repository:** Forgejo 16 does not let the job's token open a pull request there (`404 Can't read pulls or can't read UnitTypeCode`; the branch is pushed);
- **for your CI on the pull requests:** like GitHub, Forgejo starts no workflows for a pull request the job's token opened.

The job's variables (`RW_VERSION`, `RW_ISSUES_LABEL`, `RW_LIMIT`, `RW_PROVIDER`, `RW_BUDGET_TASK_USD`, `RW_DRAFT`, `RW_BASE`, `RW_ARGS`, `RW_TRUST_REPO_CONFIG`) mean the same as the GitLab job's; change them in the workflow's `env:`. Three more:

- `RW_RELEASES`: where the release binaries and `checksums.txt` are (default GitHub's releases of this repository). A mirror with the same layout (`<base>/download/<version>/<file>`), for example a release in your own Forgejo, works too; the checksum is checked either way.
- `CLAUDE_CODE_PACKAGE`, `CODEX_PACKAGE`: the npm package specs of the agent CLIs, a version (`@anthropic-ai/claude-code@2.1.288`) or a tarball URL.

The reports go to the job log. Forgejo shows no job summary; to keep the HTML reports as an artifact, enable the commented `upload-artifact` step.

**Limits:**

- Tested on Forgejo 16 with forgejo-runner v12, not yet on Gitea or Codeberg. Not every version enforces `concurrency` (one run per issue at a time); if yours rejects the workflow over it, delete those lines.
- The job runs in a Docker container on Linux. A runner without Docker (a host executor) needs Node.js, git and curl on the runner instead.
- `rw watch` sees the pull request's failed commit statuses, but no job logs (Gitea and Forgejo serve none through their API).

---

## Agent sign-in

- **Claude Code with your subscription:** `claude setup-token` on your PC prints a long-lived token for exactly this; store it as `CLAUDE_CODE_OAUTH_TOKEN`. Runs count against your plan's limits, like on your PC.
- **Claude Code with an API key:** `ANTHROPIC_API_KEY`. Billed per token.
- **Codex with an API key:** `OPENAI_API_KEY`; the job runs `codex login --with-api-key`. Billed per token. This is what OpenAI recommends for CI.
- **Codex with a ChatGPT plan:** the contents of `~/.codex/auth.json` after `codex login` as `CODEX_AUTH_JSON`. Codex refreshes that sign-in as it runs, so the stored copy can go stale or the refresh can sign out the copy on your PC. Prefer a separate sign-in for CI, and replace the secret when runs report a login error.

Use keys with a spending limit for CI, set at the provider.

---

## Security

The agents read issue text that anyone may have written, and the job holds a token that can push. Relayweft keeps those apart where it can.

**What the agents cannot do:**

- **Get the forge token.** Agents, and the verify commands, hooks and bench checks that rw runs for them, start with an environment without `GITHUB_TOKEN`, `GH_TOKEN`, `GITLAB_TOKEN`, `GITEA_TOKEN`, `FORGEJO_TOKEN` and the CI runner's own tokens (`CI_JOB_TOKEN`, `CI_REPOSITORY_URL`, `ACTIONS_RUNTIME_TOKEN`, `ACTIONS_ID_TOKEN_REQUEST_TOKEN`, …). Only rw's own `git push` and API calls use it. Forgejo's runner puts the job's token into every step as `GITHUB_TOKEN` and `FORGEJO_TOKEN`; rw sends it only to the job's own server, never to GitHub.
- **Find it in git.** The action, the GitLab job and the Forgejo workflow give git a credential helper that reads the token from the environment at push time, so it is not stored in `.git/config`. With actions/checkout, set `persist-credentials: false` (as the example does). Otherwise the checkout token stays in `.git/config`.
- **Push or merge.** Claude Code workers can edit files and run only your verify commands (`allowedTools`); Codex workers run in Codex's `workspace-write` sandbox. rw pushes a new branch `rw/issue-N-…` and opens a pull request; it never merges or pushes to an existing branch. An unattended pull request is refused if it would carry files no agent reported changing, or commits that are not on the base branch.
- **Spend without limit.** `budget-task-usd` stops a task, `limit` caps the issues per run, and `timeout-minutes` (GitHub, Forgejo) or `timeout` (GitLab) caps the job. The day budget does not carry over between CI runs, because each runner starts empty.

**What you still need to decide:**

- **Who starts a run.** On GitHub, adding a label needs triage access (on Forgejo and Gitea, write access to issues), so only people you trust pick the issues. The nightly batch reads issues when it runs: someone could edit a labelled issue's text after you labelled it. Read the pull request like any outside contribution. It is never merged without you.
- **Which secrets the job has.** The agents need their model key; your verify commands run code the agents wrote, with that key in reach. Don't give the job other secrets:
  - On GitHub, don't set secrets in workflow- or job-level `env:`.
  - On GitLab, every CI/CD variable reaches every job. Limit deploy credentials and the like to an environment scope this job does not use.
- **Not a sandbox.** On Linux a process can read the environment of another process of the same user, so code an agent wrote could still reach the token through `rw`'s own process. Hence:
  - keep the job's permissions to the three in the example: contents, pull requests and issues;
  - protect your default branch, so a leaked token cannot push to it;
  - the token ends with the job.
- **Artifacts are public on public repositories.** They hold the task text, the plan, the agents' output and the diff (no tokens). Set `upload-artifacts: false` if that matters.

---

## Troubleshooting

- **"no git repository here":** run `actions/checkout` before the action.
- **"not opening a pull request nobody reviewed: HEAD has N commit(s) that are not on origin/main":** the job checked out another branch than the base. Check out the base branch (the example does), or set `base`.
- **The pull request opened, but no checks ran:** see [Pull requests that start your CI](#pull-requests-that-start-your-ci).
- **Forgejo: "404 Can't read pulls or can't read UnitTypeCode":** a private repository; set the secret `RELAYWEFT_TOKEN` (see [Forgejo and Gitea Actions](#forgejo-and-gitea-actions)).
- **Forgejo: the workflow never starts:** check `runs-on` against your runner's labels (Settings > Actions > Runners), and that Actions are enabled for the repository.
- **"403 … Resource not accessible by integration":** the workflow lacks `pull-requests: write` or `issues: write`, or step 2 of the GitHub setup is missing.
- **Codex fails with a sandbox error in a container:** Codex's Linux sandbox needs kernel features some container runtimes block. Run with `provider: claude` (`RW_PROVIDER: claude`), or use a VM-based runner.
- **A task stopped "by budget":** raise `budget-task-usd`, or set it to `0` for no limit.
