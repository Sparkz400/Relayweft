package router

import (
	"strings"
	"testing"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
)

// A decision on a learned route says so in its reason; the same role on
// another route (a limit fallback, an edit) does not.
func TestLearnedRouteInReason(t *testing.T) {
	s := config.NewStore(config.Default(), "")
	s.ApplyLearned(&config.Learned{Routes: map[string]config.LearnedRoute{
		event.RoleWorker: {Provider: event.Claude, Model: "sonnet", Effort: "high", Why: "succeeded 95% of 12 runs vs 60% of 20"},
	}})
	r := &Router{Cfg: s.Get, State: state{}}
	d := r.Route(Step{Kind: KindEdit, Prompt: "add a flag"})
	if d.Label() != "claude:sonnet@high" || d.Rule != RuleDefault ||
		d.Reason != "default worker route; learned route claude:sonnet:high: succeeded 95% of 12 runs vs 60% of 20" {
		t.Fatalf("decision = %+v", d)
	}
	r.State = state{limited: map[string]bool{event.Claude: true}}
	if d := r.Route(Step{Kind: KindEdit, Prompt: "add a flag"}); d.Provider != event.Codex || strings.Contains(d.Reason, "learned") {
		t.Fatalf("fallback = %+v", d)
	}
	r.State = state{}
	if err := s.SetRoute(event.RoleWorker, event.Claude, config.Route{Model: "opus", Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	if d := r.Route(Step{Kind: KindEdit, Prompt: "add a flag"}); strings.Contains(d.Reason, "learned") {
		t.Fatalf("an edited route still says learned: %+v", d)
	}
}
