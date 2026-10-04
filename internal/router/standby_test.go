package router

import (
	"strings"
	"testing"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
)

// A local model standing by takes cheap read-only work once Codex and
// Claude are at, or close to, their limits - and nothing else.
func TestStandbyTakesReadOnlyWorkNearTheLimit(t *testing.T) {
	cfg := withProviders(t, event.Qwen, "ollama-run")
	explore := Step{Kind: KindExplore, Prompt: "find where the config is loaded"}

	// Both have room: as before.
	r := routerFor(cfg, state{util: map[string]float64{event.Codex: 0.5, event.Claude: 0.5}})
	if d := r.Route(explore); d.Provider != event.Claude || d.Fallback {
		t.Fatalf("room left: %+v", d)
	}
	// One is nearly out, the other has room: the other, not the local model.
	r = routerFor(cfg, state{util: map[string]float64{event.Claude: 0.95, event.Codex: 0.4}})
	if d := r.Route(explore); d.Provider != event.Codex || d.Rule != RuleQuota {
		t.Fatalf("codex has room: %+v", d)
	}
	// Both at or over switch_at_utilization (0.9): Qwen Code on Ollama.
	r = routerFor(cfg, state{util: map[string]float64{event.Claude: 0.95, event.Codex: 0.92}})
	d := r.Route(explore)
	if d.Provider != event.Qwen || d.Model != "qwen3.6:35b-a3b-coding" || d.Rule != RuleStandby || !d.Fallback || d.From != event.Claude {
		t.Fatalf("both nearly out: %+v", d)
	}
	if !strings.Contains(d.Reason, "codex at 92% of its limit, no other provider has room -> qwen, standing by for explorer") {
		t.Errorf("reason = %q", d.Reason)
	}
	// Both at their limit: the same.
	r = routerFor(cfg, state{limited: map[string]bool{event.Codex: true, event.Claude: true}})
	if d := r.Route(explore); d.Provider != event.Qwen || d.Rule != RuleStandby {
		t.Fatalf("both limited: %+v", d)
	}
	// Writing work never goes to a standby that does not stand by for it.
	if d := r.Route(Step{Kind: KindEdit, Prompt: "fix the bug"}); d.Provider == event.Qwen || d.Provider == "ollama-run" {
		t.Fatalf("writer on standby: %+v", d)
	}
	// The judge goes to the plain model, which stands by for it.
	if d := r.Route(Step{Kind: KindJudge}); d.Provider != "ollama-run" || d.Rule != RuleStandby {
		t.Fatalf("judge: %+v", d)
	}
	// The planner has no standby.
	if d := r.Route(Step{Kind: KindPlan}); d.Provider == event.Qwen || d.Provider == "ollama-run" {
		t.Fatalf("planner on standby: %+v", d)
	}
	// sy --provider pins everything.
	r.ForceProvider = event.Codex
	if d := r.Route(explore); d.Provider != event.Codex {
		t.Fatalf("forced: %+v", d)
	}
	// A standby that is itself at its limit is skipped.
	r = routerFor(cfg, state{limited: map[string]bool{event.Codex: true, event.Claude: true, event.Qwen: true}})
	if d := r.Route(explore); d.Provider == event.Qwen {
		t.Fatalf("limited standby: %+v", d)
	}
	// Disabled presets never stand by.
	r = routerFor(config.Default(), state{limited: map[string]bool{event.Codex: true, event.Claude: true}})
	if d := r.Route(explore); d.Provider != event.Claude {
		t.Fatalf("disabled standby: %+v", d)
	}
}

// A provider that takes read-only work only is never routed a writing
// step, even when a role prefers it or the judge picked a read-only role.
func TestReadOnlyProviderGetsNoWritingStep(t *testing.T) {
	cfg := withProviders(t, "ollama-run")
	rc := cfg.Roles[event.RoleExplorer].With("ollama-run", config.Route{Model: "m"})
	rc.Prefer = "ollama-run"
	cfg.Roles[event.RoleExplorer] = rc
	r := routerFor(cfg, state{})
	if d := r.Route(Step{Kind: KindExplore, Prompt: "find it"}); d.Provider != "ollama-run" {
		t.Fatalf("read-only step: %+v", d)
	}
	// The judge said "explorer" for an edit step: the step still writes.
	if d := r.Route(Step{Kind: KindEdit, ForceRole: event.RoleExplorer, Prompt: "add a flag"}); d.Provider == "ollama-run" {
		t.Fatalf("writing step on a read-only provider: %+v", d)
	}
	if d := r.Preview(event.RoleExplorer, ""); d.Provider != "ollama-run" {
		t.Fatalf("preview: %+v", d)
	}
}
