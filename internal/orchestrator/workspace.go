package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/router"
)

// Multi-repo tasks (workspaces):
//
//   - A workspace is the project folder (the primary repo, named "primary")
//     plus extra git repositories, each with a short name: `--repo
//     frontend=../web` on the command line, or `workspace: repos:
//     {frontend: ../web}` in the config / .relayweft.yaml (a relative path
//     in a .relayweft.yaml is taken from that file's folder, in the user's
//     config from the project folder, in a flag from the current folder).
//     Every repo must be a git work tree of its own, not inside, around or
//     a linked worktree of another repo of the task. A config entry that
//     does not fit (folder missing on this machine, ...) is skipped with a
//     warning; a bad --repo flag is an error.
//   - The planner sees every repo (name, path, repo map, notes) and puts
//     "repo": "<name>" on each subtask; "" (or "primary") is the project
//     folder. NormalizePlan rejects unknown names.
//   - Each repo has its own git state: a snapshot before the task, its own
//     integration commit and merge lock, its own worktree pool and kept
//     branches. A writer runs in its repo (a pool slot of that repo or its
//     main tree) with that repo as its working directory. Read-only steps
//     run in their repo too and get every repo's path in their prompt.
//   - Verify runs the primary's checks in the project folder and each extra
//     repo's own `verify` (from its .relayweft.yaml, only when trusted) in
//     that repo. The final review gets the diff of every repo, labelled.
//     The fix round runs once per repo that changed (or whose checks fail),
//     inside that repo.
//   - Undo: every repo records before/after snapshots under the same task
//     key in its own refs; the primary's "before" snapshot lists the extra
//     repos, so `rw undo <key>` in the project folder undoes (or redoes)
//     every repo. The task state records the repos for history and resume.
//   - A task without extra repos takes none of these paths.

// PrimaryRepo is the name of the project folder's repo in a workspace.
const PrimaryRepo = "primary"

// Repo is an extra repository of a multi-repo workspace.
type Repo struct {
	Name string `json:"name"`
	Dir  string `json:"dir"` // absolute; the agents' working directory in this repo
}

var reRepoName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ParseRepoFlag splits a `--repo name=path` value.
func ParseRepoFlag(s string) (name, path string, err error) {
	name, path, ok := strings.Cut(s, "=")
	name, path = strings.TrimSpace(name), strings.TrimSpace(path)
	if !ok || name == "" || path == "" {
		return "", "", fmt.Errorf("--repo %q: want name=path (e.g. --repo frontend=../web)", s)
	}
	if err := checkRepoName(name); err != nil {
		return "", "", fmt.Errorf("--repo %q: %w", s, err)
	}
	return name, path, nil
}

func checkRepoName(name string) error {
	if !reRepoName.MatchString(name) {
		return fmt.Errorf("repo name %q: use lowercase letters, digits, - and _ (at most 32)", name)
	}
	if name == PrimaryRepo {
		return fmt.Errorf("repo name %q is reserved for the project folder", name)
	}
	return nil
}

// WorkspaceEntry is a workspace.repos entry from a config file. A relative
// Path is taken from Base: the folder of the .relayweft.yaml that set it,
// or the project folder for the user's config.
type WorkspaceEntry struct {
	Name, Path string
	Base       string // folder a relative Path is taken from ("" = the project folder)
	Origin     string // where it was set, for messages
}

// ResolveWorkspace builds the extra repos of the project in dir from the
// config entries and --repo flags (name=path; relative paths are taken from
// the current directory; a flag replaces a config entry of the same name).
// Every repo must be an existing git work tree that is not the project's
// own repo or another listed repo, nor inside or around one of them, nor a
// linked worktree of one of them (same git common dir). A bad --repo flag
// is an error; a bad config entry is skipped and described in skipped, so
// a committed workspace that does not fit a teammate's machine never stops
// rw from starting. Sorted by name.
func ResolveWorkspace(dir string, fromConfig []WorkspaceEntry, flags []string) (repos []Repo, skipped []string, err error) {
	type cand struct {
		name, path, origin string
		flag               bool
	}
	byName := map[string]cand{}
	for _, e := range fromConfig {
		name, p := strings.TrimSpace(e.Name), strings.TrimSpace(e.Path)
		origin := e.Origin
		if origin == "" {
			origin = "workspace.repos." + name
		}
		if err := checkRepoName(name); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v; skipped", origin, err))
			continue
		}
		if p == "" {
			skipped = append(skipped, fmt.Sprintf("%s: empty path; skipped", origin))
			continue
		}
		if !filepath.IsAbs(p) {
			base := e.Base
			if base == "" {
				base = dir
			}
			p = filepath.Join(base, filepath.FromSlash(p))
		}
		byName[name] = cand{name, p, origin, false}
	}
	for _, f := range flags {
		name, p, err := ParseRepoFlag(f)
		if err != nil {
			return nil, nil, err
		}
		if p, err = filepath.Abs(p); err != nil {
			return nil, nil, err
		}
		byName[name] = cand{name, p, "--repo " + f, true}
	}
	if len(byName) == 0 {
		return nil, skipped, nil
	}
	// Flags first: they claim their repos, and a config entry that clashes
	// with one is the one skipped.
	cands := make([]cand, 0, len(byName))
	for _, c := range byName {
		cands = append(cands, c)
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].flag != cands[j].flag {
			return cands[i].flag
		}
		return cands[i].name < cands[j].name
	})
	fail := func(c cand, msg string) error {
		if c.flag {
			return fmt.Errorf("%s: %s", c.origin, msg)
		}
		skipped = append(skipped, fmt.Sprintf("%s: %s; skipped", c.origin, msg))
		return nil
	}
	if !isRepo(dir) {
		for _, c := range cands {
			if err := fail(c, fmt.Sprintf("multi-repo tasks need the project folder %s to be a git repository", dir)); err != nil {
				return nil, nil, err
			}
		}
		return nil, skipped, nil
	}
	primaryRoot, err := repoRoot(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("project folder %s: %w", dir, err)
	}
	type claimed struct{ name, root, common string }
	taken := []claimed{{PrimaryRepo, canonPath(primaryRoot), commonDir(primaryRoot)}}
	for _, c := range cands {
		p := filepath.Clean(c.path)
		st, err := os.Stat(p)
		if err != nil || !st.IsDir() {
			if err := fail(c, p+" is not a directory"); err != nil {
				return nil, nil, err
			}
			continue
		}
		if !isRepo(p) {
			if err := fail(c, p+" is not a git repository (every repo of a multi-repo task must be one: git init)"); err != nil {
				return nil, nil, err
			}
			continue
		}
		root, err := repoRoot(p)
		if err != nil {
			if err := fail(c, err.Error()); err != nil {
				return nil, nil, err
			}
			continue
		}
		me := claimed{c.name, canonPath(root), commonDir(root)}
		clash := ""
		for _, o := range taken {
			switch {
			case me.root == o.root:
				clash = fmt.Sprintf("%s is in the same git repository as %q", p, o.name)
			case within(me.root, o.root):
				clash = fmt.Sprintf("%s is inside repo %q (%s)", p, o.name, o.root)
			case within(o.root, me.root):
				clash = fmt.Sprintf("%s contains repo %q (%s)", p, o.name, o.root)
			case me.common != "" && me.common == o.common:
				clash = fmt.Sprintf("%s is a worktree of the same git repository as %q", p, o.name)
			}
			if clash != "" {
				break
			}
		}
		if clash != "" {
			if err := fail(c, clash); err != nil {
				return nil, nil, err
			}
			continue
		}
		taken = append(taken, me)
		repos = append(repos, Repo{Name: c.name, Dir: p})
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })
	return repos, skipped, nil
}

// commonDir is the canonical git common dir of the work tree at root (the
// .git folder its linked worktrees share), "" when git cannot tell.
func commonDir(root string) string {
	s, err := (git{root}).out("rev-parse", "--git-common-dir")
	if err != nil || s == "" {
		return ""
	}
	s = filepath.FromSlash(s)
	if !filepath.IsAbs(s) {
		s = filepath.Join(root, s)
	}
	return canonPath(s)
}

// WorkspaceLabel names a project for headers: "api" or "api + web, docs".
func WorkspaceLabel(dir string, repos []Repo) string {
	s := filepath.Base(dir)
	if len(repos) == 0 {
		return s
	}
	names := make([]string, len(repos))
	for i, r := range repos {
		names[i] = r.Name
	}
	return s + " + " + strings.Join(names, ", ")
}

// Repos returns the extra repos every task of this orchestrator works in
// (nil for a single-repo project).
func (o *Orchestrator) Repos() []Repo {
	if o.opts.NoGit {
		return nil
	}
	return append([]Repo(nil), o.opts.Repos...)
}

// RepoError is a subtask naming a repo that is not in the workspace.
type RepoError struct {
	Subtask, Repo string
	Repos         []string
}

func (e *RepoError) Error() string {
	return fmt.Sprintf("subtask %s: unknown repo %q (repos: %s)", e.Subtask, e.Repo, strings.Join(e.Repos, ", "))
}

// normalizeRepos checks each subtask's repo against p.Repos (the workspace's
// repo names, primary first). The primary becomes "". Without repos every
// subtask runs in the project folder.
func normalizeRepos(p *Plan) error {
	if len(p.Repos) == 0 {
		for i := range p.Subtasks {
			p.Subtasks[i].Repo = ""
		}
		p.Repos = nil
		return nil
	}
	valid := map[string]bool{}
	repos := make([]string, len(p.Repos))
	for i, r := range p.Repos {
		repos[i] = strings.ToLower(strings.TrimSpace(r))
		valid[repos[i]] = true
	}
	p.Repos = repos
	for i := range p.Subtasks {
		st := &p.Subtasks[i]
		r := strings.ToLower(strings.TrimSpace(st.Repo))
		switch {
		case r == "" || r == p.Repos[0]:
			st.Repo = ""
		case valid[r]:
			st.Repo = r
		default:
			return &RepoError{Subtask: st.ID, Repo: st.Repo, Repos: p.Repos}
		}
	}
	return nil
}

// workspaceNames lists the repo names of a multi-repo task (primary first),
// nil for a single-repo task.
func (t *task) workspaceNames() []string {
	if len(t.repos) == 0 {
		return nil
	}
	names := []string{PrimaryRepo}
	for _, r := range t.repos {
		names = append(names, r.repoName)
	}
	return names
}

// repoNamed returns the repo state of a workspace repo ("" = primary). A
// repo's state is held in a *task of its own; only its git fields (root,
// snapshot, mergeMu, pool, ...), dir and cfg.Verify are used.
func (t *task) repoNamed(name string) *task {
	for _, r := range t.repos {
		if r.repoName == name {
			return r
		}
	}
	return t
}

func (t *task) repoOf(st Subtask) *task { return t.repoNamed(st.Repo) }

// allRepos is the primary followed by the extra repos.
func (t *task) allRepos() []*task { return append([]*task{t}, t.repos...) }

// repoAt finds the repo an agent working in dir belongs to: its tree or one
// of its pool worktrees.
func (t *task) repoAt(dir string) *task {
	for _, r := range t.repos {
		if within(dir, r.root) || within(dir, r.dir) || (r.pool != "" && within(dir, r.pool)) {
			return r
		}
	}
	return t
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	if path == "" || dir == "" {
		return false
	}
	rel, err := filepath.Rel(canonPath(dir), canonPath(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// addNote records a note for the reviewer (parallel steps of every repo).
func (t *task) addNote(s string) {
	t.notesMu.Lock()
	t.notes = append(t.notes, s)
	t.notesMu.Unlock()
}

// setupWorkspace creates the repo states of a multi-repo task. Each extra
// repo checks its own .relayweft.yaml for verify commands; only a trusted
// file's commands are used.
func (o *Orchestrator) setupWorkspace(t *task, repos []Repo) error {
	if o.opts.NoGit || len(repos) == 0 {
		return nil
	}
	if !isRepo(o.opts.Dir) {
		return fmt.Errorf("multi-repo task, but the project folder %s is not a git repository", o.opts.Dir)
	}
	for _, rp := range repos {
		if err := checkRepoName(rp.Name); err != nil {
			return err
		}
		root, err := repoRoot(rp.Dir)
		if err != nil || !isRepo(rp.Dir) {
			return fmt.Errorf("repo %s: %s is not a git repository (any more)", rp.Name, rp.Dir)
		}
		c := *t.cfg
		c.Verify = o.repoVerify(rp, t.cfg.Verify.Timeout)
		t.repos = append(t.repos, &task{id: t.id, key: t.key, repoName: rp.Name, dir: rp.Dir, root: root, cfg: &c})
	}
	return nil
}

// repoVerify reads an extra repo's own verify commands (its trusted
// .relayweft.yaml); the user's config does not apply to other repos.
func (o *Orchestrator) repoVerify(rp Repo, timeout config.Duration) config.VerifyCfg {
	c := config.Default()
	c.Verify = config.VerifyCfg{Timeout: timeout}
	info, err := config.ApplyRepo(c, rp.Dir)
	if err != nil {
		o.logf("repo %s: %v (its verify commands are not used)", rp.Name, err)
		return config.VerifyCfg{Timeout: timeout}
	}
	for _, k := range info.Ignored {
		if k == "verify" {
			o.logf("repo %s: %s sets verify commands, ignored until you trust it (rw trust --dir %s)", rp.Name, info.Path, rp.Dir)
		}
	}
	if c.Verify.Timeout <= 0 {
		c.Verify.Timeout = timeout
	}
	return c.Verify
}

// snapshotExtra snapshots an extra repo before the task.
func (o *Orchestrator) snapshotExtra(t, r *task) error {
	root := r.root
	snap, big, err := (git{root}).snapshotSkipping(extraBeforeMessage(t))
	if err != nil {
		return err
	}
	if len(big) > 0 {
		o.logf("repo %s: left %d untracked file(s) over %d MB out of the snapshot: %s", r.repoName, len(big), snapshotMaxFile.Load()>>20, clip(strings.Join(big, ", "), 300))
	}
	r.useGit = true
	r.snapshot, r.start = snap, snap
	if t.keepBefore && t.key != "" {
		t.tokensMu.Lock()
		git{root}.keepAgentFiles(t.key, &r.agentFiles)
		t.tokensMu.Unlock()
	}
	if o.opts.Bench == "" && !t.keepBefore && t.key != "" {
		if err := (git{root}).recordSnapshot(t.key, "before", snap); err != nil {
			o.logf("warning: repo %s: could not record the start state, so rw undo cannot undo this task there: %v", r.repoName, err)
		}
	}
	return nil
}

// prepareExtras finishes the git setup of the extra repos after the
// snapshots: a repo that could not be snapshotted stops the task (it could
// not be undone), the others get their worktree setting and hand-off.
func (o *Orchestrator) prepareExtras(t *task) error {
	if len(t.repos) == 0 {
		return nil
	}
	if !t.useGit {
		return fmt.Errorf("the project folder could not be snapshotted; a multi-repo task needs every repo under git")
	}
	for _, r := range t.repos {
		if !r.useGit {
			return fmt.Errorf("repo %s (%s) could not be snapshotted", r.repoName, r.dir)
		}
		r.wtOK = o.worktreesAllowedIn(t, r)
		if t.cfg.Orchestrator.Handoff {
			r.repoMap = repoMap(r.root)
			r.repoNotes = repoNotes(r.root, t.text)
		}
		r.repoDocs = repoDocs(t.cfg, r.root)
	}
	names := t.workspaceNames()
	o.logf("multi-repo task: %s", strings.Join(names, ", "))
	return nil
}

// workspaceMark starts the list of extra repos in the primary's "before"
// snapshot message: "<name>\t<repo root>" per line.
const workspaceMark = "workspace-repos:"

// beforeMessage is the primary's "before" snapshot message.
func (t *task) beforeMessage() string {
	msg := subject("relayweft before: ", t.text)
	if len(t.repos) == 0 {
		return msg
	}
	var b strings.Builder
	b.WriteString(msg + "\n\n" + workspaceMark + "\n")
	for _, r := range t.repos {
		root := r.root
		if root == "" {
			root = r.dir
		}
		b.WriteString(r.repoName + "\t" + root + "\n")
	}
	return b.String()
}

// primaryMark starts the primary's root in an extra repo's "before"
// snapshot message: trimUndo keeps that record while the primary keeps
// its own.
const primaryMark = "workspace-primary:"

// extraBeforeMessage is an extra repo's "before" snapshot message.
func extraBeforeMessage(t *task) string {
	msg := subject("relayweft before: ", t.text)
	if t.root == "" {
		return msg
	}
	return msg + "\n\n" + primaryMark + "\n" + t.root + "\n"
}

// primaryOf reads the primary's root from an extra repo's "before"
// snapshot ("" for a task that started in this repo).
func (g git) primaryOf(before string) string {
	msg, err := g.run(nil, nil, "log", "-1", "--format=%B", before)
	if err != nil {
		return ""
	}
	i := strings.Index(msg, primaryMark)
	if i < 0 {
		return ""
	}
	l, _, _ := strings.Cut(strings.TrimSpace(msg[i+len(primaryMark):]), "\n")
	return strings.TrimSpace(l)
}

// workspaceOf reads the extra repos recorded in a "before" snapshot.
func (g git) workspaceOf(before string) []Repo {
	msg, err := g.run(nil, nil, "log", "-1", "--format=%B", before)
	if err != nil {
		return nil
	}
	i := strings.Index(msg, workspaceMark)
	if i < 0 {
		return nil
	}
	var out []Repo
	for _, l := range strings.Split(msg[i+len(workspaceMark):], "\n") {
		name, dir, ok := strings.Cut(strings.TrimSpace(l), "\t")
		if ok && name != "" && dir != "" {
			out = append(out, Repo{Name: name, Dir: dir})
		}
	}
	return out
}

// planContext is the planner's context: the hand-off, plus every repo of a
// multi-repo task.
func (t *task) planContext() string {
	if len(t.repos) == 0 {
		return t.handoff()
	}
	var b strings.Builder
	b.WriteString("\nWORKSPACE: this task spans several git repositories. Each subtask works in exactly one of them:\n")
	for _, r := range t.allRepos() {
		name := r.repoName
		if name == "" {
			name = PrimaryRepo
		}
		fmt.Fprintf(&b, "\n=== repo %q at %s ===\n", name, r.dir)
		b.WriteString(r.handoff())
	}
	b.WriteString(`
Add "repo": "<name>" to every subtask (` + strings.Join(t.workspaceNames(), ", ") + `; "` + PrimaryRepo + `" is the project folder).
An agent can only change files in its own repo: split work that touches several repos into one subtask per repo,
and use depends_on when one repo's change needs another's (e.g. the frontend after the API). Edit subtasks in
different repos can run in parallel. "files" are relative to the subtask's repo.
`)
	return b.String()
}

// workspaceList lists the repos for step and fix prompts.
func (t *task) workspaceList() string {
	var b strings.Builder
	for _, r := range t.allRepos() {
		name := r.repoName
		if name == "" {
			name = PrimaryRepo
		}
		fmt.Fprintf(&b, "  - %s: %s\n", name, r.dir)
	}
	return b.String()
}

// stepContext is what a step prompt gets after the subtask: its repo's
// hand-off and, in a multi-repo task, where it works and where the others
// are.
func (t *task) stepContext(st Subtask) string {
	r := t.repoOf(st)
	if len(t.repos) == 0 {
		return r.handoff()
	}
	name := r.repoName
	if name == "" {
		name = PrimaryRepo
	}
	var b strings.Builder
	b.WriteString("\nWORKSPACE: this task spans several git repositories:\n" + t.workspaceList())
	if st.Kind.ReadOnly() {
		fmt.Fprintf(&b, "Your repo is %q (your working directory). You may read the other repos at the paths above.\n", name)
	} else {
		fmt.Fprintf(&b, "You work in repo %q (your working directory is a checkout of it). Change files in this repo only; other agents handle the other repos.\n", name)
	}
	b.WriteString(r.handoff())
	return b.String()
}

// verifying reports whether any repo of the task has checks.
func (t *task) verifying() bool {
	for _, r := range t.allRepos() {
		if len(r.cfg.Verify.Commands) > 0 {
			return true
		}
	}
	return false
}

// verifyCommands lists every repo's checks ("name: cmd" for extra repos).
func (t *task) verifyCommands() []string {
	var out []string
	for _, r := range t.allRepos() {
		for _, c := range r.cfg.Verify.Commands {
			if r.repoName != "" {
				c = r.repoName + ": " + c
			}
			out = append(out, c)
		}
	}
	return out
}

// workspaceDiff is the final review's diff: the primary's as before, and
// for a multi-repo task every repo's diff, labelled.
func (t *task) workspaceDiff(limit int) (stat, diff string) {
	if len(t.repos) == 0 {
		if t.useGit {
			return git{t.root}.diff(t.start, limit)
		}
		return "", ""
	}
	var sb, db strings.Builder
	per := limit / (len(t.repos) + 1)
	for _, r := range t.allRepos() {
		if !r.useGit {
			continue
		}
		s, d := git{r.root}.diff(r.start, per)
		if s == "" && d == "" {
			continue
		}
		name := r.repoName
		if name == "" {
			name = PrimaryRepo
		}
		head := fmt.Sprintf("=== repo %s (%s) ===\n", name, r.dir)
		sb.WriteString(head + s + "\n")
		db.WriteString(head + d + "\n")
	}
	return strings.TrimRight(sb.String(), "\n"), db.String()
}

// fixTargets are the repos a fix round works in: every repo that changed in
// this task or whose checks fail (the primary when none did).
func (t *task) fixTargets(failing map[string]bool) []*task {
	if len(t.repos) == 0 {
		return []*task{t}
	}
	var out []*task
	for _, r := range t.allRepos() {
		if failing[r.repoName] || r.changedSinceStart() {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		out = []*task{t}
	}
	return out
}

// changedSinceStart reports whether the repo's files (new untracked ones
// too) differ from its snapshot at the start of the task.
func (r *task) changedSinceStart() bool {
	if !r.useGit || r.start == "" {
		return false
	}
	g := git{r.root}
	now, err := g.snapshot("relayweft fix check")
	if err != nil {
		return true // cannot tell: let a fix agent look
	}
	s, err := g.out("diff", "--name-only", r.start, now)
	return err != nil || s != ""
}

// runFix runs one fix round in every target repo and reports whether all
// fixes succeeded.
func (o *Orchestrator) runFix(ctx context.Context, t *task, round int, v Verdict, reviewAsked bool, failing map[string]bool, results map[string]stepResult) bool {
	ok := true
	for _, r := range t.fixTargets(failing) {
		prompt := fixPrompt(t.text, v, reviewAsked) + t.testsHint()
		id := fmt.Sprintf("fix-%d", round+1)
		if len(t.repos) > 0 {
			name := r.repoName
			if name == "" {
				name = PrimaryRepo
			} else {
				id += "-" + name
			}
			prompt += "\nWORKSPACE: this task spans several git repositories:\n" + t.workspaceList() +
				fmt.Sprintf("You work in repo %q (your working directory). Fix only what belongs to this repo; the other repos get their own fix agent.\n", name)
		}
		fix := Subtask{ID: id, Title: "Apply review fixes", Kind: router.KindFix, Prompt: prompt, Repo: r.repoName}
		if w := o.fixSession(t, r); w != nil {
			t.continueAs(fix.ID, *w)
		}
		var res stepResult
		if o.reviewing(t) && r.wtOK {
			res = o.runInWorktree(ctx, t, fix, nil, prompt)
		} else {
			res = o.runStep(ctx, t, fix, nil, stepLoc{dir: r.dir}, prompt)
		}
		results[fix.ID] = res
		if !res.ok {
			ok = false
			break
		}
	}
	return ok
}

// fixSession is the session a fix agent in repo r continues: the last
// writing agent's, when it worked in r's own tree and its provider can take
// it now. Its context (the repo it read, what it changed and why) is cached,
// so the fix costs a fraction of a fresh agent's fresh tokens. A fix in a
// pool worktree (change review) or a multi-repo task starts fresh.
func (o *Orchestrator) fixSession(t *task, r *task) *StepRun {
	if len(t.repos) > 0 || (o.reviewing(t) && r.wtOK) {
		return nil
	}
	w := t.writer()
	if w == nil || w.Dir != r.dir || w.Session == "" || o.opts.Tracker.Limited(w.Provider) {
		return nil
	}
	if pc, ok := t.cfg.Providers[w.Provider]; !ok || pc.Disabled || t.cfg.Kind(w.Provider) != w.Kind || t.runners[w.Provider] == nil {
		return nil
	}
	w.Attempt = 1
	w.why = "continues the writer's session (its context is cached)"
	return w
}

// recordAfterExtras records the extra repos' "after" snapshots.
func (o *Orchestrator) snapshotAfterExtras(t *task) {
	for _, r := range t.repos {
		if !r.useGit || r.start == "" || t.key == "" {
			continue
		}
		g := git{r.root}
		t.tokensMu.Lock()
		msg := afterMessage(t.text, r.agentFiles)
		t.tokensMu.Unlock()
		snap, err := g.snapshot(msg)
		if err == nil {
			err = g.recordSnapshot(t.key, "after", snap)
		}
		if err != nil {
			o.logf("warning: repo %s: could not record the end state of this task, so rw undo cannot undo it there: %v", r.repoName, err)
			continue
		}
		trimUndo(r.root)
	}
}

// workspaceUndoPlans previews the extra repos of a task recorded in the
// primary at root (none for a single-repo task). A repo whose part is
// already in the wanted state (undone there on its own) is skipped; a repo
// whose folder is gone or that has no record of the task any more is
// listed in missing (with the reason) and left out.
func workspaceUndoPlans(root string, t UndoTask, redo bool) (plans []UndoPlan, missing []string, err error) {
	if root == "" || t.Before == "" {
		return nil, nil, nil
	}
	for _, rp := range (git{root}).workspaceOf(t.Before) {
		if st, err := os.Stat(rp.Dir); err != nil || !st.IsDir() {
			missing = append(missing, rp.Name+" ("+rp.Dir+": the folder is gone)")
			continue
		}
		list, err := UndoList(rp.Dir)
		if err != nil {
			return nil, nil, fmt.Errorf("repo %s (%s): %w", rp.Name, rp.Dir, err)
		}
		found := false
		for _, x := range list {
			if x.Key != t.Key {
				continue
			}
			found = true
			if x.Undone == redo {
				p, err := previewOne(rp.Dir, t.Key, redo)
				if err != nil {
					return nil, nil, fmt.Errorf("repo %s: %w", rp.Name, err)
				}
				p.Repo, p.Dir = rp.Name, rp.Dir
				plans = append(plans, p)
			}
		}
		if !found {
			// Pruned there, or the repo was cloned again: like a missing
			// folder, it is left out and the other repos are undone.
			missing = append(missing, rp.Name+" ("+rp.Dir+": no record of this task there; its snapshots were pruned or the repo was replaced)")
		}
	}
	return plans, missing, nil
}

// repoTag tags log text with the repo of an extra-repo step.
func repoTag(r *task) string {
	if r.repoName == "" {
		return ""
	}
	return " in repo " + r.repoName
}

// saveBranchIn is saveBranch in repo rp; an extra repo's branch is shown
// as "<repo>:<branch>".
func (o *Orchestrator) saveBranchIn(rp *task, stepID, commit string) string {
	b := o.saveBranch(rp, stepID, commit)
	if rp.repoName != "" {
		return rp.repoName + ":" + b
	}
	return b
}
