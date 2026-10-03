package orchestrator

import (
	"context"
	"os"
	"strconv"
	"strings"
)

// Approver lets a person check work before it happens (the plan) and before
// it lands in their working tree (each agent's changes). A nil Approver,
// and every unattended (queued) task, approves everything.
type Approver interface {
	// ApprovePlan shows the plan; the person may edit it. ok=false cancels
	// the task before any agent runs.
	ApprovePlan(ctx context.Context, task string, p Plan) (Plan, bool)
	// ReviewChanges shows one agent's changes before they are applied.
	ReviewChanges(ctx context.Context, cs ChangeSet) ChangeDecision
}

// FileChange is one file in a change set.
type FileChange struct {
	Path    string
	Status  string // A, M, D
	Added   int
	Deleted int
	Binary  bool
	Patch   string // unified diff, truncated for huge files
}

// ChangeSet is one agent's work, ready to be applied.
type ChangeSet struct {
	StepID  string
	Title   string
	Summary string // the agent's own summary
	Round   int    // 1 for the first review, 2+ after a rerun with feedback
	Files   []FileChange
}

// ChangeDecision is the person's answer.
type ChangeDecision struct {
	// Apply lists the paths to apply; empty means reject everything.
	Apply []string
	// Feedback, when set, sends the agent back to work with this message
	// (its current changes stay in its worktree) and the result is shown
	// again.
	Feedback string
}

// AllPaths returns every path of a change set (accept everything).
func (cs ChangeSet) AllPaths() []string {
	out := make([]string, len(cs.Files))
	for i, f := range cs.Files {
		out[i] = f.Path
	}
	return out
}

const maxPatch = 200 << 10

// changeSet describes the change between two commits.
func (g git) changeSet(from, to string) ([]FileChange, error) {
	status, err := g.run(nil, nil, "diff", "--name-status", "--no-renames", "-z", from, to)
	if err != nil {
		return nil, err
	}
	nums, _ := g.run(nil, nil, "diff", "--numstat", "--no-renames", "-z", from, to)
	type count struct {
		add, del int
		bin      bool
	}
	counts := map[string]count{}
	// numstat -z: "<add>\t<del>\t<path>\x00" per file.
	for _, rec := range strings.Split(nums, "\x00") {
		f := strings.SplitN(rec, "\t", 3)
		if len(f) != 3 {
			continue
		}
		a, errA := strconv.Atoi(f[0])
		d, _ := strconv.Atoi(f[1])
		counts[f[2]] = count{a, d, errA != nil}
	}
	var out []FileChange
	fields := strings.Split(strings.TrimRight(status, "\x00"), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		path := fields[i+1]
		c := counts[path]
		out = append(out, FileChange{Path: path, Status: fields[i][:1], Added: c.add, Deleted: c.del, Binary: c.bin})
	}
	// One git diff for all files (a process per file is slow on Windows);
	// git prints the files in the same order as --name-status.
	all, _ := g.run(nil, nil, "diff", "--no-renames", from, to)
	chunks := splitPatch(all)
	for i := range out {
		if out[i].Binary {
			continue
		}
		var p string
		if len(chunks) == len(out) {
			p = chunks[i]
		} else {
			p, _ = g.run(nil, nil, "diff", "--no-renames", from, to, "--", out[i].Path)
		}
		if len(p) > maxPatch {
			p = p[:maxPatch] + "\n... (diff truncated) ..."
		}
		out[i].Patch = p
	}
	return out, nil
}

// splitPatch splits a multi-file diff at its "diff --git" headers.
func splitPatch(s string) []string {
	var out []string
	for s != "" {
		next := strings.Index(s[1:], "\ndiff --git ")
		if next < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:next+2])
		s = s[next+2:]
	}
	return out
}

// partialCommit builds a commit on top of base that contains only the
// given paths as they are in commit (deleted where commit deleted them).
// The real index is never touched.
func (g git) partialCommit(base, commit string, paths []string, msg string) (string, error) {
	f, err := os.CreateTemp("", "sy-index-*")
	if err != nil {
		return "", err
	}
	idx := f.Name()
	f.Close()
	os.Remove(idx)
	defer os.Remove(idx)
	env := []string{"GIT_INDEX_FILE=" + idx}
	if _, err := g.run(env, nil, "read-tree", base); err != nil {
		return "", err
	}
	for _, p := range paths {
		entry, _ := g.out("ls-tree", commit, "--", p)
		if entry == "" {
			if _, err := g.run(env, nil, "update-index", "--force-remove", "--", p); err != nil {
				return "", err
			}
			continue
		}
		// "<mode> blob <sha>\t<path>"
		meta := strings.Fields(strings.SplitN(entry, "\t", 2)[0])
		if len(meta) < 3 {
			continue
		}
		if _, err := g.run(env, nil, "update-index", "--add", "--cacheinfo", meta[0]+","+meta[2]+","+p); err != nil {
			return "", err
		}
	}
	tree, err := g.run(env, nil, "write-tree")
	if err != nil {
		return "", err
	}
	return g.commitTree("commit-tree", strings.TrimSpace(tree), "-p", base, "-m", msg)
}
