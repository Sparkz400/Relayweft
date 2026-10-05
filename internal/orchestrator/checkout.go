package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/proc"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// Checkout is a detached worktree of a repository at one commit, in rw's
// cache next to the pool. Work that must not touch the user's working
// tree, index or current branch runs there (rw watch: a follow-up task on
// a pull request's branch). An orchestrator whose Dir is the checkout runs
// tasks in it as in any working tree; the undo records they leave are
// dropped with the checkout.
type Checkout struct {
	Dir  string // the checkout's top level, as git reports it
	root string // the repository it belongs to
}

// NewCheckout creates the checkout called name (any text; it is made
// path-safe) of the repository containing dir, at commit. A leftover
// checkout of the same name (a crashed run) is replaced.
func NewCheckout(dir, name, commit string) (*Checkout, error) {
	root, err := repoRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %s", dir)
	}
	n := slug(name)
	if n == "" {
		return nil, errors.New("checkout needs a name")
	}
	path := filepath.Join(repoCache(root), "checkouts", n)
	if _, err := os.Lstat(path); err == nil {
		if err := removeSlot(git{root}.commonDir(), path); err != nil {
			return nil, err
		}
	}
	removeTrash(filepath.Dir(path)) // checkouts Windows could not delete before
	if err := checkDisk(filepath.Dir(filepath.Dir(path))); err != nil {
		return nil, err
	}
	if err := (git{root}).addWorktree(path, commit); err != nil {
		return nil, err
	}
	top, err := repoRoot(path)
	if err != nil {
		_ = removeSlot(git{root}.commonDir(), path)
		return nil, err
	}
	return &Checkout{Dir: top, root: root}, nil
}

// Remove deletes the checkout with its worktree record and the undo
// records of the tasks that ran in it (nothing could be undone there once
// it is gone).
func (c *Checkout) Remove() error {
	if list, err := UndoList(c.Dir); err == nil {
		g := git{c.Dir}
		for _, t := range list {
			for _, w := range []string{"before", "after", "undone"} {
				g.deleteSnapshot(t.Key, w)
			}
		}
	}
	return removeSlot(git{c.root}.commonDir(), c.Dir)
}

// ReadResult is what RunRead's agent produced.
type ReadResult struct {
	TaskResult
	Reply    string // the agent's last message
	Provider string
	Model    string
}

// RunRead runs one read-only agent as a task of its own (rw review): the
// router picks the route for kind (KindReview: the reviewer role, or the
// forced provider), the budget applies as for any task and the cost counts
// into the day. text names the task in the logs; the agent gets prompt.
func (o *Orchestrator) RunRead(ctx context.Context, text, prompt string, kind router.Kind) ReadResult {
	if !kind.ReadOnly() {
		return ReadResult{TaskResult: TaskResult{Summary: "internal error: RunRead needs a read-only step kind"}}
	}
	o.mu.Lock()
	o.taskSeq++
	seq := o.taskSeq
	o.mu.Unlock()
	began := time.Now()
	cfg := o.opts.Store.Get()
	proc.SetLowPriority(cfg.Orchestrator.LowPriority)
	setPoolLimits(cfg)
	t := &task{id: fmt.Sprintf("%stask-%d", o.opts.TaskIDPrefix, seq), text: text, cfg: cfg, runners: o.opts.Runners(cfg), dir: o.opts.Dir}
	t.key = o.opts.Log.Session() + "-" + t.id
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeTask, TaskID: t.id, Task: text, Mode: o.opts.Mode})
	o.emit(event.Event{Kind: event.TaskStart, Text: text})
	t.quotaBefore = o.quotaNow()
	agent := AgentMain
	if kind == router.KindReview {
		agent = AgentReviewer
	}
	bctx := o.startBudget(ctx, t)
	step := router.Step{ID: string(kind), Title: firstWords(text, 6), Kind: kind, Prompt: prompt}
	d, res := o.runOnce(bctx, t, step, agent, "", prompt)
	out := ReadResult{Reply: res.Final, Provider: d.Provider, Model: d.Model}
	out.OK = res.OK()
	out.Summary = clip(res.Final, 300)
	if res.Err != nil {
		out.Summary = res.Err.Error()
	}
	if why := t.budgetStopped(); why != "" && ctx.Err() == nil {
		out.OK = false
		out.Summary = "stopped by budget: " + why
	}
	if ctx.Err() != nil {
		out.OK = false
		out.Summary = "cancelled: " + out.Summary
	}
	out.Duration = time.Since(began)
	out.Tokens = t.usage()
	out.Cost = o.cost(t)
	tk, cost := out.Tokens, out.Cost
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeTaskEnd, TaskID: t.id, Task: text, Mode: o.opts.Mode,
		OK: sessionlog.Bool(out.OK), Text: out.Summary, Tokens: &tk, DurationMS: out.Duration.Milliseconds(), Cost: &cost, Bench: o.opts.Bench})
	o.endBudget(t, cost)
	o.emit(event.Event{Kind: event.TaskDone, OK: out.OK, Text: out.Summary, Tokens: tk, Cost: &cost})
	return out
}
