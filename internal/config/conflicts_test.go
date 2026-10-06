package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestConflictsConfig(t *testing.T) {
	c := Default()
	if oc := c.Orchestrator; oc.ConflictMode() != ConflictsAuto || oc.ResolveRounds() != 2 || oc.ResolveRole != "" {
		t.Fatalf("defaults: %+v", oc)
	}
	for _, bad := range []func(*OrchestratorCfg){
		func(o *OrchestratorCfg) { o.Conflicts = "sometimes" },
		func(o *OrchestratorCfg) { o.MaxResolveRounds = -1 },
		func(o *OrchestratorCfg) { o.MaxResolveRounds = MaxResolveRounds + 1 },
		func(o *OrchestratorCfg) { o.ResolveRole = "reviewer" },
	} {
		c := Default()
		bad(&c.Orchestrator)
		if c.Validate() == nil {
			t.Errorf("%+v is valid", c.Orchestrator)
		}
	}
	c.Orchestrator.Conflicts, c.Orchestrator.MaxResolveRounds, c.Orchestrator.ResolveRole = ConflictsAsk, 3, "worker_high"
	if err := c.Validate(); err != nil || c.Orchestrator.ResolveRounds() != 3 {
		t.Errorf("valid settings refused: %v", err)
	}
	c.Orchestrator.Conflicts = ""
	if c.Orchestrator.ConflictMode() != ConflictsAuto {
		t.Error("empty conflicts is not auto")
	}
}

// A resolve agent runs (and pays for) another agent and may write into
// files you edit: an untrusted repo file may make conflicts stricter, not
// looser; trusted, it applies.
func TestRepoFileConflicts(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, ".git"), 0o755)
	repo := filepath.Join(root, RepoFileName)
	apply := func(user *Config) (OrchestratorCfg, RepoInfo) {
		s := NewStore(user, filepath.Join(t.TempDir(), "user.yaml"))
		info, err := s.ApplyRepo(root)
		if err != nil {
			t.Fatal(err)
		}
		return s.Get().Orchestrator, info
	}
	user := Default()
	user.Orchestrator.Conflicts = ConflictsAsk
	for _, loosen := range []string{"conflicts: resolve", "conflicts: auto", "max_resolve_rounds: 4"} {
		os.WriteFile(repo, []byte("orchestrator:\n  "+loosen+"\n"), 0o644)
		oc, info := apply(user.Clone())
		if oc.Conflicts != ConflictsAsk || oc.ResolveRounds() != 2 || !slices.Contains(info.Ignored, "orchestrator.conflicts") {
			t.Errorf("untrusted %s: %s, %d rounds, ignored %v", loosen, oc.Conflicts, oc.ResolveRounds(), info.Ignored)
		}
	}
	os.WriteFile(repo, []byte("orchestrator:\n  conflicts: fail\n  max_resolve_rounds: 1\n"), 0o644)
	if oc, info := apply(user.Clone()); oc.Conflicts != ConflictsFail || oc.ResolveRounds() != 1 || len(info.Ignored) != 0 {
		t.Errorf("stricter: %s, %d rounds, ignored %v", oc.Conflicts, oc.ResolveRounds(), info.Ignored)
	}
	os.WriteFile(repo, []byte("orchestrator:\n  conflicts: resolve\n"), 0o644)
	if err := Trust(repo); err != nil {
		t.Fatal(err)
	}
	if oc, _ := apply(user.Clone()); oc.Conflicts != ConflictsResolve {
		t.Errorf("trusted: %s", oc.Conflicts)
	}
}

// The same for an untrusted ./relayweft.yaml; trusting it applies it, and
// a later change asks again.
func TestUntrustedLocalConfigConflicts(t *testing.T) {
	isolateTrust(t)
	writeUserConfig(t, "orchestrator:\n  conflicts: ask\n")
	t.Chdir(t.TempDir())
	os.WriteFile(FileName, []byte("orchestrator:\n  conflicts: resolve\n"), 0o644)
	c, _, ignored, err := LoadInfo("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Orchestrator.Conflicts != ConflictsAsk || !slices.Contains(ignored, "orchestrator.conflicts") {
		t.Errorf("untrusted: %s, ignored %v", c.Orchestrator.Conflicts, ignored)
	}
	if err := TrustLocal(FileName); err != nil {
		t.Fatal(err)
	}
	if c, _, ignored, _ := LoadInfo(""); c.Orchestrator.Conflicts != ConflictsResolve || len(ignored) != 0 {
		t.Errorf("trusted: %s, ignored %v", c.Orchestrator.Conflicts, ignored)
	}
	os.WriteFile(FileName, []byte("orchestrator:\n  conflicts: resolve\n  max_resolve_rounds: 5\n"), 0o644)
	if c, _, _, _ := LoadInfo(""); c.Orchestrator.Conflicts != ConflictsAsk {
		t.Errorf("a changed conflicts setting kept the trust: %s", c.Orchestrator.Conflicts)
	}
	os.WriteFile(FileName, []byte("orchestrator:\n  conflicts: fail\n"), 0o644)
	if c, _, ignored, _ := LoadInfo(""); c.Orchestrator.Conflicts != ConflictsFail || len(ignored) != 0 {
		t.Errorf("stricter: %s, ignored %v", c.Orchestrator.Conflicts, ignored)
	}
}
