package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/runner"
	"gopkg.in/yaml.v3"
)

func TestParseBenchMode(t *testing.T) {
	sonnet := config.Route{Model: "sonnet", Effort: "medium"}
	for _, tc := range []struct {
		in   string
		want benchMode
	}{
		{"routed", benchMode{name: "routed"}},
		{"routed-nohandoff", benchMode{name: "routed-nohandoff", noHandoff: true}},
		{"single:claude:opus:high", benchMode{name: "single:claude:opus:high", provider: event.Claude, route: config.Route{Model: "opus", Effort: "high"}}},
		{"routed:worker=claude:sonnet:medium", benchMode{name: "routed:worker=claude:sonnet:medium",
			routes: []roleRoute{{event.RoleWorker, event.Claude, sonnet}}}},
		{"routed:worker=claude:sonnet:medium, planner=codex:gpt-6.1-sol", benchMode{name: "routed:worker=claude:sonnet:medium, planner=codex:gpt-6.1-sol",
			routes: []roleRoute{{event.RoleWorker, event.Claude, sonnet}, {event.RolePlanner, event.Codex, config.Route{Model: "gpt-6.1-sol"}}}}},
	} {
		got, err := parseBenchMode(tc.in)
		if err != nil {
			t.Errorf("%s: %v", tc.in, err)
			continue
		}
		if got.name != tc.want.name || got.provider != tc.want.provider || got.route != tc.want.route ||
			got.noHandoff != tc.want.noHandoff || len(got.routes) != len(tc.want.routes) {
			t.Errorf("%s: got %+v, want %+v", tc.in, got, tc.want)
			continue
		}
		for i := range got.routes {
			if got.routes[i] != tc.want.routes[i] {
				t.Errorf("%s: route %d = %+v, want %+v", tc.in, i, got.routes[i], tc.want.routes[i])
			}
		}
	}
	for _, bad := range []string{"fast", "routed:", "routed:worker", "routed:boss=claude:opus", "routed:worker=claude",
		"routed:worker=claude:sonnet,worker=codex:gpt-6.1-sol", "single:claude"} {
		if _, err := parseBenchMode(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// routed-bestof runs every writing step as best of N; the bench's own
// config is unchanged.
func TestBenchModeBestOf(t *testing.T) {
	base := config.NewStore(config.Default(), "")
	m, err := parseBenchMode("routed-bestof")
	if err != nil || !m.bestOf || m.provider != "" {
		t.Fatalf("parse: %+v, %v", m, err)
	}
	st, err := m.store(base)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Get().Routing.BestOf.When; got != config.BestOfAlways {
		t.Errorf("best_of.when = %q", got)
	}
	if got := base.Get().Routing.BestOf.When; got != config.BestOfOff {
		t.Errorf("the bench's config changed: %q", got)
	}
}

// A variant run sets the role like --route does: that provider, that
// route, and no learned route for it; the bench's own config is unchanged.
func TestBenchModeStoreVariant(t *testing.T) {
	cfg := config.Default()
	base := config.NewStore(cfg, "")
	base.ApplyLearned(&config.Learned{Routes: map[string]config.LearnedRoute{
		event.RoleWorker:  {Provider: event.Codex, Model: "gpt-6.1-sol", Effort: "high", Why: "test"},
		event.RolePlanner: {Provider: event.Claude, Model: "opus", Effort: "high", Why: "test"},
	}})
	m, err := parseBenchMode("routed:worker=claude:sonnet:low")
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.store(base)
	if err != nil {
		t.Fatal(err)
	}
	got := st.Get()
	if rc := got.Roles[event.RoleWorker]; rc.Prefer != event.Claude || rc.Claude != (config.Route{Model: "sonnet", Effort: "low"}) {
		t.Errorf("worker = %+v", rc)
	}
	if _, ok := got.Learned[event.RoleWorker]; ok {
		t.Error("the variant role kept its learned route")
	}
	if _, ok := got.Learned[event.RolePlanner]; !ok {
		t.Error("the other roles lost their learned routes")
	}
	if rc := base.Get().Roles[event.RoleWorker]; rc.Prefer != event.Codex || rc.Codex.Effort != "high" {
		t.Errorf("the bench's own config changed: %+v", rc)
	}
	if plain, _ := parseBenchMode("routed"); func() *config.Store { s, _ := plain.store(base); return s }() != base {
		t.Error("routed got a copy of the config")
	}
	if bad := unlearnable(got, []benchMode{m, {name: "x", routes: []roleRoute{{event.RoleWorker, event.Claude, config.Route{Model: "nope"}}}}}); strings.Join(bad, ",") != "worker=claude:nope" {
		t.Errorf("unlearnable = %v", bad)
	}
}

func TestHistoryVariants(t *testing.T) {
	cfg := config.Default()
	if got := strings.Join(historyVariants(cfg), ","); got != "routed:worker=claude:sonnet:medium" {
		t.Errorf("variants = %q", got)
	}
	// With a learned worker on Claude, the variant is the Codex route.
	st := config.NewStore(config.Default(), "")
	st.ApplyLearned(&config.Learned{Routes: map[string]config.LearnedRoute{
		event.RoleWorker: {Provider: event.Claude, Model: "sonnet", Effort: "medium", Why: "test"}}})
	if got := strings.Join(historyVariants(st.Get()), ","); got != "routed:worker=codex:gpt-6.1-sol:medium" {
		t.Errorf("variants with a learned route = %q", got)
	}
	off := config.Default()
	pc := off.Providers[event.Claude]
	pc.Disabled = true
	off.Providers[event.Claude] = pc
	if got := historyVariants(off); len(got) != 0 {
		t.Errorf("variants with Claude off = %v", got)
	}
}

// claudeWrites is the fake pipeline where only a Claude worker gets the task
// right: its writing steps add ok.txt, which the bench check needs.
type claudeWrites struct{ fake *runner.Fake }

func (c claudeWrites) Run(ctx context.Context, s runner.Spec, emit func(event.Event)) runner.Result {
	r := c.fake.Run(ctx, s, emit)
	if c.fake.Provider == event.Claude && !s.ReadOnly && strings.Contains(s.Prompt, runner.MarkerStep) && r.OK() {
		os.WriteFile(filepath.Join(s.Dir, "ok.txt"), []byte("ok\n"), 0o644)
		r.Files = append(r.Files, "ok.txt")
	}
	return r
}

// End to end: a bench with learn: true and a worker variant on Claude, where
// only Claude's work passes the check, leaves this repo with a learned
// worker route on Claude, and decisions in the next task say so.
func TestBenchLearnsRoutes(t *testing.T) {
	isolate(t)
	dir := gitInit(t)
	chdir(t, dir)
	cfgPath := filepath.Join(t.TempDir(), "switchyard.yaml")
	os.WriteFile(cfgPath, []byte("routing:\n  learn: suggest\n  learn_min_samples: 2\n"), 0o644)
	benchRunners = func(*config.Config) runner.Set {
		return runner.Set{
			event.Codex:  claudeWrites{&runner.Fake{Provider: event.Codex}},
			event.Claude: claudeWrites{&runner.Fake{Provider: event.Claude}},
		}
	}
	defer func() { benchRunners = runner.New }()
	bf := benchFile{
		Modes:   []string{"routed", "routed:worker=claude:sonnet:medium"},
		Timeout: config.Duration(2 * time.Minute),
		Learn:   true,
	}
	for _, name := range []string{"a", "b"} {
		bf.Tasks = append(bf.Tasks, benchTask{Name: name, Prompt: "fix the parser " + name, Check: "git hash-object ok.txt"})
	}
	data, _ := yaml.Marshal(bf)
	benchPath := filepath.Join(t.TempDir(), "bench.yaml")
	os.WriteFile(benchPath, data, 0o644)

	// --no-learn leaves the learned routes alone.
	if err := cmdBench([]string{"--config", cfgPath, "--file", benchPath, "--yes", "--no-learn"}); err != nil {
		t.Fatal(err)
	}
	root, _ := orchestrator.LearnedRoot(dir)
	if l, _ := config.LoadLearned(root); !l.Updated.IsZero() {
		t.Fatalf("--no-learn saved learned routes: %+v", l)
	}
	if err := cmdBench([]string{"--config", cfgPath, "--file", benchPath, "--yes"}); err != nil {
		t.Fatal(err)
	}
	reports, _ := filepath.Glob("bench-results-*.md")
	if len(reports) == 0 {
		t.Fatal("no results file")
	}
	b, _ := os.ReadFile(reports[len(reports)-1])
	totals := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) > 1 && strings.Contains(f[1], "/") {
			totals[f[0]] = f[1]
		}
	}
	if totals["routed"] != "0/2" || totals["routed:worker=claude:sonnet:medium"] != "2/2" {
		t.Fatalf("totals %v in:\n%s", totals, b)
	}
	l, err := config.LoadLearned(root)
	if err != nil {
		t.Fatal(err)
	}
	lr, ok := l.Routes[event.RoleWorker]
	if !ok || lr.Spec() != "claude:sonnet:medium" {
		t.Fatalf("learned routes: %+v", l.Routes)
	}
	if !strings.Contains(lr.Why, "succeeded 100%") || !strings.Contains(lr.Why, "codex:gpt-6.1-sol:medium") {
		t.Errorf("why = %q", lr.Why)
	}
	// The next task's setup layers it in.
	var c common
	c.configPath = cfgPath
	store, _, err := c.setup()
	if err != nil {
		t.Fatal(err)
	}
	if rc := store.Get().Roles[event.RoleWorker]; rc.Prefer != event.Claude || rc.Claude.Model != "sonnet" {
		t.Errorf("worker after the bench = %+v", rc)
	}

	// routing.learn: off: the bench does not touch them.
	config.ResetLearned(root)
	os.WriteFile(cfgPath, []byte("routing:\n  learn: \"off\"\n  learn_min_samples: 2\n"), 0o644)
	if err := cmdBench([]string{"--config", cfgPath, "--file", benchPath, "--yes"}); err != nil {
		t.Fatal(err)
	}
	if l, _ := config.LoadLearned(root); !l.Updated.IsZero() {
		t.Errorf("routing.learn: off saved learned routes: %+v", l)
	}
}

// --from-history files learn and carry the worker variant.
func TestHistoryBenchFileLearns(t *testing.T) {
	h := historyCandidate{HistoryCommit: orchestrator.HistoryCommit{SHA: "abcdef1234", Parent: "1234abcdef"},
		tests: []string{"a_test.go"}, prompt: "Add the thing to the parser"}
	data, err := historyBenchFile([]historyCandidate{h}, historyOpts{out: "bench-history.yaml", check: "go test ./..."}, historyVariants(config.Default()))
	if err != nil {
		t.Fatal(err)
	}
	var bf benchFile
	if err := yaml.Unmarshal(data, &bf); err != nil {
		t.Fatal(err)
	}
	if !bf.Learn || strings.Join(bf.Modes, ",") != "routed,routed:worker=claude:sonnet:medium,single:codex:gpt-6.1-sol:high,single:claude:opus:high" {
		t.Errorf("learn %v modes %v", bf.Learn, bf.Modes)
	}
	for _, m := range bf.Modes {
		if _, err := parseBenchMode(m); err != nil {
			t.Error(err)
		}
	}
}
