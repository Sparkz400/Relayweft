package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sparkz400/switchyard/internal/event"
)

func TestDefaultIsValid(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, r := range event.Roles {
		if c.Roles[r].Codex.Model == "" || c.Roles[r].Claude.Model == "" {
			t.Errorf("role %s should have a default on both providers", r)
		}
	}
	if c.Providers[event.Claude].LimitCooldown.D().Hours() != 1 {
		t.Errorf("cooldown = %v", c.Providers[event.Claude].LimitCooldown)
	}
}

func TestRepoConfigMatchesEmbeddedDefault(t *testing.T) {
	root, err := os.ReadFile(filepath.Join("..", "..", FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(root, DefaultYAML()) {
		t.Fatal("switchyard.yaml at the repo root differs from internal/config/default.yaml; copy one over the other")
	}
}

func TestPartialUserConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sy.yaml")
	os.WriteFile(p, []byte(`
roles:
  worker:
    prefer: claude
    codex: {model: gpt-6-luna, effort: low}
    claude: {model: sonnet, effort: high}
providers:
  codex:
    command: C:\tools\codex.cmd
orchestrator:
  max_threads: 5
  parallel: false
`), 0o644)
	c, path, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if path != p {
		t.Errorf("path = %s", path)
	}
	if c.Roles["worker"].Prefer != "claude" || c.Roles["worker"].Claude.Effort != "high" {
		t.Errorf("worker = %+v", c.Roles["worker"])
	}
	if c.Roles["planner"].Codex.Model != "gpt-6.1-sol" {
		t.Errorf("planner default lost: %+v", c.Roles["planner"])
	}
	cx := c.Providers["codex"]
	if cx.Command != `C:\tools\codex.cmd` || len(cx.Models) == 0 || len(cx.Efforts) == 0 || cx.LimitCooldown == 0 {
		t.Errorf("codex provider not filled from defaults: %+v", cx)
	}
	if c.Orchestrator.MaxThreads != 5 || c.Orchestrator.Parallel {
		t.Errorf("orchestrator = %+v", c.Orchestrator)
	}
}

func TestInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.yaml")
	os.WriteFile(p, []byte("roles:\n  worker:\n    prefer: sometimes\n    codex: {model: x}\n"), 0o644)
	if _, _, err := Load(p); err == nil {
		t.Fatal("want validation error for prefer: sometimes")
	}
}

func TestBestOfConfig(t *testing.T) {
	c := Default()
	if c.Routing.BestOf.On() || c.Routing.BestOf.Count() != 2 {
		t.Fatalf("best_of is not off by default: %+v", c.Routing.BestOf)
	}
	for _, bad := range []BestOfCfg{{When: "sometimes"}, {N: 1}, {N: MaxBestOf + 1}, {Routes: []string{":opus"}}} {
		c := Default()
		c.Routing.BestOf = bad
		if c.Validate() == nil {
			t.Errorf("%+v is valid", bad)
		}
	}
	c.Routing.BestOf = BestOfCfg{When: BestOfHard, N: 3, Routes: []string{"claude", "codex:gpt-6.1-sol:high"}}
	if err := c.Validate(); err != nil || !c.Routing.BestOf.On() || c.Routing.BestOf.Count() != 3 {
		t.Errorf("valid best_of refused: %v", err)
	}
}

// best_of in a repo file layers over your config like other routing
// settings (it sets only what it names), but an untrusted file may only
// lower it: more candidates cost more.
func TestRepoFileBestOf(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, ".git"), 0o755)
	repo := filepath.Join(root, RepoFileName)
	apply := func(user *Config) (BestOfCfg, RepoInfo) {
		s := NewStore(user, filepath.Join(t.TempDir(), "user.yaml"))
		info, err := s.ApplyRepo(root)
		if err != nil {
			t.Fatal(err)
		}
		return s.Get().Routing.BestOf, info
	}
	user := Default()
	user.Routing.BestOf.N = 3
	for _, raise := range []string{"{when: hard}", "{n: 4}", "{routes: [claude, \"claude:opus:high\"]}", "{routes: []}"} {
		os.WriteFile(repo, []byte("routing:\n  best_of: "+raise+"\n"), 0o644)
		u := user.Clone()
		u.Routing.BestOf.When = map[bool]string{true: BestOfOff, false: BestOfHard}[raise == "{when: hard}"]
		if raise == "{routes: []}" {
			u.Routing.BestOf.Routes = []string{"codex", "codex:gpt-6.1-sol:low"} // emptying means the default candidates
		}
		bo, info := apply(u)
		if !reflect.DeepEqual(bo, u.Routing.BestOf) || !contains(info.Ignored, "routing.best_of") {
			t.Errorf("untrusted %s: best_of = %+v, ignored %v", raise, bo, info.Ignored)
		}
	}
	os.WriteFile(repo, []byte("routing:\n  best_of: {when: hard}\n"), 0o644)
	if err := Trust(repo); err != nil {
		t.Fatal(err)
	}
	if bo, _ := apply(user.Clone()); bo.When != BestOfHard || bo.N != 3 {
		t.Errorf("trusted: best_of = %+v, want when from the repo file and n from yours", bo)
	}
	// Lowering needs no trust.
	os.WriteFile(repo, []byte("routing:\n  best_of: {when: off}\n"), 0o644)
	u := user.Clone()
	u.Routing.BestOf.When = BestOfAlways
	if bo, info := apply(u); bo.When != BestOfOff || len(info.Ignored) != 0 {
		t.Errorf("lowered: best_of = %+v, ignored %v", bo, info.Ignored)
	}
}

func TestStoreEditAndSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "switchyard.yaml")
	s := NewStore(Default(), path)
	if err := s.SetRoute("reviewer", event.Claude, Route{Model: "fable", Effort: "max"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPrefer("explorer", "auto"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPrefer("explorer", "nope"); err == nil {
		t.Fatal("invalid prefer accepted")
	}
	if err := s.SetRoute("nobody", event.Codex, Route{Model: "x"}); err == nil {
		t.Fatal("unknown role accepted")
	}
	// Removing every model of a role must be rejected and rolled back.
	for _, p := range s.Get().ProviderNames() {
		if p != event.Claude {
			if err := s.SetRoute("judge", p, Route{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.SetRoute("judge", event.Claude, Route{}); err == nil {
		t.Fatal("role without any model accepted")
	}
	if s.Get().Roles["judge"].Claude.Model == "" {
		t.Fatal("failed update was not rolled back")
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	c, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if r := c.Roles["reviewer"].Claude; r.Model != "fable" || r.Effort != "max" {
		t.Errorf("saved reviewer = %+v", r)
	}
	if c.Roles["explorer"].Prefer != "auto" {
		t.Errorf("saved prefer = %s", c.Roles["explorer"].Prefer)
	}
}

func TestParseRouteSpec(t *testing.T) {
	p, r, err := ParseRouteSpec("claude:opus:high")
	if err != nil || p != "claude" || r.Model != "opus" || r.Effort != "high" {
		t.Errorf("got %s %+v %v", p, r, err)
	}
	p, r, err = ParseRouteSpec("codex:gpt-6.1-sol")
	if err != nil || p != "codex" || r.Effort != "" {
		t.Errorf("got %s %+v %v", p, r, err)
	}
	for _, bad := range []string{"opus", "Gem ini:x", "claude:", "9x:m"} {
		if _, _, err := ParseRouteSpec(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
