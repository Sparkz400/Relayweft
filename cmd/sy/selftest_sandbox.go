package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/sandbox"
	"gopkg.in/yaml.v3"
)

// sandboxTask is the scripted task of the sandbox scenario (the image's
// stand-in agent plans it: two files in parallel pool worktrees, then a
// third that combines them).
const sandboxTask = "Switchyard sandbox self-test task: create two small files in parallel, then combine them into a third file"

// sandboxScenario runs tasks with the agent in a container
// (docs/sandbox.md): a task whose writers run in pool worktrees, with
// sy's verify command in the container too; a step stopped by its
// timeout; and an sy killed hard while its agent works. The stand-in
// agent in the test image checks the sandbox from the inside. It needs
// docker (or podman) with Linux containers and is skipped without.
func (t *selftest) sandboxScenario() {
	rt, bin, why := "docker", "", ""
	if bin, why = sandbox.Usable("docker"); bin == "" {
		if b, _ := sandbox.Usable("podman"); b != "" {
			rt, bin = "podman", b
		}
	}
	if bin == "" {
		t.check(markSkip, "sandbox", "%s (docker or podman with Linux containers is needed)", why)
		return
	}
	began := time.Now()
	image, err := sandbox.BuildSelftestImage(bin)
	if err != nil {
		t.check(markWarn, "sandbox", "%s works, but the test image could not be built (no network for alpine?): %v", rt, err)
		return
	}
	t.check(markOK, "sandbox", "%s runs Linux containers; test image %s ready in %s", rt, image, time.Since(began).Round(100*time.Millisecond))

	proj := filepath.Join(t.work, "projects", "sandbox project")
	if err := t.makeRepo(proj, 20); err != nil {
		t.check(markFail, "sandbox", "create the test repo: %v", err)
		return
	}
	// A remote whose URL holds a token: the sandbox must not see it.
	if out, err := exec.Command("git", "-C", proj, "remote", "add", "origin", "https://x-access-token:ghs_selftest@example.invalid/repo.git").CombinedOutput(); err != nil {
		t.check(markFail, "sandbox", "git remote add: %v %s", err, out)
		return
	}
	cfgPath := filepath.Join(t.work, "sandbox.yaml")
	writeCfg := func(orch map[string]any) error {
		cfg := map[string]any{
			"providers": map[string]any{
				// "claude" exists only in the image, not on this machine.
				"claude": map[string]any{"command": "claude"},
				"codex":  map[string]any{"disabled": true},
			},
			"sandbox": map[string]any{"mode": rt, "image": image, "env": []string{"SY_SELFTEST_SANDBOX_KEY"}},
			// sy's own check runs in the container too: `test` and $SY_SANDBOX
			// exist only there.
			"verify":       map[string]any{"commands": []string{`test "$SY_SANDBOX" = 1 && test "$SY_SELFTEST_SANDBOX_KEY" = k1 && test -f c.txt`}},
			"notify":       map[string]any{"enabled": false},
			"orchestrator": orch,
		}
		data, _ := yaml.Marshal(cfg)
		return os.WriteFile(cfgPath, data, 0o644)
	}
	env := []string{"GITHUB_TOKEN=ghs_selftest", "SY_SELFTEST_SANDBOX_KEY=k1"}
	if err := writeCfg(map[string]any{"approve_plan": false}); err != nil {
		t.check(markFail, "sandbox", "%v", err)
		return
	}
	before, _ := sandbox.Running(bin)

	// 1. The task, with pool worktrees.
	began = time.Now()
	out, err := t.sy(proj, "sandbox run", env, "run", "--config", cfgPath, "--provider", "claude", sandboxTask)
	if err != nil {
		t.check(markFail, "sandbox", "sy run in the sandbox failed: %v\n%s", err, tailLines(out, 20))
		return
	}
	want := map[string]string{"notes dir/a file.txt": "sandbox-a\n", "b.txt": "sandbox-b\n", "c.txt": "sandbox-a\nsandbox-b\n"}
	if !t.filesAre(proj, want, "sandbox", fmt.Sprintf("sy run finished the task with every agent and the verify command in a container (%s)", time.Since(began).Round(100*time.Millisecond))) {
		return
	}
	dirs := t.sandboxLog("gitdirs.log")
	pool, main := false, false
	for _, d := range dirs {
		pool = pool || strings.HasPrefix(d, "/sy/git/worktrees/")
		main = main || d == "/work/.git"
	}
	if !pool || !main {
		t.check(markFail, "sandbox", "git folders seen in the containers: %v; want a pool worktree's (/sy/git/worktrees/...) and the project's (/work/.git)", dirs)
		return
	}
	t.check(markOK, "sandbox", "git worked read-only in pool worktrees and the project folder; no forge token or remote URL got in")
	if left := newContainers(bin, before); len(left) > 0 {
		t.check(markFail, "sandbox", "containers left after the task: %v", left)
		return
	}

	// 2. A step stopped by its timeout.
	if err := writeCfg(map[string]any{"approve_plan": false, "agent_timeout": "8s", "max_attempts": 1}); err != nil {
		t.check(markFail, "sandbox", "%v", err)
		return
	}
	began = time.Now()
	out, err = t.sy(proj, "sandbox timeout", env, "run", "--config", cfgPath, "--provider", "claude", "SANDBOX-HANG: wait")
	if err == nil || !strings.Contains(out, "timed out") {
		t.check(markFail, "sandbox", "the hanging step was not stopped by its timeout (%v):\n%s", err, tailLines(out, 15))
		return
	}
	if left := waitContainersGone(bin, before, 15*time.Second); len(left) > 0 {
		t.check(markFail, "sandbox", "the timed-out step's container still runs: %v", left)
		return
	}
	t.check(markOK, "sandbox", "a step's timeout stopped its container (%s)", time.Since(began).Round(100*time.Millisecond))

	// 3. sy killed hard while its agent works in a container.
	if err := writeCfg(map[string]any{"approve_plan": false}); err != nil {
		t.check(markFail, "sandbox", "%v", err)
		return
	}
	t.sandboxKill(proj, bin, cfgPath, env, before)
}

// sandboxKill starts sy run with the hanging step, kills sy hard once the
// step's container runs, and checks that the container goes with it.
func (t *selftest) sandboxKill(proj, bin, cfgPath string, env, before []string) {
	logf, err := os.Create(filepath.Join(t.logs, "sandbox run (killed).log"))
	if err != nil {
		t.check(markFail, "sandbox", "%v", err)
		return
	}
	defer logf.Close()
	cmd := exec.Command(t.bin, "run", "--config", cfgPath, "--provider", "claude", "SANDBOX-HANG: wait")
	cmd.Dir = proj
	cmd.Env = t.childEnv(env...)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.check(markFail, "sandbox", "start sy run: %v", err)
		return
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	// The hanging step's container is the one that stays.
	var name string
	seen := map[string]time.Time{}
	deadline := time.Now().Add(3 * time.Minute)
	for name == "" {
		select {
		case err := <-exited:
			t.check(markFail, "sandbox", "sy run ended before its agent hung (%v):\n%s", err, tailLines(fileText(logf.Name()), 15))
			return
		case <-time.After(300 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			<-exited
			t.check(markFail, "sandbox", "no container ran for 3s within 3 minutes:\n%s", tailLines(fileText(logf.Name()), 15))
			return
		}
		for _, n := range newContainers(bin, before) {
			if _, ok := seen[n]; !ok {
				seen[n] = time.Now()
			} else if time.Since(seen[n]) > 3*time.Second {
				name = n
			}
		}
	}
	if err := cmd.Process.Kill(); err != nil {
		t.check(markFail, "sandbox", "could not kill sy run: %v", err)
		return
	}
	<-exited
	began := time.Now()
	if left := waitContainersGone(bin, before, 20*time.Second); len(left) > 0 {
		t.check(markFail, "sandbox", "container %v still runs 20s after sy was killed", left)
		for _, n := range left {
			exec.Command(bin, "rm", "-f", n).Run()
		}
		return
	}
	t.check(markOK, "sandbox", "sy killed hard: its agent's container %s stopped %s later", name, time.Since(began).Round(100*time.Millisecond))
}

// sandboxLog reads a log the stand-in agent keeps in the project's home
// folder (sy's per-project sandbox state of the test profile).
func (t *selftest) sandboxLog(name string) []string {
	cache := filepath.Join(t.profile, ".cache")
	switch runtime.GOOS {
	case "windows":
		cache = filepath.Join(t.profile, "AppData", "Local")
	case "darwin":
		cache = filepath.Join(t.profile, "Library", "Caches")
	}
	files, _ := filepath.Glob(filepath.Join(cache, "switchyard", "sandbox", "*", "home", name))
	var lines []string
	for _, f := range files {
		lines = append(lines, strings.Fields(fileText(f))...)
	}
	return lines
}

// newContainers lists this user's sy containers that were not there before.
func newContainers(bin string, before []string) []string {
	now, _ := sandbox.Running(bin)
	var out []string
	for _, n := range now {
		if !slices.Contains(before, n) {
			out = append(out, n)
		}
	}
	return out
}

// waitContainersGone waits until no new container is left, at most d.
func waitContainersGone(bin string, before []string, d time.Duration) []string {
	end := time.Now().Add(d)
	for {
		left := newContainers(bin, before)
		if len(left) == 0 || time.Now().After(end) {
			return left
		}
		time.Sleep(500 * time.Millisecond)
	}
}
