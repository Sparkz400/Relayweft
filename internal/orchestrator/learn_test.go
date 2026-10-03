package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/runner"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// seedRoutes writes this repo's worker history: the configured route
// (codex) fails half its runs, claude:sonnet:medium none.
func seedRoutes(t *testing.T, logDir, repo string) {
	t.Helper()
	w, err := sessionlog.Open(logDir, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i := 0; i < 10; i++ {
		for _, r := range []struct {
			prov, model string
			ok          bool
		}{{event.Codex, "gpt-6.1-sol", i%2 == 0}, {event.Claude, "sonnet", true}} {
			w.Write(sessionlog.Record{Type: sessionlog.TypeAgentEnd, TS: time.Now().Add(-48 * time.Hour), TaskID: "old", Step: "s", Kind: "edit",
				Attempt: 1, Role: event.RoleWorker, Provider: r.prov, Model: r.model, Effort: "medium", OK: sessionlog.Bool(r.ok),
				Tokens: &event.TokenUsage{Input: 20_000}, DurationMS: 60_000})
		}
	}
}

// routing.learn: auto learns at task start (once a day), and every
// decision on the learned route says so, in the events and the log.
func TestAutoLearnAtTaskStart(t *testing.T) {
	isolateUserConfig(t)
	dir := gitRepo(t)
	var mu sync.Mutex
	used := map[string]string{}
	set := both(func(s runner.Spec) runner.Result {
		if r, ok := twoEdits(s); ok {
			return r
		}
		mu.Lock()
		used[s.StepID] = s.Provider + ":" + s.Model
		mu.Unlock()
		return runner.Result{Final: "done"}
	})
	o, rec := newOrc(t, dir, set, func(c *config.Config) {
		c.Routing.Learn = config.LearnAuto
		c.Orchestrator.ReviewBeforePlan, c.Orchestrator.ReviewBeforeDone = false, false
	})
	seedRoutes(t, o.logDir(), dir)
	if res := o.Run(context.Background(), longTask); !res.OK {
		t.Fatalf("task failed: %+v", res)
	}
	if used["a"] != "claude:sonnet" || used["b"] != "claude:sonnet" {
		t.Fatalf("steps ran on %v", used)
	}
	learnedReasons := 0
	for _, e := range rec.all() {
		if e.Kind == event.Route && e.Decision.Role == event.RoleWorker {
			if !strings.Contains(e.Decision.Reason, "learned route claude:sonnet:medium: succeeded 100% of 10 runs vs 50% of 10") {
				t.Errorf("reason = %q", e.Decision.Reason)
			}
			learnedReasons++
		}
	}
	if learnedReasons != 2 || countLogs(rec.all(), event.Log, "learned route for worker: codex:gpt-6.1-sol:medium -> claude:sonnet:medium") != 1 {
		t.Fatalf("learned decisions %d, logs: %v", learnedReasons, rec.all())
	}
	recs, _ := sessionlog.ReadDir(o.logDir())
	logged := 0
	for _, r := range recs {
		if r.Type == sessionlog.TypeDecision && r.Role == event.RoleWorker && strings.Contains(r.Reason, "learned route") && r.Kind == "edit" {
			logged++
		}
	}
	if logged != 2 {
		t.Fatalf("%d logged decisions carry the learned reason", logged)
	}
	root, _ := repoRoot(dir)
	l, err := config.LoadLearned(root)
	if err != nil || l.Routes[event.RoleWorker].Spec() != "claude:sonnet:medium" || time.Since(l.Updated) > time.Minute {
		t.Fatalf("learned file = %+v %v", l, err)
	}
	// Refreshed at most once a day: the next task keeps the file as is.
	stamp := l.Updated
	if res := o.Run(context.Background(), longTask); !res.OK {
		t.Fatalf("second task: %+v", res)
	}
	if l2, _ := config.LoadLearned(root); !l2.Updated.Equal(stamp) {
		t.Fatal("refreshed twice in a day")
	}
}

func TestApplyLearnedFollowsTheMode(t *testing.T) {
	isolateUserConfig(t)
	dir := gitRepo(t)
	logDir := t.TempDir()
	seedRoutes(t, logDir, dir)
	recs, _ := sessionlog.ReadDir(logDir)
	cfg := config.Default()
	// A dry run (plain `sy tune`) saves nothing.
	rep, err := UpdateLearned(dir, cfg, recs, time.Now(), true)
	if err != nil || len(rep.Result.Changes) != 1 {
		t.Fatalf("dry run: %+v %v", rep.Result.Changes, err)
	}
	store := config.NewStore(cfg.Clone(), "")
	if roles, _ := ApplyLearned(store, dir); len(roles) != 0 {
		t.Fatalf("dry run applied %v", roles)
	}
	if _, err := UpdateLearned(dir, cfg, recs, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	if roles, _ := ApplyLearned(store, dir); strings.Join(roles, ",") != event.RoleWorker {
		t.Fatalf("suggest mode applies what --apply stored: %v", roles)
	}
	off := cfg.Clone()
	off.Routing.Learn = config.LearnOff
	if roles, _ := ApplyLearned(config.NewStore(off, ""), dir); len(roles) != 0 {
		t.Fatalf("off applied %v", roles)
	}
	// Outside a git repo there are none (and no error).
	if roles, err := ApplyLearned(config.NewStore(cfg.Clone(), ""), t.TempDir()); len(roles) != 0 || err != nil {
		t.Fatalf("outside a repo: %v %v", roles, err)
	}
}
