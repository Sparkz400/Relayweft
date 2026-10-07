# Bitbucket Cloud

`rw pr`, issues as tasks (`rw run --issue`, `--issues label:...`, `--team`), `rw watch` and `rw review --post` work on Bitbucket Cloud (bitbucket.org), as on GitHub, GitLab and Gitea. An `origin` remote on bitbucket.org picks Bitbucket; nothing else needs to be set.

Bitbucket Data Center and Server are **not supported**: they have a different API. A remote on your own Bitbucket server gets the usual "is not on github.com, gitlab.com, codeberg.org or bitbucket.org" error.

> **Status:** tested against a fake Bitbucket API only, not yet against bitbucket.org. The scope names below are Atlassian's as documented; check them when you create the token, and send the output of any failing command.

---

## Token

rw reads the token from `BITBUCKET_TOKEN`. It goes to `https://api.bitbucket.org` only, and only for repositories on bitbucket.org: not to an `--api` URL elsewhere, never to another forge, never to agents, verify commands or hooks, and never onto a command line. `git push` uses your own git credentials, as on every forge.

Use one of these:

- **API token** (for yourself). Atlassian account settings > Security > **Create API token with scopes**, app Bitbucket. Set `BITBUCKET_TOKEN=<your Atlassian email>:<token>`; rw sends it as Basic authentication.
- **Access token** (repository, project or workspace; for a bot or CI, since it belongs to no person). Repository settings > Security > **Access tokens**. Set `BITBUCKET_TOKEN=<token>`; rw sends it as a Bearer token.

App passwords are being retired by Atlassian. While one still works, `BITBUCKET_TOKEN=<username>:<app password>` uses it like an API token.

Scopes, by what you use:

| Feature | API token scopes | Access token scopes |
|---|---|---|
| `rw pr` | `read:repository:bitbucket`, `write:pullrequest:bitbucket` | Repositories: Read, Pull requests: Write |
| issues as tasks, issue comments | `read:issue:bitbucket`, `write:issue:bitbucket` | none: access tokens have no issue scopes, so use an API token |
| `rw watch` | the `rw pr` scopes, `read:pullrequest:bitbucket`, `read:pipeline:bitbucket`, `read:user:bitbucket` | Pull requests: Write, Pipelines: Read |
| `rw review --post` | `read:pullrequest:bitbucket`, `write:pullrequest:bitbucket` | Pull requests: Write |
| telling reviewers from strangers (`rw watch`, `--team`) | `read:workspace:bitbucket`; workspace admins also see the effective permissions | Repositories: Admin to read the repository's permissions |

`rw watch` and `--team` ask Bitbucket who owns the token (`GET /user`). If that fails with an access token, use an API token for them.

---

## What maps to what

- **Pull request:** a Bitbucket pull request from a branch of the same repository. A draft uses Bitbucket's draft flag. The description ends with `Closes #N` for an issue task.
- **`--issues label:rw`:** Bitbucket issues have no labels. rw takes the open issues (state *new* or *open*) whose **component** is `rw`, oldest first. Components are a list the repository admin keeps (Repository settings > Issue tracker > Components): add one called `rw` (or any name you pass after `label:`). Kind (bug, task, ...) is a fixed list and milestones are for releases, so neither fits a work queue. The task text names the issue's component and kind.
  - Unlike a label, the person who files an issue may be able to pick its component. So an issue counts only when its reporter has write access, or when the component was last set (to `rw`) by someone with write access, as the issue's history shows. Other issues are skipped with a note. To queue a stranger's issue, set its component yourself.
- **Closing the issue:** Bitbucket resolves an issue from a commit message (`Closes #N`), not from a pull request's description. Whether merging closes it depends on the merge commit's message carrying the description; otherwise close the issue by hand. rw also comments the pull request's link on the issue.
- **Failed checks (`rw watch`):** the failed steps of the newest pipeline per target (branch, pull request, custom run) on the head commit, each with the tail of its log, plus the failed commit statuses that other CI posted. A pipeline that failed before any step (a broken `bitbucket-pipelines.yml`) counts as one check. Without the pipeline scope, the pipeline's own commit status stands in, without a log.
- **Review feedback (`rw watch`):** reviewers who chose **Request changes** (once per reviewer and pull request: Bitbucket gives a change request no id of its own), and inline comments that are not resolved, not drafts and not deleted. rw reads the newest comments, so a flood of old ones cannot hide new feedback or a team claim.
- **Who counts:** someone with write or admin access to the repository. rw reads the effective permission when the token may (a workspace admin's token), else the permission given to the user on the repository; only when it can read neither does a member of the workspace count. Apps never count, and nor do you.
- **`rw review --post`:** one comment per finding on a line of the diff, then one comment with the review. A finding Bitbucket cannot place on its line moves into that comment. It never approves or requests changes.
- **People:** Bitbucket has no fixed user names. rw names a person by their account UUID (`{0f3c...}`), so nobody can pass for you, or for a reviewer, by picking a name.

---

## Repositories without an issue tracker

The issue tracker is optional per repository, and many teams use Jira instead. rw reads Bitbucket's own tracker only. Without one, `--issue` and `--issues` fail with "has no Bitbucket issue tracker (it may use Jira instead)". A repository admin turns it on under Repository settings > Features. `rw pr`, `rw watch` and `rw review` work without it.

---

## Safety

- CI logs and pull request comments reach `rw watch`'s agents only as fenced, untrusted data, as on every forge. An issue's text is the task itself, as on the other forges: only queue issues you would run.
- In everything rw posts and commits, mentions (`@name`, `@{account id}`), Bitbucket's issue commands (`fixes issue #3`, `reopen #3`, `hold #3`, `wontfix #3`, `invalidate #3`, ...) and, next to a Jira key, Jira smart commit commands (`PROJ-12 #close`, `#time`, `#comment`) are broken with an invisible character, so they notify nobody and change no issue. Only the explicit `Closes #N` line counts.
- `rw watch` never pushes a change to `bitbucket-pipelines.yml`, nor to any other forge's CI config.
- A next-page link from the API is followed only when it points at the same API; the token never follows a link anywhere else.
- In Bitbucket Pipelines, rw also keeps the step's OpenID Connect token (`BITBUCKET_STEP_OIDC_TOKEN`) from the agents.

---

## Not yet

- Hosted validation of the [Bitbucket Pipelines template](ci-azure-bitbucket.md).
- A run against bitbucket.org.
