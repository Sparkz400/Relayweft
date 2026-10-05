package orchestrator

import (
	"context"
	"os/exec"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/proc"
	"github.com/sparkz400/switchyard/internal/sandbox"
)

// checkCmd is one verify command sy runs in dir: in the sandbox when the
// config's top-level sandbox section is on (docs/sandbox.md), through the
// system shell otherwise. done must be called with the command's error
// and output once it ended; it returns why the container could not start
// ("" when the command itself ran). A sandbox that is configured but
// cannot start is an error: the command never runs on this machine
// instead.
func checkCmd(ctx context.Context, cfg *config.Config, dir, line string) (cmd *exec.Cmd, done func(err error, out []byte) string, err error) {
	sb := cfg.Sandbox
	if !sb.On() {
		cmd = proc.Shell(ctx, line)
		cmd.Dir = dir
		return cmd, func(error, []byte) string { return "" }, nil
	}
	cmd, box, err := sandbox.Command(ctx, sandbox.Spec{Cfg: sb, Dir: dir, Argv: []string{"sh", "-c", line},
		Env: sandbox.PassEnv(sb.Env, nil), Label: "verify"})
	if err != nil {
		return nil, nil, err
	}
	return cmd, func(runErr error, out []byte) string {
		box.Close(runErr == nil && ctx.Err() == nil)
		if runErr == nil || cmd.ProcessState == nil || ctx.Err() != nil {
			return ""
		}
		if e := box.Explain(cmd.ProcessState.ExitCode(), string(out)); e != nil {
			return e.Error()
		}
		return ""
	}, nil
}
