package orchestrator

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/sandbox"
)

// sy's verify commands follow the top-level sandbox: with it on and no
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
}
