package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/forge"
	"github.com/sparkz400/relayweft/internal/orchestrator"
)

// Issues as tasks (rw run --issue / --issues), on GitHub, GitLab or
// Gitea/Forgejo (the origin remote's forge, or the issue URL's):
//
//   - --issue N|URL reads the issue (title, body, labels; comments with
//     --with-comments) and runs "Fix GitHub issue #N: <title>" (GitLab
//     issue, Gitea issue) plus the body as the task. Public repositories
//     need no token.
//   - --issues label:<name> --pr runs the open issues with that label one
//     after another, unattended, oldest first; pull requests and issues an
//     open pull request already closes ("Closes #N") are skipped. A batch
//     needs --pr: without pull requests each task's changes would pile up
//     in the working tree.
//   - --pr turns each successful task into a pull request (the rw pr flow,
//     without asking) whose body says "Closes #N", and comments the link on
//     the issue (--comment=false turns that off). Nobody reviews that pull
//     request before it is pushed, so it is refused (the work stays in the
//     working tree and a batch stops) when it would carry more than the
//     agents' work: files no agent reported changing, or commits of HEAD
//     not on origin/<base>. A multi-repo workspace is refused up front.
//   - A batch never discards your work: it starts only on a clean working
//     tree, and after a task's pull request is open the task's changes are
//     taken out of the working tree with rw undo (they live on in the PR
//     branch, and `rw undo --redo <key>` puts them back), so the next issue
//     starts from HEAD. If that is not possible, or a task left changes
//     without a pull request, the batch stops.
//   - With --at/--in/--when-reset the issues are read, and the working tree
//     checked, when the run starts, not when it is scheduled.
//   - --team shares the label with other machines: each issue is claimed
//     on the issue tracker right before it runs (teamqueue.go), and
//     --every keeps pulling.

// issueFlags are rw run's issue flags and what they resolved to.
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
	team         bool
	every        time.Duration
	lease        time.Duration
	retryFailed  bool

	dir       string
	origin    forge.Repo // the origin remote's repository (zero if none)
	originErr error
	ref       forge.Ref // --issue, resolved by prepare
	label     string    // --issues label
	items     []issueItem
	pulls     int
	lastPR    *prResult // the last task's pull request (nil if none)
	noPR      int       // tasks that finished ok but got no pull request with --pr
}

type issueItem struct {
	repo   forge.Repo
	client forge.Client
	issue  forge.Issue
	task   string
	closes string // "#12" or "owner/repo#12"
}

func registerIssueFlags(fs *flag.FlagSet) *issueFlags {
	f := &issueFlags{}
	fs.StringVar(&f.issue, "issue", "", "run an issue (number or URL; GitHub, GitLab or Gitea) as the task")
	fs.StringVar(&f.issues, "issues", "", "label:<name>: run the open issues with this label one after another, unattended (needs --pr)")
	fs.IntVar(&f.limit, "limit", 5, "with --issues: at most this many issues")
	fs.BoolVar(&f.withComments, "with-comments", false, "with --issue(s): include the issue's comments in the task")
	fs.BoolVar(&f.pr, "pr", false, "with --issue(s): open a pull request (Closes #N) after each successful task")
	fs.BoolVar(&f.comment, "comment", true, "with --pr: comment the pull request link on the issue")
	fs.StringVar(&f.base, "base", "", "with --pr: branch to merge into (default: the remote's default branch)")
	fs.BoolVar(&f.draft, "draft", false, "with --pr: open pull requests as drafts")
	fs.StringVar(&f.api, "api", "", apiFlagHelp)
	fs.BoolVar(&f.team, "team", false, "with --issues: share the label with other machines; each issue is claimed on the issue tracker before it runs")
	fs.DurationVar(&f.every, "every", 0, "with --team: pull from the label again at this interval (e.g. 10m) until Ctrl+C, keeping the PC awake")
	fs.DurationVar(&f.lease, "lease", queueDefaultLease, "with --team: a claim lapses this long after its machine stopped renewing it")
	fs.BoolVar(&f.retryFailed, "retry-failed", false, "with --team: also take issues whose last claim failed")
	return f
}

func (f *issueFlags) active() bool { return f.issue != "" || f.issues != "" }
func (f *issueFlags) batch() bool  { return f.issues != "" }

// errNoIssues means a batch found nothing to do.
var errNoIssues = errors.New("no issues to run")

// load is prepare and fetch in one go.
func (f *issueFlags) load(fs *flag.FlagSet, dir string) ([]string, error) {
	if err := f.prepare(fs, dir); err != nil {
		return nil, err
	}
	return f.fetch()
}

// prepare checks the flags and resolves the repository, without reading
// any issue: a scheduled run reads them when it starts (fetch).
func (f *issueFlags) prepare(fs *flag.FlagSet, dir string) error {
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "draft" {
			f.draftSet = true
		}
	})
	if f.issue != "" && f.issues != "" {
		return errors.New("give either --issue or --issues, not both")
	}
	if f.issue != "" && (f.team || f.every != 0 || f.retryFailed) {
		return errors.New("--team, --every and --retry-failed go with --issues label:<name>")
	}
	if fs.NArg() > 0 {
		return errors.New("give either --issue/--issues or a task, not both")
	}
	if f.limit < 1 {
		return errors.New("--limit must be at least 1")
	}
	f.dir = dir
	hosts := forge.EnvHosts()
	if u, err := originURL(dir); err != nil {
		f.originErr = fmt.Errorf("%s has no git remote `origin` on GitHub, GitLab or Gitea", dir)
	} else {
		hosts = forgeHosts(u, f.api)
		f.origin, f.originErr = forge.ParseRemote(u, hosts)
	}
	if f.issue != "" {
		ref, err := forge.ParseIssueRef(f.issue, hosts)
		if err != nil {
			return err
		}
		if ref.Repo.IsZero() {
			if f.originErr != nil {
				return fmt.Errorf("%w; give the issue as a URL", f.originErr)
			}
			ref.Repo = f.origin
		}
		if f.pr && f.originErr != nil {
			return fmt.Errorf("--pr: %w", f.originErr)
		}
		f.ref = ref
		return nil
	}
	label, ok := strings.CutPrefix(f.issues, "label:")
	if label = strings.TrimSpace(label); !ok || label == "" {
		return fmt.Errorf("--issues %q: want label:<name>", f.issues)
	}
	if !f.pr {
		return errors.New("--issues needs --pr: each task's changes go to its pull request so the next issue starts from HEAD (for one issue without a pull request use --issue N)")
	}
	if !f.team && (f.every != 0 || f.retryFailed) {
		return errors.New("--every and --retry-failed go with --team")
	}
	if f.every < 0 || (f.every > 0 && f.every < time.Minute) {
		return errors.New("--every must be at least 1m")
	}
	if f.lease < 3*time.Minute {
		return errors.New("--lease must be at least 3m")
	}
	f.label = label
	return f.originErr
}

// checkWorkspace refuses --pr for a multi-repo workspace: rw undo takes a
// task's changes out of every repo, but one pull request only holds the
// primary repo's (rw pr --repo opens the others by hand).
func (f *issueFlags) checkWorkspace(repos []orchestrator.Repo) error {
	if !f.pr || len(repos) == 0 {
		return nil
	}
	return errors.New("--pr with --issue/--issues does not work in a multi-repo workspace (workspace.repos or --repo): a task's pull request would hold only this repo's changes. Run the issue without --pr, then open each repo's pull request with rw pr <task> [--repo <name>]")
}

// fetch reads the issue(s) and returns the task texts in order. A batch
// checks for a clean working tree first.
func (f *issueFlags) fetch() ([]string, error) {
	if f.issue != "" {
		c := f.client(f.ref.Repo)
		it, err := f.fetchOne(c, f.ref.Repo, f.ref.Number)
		if err != nil {
			return nil, err
		}
		f.items = []issueItem{it}
		return []string{it.task}, nil
	}

	// Batch.
	if err := cleanTree(f.dir); err != nil {
		return nil, err
	}
	c := f.client(f.origin)
	open, err := c.OpenIssues(f.origin, f.label, 0)
	if err != nil {
		return nil, err
	}
	pulls, err := c.OpenPulls(f.origin)
	if err != nil {
		return nil, err
	}
	taken := forge.ClosedBy(pulls)
	var tasks []string
	for _, is := range open {
		if len(tasks) == f.limit {
			break
		}
		if taken[is.Number] {
			fmt.Printf("skipping #%d: an open %s already closes it\n", is.Number, f.origin.Kind.PullNoun())
			continue
		}
		it, err := f.fetchOne(c, f.origin, is.Number)
		if err != nil {
			return nil, err
		}
		f.items = append(f.items, it)
		tasks = append(tasks, it.task)
	}
	if len(tasks) == 0 {
		fmt.Printf("no open issues labelled %q in %s without an open %s\n", f.label, f.origin, f.origin.Kind.PullNoun())
		return nil, errNoIssues
	}
	fmt.Printf("%d issue(s) labelled %q to run:", len(tasks), f.label)
	for _, it := range f.items {
		fmt.Printf(" #%d", it.issue.Number)
	}
	fmt.Println()
	return tasks, nil
}

func (f *issueFlags) client(repo forge.Repo) forge.Client {
	return forgeClient(repo, f.apiFor(repo), prOut)
}

// apiFor is the API override for repo. --api is the origin's (as for rw
// pr): an issue given as a URL on another host is read from that host's
// own API, with its own token, never through the origin's.
func (f *issueFlags) apiFor(repo forge.Repo) string {
	if f.api == "" || f.origin.IsZero() || strings.EqualFold(repo.Host, f.origin.Host) || forge.APIServes(f.api, repo.Host) {
		return f.api
	}
	return ""
}

// fetchOne reads one issue (and its comments) and builds the task text.
func (f *issueFlags) fetchOne(c forge.Client, repo forge.Repo, n int) (issueItem, error) {
	is, err := c.Issue(repo, n)
	if err != nil {
		return issueItem{}, fmt.Errorf("read issue %s#%d: %w", repo, n, err)
	}
	if is.IsPull {
		return issueItem{}, fmt.Errorf("%s#%d is a pull request, not an issue", repo, n)
	}
	if is.State == "closed" {
		fmt.Printf("note: %s#%d is closed\n", repo, n)
	}
	var comments []forge.Comment
	if f.withComments {
		all, err := c.Comments(repo, n)
		if err != nil {
			return issueItem{}, fmt.Errorf("read comments of %s#%d: %w", repo, n, err)
		}
		for _, cm := range all {
			if !isQueueComment(cm.Body) { // team queue claims are no part of the task
				comments = append(comments, cm)
			}
		}
	}
	closes := fmt.Sprintf("#%d", n)
	if !f.origin.IsZero() && !repo.Same(f.origin) {
		closes = fmt.Sprintf("%s#%d", repo, n)
	}
	return issueItem{repo: repo, client: c, issue: *is, task: issueTask(repo, *is, comments), closes: closes}, nil
}

// issueTask is the task text for an issue of repo.
func issueTask(repo forge.Repo, is forge.Issue, comments []forge.Comment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Fix %s issue #%d: %s\n\n", repo.ForgeName(), is.Number, strings.TrimSpace(is.Title))
	if body := strings.TrimSpace(strings.ReplaceAll(is.Body, "\r\n", "\n")); body != "" {
		b.WriteString(clipText(body, 20000) + "\n")
	}
	if len(is.Labels) > 0 {
		fmt.Fprintf(&b, "\nLabels: %s\n", strings.Join(is.Labels, ", "))
	}
	if len(comments) > 0 {
		b.WriteString("\nComments:\n")
		for _, c := range comments {
			fmt.Fprintf(&b, "\n@%s wrote:\n%s\n", c.Author, clipText(strings.TrimSpace(strings.ReplaceAll(c.Body, "\r\n", "\n")), 4000))
		}
	}
	return strings.TrimSpace(b.String())
}

// afterTask runs after task i: the pull request, the issue comment and, in
// a batch, returning the working tree to HEAD. stop ends the batch.
func (f *issueFlags) afterTask(i int, res orchestrator.TaskResult) (stop bool) {
	it := f.items[i]
	var pr *prResult
	defer func() { f.lastPR = pr }()
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
			branch := "rw/issue-" + fmt.Sprint(it.issue.Number)
			if s := slugify(it.issue.Title, 32); s != "" {
				branch += "-" + s
			}
			// An earlier run's branch (its pull request closed, say) is
			// never reused: the new work gets a branch of its own.
			branch = freeBranch(st.Dir, branch)
			pr, err = makePR(st, prOptions{base: f.base, branch: branch, draft: f.draft, draftSet: f.draftSet, yes: true, unattended: true, closes: it.closes, api: f.api})
			if err != nil {
				fmt.Printf("%s for #%d failed: %v\n", it.repo.Kind.PullNoun(), it.issue.Number, err)
				pr = nil // a branch that was not pushed is no pull request
			}
		}
	}
	if f.pr && res.OK && (pr == nil || pr.URL == "") {
		// --pr asked for a pull request: without one the run failed, so CI
		// jobs and scripts see it in the exit status.
		f.noPR++
	}
	if pr != nil && pr.URL != "" {
		f.pulls++
		if f.comment && !f.team { // with --team the claim comment carries the link
			if !it.client.HasToken() {
				fmt.Printf("not commenting on #%d: %s\n", it.issue.Number, noTokenText(it.repo.Kind))
			} else if err := it.client.CommentIssue(it.repo, it.issue.Number, fmt.Sprintf("Relayweft opened a %s for this issue: %s", it.repo.Kind.PullNoun(), pr.URL)); err != nil {
				fmt.Printf("comment on #%d failed: %v\n", it.issue.Number, err)
			}
		}
	}
	if !f.batch() {
		return false
	}
	// Batch (always --pr): the next issue must start from a clean tree.
	if pr != nil && res.UndoKey != "" {
		if _, err := orchestrator.Undo(f.dir, res.UndoKey, false, false); err != nil {
			fmt.Printf("could not take #%d's changes out of the working tree (%v); stopping the batch. They are on branch %s.\n", it.issue.Number, err, pr.Branch)
			return true
		}
		fmt.Printf("working tree back to HEAD: #%d's changes live on branch %s (rw undo --redo %s puts them back here)\n", it.issue.Number, pr.Branch, res.UndoKey)
	}
	if err := cleanTree(f.dir); err != nil {
		fmt.Printf("stopping the batch: the working tree is not clean after #%d", it.issue.Number)
		if res.UndoKey != "" {
			fmt.Printf(" (rw undo %s reverts that task's changes)", res.UndoKey)
		}
		fmt.Println()
		return true
	}
	return false
}

// freeBranch is name, or name-2, name-3, ... when name already exists
// here or on origin.
func freeBranch(dir, name string) string {
	for i := 1; i < 100; i++ {
		b := name
		if i > 1 {
			b = fmt.Sprintf("%s-%d", name, i)
		}
		if _, err := prGit(dir, nil, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+b); err == nil {
			continue
		}
		if prRemoteHas(dir, b) {
			continue
		}
		return b
	}
	return name
}

// cleanTree fails when dir has uncommitted changes, untracked files or
// changed submodules. The flags override config (status.showUntrackedFiles,
// submodule.<name>.ignore) that would hide some of them.
func cleanTree(dir string) error {
	s, err := prGit(dir, nil, nil, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return fmt.Errorf("--issues needs a git repository: %w", err)
	}
	if strings.TrimSpace(s) != "" {
		return errors.New("--issues runs only on a clean working tree (commit or stash your changes first): each issue must start from HEAD, and rw never discards your work")
	}
	return nil
}
