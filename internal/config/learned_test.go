package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/event"
)

// sonnetHigh is a learned worker route.
var sonnetHigh = LearnedRoute{Provider: event.Claude, Model: "sonnet", Effort: "high", Why: "succeeded 95% vs 60%"}

func learnedSet(routes map[string]LearnedRoute) *Learned {
	return &Learned{Root: "/r", Updated: time.Now(), Routes: routes}
}

func TestLearnedFileRoundTripAndCanonicalKey(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	l, err := LoadLearned(root)
	if err != nil || len(l.Routes) != 0 || !l.Updated.IsZero() {
		t.Fatalf("missing file: %+v %v", l, err)
	}
	l.Routes = map[string]LearnedRoute{event.RoleWorker: sonnetHigh}
	l.Updated = time.Now()
	if err := SaveLearned(l); err != nil {
		t.Fatal(err)
	}
	// The same repo through a symlink (macOS /var -> /private/var) or in
	// other letter case on Windows shares the file.
	alias := root
	if runtime.GOOS == "windows" {
		alias = strings.ToUpper(root)
	} else {
		alias = filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(root, alias); err != nil {
			t.Fatal(err)
		}
	}
	if LearnedPath(alias) != LearnedPath(root) {
		t.Fatalf("%s and %s have different files", alias, root)
	}
	got, err := LoadLearned(alias)
	if err != nil || got.Routes[event.RoleWorker].Spec() != "claude:sonnet:high" {
		t.Fatalf("loaded %+v %v", got, err)
	}
	if strings.HasPrefix(LearnedPath(root), root) {
		t.Fatal("learned routes must not live in the repo")
	}
	if err := ResetLearned(alias); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadLearned(root); len(got.Routes) != 0 {
		t.Fatalf("after reset: %+v", got)
	}
	if err := ResetLearned(root); err != nil {
		t.Fatalf("second reset: %v", err)
	}
}

// Learned routes sit above your config and below the repo file and flags:
// an explicit setting always wins.
func TestLearnedLayering(t *testing.T) {
	isolateTrust(t)
	def := Default()
	userPath := filepath.Join(t.TempDir(), "user.yaml")
	s := NewStore(Default(), userPath)
	applied := s.ApplyLearned(learnedSet(map[string]LearnedRoute{event.RoleWorker: sonnetHigh,
		event.RoleExplorer: {Provider: event.Codex, Model: "gpt-6-luna", Effort: "low", Why: "cheaper"}}))
	if strings.Join(applied, ",") != "explorer,worker" {
		t.Fatalf("applied = %v", applied)
	}
	c := s.Get()
	if w := c.Roles[event.RoleWorker]; w.Prefer != event.Claude || w.Claude != (Route{"sonnet", "high"}) || w.Codex != def.Roles[event.RoleWorker].Codex {
		t.Fatalf("worker = %+v", w)
	}
	if c.Learned[event.RoleWorker].Why != sonnetHigh.Why {
		t.Fatalf("annotation = %+v", c.Learned)
	}
	// Never saved into your config.
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(userPath)
	saved, _ := Parse(data)
	if saved.Roles[event.RoleWorker] != def.Roles[event.RoleWorker] {
		t.Fatalf("saved worker = %+v", saved.Roles[event.RoleWorker])
	}

	// A flag naming the route it already has still pins it: a later
	// refresh must not move it.
	if err := s.SetPrefer(event.RoleExplorer, event.Codex); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get().Learned[event.RoleExplorer]; ok || !s.Pinned(event.RoleExplorer) {
		t.Fatal("explorer still learned after --prefer")
	}
	s.ApplyLearned(learnedSet(map[string]LearnedRoute{event.RoleExplorer: {Provider: event.Claude, Model: "haiku", Why: "x"}}))
	if e := s.Get().Roles[event.RoleExplorer]; e.Prefer != event.Codex {
		t.Fatalf("refresh moved a pinned role: %+v", e)
	}
	// The worker's learned route is gone with the new set: back to yours.
	if w := s.Get().Roles[event.RoleWorker]; w != def.Roles[event.RoleWorker] {
		t.Fatalf("worker after refresh = %+v", w)
	}
}

func TestRepoFileAndEditsWinOverLearned(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, ".git"), 0o755)
	os.WriteFile(filepath.Join(root, RepoFileName), []byte("roles:\n  worker: {prefer: claude}\n  planner: {prefer: codex}\n"), 0o644)
	def := Default()
	s := NewStore(Default(), filepath.Join(t.TempDir(), "user.yaml"))
	if _, err := s.ApplyRepo(root); err != nil {
		t.Fatal(err)
	}
	set := learnedSet(map[string]LearnedRoute{
		event.RoleWorker:     {Provider: event.Codex, Model: "gpt-6-sol", Effort: "high", Why: "w"},
		event.RolePlanner:    {Provider: event.Claude, Model: "fable", Effort: "max", Why: "p"},
		event.RoleWorkerHigh: {Provider: event.Claude, Model: "opus", Effort: "xhigh", Why: "wh"},
	})
	applied := s.ApplyLearned(set)
	// worker: the repo file changes it. planner: the file only repeats
	// your config (prefer codex), so it does not pin it.
	if strings.Join(applied, ",") != "planner,worker_high" {
		t.Fatalf("applied = %v", applied)
	}
	c := s.Get()
	if w := c.Roles[event.RoleWorker]; w.Prefer != event.Claude || w.Codex != def.Roles[event.RoleWorker].Codex {
		t.Fatalf("worker = %+v", w)
	}
	// Applying the repo file again (or after the learned routes) keeps the
	// same layering.
	if _, err := s.ApplyRepo(root); err != nil {
		t.Fatal(err)
	}
	if c2 := s.Get(); c2.Roles[event.RolePlanner].Prefer != event.Claude || c2.Roles[event.RoleWorker].Prefer != event.Claude {
		t.Fatalf("after re-apply: %+v", c2.Roles)
	}
	// A session edit (the model picker, /route) of a learned role takes
	// your setting with the edit, not the learned route.
	if err := s.Update(func(c *Config) error {
		r := c.Roles[event.RoleWorkerHigh]
		r.Codex.Effort = "high"
		c.Roles[event.RoleWorkerHigh] = r
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wh := s.Get().Roles[event.RoleWorkerHigh]
	if wh.Prefer != def.Roles[event.RoleWorkerHigh].Prefer || wh.Codex.Effort != "high" || wh.Claude != def.Roles[event.RoleWorkerHigh].Claude {
		t.Fatalf("worker_high = %+v", wh)
	}
	if _, ok := s.Get().Learned[event.RoleWorkerHigh]; ok {
		t.Fatal("edited role still marked learned")
	}
	// /save repo writes your routes, never the learned ones.
	p, err := s.SaveRepo(root)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if strings.Contains(string(data), "fable") {
		t.Fatalf("learned planner route in the repo file:\n%s", data)
	}
	rc, _ := Parse(data)
	if rc.Roles[event.RolePlanner].Prefer != event.Codex {
		t.Fatalf("repo file planner = %+v", rc.Roles[event.RolePlanner])
	}
}

func TestLearnedRouteMustBeAvailable(t *testing.T) {
	c := Default()
	if !c.RouteAvailable(event.Claude, Route{"sonnet", "high"}) || !c.RouteAvailable(event.Claude, Route{"haiku", ""}) {
		t.Fatal("catalog routes unavailable")
	}
	for _, r := range []struct {
		prov string
		r    Route
	}{{event.Claude, Route{"no-such-model", ""}}, {event.Claude, Route{"sonnet", "ultra"}}, {"gemini", Route{"x", ""}}} {
		if c.RouteAvailable(r.prov, r.r) {
			t.Errorf("%s %+v available", r.prov, r.r)
		}
	}
	// A model only a role uses is configured too.
	r := c.Roles[event.RoleWorker]
	r.Codex.Model = "my-own-model"
	c.Roles[event.RoleWorker] = r
	if !c.RouteAvailable(event.Codex, Route{"my-own-model", "low"}) {
		t.Fatal("a role's own model is unavailable")
	}
	p := c.Providers[event.Claude]
	p.Disabled = true
	c.Providers[event.Claude] = p
	s := NewStore(c, "")
	if applied := s.ApplyLearned(learnedSet(map[string]LearnedRoute{event.RoleWorker: sonnetHigh})); len(applied) != 0 {
		t.Fatalf("a disabled provider's route applied: %v", applied)
	}
	if s.Get().Roles[event.RoleWorker].Prefer == event.Claude {
		t.Fatal("worker moved to a disabled provider")
	}
}

func TestLearnModeAndClone(t *testing.T) {
	c := Default()
	if c.LearnMode() != LearnSuggest || c.LearnMin() != 8 {
		t.Fatalf("defaults: %q %d", c.LearnMode(), c.LearnMin())
	}
	c.Routing.Learn = ""
	if c.LearnMode() != LearnSuggest {
		t.Fatal(`"" is not suggest`)
	}
	c.Routing.Learn = "always"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "routing.learn") {
		t.Fatalf("validate: %v", err)
	}
	c.Routing.Learn = LearnAuto
	c.Learned = map[string]LearnedRoute{event.RoleWorker: sonnetHigh}
	if cl := c.Clone(); cl.Learned[event.RoleWorker].Why != sonnetHigh.Why || cl.Routing.Learn != LearnAuto {
		t.Fatalf("clone lost the learned routes: %+v", cl.Learned)
	}
}
