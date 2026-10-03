package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/sparkz400/switchyard/internal/gh"
	"github.com/sparkz400/switchyard/internal/orchestrator"
)

// GitHub issues as tasks (sy run --issue / --issues):
//
//   - --issue N|URL reads the issue (title, body, labels; comments with
//     --with-comments) and runs "Fix GitHub issue #N: <title>" plus the body
//     as the task. Public repositories need no token.
//   - --issues label:<name> runs the open issues with that label one after
//     another, unattended, oldest first; pull requests and issues an open
//     pull request already closes ("Closes #N") are skipped.
//   - --pr turns each successful task into a pull request (the sy pr flow,
//     without asking) whose body says "Closes #N", and comments the link on
//     the issue (--comment=false turns that off).
//   - A batch never discards your work: it starts only on a clean working
//     tree, and after a task's pull request is open the task's changes are
//     taken out of the working tree with sy undo (they live on in the PR
//     branch, and `sy undo --redo <key>` puts them back), so the next issue
//     starts from HEAD. If that is not possible, or a task left changes
//     without a pull request, the batch stops.

// issueFlags are sy run's GitHub issue flags and what they resolved to.
type issueFlags struct {
	issue        string
	issues       string
	limit        int
	withComments bool
	pr           bool
	comment      bool
	base         string
	draft        bool
	draftSet     bool
	api          string

	dir    string
	origin gh.Repo // the origin remote's repository (zero if none)
	items  []issueItem
	pulls  int
}

type issueItem struct {
	repo   gh.Repo
	client *gh.Client
	issue  gh.Issue
	task   string
	closes string // "#12" or "owner/repo#12"
}

func registerIssueFlags(fs *flag.FlagSet) *issueFlags {
	f := &issueFlags{}
	fs.StringVar(&f.issue, "issue", "", "run a GitHub issue (number or URL) as the task")
	fs.StringVar(&f.issues, "issues", "", "label:<name>: run the open issues with this label one after another, unattended")
	fs.IntVar(&f.limit, "limit", 5, "with --issues: at most this many issues")
	fs.BoolVar(&f.withComments, "with-comments", false, "with --issue(s): include the issue's comments in the task")
	fs.BoolVar(&f.pr, "pr", false, "with --issue(s): open a pull request (Closes #N) after each successful task")
	fs.BoolVar(&f.comment, "comment", true, "with --pr: comment the pull request link on the issue")
	fs.StringVar(&f.base, "base", "", "with --pr: branch to merge into (default: the remote's default branch)")
	fs.BoolVar(&f.draft, "draft", false, "with --pr: open pull requests as drafts")
	fs.StringVar(&f.api, "api", "", "GitHub API base URL (GitHub Enterprise: https://<host>/api/v3)")
	return f
}

func (f *issueFlags) active() bool { return f.issue != "" || f.issues != "" }
func (f *issueFlags) batch() bool  { return f.issues != "" }

// errNoIssues means a batch found nothing to do.
var errNoIssues = errors.New("no issues to run")

// load resolves the repository and reads the issue(s); it returns the task
// texts in order.
func (f *issueFlags) load(fs *flag.FlagSet, dir string) ([]string, error) {
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "draft" {
			f.draftSet = true
		}
	})
	if f.issue != "" && f.issues != "" {
		return nil, errors.New("give either --issue or --issues, not both")
	}
	if fs.NArg() > 0 {
		return nil, errors.New("give either --issue/--issues or a task, not both")
	}
	if f.limit < 1 {
		return nil, errors.New("--limit must be at least 1")
	}
	f.dir = dir
	ent := gh.EnterpriseHost()
	var originErr error
	if u, err := prGit(dir, nil, nil, "remote", "get-url", "origin"); err != nil {
		originErr = fmt.Errorf("%s has no git remote `origin` on GitHub", dir)
	} else {
		u = strings.TrimSpace(u)
		if f.api != "" {
			ent = hostOf(u)
		}
		f.origin, originErr = gh.ParseRemote(u, ent)
	}
	if f.issue != "" {
		ref, err := gh.ParseIssueRef(f.issue, ent)
		if err != nil {
			return nil, err
		}
		repo := ref.Repo
		if repo.IsZero() {
			if originErr != nil {
				return nil, fmt.Errorf("%w; give the issue as a URL", originErr)
			}
			repo = f.origin
		}
		if f.pr && originErr != nil {
			return nil, fmt.Errorf("--pr: %w", originErr)
		}
		c := f.client(repo)
		it, err := f.fetch(c, repo, ref.Number)
		if err != nil {
			return nil, err
		}
		f.items = []issueItem{it}
		return []string{it.task}, nil
	}

	// Batch.
	label, ok := strings.CutPrefix(f.issues, "label:")
	if label = strings.TrimSpace(label); !ok || label == "" {
		return nil, fmt.Errorf("--issues %q: want label:<name>", f.issues)
	}
	if originErr != nil {
		return nil, originErr
	}
	if err := cleanTree(dir); err != nil {
		return nil, err
	}
	c := f.client(f.origin)
	open, err := c.OpenIssues(f.origin, label, 0)
	if err != nil {
		return nil, err
	}
	pulls, err := c.OpenPulls(f.origin)
	if err != nil {
		return nil, err
	}
	taken := gh.ClosedBy(pulls)
	var tasks []string
	for _, is := range open {
		if len(tasks) == f.limit {
			break
		}
		if taken[is.Number] {
			fmt.Printf("skipping #%d: an open pull request already closes it\n", is.Number)
			continue
		}
		it, err := f.fetch(c, f.origin, is.Number)
		if err != nil {
			return nil, err
		}
		f.items = append(f.items, it)
		tasks = append(tasks, it.task)
	}
	if len(tasks) == 0 {
		fmt.Printf("no open issues labelled %q in %s without an open pull request\n", label, f.origin)
		return nil, errNoIssues
	}
	fmt.Printf("%d issue(s) labelled %q to run:", len(tasks), label)
	for _, it := range f.items {
		fmt.Printf(" #%d", it.issue.Number)
	}
	fmt.Println()
	return tasks, nil
}

func (f *issueFlags) client(repo gh.Repo) *gh.Client {
	api := f.api
	if api == "" {
		api = repo.APIBase()
	}
	tok, _ := prToken(repo.Host)
	c := gh.NewClient(api, tok)
	c.Notes = prOut
	return c
}

// fetch reads one issue (and its comments) and builds the task text.
func (f *issueFlags) fetch(c *gh.Client, repo gh.Repo, n int) (issueItem, error) {
	is, err := c.Issue(repo, n)
	if err != nil {
		return issueItem{}, fmt.Errorf("read issue %s#%d: %w", repo, n, err)
	}
	if is.PullRequest != nil {
		return issueItem{}, fmt.Errorf("%s#%d is a pull request, not an issue", repo, n)
	}
	if is.State == "closed" {
		fmt.Printf("note: %s#%d is closed\n", repo, n)
	}
	var comments []gh.Comment
	if f.withComments {
		if comments, err = c.Comments(repo, n); err != nil {
			return issueItem{}, fmt.Errorf("read comments of %s#%d: %w", repo, n, err)
		}
	}
	closes := fmt.Sprintf("#%d", n)
	if !f.origin.IsZero() && !repo.Same(f.origin) {
		closes = fmt.Sprintf("%s#%d", repo, n)
	}
	return issueItem{repo: repo, client: c, issue: *is, task: issueTask(*is, comments), closes: closes}, nil
}

// issueTask is the task text for an issue.
func issueTask(is gh.Issue, comments []gh.Comment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Fix GitHub issue #%d: %s\n\n", is.Number, strings.TrimSpace(is.Title))
	if body := strings.TrimSpace(strings.ReplaceAll(is.Body, "\r\n", "\n")); body != "" {
		b.WriteString(clipText(body, 20000) + "\n")
	}
	if labels := is.LabelNames(); len(labels) > 0 {
		fmt.Fprintf(&b, "\nLabels: %s\n", strings.Join(labels, ", "))
	}
	if len(comments) > 0 {
		b.WriteString("\nComments:\n")
		for _, c := range comments {
			fmt.Fprintf(&b, "\n@%s wrote:\n%s\n", c.User.Login, clipText(strings.TrimSpace(strings.ReplaceAll(c.Body, "\r\n", "\n")), 4000))
		}
	}
	return strings.TrimSpace(b.String())
}

// afterTask runs after task i: the pull request, the issue comment and, in
// a batch, returning the working tree to HEAD. stop ends the batch.
func (f *issueFlags) afterTask(i int, res orchestrator.TaskResult) (stop bool) {
	it := f.items[i]
	var pr *prResult
	if f.pr {
		switch {
		case !res.OK:
			fmt.Printf("no pull request for #%d: the task did not finish ok\n", it.issue.Number)
		case res.UndoKey == "":
			fmt.Printf("no pull request for #%d: the task's changes were not recorded (not a git repository?)\n", it.issue.Number)
		default:
			st, err := orchestrator.LoadTask(res.UndoKey)
			if err != nil {
				fmt.Printf("no pull request for #%d: %v\n", it.issue.Number, err)
				break
			}
			branch := "sy/issue-" + fmt.Sprint(it.issue.Number)
			if s := slugify(it.issue.Title, 32); s != "" {
				branch += "-" + s
			}
			pr, err = makePR(st, prOptions{base: f.base, branch: branch, draft: f.draft, draftSet: f.draftSet, yes: true, closes: it.closes, api: f.api})
			if err != nil {
				fmt.Printf("pull request for #%d failed: %v\n", it.issue.Number, err)
				pr = nil // a branch that was not pushed is no pull request
			}
		}
	}
	if pr != nil && pr.URL != "" {
		f.pulls++
		if f.comment {
			if !it.client.HasToken() {
				fmt.Printf("not commenting on #%d: no GitHub token\n", it.issue.Number)
			} else if err := it.client.AddComment(it.repo, it.issue.Number, fmt.Sprintf("Switchyard opened a pull request for this issue: %s", pr.URL)); err != nil {
				fmt.Printf("comment on #%d failed: %v\n", it.issue.Number, err)
			}
		}
	}
	if !f.batch() || !f.pr {
		return false
	}
	// Batch with --pr: the next issue must start from a clean tree.
	if pr != nil && res.UndoKey != "" {
		if _, err := orchestrator.Undo(f.dir, res.UndoKey, false, false); err != nil {
			fmt.Printf("could not take #%d's changes out of the working tree (%v); stopping the batch. They are on branch %s.\n", it.issue.Number, err, pr.Branch)
			return true
		}
		fmt.Printf("working tree back to HEAD: #%d's changes live on branch %s (sy undo --redo %s puts them back here)\n", it.issue.Number, pr.Branch, res.UndoKey)
	}
	if err := cleanTree(f.dir); err != nil {
		fmt.Printf("stopping the batch: the working tree is not clean after #%d", it.issue.Number)
		if res.UndoKey != "" {
			fmt.Printf(" (sy undo %s reverts that task's changes)", res.UndoKey)
		}
		fmt.Println()
		return true
	}
	return false
}

// cleanTree fails when dir has uncommitted changes or untracked files.
func cleanTree(dir string) error {
	s, err := prGit(dir, nil, nil, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("--issues needs a git repository: %w", err)
	}
	if strings.TrimSpace(s) != "" {
		return errors.New("--issues runs only on a clean working tree (commit or stash your changes first): each issue must start from HEAD, and sy never discards your work")
	}
	return nil
}
