# Azure Pipelines and Bitbucket Pipelines

Copy the appropriate template from `ci/azure-pipelines.yml` or
`ci/bitbucket-pipelines.yml` to the same filename at your repository root. Copy
`ci/forge-job.sh` to `.relayweft-ci/forge-job.sh` and commit both files. The job
runs only when manually started (or explicitly scheduled), from a reviewed
branch. It reads queued issues, runs one by default and opens a draft PR.

The templates use Linux and Node 22. The runner installs the agent CLI, fetches
Relayweft v0.4.0 and checks an independently pinned SHA-256 digest before
executing it. To change `RW_VERSION`, also set `RW_SHA256` to the reviewed release
asset's digest for the runner architecture. `latest` is refused. A trusted
preinstalled executable can instead be supplied through `RW_BINARY`.

## Azure setup

Create secret pipeline variables `RW_AZURE_PAT` and `RW_ANTHROPIC_API_KEY`. The
template maps them to `AZURE_DEVOPS_TOKEN` and `ANTHROPIC_API_KEY`. Use a PAT
with Code: Read & write and Work Items: Read & write. See
[Azure DevOps](azure-devops.md) for additional scopes and server support. Use a
disposable hosted agent and keep checkout `persistCredentials: false`.

Work items tagged `rw` form the default queue. Set `RW_ISSUE` in the runner's
environment for one work item, or change `RW_ISSUES_LABEL` for another tag.
Azure exposes the HTTPS repository URL through `BUILD_REPOSITORY_URI`.

## Bitbucket setup

Create secured repository variables `BITBUCKET_TOKEN` and `ANTHROPIC_API_KEY`.
For issue tasks, use `BITBUCKET_TOKEN=email:API-token` with repository read/write,
pull-request read/write and issue read/write scopes. Repository access tokens
do not have issue scopes. Enable the repository's issue tracker and create an
`rw` component for the queue. See [Bitbucket](bitbucket.md).

Run the `relayweft` custom pipeline on the reviewed default branch. The optional
`RW_ISSUE` field selects one issue. Serialize runs for the same repository;
these templates do not create a distributed queue lock across hosted agents.

## Agent, checks and budget

The supplied templates use Claude. To use Codex, set `RW_PROVIDER=codex` and
pass a secret `OPENAI_API_KEY` to the job. The runner installs Codex and performs
`codex login --with-api-key`. Set `CLAUDE_CODE_VERSION` or `CODEX_VERSION` to your
validated CLI version. The defaults pin Claude Code 2.1.288 and Codex 0.160.0,
the versions recorded in Relayweft's provider configuration.

| Variable | Default | Meaning |
|---|---|---|
| `RW_BUDGET_TASK_USD` | `5` | Positive API-equivalent cost cap per task |
| `RW_LIMIT` | `1` | Maximum queued issues per run |
| `RW_DRAFT` | `true` | Open draft PRs |
| `RW_BASE` | empty | Override the PR base branch |
| `RW_ISSUES_LABEL` | `rw` | Azure tag or Bitbucket component |
| `RW_TRUST_REPO_CONFIG` | `false` | Explicitly authorize checked-in commands |

Configure project verification with `rw init`, review `.relayweft.yaml`, then
opt into `RW_TRUST_REPO_CONFIG=true` if it contains commands. Hosted agents
need your project's test/build dependencies too; add their setup before the
runner step. The job preserves a failing task's exit code and uploads
`relayweft-report/` with its log and generated HTML/Markdown reports.

Forge credentials live in the job environment. The Git credential helper checks
the exact HTTPS host and repository path and is removed when the script exits.
Credentials are not stored in Git URLs. Relayweft removes forge credentials from
agent and check environments. Use ephemeral runners, keep secrets away from PR
pipelines and restrict changes to the template/helper. `rw watch` treats
`.relayweft-ci/` as protected CI configuration.

## Verification

`go test ./ci` parses both templates and runs the shared shell with scripted
executables in temporary Git repositories. It covers both forges, credentials,
reports, budgets and success/failure exit codes without calling models or
hosted services. Git Bash is used on Windows. Actual hosted execution still
requires the relevant Azure or Bitbucket account and secret configuration.
