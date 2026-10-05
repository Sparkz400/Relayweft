package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/runner"
)

// A task in a checkout changes only the checkout; removing it drops the
// worktree, its record and its undo refs, and the user's tree, index and
// branch never move.
func TestCheckoutRunsTaskApartFromUserTree(t *testing.T) {
	dir := gitRepo(t)
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("user edit\n"), 0o644)
	head := headOf(t, dir)
	co, err := NewCheckout(dir, "o/r#7", head)
	if err != nil {
		t.Fatal(err)
	}
	if SamePath(co.Dir, dir) || !isWorktreeOf(dir, co.Dir) {
		t.Fatalf("checkout %s is not a separate worktree of %s", co.Dir, dir)
	}
	if got := read(t, filepath.Join(co.Dir, "shared.txt")); got != "base\n" {
		t.Fatalf("checkout has %q, want the commit's content", got)
	}
	set := both(func(s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, runner.MarkerStep) {
			os.WriteFile(filepath.Join(s.Dir, "shared.txt"), []byte("agent\n"), 0o644)
			return runner.Result{Final: "edited", Files: []string{"shared.txt"}}
		}
		return approve()
	})
	o, _ := newOrc(t, co.Dir, set, func(c *config.Config) {
		c.Orchestrator.ReviewBeforeDone = false
		c.Orchestrator.ApprovePlan = false
	})
	res := o.RunWith(context.Background(), "Change the shared file", TaskOptions{Unattended: true})
	if !res.OK || res.UndoKey == "" {
		t.Fatalf("%+v", res)
	}
	if got := read(t, filepath.Join(co.Dir, "shared.txt")); got != "agent\n" {
		t.Fatalf("checkout = %q", got)
	}
	if got := read(t, filepath.Join(dir, "shared.txt")); got != "user edit\n" || headOf(t, dir) != head {
		t.Fatalf("the user's tree moved: %q", got)
	}
	st, err := LoadTask(res.UndoKey)
	if err != nil || st.Author() == "" {
		t.Fatalf("author not recorded: %+v %v", st, err)
	}
	if list, _ := UndoList(co.Dir); len(list) != 1 {
		t.Fatalf("undo records in the checkout: %d", len(list))
	}
	path := co.Dir
	if err := co.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("checkout still there: %v", err)
	}
	if refs, _ := (git{dir}).out("for-each-ref", undoRefs); refs != "" {
		t.Fatalf("undo refs left behind:\n%s", refs)
	}
	if wts, _ := (git{dir}).out("worktree", "list", "--porcelain"); strings.Count(wts, "worktree ") != 1 {
		t.Fatalf("worktree record left:\n%s", wts)
	}
	// The same name can be checked out again (also after a crash left it).
	co2, err := NewCheckout(dir, "o/r#7", head)
	if err != nil {
		t.Fatal(err)
	}
	co3, err := NewCheckout(dir, "o/r#7", head)
	if err != nil || !SamePath(co3.Dir, co2.Dir) {
		t.Fatalf("recreate: %v", err)
	}
	co3.Remove()
}

func TestTaskAuthor(t *testing.T) {
	for _, c := range []struct {
		authors map[string]int
		want    string
	}{
		{nil, ""},
		{map[string]int{"codex": 2, "claude": 1}, "codex"},
		{map[string]int{"claude": 1}, "claude"},
		{map[string]int{"codex": 1, "claude": 1}, ""},
	} {
		if got := (TaskState{Authors: c.authors}).Author(); got != c.want {
			t.Errorf("%v: author %q, want %q", c.authors, got, c.want)
		}
	}
}

// RunRead runs one read-only reviewer under the budget and logs its cost
// as a task, so it counts into the day.
func TestRunReadIsReadOnlyAndCounted(t *testing.T) {
	var specs []runner.Spec
	set := both(func(s runner.Spec) runner.Result {
		specs = append(specs, s)
		return runner.Result{Final: `{"findings": []}`, Tokens: event.TokenUsage{Input: 1000, CostUSD: 0.5}}
	})
	o, _ := newOrc(t, "", set, budgetCfg(func(b *config.BudgetCfg) { b.DayUSD = 0.6 }))
	res := o.RunRead(context.Background(), "Review pull request #3", "the prompt", router.KindReview)
	if !res.OK || res.Reply != `{"findings": []}` || res.Provider == "" || res.Cost.CostUSD != 0.5 {
		t.Fatalf("%+v", res)
	}
	if len(specs) != 1 || !specs[0].ReadOnly || specs[0].Role != event.RoleReviewer || specs[0].Prompt != "the prompt" {
		t.Fatalf("specs %+v", specs)
	}
	if st := o.BudgetStatus(); st.DayUSD < 0.49 {
		t.Fatalf("not counted into the day: %+v", st)
	}
	// Under the day limit still, so the next one starts; then it is over.
	o.RunRead(context.Background(), "again", "p", router.KindReview)
	res = o.RunRead(context.Background(), "third", "p", router.KindReview)
	if res.OK || !strings.Contains(res.Summary, "budget") || len(specs) != 2 {
		t.Fatalf("over the day budget: %+v (%d runs)", res, len(specs))
	}
	if r := o.RunRead(context.Background(), "x", "p", router.KindEdit); r.OK || !strings.Contains(r.Summary, "read-only") {
		t.Fatalf("write kind accepted: %+v", r)
	}
}
