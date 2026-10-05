package orchestrator

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/sandbox"
)

// rw's verify commands follow the top-level sandbox: with it on and no
// runtime they fail with what to do, and never run on this machine.
func TestCheckCmdSandbox(t *testing.T) {
	cfg := config.Default()
	cmd, done, err := checkCmd(context.Background(), cfg, t.TempDir(), "exit 0")
	if err != nil || cmd == nil || done(nil, nil) != "" {
		t.Fatalf("sandbox off: %v", err)
	}
	if err := cmd.Run(); err != nil {
		t.Errorf("the shell command failed: %v", err)
	}

	old := sandbox.LookPath
	sandbox.LookPath = func(string) (string, error) { return "", os.ErrNotExist }
	defer func() { sandbox.LookPath = old }()
	cfg.Sandbox = config.SandboxCfg{Mode: config.SandboxDocker}
	if _, _, err := checkCmd(context.Background(), cfg, t.TempDir(), "exit 0"); err == nil || !strings.Contains(err.Error(), "docker is not installed") {
		t.Errorf("sandbox on without docker: %v", err)
	}

	// Only one provider's agents in the sandbox: the code they write still
	// never runs here (verify, hooks after a merge, bench checks).
	cfg = config.Default()
	pc := cfg.Providers["codex"]
	pc.Sandbox = &config.SandboxCfg{Mode: config.SandboxPodman}
	cfg.Providers["codex"] = pc
	if _, _, err := checkCmd(context.Background(), cfg, t.TempDir(), "exit 0"); err == nil || !strings.Contains(err.Error(), "podman is not installed") {
		t.Errorf("verify with a sandboxed writer: %v", err)
	}
	if ok, out := RunCheck(context.Background(), cfg, t.TempDir(), "exit 0"); ok || !strings.Contains(out, "podman is not installed") {
		t.Errorf("bench check with a sandboxed writer: %v %s", ok, out)
	}
}

// With the sandbox on, the full and the narrowed verify runs both go into
// the container, and so do the tools that pick the narrowed tests: here,
// without docker, every run fails with the sandbox's message and nothing
// runs on this machine.
func TestVerifyRunsInSandbox(t *testing.T) {
	old := sandbox.LookPath
	sandbox.LookPath = func(string) (string, error) { return "", os.ErrNotExist }
	defer func() { sandbox.LookPath = old }()
	dir := gitRepo(t)
	cfg := config.Default()
	cfg.Sandbox = config.SandboxCfg{Mode: config.SandboxDocker}
	o := New(Options{Dir: dir, Store: config.NewStore(cfg, ""), Events: make(chan event.Event, 100)})
	tk := &task{id: "t", cfg: cfg}
	vc := config.VerifyCfg{Commands: []string{"go test ./..."}}
	for _, scope := range []verifyScope{verifyFull, verifyAffected} {
		ok, rep, _ := o.verifyAt(context.Background(), tk, vc, checkSite{dir: dir, root: dir, base: headOf(t, dir)}, scope)
		if ok || !strings.Contains(rep, "docker is not installed") {
			t.Errorf("scope %d: ok=%v report=%q", scope, ok, rep)
		}
	}
	if selectExec(config.Default()) != nil {
		t.Error("selection leaves this machine without a sandbox")
	}
	if _, err := selectExec(cfg)(context.Background(), dir, []string{"go", "list"}); err == nil || !strings.Contains(err.Error(), "docker is not installed") {
		t.Errorf("selection: %v", err)
	}
}

// after_merge and after_task hooks run on code agents wrote: in the
// sandbox when agents write in one. before_task runs before any agent and
// stays here.
func TestHooksInSandbox(t *testing.T) {
	old := sandbox.LookPath
	sandbox.LookPath = func(string) (string, error) { return "", os.ErrNotExist }
	defer func() { sandbox.LookPath = old }()
	dir := gitRepo(t)
	cfg := config.Default()
	cfg.Sandbox = config.SandboxCfg{Mode: config.SandboxDocker}
	cfg.Hooks.AfterMerge = []string{"exit 0"}
	cfg.Hooks.BeforeTask = []string{"exit 0"}
	o := New(Options{Dir: dir, Store: config.NewStore(cfg, ""), Events: make(chan event.Event, 100)})
	tk := &task{id: "t", cfg: cfg}
	if err := o.runHooks(context.Background(), tk, "after_merge", cfg.Hooks.AfterMerge, nil); err == nil || !strings.Contains(err.Error(), "docker is not installed") {
		t.Errorf("after_merge ran outside the sandbox: %v", err)
	}
	if err := o.runHooks(context.Background(), tk, "before_task", cfg.Hooks.BeforeTask, nil); err != nil {
		t.Errorf("before_task: %v", err)
	}
}
