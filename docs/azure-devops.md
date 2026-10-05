# Azure DevOps

`rw pr`, issues as tasks (`--issue`, `--issues label:...`, `--team`), `rw watch` and `rw review --post` work with Azure Repos:

- Azure DevOps Services: `dev.azure.com`, and the old `<org>.visualstudio.com` names.
- Azure DevOps Server 2020 or later, named in `AZURE_DEVOPS_HOST`.

> **Status:** tested against a fake Azure DevOps API only. It has not yet run against a real organization or server. Try it on a test project first, and send a `rw bugreport` after any problem.

## Setup

The `origin` remote picks the forge, as for the other forges. These forms work:

- `https://dev.azure.com/<org>/<project>/_git/<repo>` (also with `<org>@` in front)
- `git@ssh.dev.azure.com:v3/<org>/<project>/<repo>`
- `https://<org>.visualstudio.com/[DefaultCollection/]<project>/_git/<repo>`
- On a server: `https://<server>/[<path>/]<collection>/<project>/_git/<repo>`, and its SSH form.

Project and repository names may contain spaces. rw writes a repository as `<org>/<project>/<repo>` and a pull request as `<org>/<project>/<repo>!12`.

### Token

Set `AZURE_DEVOPS_TOKEN` (or `AZURE_DEVOPS_EXT_PAT`, the variable of the `az devops` CLI). In PowerShell:

```powershell
$env:AZURE_DEVOPS_TOKEN = "<token>"
```

A **personal access token** (User settings > Personal access tokens) needs these scopes:

| Scope | For |
|---|---|
| Code: Read & write | pull requests, comment threads, diffs, commit statuses |
| Work Items: Read & write | work items, their comments, tag queries |
| Build: Read | failed builds and their logs (`rw watch`) |
| Project and Team: Read | team members, who counts as a reviewer (`rw watch`, `--team`) |

A **Microsoft Entra token** works too. rw sends it as a bearer token (a personal access token goes as basic auth). It expires after about an hour:

```powershell
$env:AZURE_DEVOPS_TOKEN = az account get-access-token --resource 499b84ac-1321-427f-aa17-267ca6975798 --query accessToken -o tsv
```

Azure DevOps refuses an expired token and a token that lacks a scope the same way (401). rw then stops with the scopes above; unlike on the other forges, it does not go on without the token.

`git push` uses your own git credentials (Git Credential Manager on Windows), not this token.

### Azure DevOps Server

Set `AZURE_DEVOPS_HOST` to the server's URL, with its path when it has one:

```powershell
$env:AZURE_DEVOPS_HOST = "https://tfs.example.com/tfs"
```

Several servers are separated by commas. When `AZURE_DEVOPS_HOST` is set, the token goes to the servers it names only, never to `dev.azure.com`. A token is never sent to a forge of another kind, and no other forge's token goes to Azure DevOps.

## Work items as issues

- `rw run --issue 42` reads work item 42. A work item URL (`https://dev.azure.com/<org>/<project>/_workitems/edit/42`) works too. Work item numbers count per organization, so a work item of another project in your organization is fine.
- The task has the title, the description, and the repro steps and acceptance criteria when the work item has them. Their HTML becomes plain text.
- `--issues label:rw` takes the open work items of the repository's project with the **tag** `rw` (a WIQL query), oldest first. Closed means the state Closed, Done, Removed, Resolved or Completed.
- `--pr` opens a pull request whose description ends with `Closes #42`, and links work item 42 to it. Complete the pull request with "Complete associated work items" to close it. If Azure DevOps refuses the link (a work item the token cannot see), the pull request is opened without it.
- The comment on the work item, and `--team` claims, are HTML comments. rw escapes its text, so nothing in it becomes markup or a mention. The claim's marker (`<!-- relayweft:queue ... -->`) shows as text.

## Pull requests

- `--draft` opens a draft pull request.
- Azure DevOps keeps 4000 characters of a description. A longer one is cut at the end; the `Closes #N` line stays.
- Pull request templates are read from `.azuredevops/` and `.vsts/` too.
- Mentions in text from others (`@<id>`, `@name`) and closing words (`Fixes #7`) are defused, as on the other forges.

## rw watch

- **Checks:** the failed builds of the head commit, the newest per pipeline: the pull request's build validation (it builds the merge commit) and builds of the branch. Each failed task is one item, with the tail of its log. A build that failed without a failed task (a YAML error) is one item with its message. Failed commit statuses from other services count too.
- **Reviews:** a vote of "Waiting for author" (-5) or "Rejected" (-10), and active comment threads on a line of a file. Only members of the project's teams count. Members given as a group in a team are not looked into, and service identities never count.
- rw replies with one comment thread without a status, so it blocks nothing.
- `rw watch` never pushes CI settings: `azure-pipelines*.yml` in any folder, and `.azuredevops/`, `.azure-pipelines/`, `.pipelines/`, `.vsts/`. A pipeline may name a YAML file anywhere; protect those with a branch policy.

## rw review --post

- Azure DevOps has no diff API. rw reads the pull request's changed files and their two versions, and builds the diff itself.
- Each inline finding becomes an active comment thread on its line. Under the "Check for comment resolution" policy these must be resolved before completing, as with GitLab threads.
- The rest goes into one comment thread without a status.

## Not yet

- A CI template (`azure-pipelines.yml`) like the GitLab and Forgejo ones in `ci/`.
- Group members of teams, and custom processes whose closed states have other names.
