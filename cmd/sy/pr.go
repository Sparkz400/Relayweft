package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/forge"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/proc"
)

// sy pr turns a finished task into a branch, a commit and a pull request
// on GitHub, GitLab (a merge request) or Gitea/Forgejo, whichever hosts the
// origin remote (forge.go):
//
//   - The change is the task's own: its undo "after" snapshot against its
//     "before" snapshot (refs/switchyard/tasks/...), so edits you made
//     before the task are not included.
//   - The commit is built without touching your index, working tree or
//     current branch: the task's diff is applied to HEAD's tree on a
//     temporary index (GIT_INDEX_FILE) and committed with commit-tree on
//     top of HEAD. If it does not apply cleanly to HEAD, nothing is created.
//   - The branch (sy/<slug>) must not exist yet; it is pushed with a plain
//     `git push -u origin <branch>` (never forced) using your git remote and
//     credentials.
//   - The pull request is opened through the forge's REST API with its
//     token (forge.Token: GITHUB_TOKEN, GITLAB_TOKEN, GITEA_TOKEN, ...);
//     without one, sy writes the body to a file and prints the compare URL
//     instead.
//   - An opened pull request is recorded for sy watch (watch.go), which
//     follows up on its failed checks and review comments.

// Package vars so tests can drive sy pr without a terminal, network or gh.
var (
	prIn    io.Reader = os.Stdin
	prOut   io.Writer = os.Stdout
	prToken           = forge.Token
	// prPush pushes the branch; it is interactive (credential prompts).
	prPush = func(root, remote, branch string) error {
		cmd := exec.Command("git", "push", "-u", remote, branch)
		cmd.Dir = root
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
		return cmd.Run()
	}
)

// prOptions are the knobs of the sy pr flow (also used by sy run --pr).
type prOptions struct {
	base     string
	branch   string
	title    string
	api      string // API base URL override (self-hosted forges, tests)
	draft    bool
	draftSet bool // --draft given explicitly (else: draft unless the task is done)
	noPush   bool
	yes      bool
	closes   string // "#12" or "owner/repo#12": adds "Closes ..." to the body
	// unattended: nobody looks at the preview (sy run --issue(s) --pr).
	// The PR is refused when it would carry more than the agents' work:
	// files no agent reported changing (your own edits made while the task
	// ran, a secrets file) or commits of HEAD that are not on origin/<base>.
	// A multi-repo task is refused too (one PR could not hold it).
	unattended bool
}

// prResult is what sy pr created.
type prResult struct {
	Branch string
	Commit string
	Files  []string
	URL    string // the pull request; "" when none was opened
	Number int
	Repo   forge.Repo
}

func cmdPR(args []string) error {
	fs := flag.NewFlagSet("sy pr", flag.ExitOnError)
	var o prOptions
	dir := fs.String("dir", "", "project directory (default current directory)")
	repoName := fs.String("repo", "", "for a multi-repo task: open the PR of this extra repo (its name from --repo / workspace.repos)")
	fs.StringVar(&o.base, "base", "", "branch to merge into (default: the remote's default branch)")
	fs.StringVar(&o.branch, "branch", "", "branch to create (default sy/<slug of the task>)")
	fs.StringVar(&o.title, "title", "", "pull request title (default: the task's first line)")
	fs.BoolVar(&o.draft, "draft", false, "open as a draft (default: draft when the task did not finish ok)")
	fs.BoolVar(&o.noPush, "no-push", false, "only create the local branch and commit")
	fs.BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	fs.StringVar(&o.api, "api", "", apiFlagHelp)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: sy pr [task-id] [--repo name] [--base main] [--branch name] [--draft] [--title text] [--no-push] [--yes]

Turns a finished task (default: the newest finished task in this directory,
see sy history) into a branch, a commit and a pull request on GitHub,
GitLab (a merge request) or Gitea/Forgejo: whichever hosts origin.

The commit holds exactly the task's changes (its undo snapshots), applied
on top of HEAD without touching your index, working tree or current branch;
if they do not apply cleanly to HEAD, nothing is created. The branch
(sy/<task>) must not exist yet and is pushed with git push -u origin (never
forced). The PR is opened with the forge's token (GitHub: GITHUB_TOKEN,
GH_TOKEN or `+"`gh auth token`"+`; GitLab: GITLAB_TOKEN or glab; Gitea: GITEA_TOKEN);
without one the body is written to a file and the compare URL is printed.
Self-hosted forges: set GH_HOST, GITLAB_HOST or GITEA_HOST to the host. A task that did not finish ok is opened as a draft.
A multi-repo task gets one PR per repo: --repo <name> picks an extra repo.
An opened pull request is followed up by sy watch (failed checks, reviews).
`)
	}
	var id string
	rest := args
	for len(rest) > 0 {
		fs.Parse(rest)
		if fs.NArg() == 0 {
			break
		}
		if id == "" {
			id = fs.Arg(0)
		} else {
			return fmt.Errorf("unexpected argument %q", fs.Arg(0))
		}
		rest = fs.Args()[1:]
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "draft" {
			o.draftSet = true
		}
	})
	d, err := absDir(*dir)
	if err != nil {
		return err
	}
	st, err := findPRTask(d, id)
	if err != nil {
		return err
	}
	others := st.Repos
	if *repoName != "" {
		if st, err = repoTask(st, *repoName); err != nil {
			return err
		}
		others = nil
	}
	_, err = makePR(st, o)
	if err == nil && len(others) > 0 {
		// The snapshots of a multi-repo task are recorded in every repo
		// under the same key: each repo gets a PR of its own.
		fmt.Fprintln(prOut, "\nthis task also changed other repos; open their pull requests with:")
		for _, r := range others {
			fmt.Fprintf(prOut, "  sy pr %s --repo %s    (%s)\n", st.ID, r.Name, r.Dir)
		}
	}
	return err
}

// repoTask is the task as seen from one of its extra repos.
func repoTask(st *orchestrator.TaskState, name string) (*orchestrator.TaskState, error) {
	var names []string
	for _, r := range st.Repos {
		if r.Name == name {
			c := *st
			c.Dir, c.Repos = r.Dir, nil
			return &c, nil
		}
		names = append(names, r.Name)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("task %s changed only one repo; --repo is for multi-repo tasks", st.ID)
	}
	return nil, fmt.Errorf("task %s has no repo %q (its repos: %s)", st.ID, name, strings.Join(names, ", "))
}

// findPRTask picks the task by id or the newest finished one in dir.
func findPRTask(dir, id string) (*orchestrator.TaskState, error) {
	if id != "" {
		st, err := orchestrator.LoadTask(id)
		if err != nil {
			return nil, err
		}
		if st.Status == "running" {
			if st.Interrupted() {
				return nil, fmt.Errorf("task %s was interrupted; finish it first (sy resume %s)", st.ID, st.ID)
			}
			return nil, fmt.Errorf("task %s is still running", st.ID)
		}
		return st, nil
	}
	for _, s := range orchestrator.History(dir, 50) {
		if s.Status != "running" && s.UndoKey != "" {
			s := s
			return &s, nil
		}
	}
	return nil, errors.New("no finished task with recorded changes in this directory (see sy history; tasks are recorded when sy runs in a git repo)")
}

// makePR runs the whole flow for one task. A nil result with a nil error
// means the user said no.
func makePR(st *orchestrator.TaskState, o prOptions) (*prResult, error) {
	out := prOut
	if st.UndoKey == "" {
		return nil, fmt.Errorf("task %s has no recorded changes (it did not run in a git repository)", st.ID)
	}
	root, err := prGit(st.Dir, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("task %s ran in %s, which is not a git repository now: %w", st.ID, st.Dir, err)
	}
	root = strings.TrimSpace(root)
	snap, err := taskSnapshots(root, st.UndoKey)
	if err != nil {
		return nil, err
	}

	// Where it goes: the origin remote's forge.
	var repo forge.Repo
	var repoErr error
	if u, err := originURL(root); err != nil {
		repoErr = errors.New("this repository has no `origin` remote")
	} else {
		repo, repoErr = forge.ParseRemote(u, forgeHosts(u, o.api))
	}
	if repoErr != nil && !o.noPush {
		return nil, fmt.Errorf("%w (use --no-push to only create the local branch)", repoErr)
	}

	title := o.title
	if title == "" {
		title = defuseRefs(prTitle(st.Task))
	}
	commit, files, err := buildPRCommit(root, snap.Before, snap.After, defuseRefs(commitMessage(st, title)))
	if err != nil {
		return nil, err
	}

	branch := o.branch
	if branch == "" {
		branch = "sy/" + taskSlug(st)
	}
	if _, err := prGit(root, nil, nil, "check-ref-format", "--branch", branch); err != nil {
		return nil, fmt.Errorf("%q is not a valid branch name", branch)
	}
	if branchExists(root, branch) {
		return nil, fmt.Errorf("branch %s already exists; pick another with --branch (sy never overwrites a branch)", branch)
	}

	draft := o.draft
	if !o.draftSet {
		draft = st.Status != "done"
	}
	var client forge.Client
	if !o.noPush && !repo.IsZero() {
		client = forgeClient(repo, o.api, out)
	}
	base := o.base
	if base == "" {
		base = detectBase(root, client, repo)
	}
	body := renderPRBody(st, prBodyOptions{Closes: o.closes, Version: version, Draft: draft, Template: prTemplate(root)})
	unreported, unrepErr := unreportedFiles(root, st.UndoKey)
	ahead, aheadErr := aheadCount(root, base)
	if o.unattended {
		if err := unattendedPRCheck(st, base, unreported, unrepErr, ahead, aheadErr, o.noPush); err != nil {
			return nil, err
		}
	}

	// Preview.
	fmt.Fprintf(out, "\nPull request from task %s (%s):\n  %s\n\n", st.ID, st.Status, oneLine(st.Task, 200))
	fmt.Fprintf(out, "%d file(s) changed:\n", len(files))
	for i, f := range files {
		if i == 40 {
			fmt.Fprintf(out, "  ... and %d more\n", len(files)-40)
			break
		}
		fmt.Fprintln(out, "  "+f)
	}
	fmt.Fprintf(out, "\nbranch: %s (new, on top of HEAD)\n", branch)
	fmt.Fprintf(out, "title:  %s\n", title)
	if o.noPush {
		fmt.Fprintln(out, "push:   no (--no-push): only the local branch is created")
	} else {
		kind := repo.Kind.PullNoun()
		if draft {
			kind = "draft " + kind
		}
		fmt.Fprintf(out, "into:   %s %s on %s (git push -u origin %s)\n", kind, base, repo, branch)
		if ahead > 0 {
			fmt.Fprintf(out, "WARNING: HEAD has %d commit(s) that are not on origin/%s; the pull request includes them\n", ahead, base)
		}
	}
	if len(unreported) > 0 {
		fmt.Fprintf(out, "WARNING: %d file(s) changed while the task ran that no agent reported changing (your own edits? check them before you push):\n", len(unreported))
		for _, f := range unreported {
			fmt.Fprintln(out, "  ! "+f)
		}
	}
	if st.Status != "done" {
		fmt.Fprintf(out, "note:   the task is %s, not done: review it carefully\n", st.Status)
	}
	if !o.yes {
		fmt.Fprint(out, "\nCreate it? [y/N] ")
		ans, _ := bufio.NewReader(prIn).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			fmt.Fprintln(out, "Nothing created.")
			return nil, nil
		}
	}

	if _, err := prGit(root, nil, nil, "update-ref", "-m", "sy pr: task "+st.ID, "refs/heads/"+branch, commit, ""); err != nil {
		return nil, fmt.Errorf("create branch %s: %w", branch, err)
	}
	res := &prResult{Branch: branch, Commit: commit, Files: files, Repo: repo}
	fmt.Fprintf(out, "created branch %s at %s (your working tree and index are unchanged)\n", branch, short(commit))
	if o.noPush {
		fmt.Fprintf(out, "push it with: git push -u origin %s\n", branch)
		if !repo.IsZero() {
			if p, err := writePRBody(body); err == nil {
				fmt.Fprintf(out, "then open: %s\n(the pull request text is in %s)\n", repo.CompareURL(base, branch), p)
			}
		}
		return res, nil
	}
	if err := prPush(root, "origin", branch); err != nil {
		return res, fmt.Errorf("git push -u origin %s failed: %w (the branch stays local; push it yourself, then open the PR)", branch, err)
	}
	fallback := func() {
		if p, err := writePRBody(body); err == nil {
			fmt.Fprintf(out, "pull request text written to %s\n", p)
		}
		fmt.Fprintf(out, "open the pull request here: %s\n", repo.CompareURL(base, branch))
	}
	if client == nil || !client.HasToken() {
		fmt.Fprintf(out, "%s: open the %s in the browser\n", noTokenText(repo.Kind), repo.Kind.PullNoun())
		fallback()
		return res, nil
	}
	pr, err := client.CreatePull(repo, forge.NewPull{Title: title, Head: branch, Base: base, Body: body, Draft: draft})
	if err != nil {
		fallback()
		return res, fmt.Errorf("open %s: %w", repo.Kind.PullNoun(), err)
	}
	res.URL, res.Number = pr.URL, pr.Number
	fmt.Fprintf(out, "opened %s %s%d: %s\n", repo.Kind.PullNoun(), repo.PullSign(), pr.Number, pr.URL)
	err = recordWatch(watchEntry{
		Forge: forgeName(repo.Kind), Root: root, Host: repo.Host, Web: repo.Web, Owner: repo.Owner, Name: repo.Name, Number: pr.Number, URL: pr.URL, Title: title,
		Branch: branch, Head: commit, Base: base, TaskID: st.ID, Author: st.Author(), API: o.api, Added: time.Now(),
	})
	if err != nil {
		fmt.Fprintf(out, "note: not recorded for sy watch: %v\n", err)
	} else {
		fmt.Fprintln(out, "sy watch follows up on its failed checks and review comments")
	}
	return res, nil
}

// taskSnapshots finds the task's before/after snapshot commits.
func taskSnapshots(root, key string) (orchestrator.UndoTask, error) {
	list, err := orchestrator.UndoList(root)
	if err != nil {
		return orchestrator.UndoTask{}, err
	}
	for _, t := range list {
		if t.Key == key {
			return t, nil
		}
	}
	return orchestrator.UndoTask{}, fmt.Errorf("the snapshots of task %s are gone from this working tree (sy keeps the newest 30; sy undo --list)", key)
}

// buildPRCommit applies the before -> after diff to HEAD's tree on a
// temporary index and commits it on top of HEAD. The user's index, working
// tree and branches are not touched. files are "M path" lines.
func buildPRCommit(root, before, after, msg string) (commit string, files []string, err error) {
	head, err := prGit(root, nil, nil, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	if err != nil {
		return "", nil, errors.New("this repository has no commits yet; commit once before sy pr")
	}
	head = strings.TrimSpace(head)
	patch, err := prGit(root, nil, nil, "-c", "core.quotepath=false", "diff", "--binary", "--full-index", "--no-color",
		"--no-ext-diff", "--no-textconv", "--no-renames", "--src-prefix=a/", "--dst-prefix=b/", before, after)
	if err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(patch) == "" {
		return "", nil, errors.New("the task changed no files: nothing to put in a pull request")
	}
	f, err := os.CreateTemp("", "sy-pr-index-*")
	if err != nil {
		return "", nil, err
	}
	idx := f.Name()
	f.Close()
	os.Remove(idx) // git creates it
	defer os.Remove(idx)
	env := []string{"GIT_INDEX_FILE=" + idx}
	if _, err := prGit(root, env, nil, "read-tree", head); err != nil {
		return "", nil, err
	}
	if _, err := prGit(root, env, []byte(patch), "apply", "--cached", "--binary", "--whitespace=nowarn", "-"); err != nil {
		return "", nil, fmt.Errorf("the task's changes do not apply cleanly to HEAD (%s); commit or rebase so HEAD matches what the task started from, then try again. Nothing was created. (%v)", short(head), err)
	}
	tree, err := prGit(root, env, nil, "write-tree")
	if err != nil {
		return "", nil, err
	}
	tree = strings.TrimSpace(tree)
	if headTree, _ := prGit(root, nil, nil, "rev-parse", head+"^{tree}"); strings.TrimSpace(headTree) == tree {
		return "", nil, errors.New("HEAD already contains the task's changes (committed already?): nothing to put in a pull request")
	}
	commit, err = prGit(root, nil, []byte(msg), "commit-tree", tree, "-p", head, "-F", "-")
	if err != nil {
		return "", nil, err
	}
	commit = strings.TrimSpace(commit)
	ns, err := prGit(root, nil, nil, "-c", "core.quotepath=false", "diff-tree", "-r", "--no-renames", "--name-status", head, commit)
	if err != nil {
		return "", nil, err
	}
	for _, l := range strings.Split(strings.TrimSpace(ns), "\n") {
		if st, p, ok := strings.Cut(l, "\t"); ok {
			files = append(files, st+" "+p)
		}
	}
	return commit, files, nil
}

func branchExists(root, branch string) bool {
	_, err := prGit(root, nil, nil, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	return err == nil
}

// detectBase picks the base branch: origin/HEAD, then the forge's default
// branch, then main.
func detectBase(root string, client forge.Client, repo forge.Repo) string {
	if s, err := prGit(root, nil, nil, "symbolic-ref", "--short", "-q", "refs/remotes/origin/HEAD"); err == nil {
		if b := strings.TrimPrefix(strings.TrimSpace(s), "origin/"); b != "" {
			return b
		}
	}
	if client != nil && !repo.IsZero() {
		if b, err := client.DefaultBranch(repo); err == nil {
			return b
		}
	}
	return "main"
}

// aheadCount counts HEAD's commits that origin/<base> lacks; an error
// means it is unknown (no origin/<base> ref: never fetched).
func aheadCount(root, base string) (int, error) {
	s, err := prGit(root, nil, nil, "rev-list", "--count", "refs/remotes/origin/"+base+"..HEAD")
	if err != nil {
		return 0, fmt.Errorf("no origin/%s to compare HEAD with (git fetch origin)", base)
	}
	var n int
	fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n, nil
}

// unreportedFiles are the files the task changed although no agent reported
// changing them (orchestrator.UndoPlan.Unreported), sorted.
func unreportedFiles(root, key string) ([]string, error) {
	plan, err := orchestrator.PreviewUndo(root, key, false)
	if plan.Task.Key == "" {
		// Undone already (sy undo): the redo preview has the same files.
		plan, err = orchestrator.PreviewUndo(root, key, true)
	}
	if plan.Task.Key == "" {
		return nil, err
	}
	out := append([]string(nil), plan.Unreported...)
	sort.Strings(out)
	return out, nil
}

// unattendedPRCheck refuses a pull request nobody reviewed when it would
// carry more than the agents' own work. The task's changes stay in the
// working tree either way.
func unattendedPRCheck(st *orchestrator.TaskState, base string, unreported []string, unrepErr error, ahead int, aheadErr error, noPush bool) error {
	if len(st.Repos) > 0 {
		return fmt.Errorf("task %s changed several repos; an unattended run opens no pull requests for multi-repo tasks (open them with sy pr %s and sy pr %s --repo <name>)", st.ID, st.ID, st.ID)
	}
	if unrepErr != nil {
		return fmt.Errorf("cannot tell which files the agents changed (%v); not opening a pull request nobody reviewed (check, then run sy pr %s)", unrepErr, st.ID)
	}
	if len(unreported) > 0 {
		list := unreported
		if len(list) > 10 {
			list = append(list[:10:10], fmt.Sprintf("... %d more", len(unreported)-10))
		}
		return fmt.Errorf("not opening a pull request nobody reviewed: %d file(s) changed while the task ran that no agent reported changing (your own edits?): %s. The changes stay in the working tree; check them, then run sy pr %s", len(unreported), strings.Join(list, ", "), st.ID)
	}
	if noPush {
		return nil
	}
	if aheadErr != nil {
		return fmt.Errorf("not opening a pull request nobody reviewed: cannot tell whether HEAD has unpushed commits: %v", aheadErr)
	}
	if ahead > 0 {
		return fmt.Errorf("not opening a pull request nobody reviewed: HEAD has %d commit(s) that are not on origin/%s and the pull request would include them (push them or check out %s first, then run sy pr %s)", ahead, base, base, st.ID)
	}
	return nil
}

// apiFlagHelp is the --api flag's text.
const apiFlagHelp = "forge API base URL (GitHub Enterprise: https://<host>/api/v3, GitLab: https://<host>/api/v4, Gitea: https://<host>/api/v1; GH_HOST, GITLAB_HOST and GITEA_HOST also work)"

// forgeName is the kind stored in sy watch's list ("" for GitHub, as in
// lists from before GitLab and Gitea).
func forgeName(k forge.Kind) string {
	if k == forge.GitHub {
		return ""
	}
	return string(k)
}

func hostOf(remote string) string {
	if i := strings.Index(remote, "://"); i >= 0 {
		h := remote[i+3:]
		if j := strings.IndexAny(h, "/"); j >= 0 {
			h = h[:j]
		}
		if j := strings.LastIndex(h, "@"); j >= 0 {
			h = h[j+1:]
		}
		if j := strings.Index(h, ":"); j >= 0 {
			h = h[:j]
		}
		return h
	}
	if at := strings.Index(remote, ":"); at > 0 {
		h := remote[:at]
		if j := strings.LastIndex(h, "@"); j >= 0 {
			h = h[j+1:]
		}
		return h
	}
	return ""
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

func writePRBody(body string) (string, error) {
	f, err := os.CreateTemp("", "sy-pr-body-*.md")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(body); err != nil {
		return "", err
	}
	return filepath.Clean(f.Name()), nil
}

// prGit runs git in dir and returns its stdout unmodified.
func prGit(dir string, env []string, stdin []byte, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	proc.Background(cmd)
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("git %s: %s", args[0], oneLine(msg, 400))
	}
	return out.String(), nil
}

// firstLine is the task's first non-empty line, whitespace collapsed.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			return l
		}
	}
	return ""
}

// prTitle is the task's first line cut to 72 characters at a word boundary.
func prTitle(task string) string {
	t := firstLine(task)
	if t == "" {
		return "Switchyard task"
	}
	return clipWords(t, 72)
}

func clipWords(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n-3])
	if i := strings.LastIndex(cut, " "); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " .,;:-") + "..."
}

// taskSlug makes a branch-safe name from the task text.
func taskSlug(st *orchestrator.TaskState) string {
	if s := slugify(firstLine(st.Task), 40); s != "" {
		return s
	}
	return "task-" + slugify(st.ID, 30)
}

func slugify(s string, n int) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > n {
		out = out[:n]
		if i := strings.LastIndex(out, "-"); i > n/2 {
			out = out[:i]
		}
		out = strings.Trim(out, "-")
	}
	return out
}

// commitMessage: the title, then the plan.
func commitMessage(st *orchestrator.TaskState, title string) string {
	var b strings.Builder
	b.WriteString(title + "\n\n")
	if t := strings.TrimSpace(st.Task); t != firstLine(st.Task) || len([]rune(t)) > 72 {
		b.WriteString(wrapText(clipText(t, 2000), 72) + "\n\n")
	}
	if st.Plan != nil {
		if s := strings.TrimSpace(st.Plan.Summary); s != "" {
			b.WriteString("Plan: " + s + "\n")
		}
		for _, sub := range st.Plan.Subtasks {
			mark := "not run"
			if r, ok := st.Results[sub.ID]; ok {
				mark = map[bool]string{true: "ok", false: "FAILED"}[r.OK]
			}
			fmt.Fprintf(&b, "- %s: %s (%s)\n", sub.ID, oneLine(sub.Title, 80), mark)
		}
		b.WriteString("\n")
	}
	if st.Summary != "" {
		b.WriteString("Result: " + oneLine(st.Summary, 300) + "\n")
	}
	fmt.Fprintf(&b, "Switchyard task: %s\n", st.ID)
	return b.String()
}

func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n[...]"
}

// wrapText re-wraps each paragraph to width.
func wrapText(s string, width int) string {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		words := strings.Fields(line)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		cur := words[0]
		for _, w := range words[1:] {
			if len(cur)+1+len(w) > width {
				out = append(out, cur)
				cur = w
			} else {
				cur += " " + w
			}
		}
		out = append(out, cur)
	}
	return strings.Join(out, "\n")
}

type prBodyOptions struct {
	Closes  string // "#12" or "owner/repo#12"
	Version string
	Draft   bool
	// Template is the repo's pull request template: its headings are
	// filled from the task instead of the default layout (prtemplate.go).
	Template string
}

// prParts are the pieces of a pull request description, as Markdown.
type prParts struct {
	warning string // the task did not finish ok ("" otherwise)
	task    string // the task text, fenced
	summary string // the plan's one-line summary
	plan    string // the steps table ("" without a plan)
	result  string // status, summary, checks and cost
	checks  string // the checks line alone ("" when none ran)
	footer  string
}

// renderPRBody writes the pull request description: the task, the plan
// with each step's result, checks, cost and how to undo it locally; laid
// out by the repo's template when it has one.
func renderPRBody(st *orchestrator.TaskState, o prBodyOptions) string {
	p := renderPRParts(st, o)
	body := ""
	if o.Template != "" {
		body = fillPRTemplate(o.Template, p)
	}
	if body == "" {
		var b strings.Builder
		b.WriteString(p.warning)
		b.WriteString("## Task\n\n" + p.task)
		if p.plan != "" {
			b.WriteString("\n## Plan\n\n")
			if p.summary != "" {
				b.WriteString(p.summary + "\n\n")
			}
			b.WriteString(p.plan)
		}
		b.WriteString("\n## Result\n\n" + p.result)
		b.WriteString(p.footer)
		body = b.String()
	}
	// Everything above may quote untrusted text (task, plan, errors, the
	// template): defuse it, then add the one reference sy means.
	out := defuseRefs(body)
	if o.Closes != "" {
		out += fmt.Sprintf("\nCloses %s\n", o.Closes)
	}
	return out
}

func renderPRParts(st *orchestrator.TaskState, o prBodyOptions) prParts {
	var p prParts
	if st.Status != "done" {
		p.warning = fmt.Sprintf("> [!WARNING]\n> This task did **not** finish successfully (status: **%s**). Review it carefully", st.Status)
		if o.Draft {
			p.warning += "; it is opened as a draft"
		}
		p.warning += ".\n\n"
	}
	// A fenced block, not a quote: the task (an issue's text, written by
	// anyone) must not close other issues ("Fixes #7"), @-mention people or
	// render markup in the PR. Closing keywords and mentions do nothing in
	// code. The fence is longer than any backtick run in the text.
	p.task = codeFence(strings.TrimSpace(clipText(st.Task, 4000)))
	if st.Plan != nil && len(st.Plan.Subtasks) > 0 {
		var b strings.Builder
		p.summary = strings.TrimSpace(st.Plan.Summary)
		b.WriteString("| Step | Role | Kind | Result |\n|---|---|---|---|\n")
		seen := map[string]bool{}
		row := func(id, title, role, kind string) {
			seen[id] = true
			res := "not run"
			if r, ok := st.Results[id]; ok {
				if r.OK {
					res = "ok"
				} else {
					res = "**FAILED**"
					if r.Err != "" {
						res += ": " + mdCell(oneLine(r.Err, 120))
					}
				}
			}
			fmt.Fprintf(&b, "| `%s` %s | %s | %s | %s |\n", id, mdCell(oneLine(title, 100)), role, kind, res)
		}
		for _, sub := range st.Plan.Subtasks {
			role := sub.Role
			if role == "" {
				role = "auto (router)"
			}
			row(sub.ID, sub.Title, role, string(sub.Kind))
		}
		// Steps the plan did not list: review fix rounds.
		var extra []string
		for id := range st.Results {
			if !seen[id] {
				extra = append(extra, id)
			}
		}
		sort.Strings(extra)
		for _, id := range extra {
			title := "extra step"
			if strings.HasPrefix(id, "fix-") {
				title = "review fixes"
			}
			row(id, title, "auto (router)", "fix")
		}
		p.plan = b.String()
	}
	var b strings.Builder
	status := st.Status
	if status != "done" {
		status = "**" + strings.ToUpper(status) + "**"
	}
	fmt.Fprintf(&b, "- Status: %s\n", status)
	if st.Summary != "" {
		fmt.Fprintf(&b, "- Summary: %s\n", mdLine(st.Summary))
	}
	if c := checksFrom(st.Summary); c != "" {
		p.checks = fmt.Sprintf("- Checks: %s\n", c)
		b.WriteString(p.checks)
	}
	if st.CostLine != "" {
		fmt.Fprintf(&b, "- Cost: %s\n", mdLine(st.CostLine))
	}
	p.result = b.String()
	v := o.Version
	if v == "" {
		v = "dev"
	}
	p.footer = fmt.Sprintf("\n---\n<sub>Made with Switchyard (`sy` %s). Task `%s`; undo key `%s` (`sy undo %s` reverts it in the working tree it ran in).</sub>\n", v, st.ID, st.UndoKey, st.UndoKey)
	return p
}

// reCloseRef finds closing keywords (GitHub's and Gitea's, plus GitLab's
// -ing forms and "implements") followed by an issue reference (#7,
// owner/repo#7, group/sub/project#7 or an issues URL); reMention finds
// @user / @org/team; reQuickAction finds GitLab quick actions ("/merge",
// "/approve" at the start of a line), which GitLab runs with the poster's
// rights.
var (
	reCloseRef    = regexp.MustCompile(`(?i)\b(clos(?:e[sd]?|ing)|fix(?:e[sd]|ing)?|resolv(?:e[sd]?|ing)|implement(?:s|ed|ing)?)(\s*:?\s*(?:[\w.-]+(?:/[\w.-]+)+)?#\d|\s*:?\s*https?://[^\s]*/issues/\d)`)
	reMention     = regexp.MustCompile(`(^|[^\w@])@([A-Za-z0-9])`)
	reQuickAction = regexp.MustCompile(`(?m)^([ \t]*)/([A-Za-z])`)
)

// defuseRefs stops text written by others (an issue's body, a task, CI
// output) from closing issues, notifying people or running GitLab quick
// actions when it lands in a commit message, a PR title, a comment or a
// squash commit built from the PR body: a word joiner (U+2060, invisible)
// breaks the keyword, the @ and the /. The text reads the same.
func defuseRefs(s string) string {
	s = reCloseRef.ReplaceAllStringFunc(s, func(m string) string {
		return m[:1] + "\u2060" + m[1:]
	})
	s = reQuickAction.ReplaceAllString(s, "${1}/\u2060${2}")
	return reMention.ReplaceAllString(s, "${1}@\u2060${2}")
}

// checksFrom pulls the verify results out of a task summary
// ("...; checks pass" / "...; checks still fail (go test ./...)").
func checksFrom(summary string) string {
	for _, part := range strings.Split(summary, "; ") {
		p := strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(p, "checks pass"):
			return "pass"
		case strings.HasPrefix(p, "checks still fail"):
			return "**FAIL** " + mdLine(strings.TrimPrefix(p, "checks still fail"))
		}
	}
	return ""
}

// codeFence puts text in a fenced code block that the text cannot end.
func codeFence(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "text\n" + text + "\n" + fence + "\n"
}

func mdLine(s string) string { return oneLine(s, 500) }

func mdCell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }
