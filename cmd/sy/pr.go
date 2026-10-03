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
	"sort"
	"strings"

	"github.com/sparkz400/switchyard/internal/gh"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/proc"
)

// sy pr turns a finished task into a branch, a commit and a GitHub pull
// request:
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
//   - The pull request is opened through the GitHub REST API with a token
//     from GITHUB_TOKEN, GH_TOKEN or `gh auth token`; without one, sy
//     writes the body to a file and prints the compare URL instead.

// Package vars so tests can drive sy pr without a terminal, network or gh.
var (
	prIn    io.Reader = os.Stdin
	prOut   io.Writer = os.Stdout
	prToken           = gh.Token
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
	api      string // API base URL override (GitHub Enterprise, tests)
	draft    bool
	draftSet bool // --draft given explicitly (else: draft unless the task is done)
	noPush   bool
	yes      bool
	closes   string // "#12" or "owner/repo#12": adds "Closes ..." to the body
}

// prResult is what sy pr created.
type prResult struct {
	Branch string
	Commit string
	Files  []string
	URL    string // the pull request; "" when none was opened
	Number int
	Repo   gh.Repo
}

func cmdPR(args []string) error {
	fs := flag.NewFlagSet("sy pr", flag.ExitOnError)
	var o prOptions
	dir := fs.String("dir", "", "project directory (default current directory)")
	fs.StringVar(&o.base, "base", "", "branch to merge into (default: the remote's default branch)")
	fs.StringVar(&o.branch, "branch", "", "branch to create (default sy/<slug of the task>)")
	fs.StringVar(&o.title, "title", "", "pull request title (default: the task's first line)")
	fs.BoolVar(&o.draft, "draft", false, "open as a draft (default: draft when the task did not finish ok)")
	fs.BoolVar(&o.noPush, "no-push", false, "only create the local branch and commit")
	fs.BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	fs.StringVar(&o.api, "api", "", "GitHub API base URL (GitHub Enterprise: https://<host>/api/v3; GH_HOST also works)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: sy pr [task-id] [--base main] [--branch name] [--draft] [--title text] [--no-push] [--yes]

Turns a finished task (default: the newest finished task in this directory,
see sy history) into a branch, a commit and a GitHub pull request.

The commit holds exactly the task's changes (its undo snapshots), applied
on top of HEAD without touching your index, working tree or current branch;
if they do not apply cleanly to HEAD, nothing is created. The branch
(sy/<task>) must not exist yet and is pushed with git push -u origin (never
forced). The PR is opened with a token from GITHUB_TOKEN, GH_TOKEN or
`+"`gh auth token`"+`; without one the body is written to a file and the compare
URL is printed. A task that did not finish ok is opened as a draft.
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
	_, err = makePR(st, o)
	return err
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

	// Where it goes: the origin remote on GitHub.
	var repo gh.Repo
	var repoErr error
	if u, err := prGit(root, nil, nil, "remote", "get-url", "origin"); err != nil {
		repoErr = errors.New("this repository has no `origin` remote")
	} else {
		ent := gh.EnterpriseHost()
		if o.api != "" {
			// An explicit API URL means: trust the remote's host.
			if r, err := gh.ParseRemote(strings.TrimSpace(u), hostOf(strings.TrimSpace(u))); err == nil {
				ent = r.Host
			}
		}
		repo, repoErr = gh.ParseRemote(strings.TrimSpace(u), ent)
	}
	if repoErr != nil && !o.noPush {
		return nil, fmt.Errorf("%w (use --no-push to only create the local branch)", repoErr)
	}

	title := o.title
	if title == "" {
		title = prTitle(st.Task)
	}
	commit, files, err := buildPRCommit(root, snap.Before, snap.After, commitMessage(st, title))
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
	var client *gh.Client
	if !o.noPush && !repo.IsZero() {
		api := o.api
		if api == "" {
			api = repo.APIBase()
		}
		tok, _ := prToken(repo.Host)
		client = gh.NewClient(api, tok)
		client.Notes = out
	}
	base := o.base
	if base == "" {
		base = detectBase(root, client, repo)
	}
	body := renderPRBody(st, prBodyOptions{Closes: o.closes, Version: version, Draft: draft})

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
		kind := "pull request"
		if draft {
			kind = "draft pull request"
		}
		fmt.Fprintf(out, "into:   %s %s on %s (git push -u origin %s)\n", kind, base, repo, branch)
		if n := aheadOf(root, base); n > 0 {
			fmt.Fprintf(out, "note:   HEAD has %d commit(s) that are not on origin/%s; the pull request includes them\n", n, base)
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
		fmt.Fprintln(out, "no GitHub token (GITHUB_TOKEN, GH_TOKEN or `gh auth login`): open the pull request in the browser")
		fallback()
		return res, nil
	}
	pr, err := client.CreatePull(repo, gh.NewPull{Title: title, Head: branch, Base: base, Body: body, Draft: draft})
	if err != nil {
		fallback()
		return res, fmt.Errorf("open pull request: %w", err)
	}
	res.URL, res.Number = pr.HTMLURL, pr.Number
	fmt.Fprintf(out, "opened pull request #%d: %s\n", pr.Number, pr.HTMLURL)
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

// detectBase picks the base branch: origin/HEAD, then GitHub's default
// branch, then main.
func detectBase(root string, client *gh.Client, repo gh.Repo) string {
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

// aheadOf counts HEAD's commits that origin/<base> lacks (0 when unknown).
func aheadOf(root, base string) int {
	s, err := prGit(root, nil, nil, "rev-list", "--count", "refs/remotes/origin/"+base+"..HEAD")
	if err != nil {
		return 0
	}
	var n int
	fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n
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
}

// renderPRBody writes the pull request description: the task, the plan
// with each step's result, checks, cost and how to undo it locally.
func renderPRBody(st *orchestrator.TaskState, o prBodyOptions) string {
	var b strings.Builder
	if st.Status != "done" {
		fmt.Fprintf(&b, "> [!WARNING]\n> This task did **not** finish successfully (status: **%s**). Review it carefully", st.Status)
		if o.Draft {
			b.WriteString("; it is opened as a draft")
		}
		b.WriteString(".\n\n")
	}
	b.WriteString("## Task\n\n")
	for _, l := range strings.Split(strings.TrimSpace(clipText(st.Task, 4000)), "\n") {
		b.WriteString(strings.TrimRight("> "+l, " ") + "\n")
	}
	if st.Plan != nil && len(st.Plan.Subtasks) > 0 {
		b.WriteString("\n## Plan\n\n")
		if s := strings.TrimSpace(st.Plan.Summary); s != "" {
			b.WriteString(s + "\n\n")
		}
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
	}
	b.WriteString("\n## Result\n\n")
	status := st.Status
	if status != "done" {
		status = "**" + strings.ToUpper(status) + "**"
	}
	fmt.Fprintf(&b, "- Status: %s\n", status)
	if st.Summary != "" {
		fmt.Fprintf(&b, "- Summary: %s\n", mdLine(st.Summary))
	}
	if c := checksFrom(st.Summary); c != "" {
		fmt.Fprintf(&b, "- Checks: %s\n", c)
	}
	if st.CostLine != "" {
		fmt.Fprintf(&b, "- Cost: %s\n", mdLine(st.CostLine))
	}
	v := o.Version
	if v == "" {
		v = "dev"
	}
	fmt.Fprintf(&b, "\n---\n<sub>Made with Switchyard (`sy` %s). Task `%s`; undo key `%s` (`sy undo %s` reverts it in the working tree it ran in).</sub>\n", v, st.ID, st.UndoKey, st.UndoKey)
	if o.Closes != "" {
		fmt.Fprintf(&b, "\nCloses %s\n", o.Closes)
	}
	return b.String()
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

func mdLine(s string) string { return oneLine(s, 500) }

func mdCell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }
