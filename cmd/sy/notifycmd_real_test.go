package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/runner"
)

// failAll is an agent whose every run fails with a hostile message, so the
// "failed" webhook carries mentions, markdown and non-ASCII text.
type failAll struct{}

func (failAll) Run(context.Context, runner.Spec, func(event.Event)) runner.Result {
	return runner.Result{Err: errors.New("boom @everyone @here <!channel> [click me](https://example.com/x) **bold** Grüße 日本")}
}

// TestRealNtfy posts to a real ntfy topic: sy notify --test, then sy run
// with fake agents once ok ("done") and once failing ("failed"). It is
// opt-in and needs network access:
//
//	SY_REAL_NTFY_URL=https://ntfy.sh/<long random topic> go test ./cmd/sy -run RealNtfy
//
// Read the result back with curl -s "<url>/json?poll=1".
func TestRealNtfy(t *testing.T) {
	url := os.Getenv("SY_REAL_NTFY_URL")
	if url == "" {
		t.Skip("set SY_REAL_NTFY_URL to a ntfy topic URL to post real notifications")
	}
	isolate(t)
	dir := gitInit(t)
	chdir(t, dir)
	cfgPath := filepath.Join(t.TempDir(), "switchyard.yaml")
	os.WriteFile(cfgPath, []byte("notify:\n  enabled: false\n  min_task: 0s\n  webhooks:\n    - url: "+url+"\n"), 0o644)

	if err := cmdNotify([]string{"--config", cfgPath}); err != nil {
		t.Fatal(err)
	}
	if err := cmdNotify([]string{"--config", cfgPath, "--test"}); err != nil {
		t.Fatalf("sy notify --test: %v", err)
	}

	defer func() { headlessRunners = runner.New }()
	fake := runner.NewFakeSet(0)
	headlessRunners = func(*config.Config) runner.Set { return fake }
	if err := cmdRun([]string{"--config", cfgPath, "--no-review", "add a greeting ä 日本 to README"}); err != nil {
		t.Errorf("ok run: %v", err)
	}
	headlessRunners = func(*config.Config) runner.Set {
		return runner.Set{event.Codex: failAll{}, event.Claude: failAll{}}
	}
	if err := cmdRun([]string{"--config", cfgPath, "--no-review", "this task fails @everyone"}); !errors.Is(err, errTaskFailed) {
		t.Errorf("failing run: want errTaskFailed, got %v", err)
	}
}
