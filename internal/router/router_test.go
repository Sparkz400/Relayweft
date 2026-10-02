package router

import (
	"testing"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
)

type state struct {
	limited map[string]bool
	share   map[string]float64
}

func (s state) Limited(p string) bool  { return s.limited[p] }
func (s state) Share(p string) float64 { return s.share[p] }

func newRouter(st state) *Router {
	cfg := config.Default()
	return &Router{Cfg: func() *config.Config { return cfg }, State: st}
}

func TestRules(t *testing.T) {
	r := newRouter(state{})
	cases := []struct {
		name     string
		step     Step
		role     string
		rule     string
		provider string
	}{
		{"plan", Step{Kind: KindPlan}, event.RolePlanner, RulePlan, event.Codex},
		{"review is other provider", Step{Kind: KindReview, MainProvider: event.Codex}, event.RoleReviewer, RuleReview, event.Claude},
		{"review other of claude", Step{Kind: KindReview, MainProvider: event.Claude}, event.RoleReviewer, RuleReview, event.Codex},
		{"explore", Step{Kind: KindExplore, Prompt: "where is the parser"}, event.RoleExplorer, RuleReadOnly, event.Claude},
		{"research", Step{Kind: KindResearch}, event.RoleResearcher, RuleReadOnly, event.Claude},
		{"repeat error escalates", Step{Kind: KindEdit, RepeatError: true, Escalations: 1}, event.RoleWorkerHigh, RuleRepeatError, event.Codex},
		{"repeat twice -> planner", Step{Kind: KindEdit, RepeatError: true, Escalations: 2}, event.RolePlanner, RuleRepeatError, event.Codex},
		{"many files", Step{Kind: KindEdit, Files: []string{"a", "b", "c", "d", "e", "f"}}, event.RoleWorkerHigh, RuleLargeDiff, event.Codex},
		{"sensitive path", Step{Kind: KindEdit, Files: []string{"internal/auth/token.go"}}, event.RoleWorkerHigh, RuleLargeDiff, event.Codex},
		{"sensitive title word", Step{Kind: KindEdit, Title: "Add DB migration"}, event.RoleWorkerHigh, RuleLargeDiff, event.Codex},
		{"author is not auth", Step{Kind: KindEdit, Title: "Show author name"}, event.RoleWorker, RuleDefault, event.Codex},
		{"default", Step{Kind: KindEdit, Prompt: "add a flag"}, event.RoleWorker, RuleDefault, event.Codex},
	}
	for _, c := range cases {
		d := r.Route(c.step)
		if d.Role != c.role || d.Rule != c.rule || d.Provider != c.provider {
			t.Errorf("%s: got %s/%s/%s, want %s/%s/%s", c.name, d.Role, d.Rule, d.Provider, c.role, c.rule, c.provider)
		}
		if d.Model == "" {
			t.Errorf("%s: no model", c.name)
		}
	}
}

func TestLimitFallbackKeepsRole(t *testing.T) {
	r := newRouter(state{limited: map[string]bool{event.Codex: true}})
	d := r.Route(Step{Kind: KindEdit, Prompt: "fix the bug"})
	if d.Provider != event.Claude || d.Role != event.RoleWorker || d.Rule != RuleLimit || !d.Fallback {
		t.Fatalf("got %+v", d)
	}
	if d.Model != "sonnet" {
		t.Errorf("fallback should use the worker's claude route, got %s", d.Model)
	}
}

func TestBothLimitedStaysPut(t *testing.T) {
	r := newRouter(state{limited: map[string]bool{event.Codex: true, event.Claude: true}})
	d := r.Route(Step{Kind: KindEdit})
	if d.Fallback {
		t.Fatalf("no fallback possible, got %+v", d)
	}
}

func TestPreferAutoAndForce(t *testing.T) {
	cfg := config.Default()
	rc := cfg.Roles[event.RoleWorker]
	rc.Prefer = config.PreferAuto
	cfg.Roles[event.RoleWorker] = rc
	r := &Router{Cfg: func() *config.Config { return cfg }, State: state{share: map[string]float64{event.Codex: 0.8, event.Claude: 0.2}}}
	if d := r.Route(Step{Kind: KindEdit}); d.Provider != event.Claude {
		t.Errorf("auto should pick the less used provider, got %s", d.Provider)
	}
	r.ForceProvider = event.Codex
	if d := r.Route(Step{Kind: KindReview}); d.Provider != event.Codex {
		t.Errorf("force provider ignored, got %s", d.Provider)
	}
}

func TestEmptyModelUsesOtherProvider(t *testing.T) {
	cfg := config.Default()
	rc := cfg.Roles[event.RoleWorker]
	rc.Codex.Model = ""
	cfg.Roles[event.RoleWorker] = rc
	r := &Router{Cfg: func() *config.Config { return cfg }}
	if d := r.Route(Step{Kind: KindEdit}); d.Provider != event.Claude || d.Fallback {
		t.Errorf("got %+v", d)
	}
}

func TestDisabledProvider(t *testing.T) {
	cfg := config.Default()
	pc := cfg.Providers[event.Codex]
	pc.Disabled = true
	cfg.Providers[event.Codex] = pc
	r := &Router{Cfg: func() *config.Config { return cfg }}
	if d := r.Route(Step{Kind: KindPlan}); d.Provider != event.Claude {
		t.Errorf("disabled codex still used: %+v", d)
	}
}

func TestJudge(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Judge = true
	r := &Router{Cfg: func() *config.Config { return cfg }}
	d := r.Route(Step{Kind: KindEdit, Prompt: "the thing"})
	if !r.NeedsJudge(d) {
		t.Fatalf("low-confidence default should ask the judge: %+v", d)
	}
	role, ok := ParseJudge(" C) worker-high")
	if !ok || role != event.RoleWorkerHigh {
		t.Errorf("ParseJudge = %s %v", role, ok)
	}
	d = r.Route(Step{Kind: KindEdit, ForceRole: role})
	if d.Role != event.RoleWorkerHigh || d.Rule != RuleJudge {
		t.Errorf("forced role ignored: %+v", d)
	}
	if _, ok := ParseJudge("maybe"); ok {
		t.Error("garbage parsed")
	}
}

func TestPlannerPreferOtherDoesNotRecurse(t *testing.T) {
	cfg := config.Default()
	for _, role := range event.Roles {
		rc := cfg.Roles[role]
		rc.Prefer = config.PreferOther
		cfg.Roles[role] = rc
	}
	r := &Router{Cfg: func() *config.Config { return cfg }}
	if d := r.Preview(event.RolePlanner, ""); d.Provider != event.Claude {
		t.Errorf("planner prefer other without a main provider: %+v", d)
	}
	if d := r.Route(Step{Kind: KindReview}); d.Provider == "" {
		t.Error("no provider")
	}
}
