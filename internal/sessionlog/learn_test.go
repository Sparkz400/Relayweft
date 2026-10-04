package sessionlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
)

// repoRuns makes n agent_end records of a role on a route in repo dir cwd at
// ts, the first fails of them failed, each using tok fresh tokens.
func repoRuns(cwd string, ts time.Time, role string, k RouteKey, n, fails int, tok int64) []Record {
	var out []Record
	for i := 0; i < n; i++ {
		out = append(out, Record{Type: TypeAgentEnd, TS: ts, Session: "s", Cwd: cwd, TaskID: fmt.Sprintf("%s-%s-%d", role, k, i),
			Step: "s1", Kind: "edit", Attempt: 1, Role: role, Provider: k.Provider, Model: k.Model, Effort: k.Effort,
			OK: Bool(i >= fails), Tokens: &event.TokenUsage{Input: tok}, DurationMS: 60_000})
	}
	return out
}

var (
	sol    = RouteKey{event.Codex, "gpt-6.1-sol", "medium"}
	sonnet = RouteKey{event.Claude, "sonnet", "medium"}
	opus   = RouteKey{event.Claude, "opus", "high"}
)

// learnIn is an update of the worker in repo, configured on sol.
func learnIn(repo string, now time.Time) LearnInput {
	return LearnInput{Root: repo, Configured: map[string]RouteKey{event.RoleWorker: sol}, MinSamples: 8, Now: now}
}

func onlyChange(t *testing.T, res LearnResult) RouteChange {
	t.Helper()
	if len(res.Changes) != 1 {
		t.Fatalf("changes = %+v, want one", res.Changes)
	}
	return res.Changes[0]
}

// A best-of candidate that lost on checks or by the reviewer counts as a
// failed run of its route; a pick by the fixed order does not.
func TestLearnBestOfLossCounts(t *testing.T) {
	repo := t.TempDir()
	cur := repoRuns(repo, t0, event.RoleWorker, sol, 10, 0, 50_000)
	alt := repoRuns(repo, t0, event.RoleWorker, sonnet, 10, 0, 50_000)
	recs := append(cur, alt...)
	for i, r := range cur {
		recs[i].Agent = "w--codex"
		by := BestOfByReviewer
		if i >= 6 {
			by = BestOfByOrder
		}
		recs = append(recs, Record{Type: TypeBestOf, TS: t0, Session: r.Session, Cwd: repo, TaskID: r.TaskID, Step: r.Step,
			Agent: "w--codex", Role: r.Role, Provider: r.Provider, Model: r.Model, Effort: r.Effort, OK: Bool(false), Reason: by})
	}
	st := learnStats(recs, repo, t0)
	if a := st[event.RoleWorker][sol]; a == nil || a.n != 10 || int(a.okW+0.5) != 4 {
		t.Fatalf("sol after 6 losses on merit: %+v", a)
	}
	if a := st[event.RoleWorker][sonnet]; a.okW < 9.9 {
		t.Errorf("sonnet runs were touched: %+v", a)
	}
	res := Learn(recs, learnIn(repo, t0))
	if ch := onlyChange(t, res); ch.To != sonnet {
		t.Errorf("change %+v, want the route that kept winning", ch)
	}
}

func TestLearnNeedsMinSamples(t *testing.T) {
	repo := t.TempDir()
	cur := repoRuns(repo, t0, event.RoleWorker, sol, 20, 10, 50_000) // 50% ok
	few := repoRuns(repo, t0, event.RoleWorker, sonnet, 7, 0, 50_000)
	if res := Learn(append(cur, few...), learnIn(repo, t0)); len(res.Changes) != 0 {
		t.Fatalf("7 runs < 8 learned a route: %+v", res.Changes)
	}
	enough := repoRuns(repo, t0, event.RoleWorker, sonnet, 8, 0, 50_000)
	c := onlyChange(t, Learn(append(cur, enough...), learnIn(repo, t0)))
	if c.Role != event.RoleWorker || c.From != sol || c.To != sonnet || c.Remove || !strings.Contains(c.Why, "100% of 8 runs vs 50% of 20") {
		t.Fatalf("change = %+v", c)
	}
	// The route it would replace needs the samples too.
	in := learnIn(repo, t0)
	if res := Learn(append(repoRuns(repo, t0, event.RoleWorker, sol, 5, 4, 50_000), enough...), in); len(res.Changes) != 0 {
		t.Fatalf("learned against a current route with 5 runs: %+v", res.Changes)
	}
	// routing.learn_min_samples raises the bar.
	in.MinSamples = 12
	if res := Learn(append(cur, enough...), in); len(res.Changes) != 0 {
		t.Fatalf("min 12: %+v", res.Changes)
	}
}

func TestLearnNeedsClearMargin(t *testing.T) {
	repo := t.TempDir()
	cur := repoRuns(repo, t0, event.RoleWorker, sol, 10, 3, 50_000) // 70%
	near := repoRuns(repo, t0, event.RoleWorker, sonnet, 10, 2, 50_000)
	if res := Learn(append(cur, near...), learnIn(repo, t0)); len(res.Changes) != 0 {
		t.Fatalf("80%% vs 70%% is not a clear margin: %+v", res.Changes)
	}
	clear := repoRuns(repo, t0, event.RoleWorker, sonnet, 10, 1, 50_000)
	if c := onlyChange(t, Learn(append(cur, clear...), learnIn(repo, t0))); c.To != sonnet {
		t.Fatalf("90%% vs 70%%: %+v", c)
	}
	// As reliable and much cheaper also counts; slightly cheaper does not.
	ok := repoRuns(repo, t0, event.RoleWorker, sol, 10, 1, 50_000)
	if res := Learn(append(ok, repoRuns(repo, t0, event.RoleWorker, sonnet, 10, 1, 40_000)...), learnIn(repo, t0)); len(res.Changes) != 0 {
		t.Fatalf("20%% fewer tokens: %+v", res.Changes)
	}
	c := onlyChange(t, Learn(append(ok, repoRuns(repo, t0, event.RoleWorker, sonnet, 10, 1, 20_000)...), learnIn(repo, t0)))
	if c.To != sonnet || !strings.Contains(c.Why, "60% fewer tokens") {
		t.Fatalf("cheaper: %+v", c)
	}
	if len(c.Evidence) != 2 || c.Evidence[0].Route != sonnet.String() || c.Evidence[0].Tokens != 20_000 || c.Evidence[1].Success != 0.9 {
		t.Fatalf("evidence = %+v", c.Evidence)
	}
}

func TestLearnDecaysOldRuns(t *testing.T) {
	repo := t.TempDir()
	now := t0.Add(120 * 24 * time.Hour) // four half-lives: weight 1/16
	cur := repoRuns(repo, now, event.RoleWorker, sol, 10, 5, 50_000)
	old := repoRuns(repo, t0, event.RoleWorker, sonnet, 40, 0, 50_000) // 40 runs weigh 2.5
	res := Learn(append(cur, old...), learnIn(repo, now))
	if len(res.Changes) != 0 {
		t.Fatalf("old runs learned a route: %+v", res.Changes)
	}
	var ev config.RouteEvidence
	for _, e := range res.Evidence[event.RoleWorker] {
		if e.Route == sonnet.String() {
			ev = e
		}
	}
	if ev.Samples != 40 || ev.Weight != 2.5 {
		t.Fatalf("evidence = %+v", ev)
	}
	// The same runs today count fully.
	if c := onlyChange(t, Learn(append(cur, old...), learnIn(repo, t0))); c.To != sonnet {
		t.Fatalf("fresh runs: %+v", c)
	}
}

func TestLearnNeverPicksAnUnavailableRoute(t *testing.T) {
	repo := t.TempDir()
	recs := repoRuns(repo, t0, event.RoleWorker, sol, 10, 5, 50_000)
	recs = append(recs, repoRuns(repo, t0, event.RoleWorker, opus, 10, 0, 50_000)...)   // best, but gone
	recs = append(recs, repoRuns(repo, t0, event.RoleWorker, sonnet, 10, 1, 50_000)...) // next best
	in := learnIn(repo, t0)
	in.Available = func(k RouteKey) bool { return k != opus }
	if c := onlyChange(t, Learn(recs, in)); c.To != sonnet {
		t.Fatalf("picked %s", c.To)
	}
	in.Available = func(k RouteKey) bool { return k == sol }
	if res := Learn(recs, in); len(res.Changes) != 0 {
		t.Fatalf("nothing available, yet: %+v", res.Changes)
	}
	// A learned route that is no longer usable goes back to the configured
	// one, whatever its numbers.
	in.Learned = map[string]config.LearnedRoute{event.RoleWorker: {Provider: opus.Provider, Model: opus.Model, Effort: opus.Effort}}
	res := Learn(recs, in)
	c := onlyChange(t, res)
	if !c.Remove || c.From != opus || c.To != sol || !strings.Contains(c.Why, "no longer") {
		t.Fatalf("change = %+v", c)
	}
	if _, ok := res.Routes[event.RoleWorker]; ok {
		t.Fatal("the unusable learned route was kept")
	}
}

func TestLearnOnlyThisRepoAndOneChangePerRole(t *testing.T) {
	repo := t.TempDir()
	sub := filepath.Join(repo, "pkg")
	os.Mkdir(sub, 0o755)
	other := t.TempDir()
	recs := repoRuns(repo, t0, event.RoleWorker, sol, 10, 5, 50_000)
	recs = append(recs, repoRuns(other, t0, event.RoleWorker, sonnet, 30, 0, 50_000)...) // another repo
	if res := Learn(recs, learnIn(repo, t0)); len(res.Changes) != 0 {
		t.Fatalf("another repo's runs counted: %+v", res.Changes)
	}
	// Runs in a subfolder are this repo's; two better routes still make
	// one change, to the best.
	recs = append(recs, repoRuns(sub, t0, event.RoleWorker, sonnet, 10, 1, 50_000)...)
	recs = append(recs, repoRuns(sub, t0, event.RoleWorker, opus, 10, 0, 50_000)...)
	recs = append(recs, repoRuns(repo, t0, event.RoleExplorer, sonnet, 10, 0, 9_000)...)
	recs = append(recs, repoRuns(repo, t0, event.RoleExplorer, RouteKey{event.Claude, "haiku", ""}, 10, 0, 3_000)...)
	in := learnIn(repo, t0)
	in.Configured[event.RoleExplorer] = sonnet
	res := Learn(recs, in)
	if len(res.Changes) != 2 || res.Changes[0].Role != event.RoleWorker || res.Changes[0].To != opus || res.Changes[1].Role != event.RoleExplorer {
		t.Fatalf("changes = %+v", res.Changes)
	}
	if lr := res.Routes[event.RoleWorker]; lr.Provider != event.Claude || lr.Model != "opus" || lr.Since != t0 || lr.Why == "" {
		t.Fatalf("learned = %+v", lr)
	}
}

func TestLearnGoesBackToTheConfiguredRoute(t *testing.T) {
	repo := t.TempDir()
	recs := repoRuns(repo, t0, event.RoleWorker, sol, 10, 0, 50_000)
	recs = append(recs, repoRuns(repo, t0, event.RoleWorker, sonnet, 10, 4, 50_000)...)
	in := learnIn(repo, t0)
	in.Learned = map[string]config.LearnedRoute{event.RoleWorker: {Provider: sonnet.Provider, Model: sonnet.Model, Effort: sonnet.Effort}}
	res := Learn(recs, in)
	if c := onlyChange(t, res); !c.Remove || c.To != sol {
		t.Fatalf("change = %+v", c)
	}
	if len(res.Routes) != 0 {
		t.Fatalf("routes = %+v", res.Routes)
	}
}

// A writing step of a bench run whose check failed counts as failed, even
// though its agent finished ok.
func TestLearnCountsFailedBenchChecks(t *testing.T) {
	repo := t.TempDir()
	recs := repoRuns(repo, t0, event.RoleWorker, sol, 10, 5, 50_000)
	good := repoRuns(repo, t0, event.RoleWorker, sonnet, 10, 0, 50_000)
	recs = append(recs, good...)
	if c := onlyChange(t, Learn(recs, learnIn(repo, t0))); c.To != sonnet {
		t.Fatalf("%+v", c)
	}
	for i, r := range good[:5] {
		recs = append(recs, Record{Type: TypeTaskEnd, TS: t0, Session: "s", TaskID: r.TaskID, Bench: fmt.Sprint("b", i)},
			Record{Type: "bench", TS: t0, Session: "s", Bench: fmt.Sprint("b", i), Passed: Bool(false)})
	}
	if res := Learn(recs, learnIn(repo, t0)); len(res.Changes) != 0 {
		t.Fatalf("failed checks ignored: %+v", res.Changes)
	}
}
