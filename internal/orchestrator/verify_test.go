package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/runner"
)

// narrowedCheck is an affected_commands template that passes when name
// exists and prints the files it was given.
func narrowedCheck(name string) string {
	if runtime.GOOS == "windows" {
		return "if exist " + name + " (echo ran for {files}) else (echo missing " + name + " & exit 1)"
	}
	return "test -f " + name + " && echo ran for {files} || { echo missing " + name + "; exit 1; }"
}

// verifyLines are the activity log's verify lines.
func verifyLines(rec *recorder) []string {
	var out []string
	for _, e := range rec.all() {
		if (e.Kind == event.Log || e.Kind == event.Error) && strings.HasPrefix(e.Text, "verify ") {
			out = append(out, e.Text)
		}
	}
	return out
}

// The last round's checks decide the task, so they run in full: with the
// default max_fix_rounds 1, a narrowed false failure (or a narrowed pass
// paid twice) must not happen.
func TestLastFixRoundRunsFull(t *testing.T) {
	dir := gitRepo(t)
	full := fileCheck("ok.txt")
	set := both(func(s runner.Spec) runner.Result {
		switch {
		case strings.Contains(s.Prompt, runner.MarkerPlan):
			return runner.Result{Final: planJSON(map[string]any{"id": "w", "title": "work", "kind": "edit", "prompt": "work"})}
		case strings.Contains(s.Prompt, runner.MarkerPlanReview), strings.Contains(s.Prompt, runner.MarkerFinalReview):
			return approve()
		case strings.Contains(s.Prompt, runner.MarkerFix):
			os.WriteFile(filepath.Join(s.Dir, "ok.txt"), []byte("ok\n"), 0o644)
		}
		return runner.Result{Final: "done"}
	})
	o, rec := newOrc(t, dir, set, func(c *config.Config) {
		c.Verify.Commands = []string{full}
		// A narrowed run would fail: never.txt is never written.
		c.Verify.AffectedCommands = map[string]string{full: narrowedCheck("never.txt")}
		c.Orchestrator.MaxFixRounds = 1
	})
	if res := o.Run(context.Background(), longTask); !res.OK {
		t.Fatalf("task: %+v\n%s", res, strings.Join(verifyLines(rec), "\n"))
	}
	for _, l := range verifyLines(rec) {
		if strings.Contains(l, "never.txt") {
			t.Errorf("the last round was narrowed: %s", l)
		}
	}
}

// verifyAt counts the configured commands that fail (best-of-N ranks
// candidates by it).
func TestVerifyAtCountsFailedCommands(t *testing.T) {
	dir := gitRepo(t)
	o, _ := newOrc(t, dir, both(func(s runner.Spec) runner.Result { return runner.Result{Final: "done"} }), nil)
	vc := config.VerifyCfg{Commands: []string{fileCheck("a.txt"), fileCheck("README.md"), fileCheck("b.txt")}}
	ok, rep, failed := o.verifyAt(context.Background(), &task{id: "t"}, vc, checkSite{dir: dir}, verifyFull)
	if ok || failed != 2 || !strings.Contains(rep, "missing a.txt") || !strings.Contains(rep, "missing b.txt") {
		t.Errorf("ok=%v failed=%d\n%s", ok, failed, rep)
	}
	if ok, _, failed := o.verifyAt(context.Background(), &task{id: "t"}, config.VerifyCfg{Commands: []string{fileCheck("README.md")}}, checkSite{dir: dir}, verifyFull); !ok || failed != 0 {
		t.Errorf("pass: ok=%v failed=%d", ok, failed)
	}
}

// A comma in an allow rule would split Claude's comma-joined
// --allowedTools into other rules.
func TestVerifyAllowedDropsCommas(t *testing.T) {
	vc := config.VerifyCfg{Commands: []string{"make check"}, AffectedCommands: map[string]string{"make check": "run a,b {files}"}}
	for _, a := range verifyAllowed(vc, "") {
		if strings.Contains(a, ",") {
			t.Errorf("allowed %q", a)
		}
	}
}

// After a fix round only the affected tests run first. A narrowed failure
// goes to the next fix round without a full run; once the narrowed run
// passes, the full checks run before the final review.
func TestFixRoundRunsAffectedTestsFirst(t *testing.T) {
	dir := gitRepo(t)
	full := fileCheck("ok.txt")
	var mu sync.Mutex
	var fixPrompts []string
	set := both(func(s runner.Spec) runner.Result {
		switch {
		case strings.Contains(s.Prompt, runner.MarkerPlan):
			return runner.Result{Final: planJSON(map[string]any{"id": "w", "title": "work", "kind": "edit", "prompt": "work"})}
		case strings.Contains(s.Prompt, runner.MarkerPlanReview), strings.Contains(s.Prompt, runner.MarkerFinalReview):
			return approve()
		case strings.Contains(s.Prompt, runner.MarkerFix):
			mu.Lock()
			fixPrompts = append(fixPrompts, s.Prompt)
			n := len(fixPrompts)
			mu.Unlock()
			if n == 1 {
				os.WriteFile(filepath.Join(s.Dir, "x.txt"), []byte("not yet\n"), 0o644)
			} else {
				os.WriteFile(filepath.Join(s.Dir, "ok.txt"), []byte("ok\n"), 0o644)
				os.WriteFile(filepath.Join(s.Dir, "narrow.txt"), []byte("ok\n"), 0o644)
			}
		default:
			os.WriteFile(filepath.Join(s.Dir, "work.txt"), []byte("work\n"), 0o644)
		}
		return runner.Result{Final: "done"}
	})
	o, rec := newOrc(t, dir, set, func(c *config.Config) {
		c.Verify.Commands = []string{full}
		c.Verify.AffectedCommands = map[string]string{full: narrowedCheck("narrow.txt")}
		c.Orchestrator.MaxFixRounds = 3
	})
	res := o.Run(context.Background(), longTask)
	if !res.OK || !strings.Contains(res.Summary, "checks pass") {
		t.Fatalf("task: %+v", res)
	}
	lines := verifyLines(rec)
	var fulls, narrowed []string
	for _, l := range lines {
		switch {
		case strings.Contains(l, " "+full+" ("):
			fulls = append(fulls, l)
		case strings.Contains(l, "ran for {files}"):
			t.Errorf("placeholder not filled: %s", l)
		case strings.Contains(l, "narrow.txt"):
			narrowed = append(narrowed, l)
		}
	}
	// Round 0 full (fails), round 1 narrowed (fails, no full run), round 2
	// narrowed (passes), then full (passes).
	if len(fulls) != 2 || !strings.HasPrefix(fulls[0], "verify ✗") || !strings.HasPrefix(fulls[1], "verify ✓") ||
		!strings.Contains(fulls[0], "full: the full checks before the final review") || !strings.Contains(fulls[1], "full: the affected tests pass") {
		t.Errorf("full runs:\n%s", strings.Join(lines, "\n"))
	}
	if len(narrowed) != 2 || !strings.HasPrefix(narrowed[0], "verify ✗") || !strings.HasPrefix(narrowed[1], "verify ✓") ||
		!strings.Contains(narrowed[0], "./work.txt ./x.txt") || !strings.Contains(narrowed[0], "affected: affected_commands, for the 2 changed files") {
		t.Errorf("narrowed runs:\n%s", strings.Join(lines, "\n"))
	}
	if len(fixPrompts) != 2 || !strings.Contains(fixPrompts[1], "affected tests only") || !strings.Contains(fixPrompts[1], "missing narrow.txt") {
		t.Errorf("second fix round did not get the narrowed failure:\n%s", fixPrompts[len(fixPrompts)-1])
	}
}

// verify.affected: off keeps the old behaviour: full checks every round.
func TestVerifyAffectedOff(t *testing.T) {
	dir := gitRepo(t)
	full := fileCheck("ok.txt")
	fixes := 0
	set := both(func(s runner.Spec) runner.Result {
		switch {
		case strings.Contains(s.Prompt, runner.MarkerPlan):
			return runner.Result{Final: planJSON(map[string]any{"id": "w", "title": "work", "kind": "edit", "prompt": "work"})}
		case strings.Contains(s.Prompt, runner.MarkerPlanReview), strings.Contains(s.Prompt, runner.MarkerFinalReview):
			return approve()
		case strings.Contains(s.Prompt, runner.MarkerFix):
			fixes++
			os.WriteFile(filepath.Join(s.Dir, "ok.txt"), []byte("ok\n"), 0o644)
		}
		return runner.Result{Final: "done"}
	})
	o, rec := newOrc(t, dir, set, func(c *config.Config) {
		c.Verify.Commands = []string{full}
		c.Verify.AffectedCommands = map[string]string{full: narrowedCheck("never.txt")}
		c.Verify.Affected = "off"
	})
	if res := o.Run(context.Background(), longTask); !res.OK || fixes != 1 {
		t.Fatalf("task: %+v (%d fixes)", res, fixes)
	}
	for _, l := range verifyLines(rec) {
		if strings.Contains(l, "never.txt") || strings.Contains(l, "affected:") {
			t.Errorf("narrowed although off: %s", l)
		}
	}
}

// Writing agents may run the narrowed forms of the checks (and are told
// how); read-only agents still get no commands.
func TestWorkersMayRunNarrowedChecks(t *testing.T) {
	var mu sync.Mutex
	var work runner.Spec
	set := both(func(s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, runner.MarkerPlan) {
			return runner.Result{Final: planJSON(map[string]any{"id": "w", "title": "work", "kind": "edit", "prompt": "work"})}
		}
		if strings.Contains(s.Prompt, runner.MarkerPlanReview) || strings.Contains(s.Prompt, runner.MarkerFinalReview) {
			return approve()
		}
		mu.Lock()
		if s.StepID == "w" {
			work = s
		}
		mu.Unlock()
		return runner.Result{Final: "done"}
	})
	// A repo without go.mod: sy's own verify run of these commands fails
	// at once instead of running this package's tests (dir "" is the
	// test's working folder).
	o, _ := newOrc(t, gitRepo(t), set, func(c *config.Config) {
		c.Verify.Commands = []string{"go build ./...", "go test -race ./..."}
		c.Orchestrator.MaxFixRounds = 0
	})
	o.Run(context.Background(), longTask)
	for _, c := range []string{"go build ./...", "go test -race ./...", "go test -race"} {
		if !slices.Contains(work.AllowedCommands, c) {
			t.Errorf("%q not allowed: %q", c, work.AllowedCommands)
		}
	}
	// "go build <anything>" would allow -toolexec and -o.
	if slices.Contains(work.AllowedCommands, "go build") {
		t.Errorf("go build allowed with any arguments: %q", work.AllowedCommands)
	}
	if !strings.Contains(work.Prompt, "go test -race <packages>") {
		t.Errorf("prompt does not name the narrowed form:\n%s", work.Prompt)
	}
	off := config.VerifyCfg{Commands: []string{"go test ./..."}, Affected: "off"}
	if got := verifyAllowed(off, ""); !slices.Equal(got, []string{"go test ./..."}) {
		t.Errorf("off: %q", got)
	}
	if strings.Contains(verifyHint(off, ""), "<packages>") {
		t.Error("off: hint names the narrowed form")
	}
}

func TestChangedSince(t *testing.T) {
	dir := gitRepo(t)
	base := headOf(t, dir)
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("changed\n"), 0o644)
	os.Remove(filepath.Join(dir, "README.md"))
	os.MkdirAll(filepath.Join(dir, "new dir"), 0o755)
	os.WriteFile(filepath.Join(dir, "new dir", "ünï.go"), []byte("package x\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "ignored"), 0o755)
	os.WriteFile(filepath.Join(dir, "ignored", "out.bin"), []byte("x"), 0o644)
	files, skipped, err := git{dir}.changedSince(base)
	if err != nil || len(skipped) != 0 {
		t.Fatal(err, skipped)
	}
	slices.Sort(files)
	if want := []string{"README.md", "new dir/ünï.go", "shared.txt"}; !slices.Equal(files, want) {
		t.Errorf("changed = %q, want %q", files, want)
	}
}

// A cancel during a narrowed run reports "cancelled" as it is: the task
// loop stops on exactly that text.
func TestNarrowedVerifyCancelled(t *testing.T) {
	dir := gitRepo(t)
	base := headOf(t, dir)
	os.WriteFile(filepath.Join(dir, "work.txt"), []byte("x\n"), 0o644)
	o, _ := newOrc(t, dir, both(func(s runner.Spec) runner.Result { return runner.Result{Final: "done"} }), nil)
	full := fileCheck("ok.txt")
	vc := config.VerifyCfg{Commands: []string{full}, AffectedCommands: map[string]string{full: narrowedCheck("ok.txt")}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ok, rep, _ := o.verifyAt(ctx, &task{id: "t"}, vc, checkSite{dir: dir, root: dir, base: base}, verifyAffected)
	if ok || rep != "cancelled" {
		t.Errorf("got %v %q", ok, rep)
	}
}
