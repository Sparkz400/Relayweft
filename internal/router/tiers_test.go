package router

import (
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
)

func tierRouter(st State) (*Router, *config.Config) {
	cfg := config.Default()
	cfg.Routing.Tiers = config.TiersAuto
	return &Router{Cfg: func() *config.Config { return cfg }, State: st}, cfg
}

func TestTiersOffByDefault(t *testing.T) {
	cfg := config.Default()
	if cfg.Routing.Tiers != config.TiersOff {
		t.Fatalf("default tiers = %q, want off", cfg.Routing.Tiers)
	}
	r := &Router{Cfg: func() *config.Config { return cfg }}
	d := r.Route(Step{Kind: KindEdit, Prompt: "fix a typo in the readme"})
	if d.Tier != "" || d.Model != cfg.Roles[event.RoleWorker].Codex.Model || strings.Contains(d.Reason, "tier") {
		t.Errorf("tiers off changed the route: %+v", d)
	}
}

// Hard (routing.best_of.when: hard) reuses the risk rules and the
// difficulty score, with or without tiers.
func TestHardSteps(t *testing.T) {
	cfg := config.Default()
	r := &Router{Cfg: func() *config.Config { return cfg }}
	for _, tc := range []struct {
		name string
		step Step
		hard bool
	}{
		{"routine edit", Step{Kind: KindEdit, Title: "Fix typo", Prompt: "fix the typo in the readme"}, false},
		{"plain edit", Step{Kind: KindEdit, Title: "Add flag", Prompt: "add a --json flag to the list command"}, false},
		{"sensitive path", Step{Kind: KindEdit, Title: "Change login", Prompt: "change it", Files: []string{"internal/auth/login.go"}}, true},
		{"many files", Step{Kind: KindEdit, Title: "Rename", Prompt: "rename it", Files: []string{"a", "b", "c", "d", "e", "f"}}, true},
		{"hard words and files", Step{Kind: KindEdit, Title: "Fix race", Prompt: "fix the race condition between the scheduler and the cache", Files: []string{"a.go", "b.go", "c.go", "d.go", "e.go"}}, true},
		{"pinned worker_high", Step{Kind: KindEdit, Title: "Small", Prompt: "small change", UserRole: event.RoleWorkerHigh}, true},
		{"read-only", Step{Kind: KindExplore, Title: "Find", Prompt: "find the race condition", Files: []string{"auth.go"}}, false},
	} {
		if got, why := r.Hard(tc.step); got != tc.hard || (got && why == "") {
			t.Errorf("%s: Hard = %v (%q), want %v", tc.name, got, why, tc.hard)
		}
	}
}

// A pinned route (a best-of candidate's) is used as it is.
func TestPinnedRoute(t *testing.T) {
	r, _ := tierRouter(state{})
	pin := event.Decision{Role: event.RoleWorker, Provider: event.Claude, Model: "opus", Effort: "high", Rule: RuleBestOf}
	d := r.Route(Step{ID: "w", Title: "work", Kind: KindEdit, Prompt: "fix a typo", Pin: &pin})
	if d.Provider != event.Claude || d.Model != "opus" || d.Rule != RuleBestOf || d.StepID != "w" || d.Tier != "" {
		t.Errorf("pinned route changed: %+v", d)
	}
}

func TestTiersByDifficulty(t *testing.T) {
	r, cfg := tierRouter(state{})
	long := strings.Repeat("the handler must keep every field in sync with the store ", 30)
	cases := []struct {
		name string
		step Step
		role string // role kept
		tier string
		via  string // role whose route is used
	}{
		{"routine edit goes fast", Step{Kind: KindEdit, Title: "Fix typo", Prompt: "fix the typo in the readme"},
			event.RoleWorker, TierFast, event.RoleExplorer},
		{"plain edit stays standard", Step{Kind: KindEdit, Title: "Add flag", Prompt: "add a --json flag to rw stats"},
			event.RoleWorker, TierStandard, event.RoleWorker},
		// Seen in the tiers bench: a planner's prompt for a logic fix
		// that mentions a routine word in passing.
		{"routine word in passing stays standard", Step{Kind: KindEdit, Title: "Keep trailing empty fields in parse_line",
			Prompt: "parse_line in inventory/csvparse.py drops trailing empty fields. Change the split so that \"a,b,,\" gives four fields. " +
				"Do not rename the function and keep its docstring. Add a unit test in tests/test_csvparse.py for the case.", Files: []string{"inventory/csvparse.py"}},
			event.RoleWorker, TierStandard, event.RoleWorker},
		{"routine word in a short prompt stays standard", Step{Kind: KindEdit, Title: "Fix off-by-one in pagination",
			Prompt: "The last page repeats one item. Fix the loop bound in paginate, keep the comments."},
			event.RoleWorker, TierStandard, event.RoleWorker},
		{"routine title goes fast", Step{Kind: KindEdit, Title: "Fix the typos in the README",
			Prompt: "The README has several spelling mistakes in the install and usage sections. Correct them without changing the meaning, " +
				"the headings or the code blocks, and keep the line breaks as they are.", Files: []string{"README.md"}},
			event.RoleWorker, TierFast, event.RoleExplorer},
		{"long hard edit goes strong", Step{Kind: KindEdit, Title: "Fix the race", Prompt: "fix the race condition in the pool. " + long, Files: []string{"a.go", "b.go", "c.go"}},
			event.RoleWorker, TierStrong, event.RoleWorkerHigh},
		{"hard explore goes standard, not strong", Step{Kind: KindExplore, Prompt: "explain the scheduler and its concurrency model. " + long},
			event.RoleExplorer, TierStandard, event.RoleWorker},
		{"plain explore stays fast", Step{Kind: KindExplore, Prompt: "where is the parser"},
			event.RoleExplorer, TierFast, event.RoleExplorer},
		{"sensitive never drops", Step{Kind: KindEdit, Title: "Fix typo in auth", Prompt: "fix a typo", Files: []string{"internal/auth/x.go"}},
			event.RoleWorkerHigh, TierStrong, event.RoleWorkerHigh},
	}
	for _, c := range cases {
		d := r.Route(c.step)
		want := cfg.Roles[c.via].For(d.Provider)
		if d.Role != c.role || d.Tier != c.tier || d.Model != want.Model || d.Effort != want.Effort {
			t.Errorf("%s: got role %s tier %s %s, want role %s tier %s %s:%s@%s (%s)",
				c.name, d.Role, d.Tier, d.Label(), c.role, c.tier, d.Provider, want.Model, want.Effort, d.Reason)
		}
		if !strings.Contains(d.Reason, "tier "+c.tier) {
			t.Errorf("%s: reason does not name the tier: %s", c.name, d.Reason)
		}
	}
}

func TestTiersSaveQuota(t *testing.T) {
	step := Step{Kind: KindEdit, Title: "Add flag", Prompt: "add a --json flag to rw stats"}
	// Plenty left: standard.
	r, _ := tierRouter(state{util: map[string]float64{event.Codex: 0.3}})
	if d := r.Route(step); d.Tier != TierStandard {
		t.Fatalf("70%% left: tier %s (%s)", d.Tier, d.Reason)
	}
	// Nearly out on Codex (and Claude worse, so no quota-preempt): fast.
	r, _ = tierRouter(state{util: map[string]float64{event.Codex: 0.85, event.Claude: 0.95}})
	d := r.Route(step)
	if d.Provider != event.Codex || d.Tier != TierFast || !strings.Contains(d.Reason, "quota left 15%") {
		t.Fatalf("15%% left: %+v", d)
	}
	// The budget counts as quota too.
	r, _ = tierRouter(state{})
	b := step
	b.BudgetUsed = 0.9
	if d := r.Route(b); d.Tier != TierFast {
		t.Fatalf("budget 90%% used: tier %s (%s)", d.Tier, d.Reason)
	}
	// A risky step keeps its floor however low the quota.
	s := Step{Kind: KindEdit, Title: "Add DB migration", Prompt: "add a migration", BudgetUsed: 1}
	if d := r.Route(s); d.Tier != TierStrong || d.Role != event.RoleWorkerHigh {
		t.Fatalf("sensitive step dropped: %+v", d)
	}
}

func TestTiersAfterQuotaPreempt(t *testing.T) {
	// Codex at 95% moves the step to Claude (quota-preempt); Claude has
	// plenty left, so the tier is picked on Claude's headroom.
	r, cfg := tierRouter(state{util: map[string]float64{event.Codex: 0.95, event.Claude: 0.1}})
	d := r.Route(Step{Kind: KindEdit, Title: "Rename", Prompt: "rename Foo to Bar"})
	if d.Rule != RuleQuota || d.Provider != event.Claude || d.Tier != TierFast || d.Model != cfg.Roles[event.RoleExplorer].Claude.Model {
		t.Fatalf("got %+v", d)
	}
}

func TestTiersLeaveFixedRolesAlone(t *testing.T) {
	r, cfg := tierRouter(state{util: map[string]float64{event.Codex: 0.99, event.Claude: 0.99}})
	for _, s := range []Step{
		{Kind: KindPlan, Prompt: "fix a typo"},
		{Kind: KindReview, MainProvider: event.Codex},
		{Kind: KindJudge},
		{Kind: KindEdit, Prompt: "fix a typo", UserRole: event.RoleWorkerHigh},
	} {
		d := r.Route(s)
		if d.Tier != "" || d.Model != cfg.Roles[d.Role].For(d.Provider).Model {
			t.Errorf("%s moved: %+v", s.Kind, d)
		}
	}
	// A role set explicitly (/route, flags, repo file) is not moved either.
	r.Pinned = func(role string) bool { return role == event.RoleWorker }
	if d := r.Route(Step{Kind: KindEdit, Prompt: "fix a typo"}); d.Tier != "" || d.Model != cfg.Roles[event.RoleWorker].Codex.Model {
		t.Errorf("pinned worker moved: %+v", d)
	}
}

func TestTiersMissingRouteKeepsOwn(t *testing.T) {
	r, cfg := tierRouter(state{})
	rc := cfg.Roles[event.RoleExplorer]
	rc.Codex.Model = ""
	cfg.Roles[event.RoleExplorer] = rc
	d := r.Route(Step{Kind: KindEdit, Prompt: "fix a typo"})
	if d.Provider != event.Codex || d.Tier != TierStandard || d.Model != cfg.Roles[event.RoleWorker].Codex.Model {
		t.Fatalf("got %+v", d)
	}
	if !strings.Contains(d.Reason, "no explorer route on codex") {
		t.Errorf("reason: %s", d.Reason)
	}
}

func TestTierJudgeFloor(t *testing.T) {
	r, _ := tierRouter(state{util: map[string]float64{event.Codex: 0.99, event.Claude: 0.99}})
	d := r.Route(Step{Kind: KindEdit, Prompt: "fix a typo", ForceRole: event.RoleWorkerHigh})
	if d.Tier != TierStrong {
		t.Fatalf("judge's pick dropped: %+v", d)
	}
}

func TestDifficultyBounds(t *testing.T) {
	huge := strings.Repeat("redesign the distributed protocol ", 200)
	files := make([]string, 50)
	if v, _ := Difficulty(Step{Kind: KindFix, Prompt: huge, Files: files}, event.RoleWorkerHigh); v > 1 {
		t.Errorf("difficulty %v > 1", v)
	}
	if v, _ := Difficulty(Step{Kind: KindExplore, Prompt: "typo", Files: []string{"a"}}, event.RoleExplorer); v < 0 {
		t.Errorf("difficulty %v < 0", v)
	}
}

func TestQuotaShift(t *testing.T) {
	for _, c := range []struct{ left, want float64 }{{1, 0}, {0.5, 0}, {0.25, 0.15}, {0, 0.3}} {
		if got := quotaShift(c.left, 0.5); got < c.want-1e-9 || got > c.want+1e-9 {
			t.Errorf("quotaShift(%v) = %v, want %v", c.left, got, c.want)
		}
	}
}

func TestTiersConfigValidation(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Tiers = "sometimes"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "routing.tiers") {
		t.Errorf("bad tiers accepted: %v", err)
	}
	cfg.Routing.Tiers = config.TiersAuto
	cfg.Routing.TiersSaveAt = 1.5
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "tiers_save_below") {
		t.Errorf("bad tiers_save_below accepted: %v", err)
	}
	cfg.Routing.TiersSaveAt = 0
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Routing.TiersSaveBelow() != config.DefaultTiersSaveBelow {
		t.Errorf("default save_below = %v", cfg.Routing.TiersSaveBelow())
	}
	// Survives a save/load round trip (Clone marshals to YAML).
	if c := cfg.Clone(); c.Routing.Tiers != config.TiersAuto {
		t.Errorf("round trip lost tiers: %q", c.Routing.Tiers)
	}
}

// A free local model standing by keeps its own route: it has no quota to
// save, and the route that stands by is the one the person set up for it.
func TestTiersLeaveStandbyAlone(t *testing.T) {
	cfg := withProviders(t, event.Qwen)
	cfg.Routing.Tiers = config.TiersAuto
	r := routerFor(cfg, state{limited: map[string]bool{event.Codex: true, event.Claude: true}})
	d := r.Route(Step{Kind: KindExplore, Prompt: "find the race condition in the scheduler",
		Files: []string{"a.go", "b.go", "c.go", "d.go", "e.go"}})
	if d.Provider != event.Qwen || d.Rule != RuleStandby {
		t.Fatalf("not on standby: %+v", d)
	}
	if d.Tier != "" || strings.Contains(d.Reason, "tier") || d.Model != cfg.Roles[event.RoleExplorer].For(event.Qwen).Model {
		t.Errorf("tiers moved the standby route: %+v", d)
	}
}
