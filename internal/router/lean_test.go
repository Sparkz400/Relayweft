package router

import (
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/event"
)

func TestLean(t *testing.T) {
	r := newRouter(state{})
	edit := Step{Kind: KindEdit, Prompt: "add a flag"}
	review := Step{Kind: KindReview, MainProvider: event.Claude}
	explore := Step{Kind: KindExplore, Prompt: "where is the parser"}

	// The day plan leans on claude: the worker (prefer: codex) moves, the
	// explorer was there already, the review still goes to the other one.
	r.SetLean(Lean{Provider: event.Claude})
	if d := r.Route(edit); d.Provider != event.Claude || d.Role != event.RoleWorker || d.Model == "" || !strings.Contains(d.Reason, "day plan leans on claude") {
		t.Fatalf("worker = %+v", d)
	}
	if d := r.Route(explore); d.Provider != event.Claude {
		t.Fatalf("explorer = %+v", d)
	}
	if d := r.Route(review); d.Provider != event.Codex {
		t.Fatalf("review should stay on the other provider: %+v", d)
	}
	// Strict: codex must be spared, so the review stays on claude too.
	r.SetLean(Lean{Provider: event.Claude, Strict: true})
	if d := r.Route(review); d.Provider != event.Claude {
		t.Fatalf("strict review = %+v", d)
	}
	// Cleared: the config's preferences again.
	r.SetLean(Lean{})
	if d := r.Route(edit); d.Provider != event.Codex || strings.Contains(d.Reason, "day plan") {
		t.Fatalf("after clear = %+v", d)
	}
}

func TestLeanGivesWayToLimitsPinsAndForce(t *testing.T) {
	edit := Step{Kind: KindEdit, Prompt: "add a flag"}
	// At its limit: work moves on as always, without the lean note.
	r := newRouter(state{limited: map[string]bool{event.Claude: true}})
	r.SetLean(Lean{Provider: event.Claude, Strict: true})
	if d := r.Route(edit); d.Provider != event.Codex || !d.Fallback || strings.Contains(d.Reason, "day plan") {
		t.Fatalf("limited lean = %+v", d)
	}
	// Near its limit: switch_at_utilization still moves it.
	r = newRouter(state{util: map[string]float64{event.Claude: 0.95, event.Codex: 0.1}})
	r.SetLean(Lean{Provider: event.Claude})
	if d := r.Route(edit); d.Provider != event.Codex || d.Rule != RuleQuota {
		t.Fatalf("near-limit lean = %+v", d)
	}
	// A role set explicitly keeps its provider.
	r = newRouter(state{})
	r.Pinned = func(role string) bool { return role == event.RoleWorker }
	r.SetLean(Lean{Provider: event.Claude})
	if d := r.Route(edit); d.Provider != event.Codex {
		t.Fatalf("pinned worker = %+v", d)
	}
	// --provider wins.
	r = newRouter(state{})
	r.ForceProvider = event.Codex
	r.SetLean(Lean{Provider: event.Claude, Strict: true})
	if d := r.Route(Step{Kind: KindExplore, Prompt: "where is x"}); d.Provider != event.Codex {
		t.Fatalf("forced = %+v", d)
	}
	// An unknown provider is ignored.
	r = newRouter(state{})
	r.SetLean(Lean{Provider: "nope"})
	if d := r.Route(edit); d.Provider != event.Codex {
		t.Fatalf("unknown lean = %+v", d)
	}
}
