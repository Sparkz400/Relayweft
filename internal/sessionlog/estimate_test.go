package sessionlog

import (
	"math"
	"testing"

	"github.com/sparkz400/relayweft/internal/event"
)

// timedRuns makes agent_end records of a role/kind on a route in cwd, one
// per token count; each takes tok/1000 seconds and costs tok/1e6 dollars.
func timedRuns(cwd, role, kind string, k RouteKey, toks ...int64) []Record {
	var out []Record
	for i, tok := range toks {
		out = append(out, Record{Type: TypeAgentEnd, TS: t0, Session: "s", Cwd: cwd, TaskID: "t", Step: string(rune('a' + i)), Kind: kind,
			Attempt: 1, Role: role, Provider: k.Provider, Model: k.Model, Effort: k.Effort, OK: Bool(true),
			Tokens: &event.TokenUsage{Input: tok, CostUSD: float64(tok) / 1e6}, DurationMS: tok})
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestEstimateAndLearningIgnorePreflightAndIncompleteUsage(t *testing.T) {
	recs := timedRuns(t.TempDir(), event.RoleWorker, "edit", sonnet, 1, 2, 3, 4, 5, 6)
	for i := range recs {
		if i < 3 {
			recs[i].Kind = "preflight"
		} else {
			recs[i].Tokens.Incomplete = true
		}
		if routedStep(recs[i]) {
			t.Fatal("probe/partial usage became learning evidence")
		}
	}
	e := NewHistory(recs, "").Estimate(event.RoleWorker, "edit", sonnet, false)
	if e.Samples != 0 || e.Source != SourceNone || e.Tokens.Mid != defaultWriteTokens {
		t.Fatalf("probe/partial usage cheapened estimates: %+v", e)
	}
}

func TestEstimatePercentiles(t *testing.T) {
	repo := t.TempDir()
	h := NewHistory(timedRuns(repo, event.RoleWorker, "edit", sonnet, 50_000, 10_000, 40_000, 20_000, 30_000), repo)
	e := h.Estimate(event.RoleWorker, "edit", sonnet, false)
	if e.Source != SourceRepo || e.Samples != 5 {
		t.Fatalf("estimate = %+v", e)
	}
	if e.Tokens != (Spread{20_000, 30_000, 40_000}) || e.Seconds != (Spread{20, 30, 40}) || !near(e.USD.Mid, 0.03) {
		t.Fatalf("estimate = %+v", e)
	}
	// Interpolated between samples.
	if s := spread([]float64{10, 20}); s != (Spread{12.5, 15, 17.5}) {
		t.Fatalf("spread = %+v", s)
	}
	if s := spread([]float64{7}); s != (Spread{7, 7, 7}) {
		t.Fatalf("one sample: %+v", s)
	}
}

func TestEstimateFallsBackToAllReposThenDefaults(t *testing.T) {
	repo, other := t.TempDir(), t.TempDir()
	recs := timedRuns(repo, event.RoleWorker, "edit", sonnet, 1_000, 1_000)                         // 2 here: too few
	recs = append(recs, timedRuns(other, event.RoleWorker, "edit", sonnet, 9_000, 9_000, 9_000)...) // 3 elsewhere
	recs = append(recs, timedRuns(repo, event.RoleWorker, "fix", sol, 5_000, 5_000, 5_000)...)      // another kind
	h := NewHistory(recs, repo)
	if e := h.Estimate(event.RoleWorker, "edit", sonnet, false); e.Source != SourceAll || e.Samples != 5 || e.Tokens.Mid != 9_000 {
		t.Fatalf("all repos: %+v", e)
	}
	// Runs of another kind (or role, or route) are not this step's.
	e := h.Estimate(event.RoleWorker, "edit", sol, false)
	if e.Source != SourceNone || e.Samples != 0 || e.Tokens.Mid != defaultWriteTokens || e.USD.High != 0 {
		t.Fatalf("no history (codex: no $): %+v", e)
	}
	// Defaults: read-only steps are smaller; Claude's price comes from its
	// runs ($1 per million here).
	e = h.Estimate(event.RoleExplorer, "explore", RouteKey{event.Claude, "haiku", ""}, true)
	if e.Source != SourceNone || e.Tokens != (Spread{10_000, 20_000, 40_000}) || !near(e.USD.Mid, 0.02) {
		t.Fatalf("default read-only: %+v", e)
	}
	// Records logged before kinds were recorded count for every kind.
	legacy := timedRuns(repo, event.RoleWorker, "", sol, 7_000, 7_000, 7_000)
	if e := NewHistory(legacy, repo).Estimate(event.RoleWorker, "fix", sol, false); e.Source != SourceRepo || e.Tokens.Mid != 7_000 {
		t.Fatalf("legacy records: %+v", e)
	}
}

func TestEstimateWithoutHistory(t *testing.T) {
	for _, h := range []*History{nil, NewHistory(nil, "")} {
		e := h.Estimate(event.RoleWorker, "edit", sonnet, false)
		if e.Source != SourceNone || e.Tokens.Mid != defaultWriteTokens || e.Seconds.Mid != defaultWriteWall.Seconds() ||
			!near(e.USD.Mid, defaultWriteTokens*defaultClaudeUSDPerToken) || e.Tokens.Low >= e.Tokens.Mid || e.Tokens.High <= e.Tokens.Mid {
			t.Fatalf("estimate = %+v", e)
		}
	}
	// Single-agent baselines, follow-ups and limit hits are not steps.
	repo := t.TempDir()
	recs := timedRuns(repo, event.RoleWorker, "edit", sonnet, 1, 2, 3)
	recs[0].Step, recs[1].Step, recs[2].LimitHit = "single", "followup", true
	if e := NewHistory(recs, repo).Estimate(event.RoleWorker, "edit", sonnet, false); e.Source != SourceNone {
		t.Fatalf("non-steps counted: %+v", e)
	}
}
