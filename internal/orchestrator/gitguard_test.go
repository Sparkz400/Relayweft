package orchestrator

import (
	"testing"
)

// Every git command sy runs where agents write overrides core.fsmonitor and
// submodule recursion (proc.GitGuard): a config in that folder, or in a
// submodule's git folder an agent pointed .git at, must not decide what
// git runs. The value git sees for this command is the guard's.
func TestGitGuardOverridesFolderConfig(t *testing.T) {
	dir := gitRepo(t)
	tgit(t, dir, "config", "core.fsmonitor", "sy-test-not-a-command")
	tgit(t, dir, "config", "submodule.recurse", "true")
	g := git{dir}
	if v, _ := g.out("config", "core.fsmonitor"); v != "false" {
		t.Errorf("core.fsmonitor = %q, want false", v)
	}
	if v, _ := g.out("config", "submodule.recurse"); v != "false" {
		t.Errorf("submodule.recurse = %q, want false", v)
	}
}
