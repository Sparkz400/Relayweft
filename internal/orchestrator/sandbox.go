package orchestrator

import (
	"context"
	"os/exec"
	"strings"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/proc"
	"github.com/sparkz400/switchyard/internal/sandbox"
)

// checkCmd is one verify command sy runs in dir: in the sandbox when
// agents write in one (config.CheckSandbox, docs/sandbox.md), through the
// system shell otherwise.
func checkCmd(ctx context.Context, cfg *config.Config, dir, line string) (cmd *exec.Cmd, done func(err error, out []byte) string, err error) {
	return shellCmd(ctx, cfg, dir, line, nil)
}

// RunCheck runs one command line on code agents wrote (sy bench's check)
// in dir, in the sandbox when agents write in one, and reports success and
// the combined output.
func RunCheck(ctx context.Context, cfg *config.Config, dir, line string) (bool, string) {
	cmd, done, err := shellCmd(ctx, cfg, dir, line, nil)
	if err != nil {
		return false, err.Error()
	}
	out, err := cmd.CombinedOutput()
	if why := done(err, out); why != "" {
		return false, string(out) + "\n" + why
	}
	return err == nil, string(out)
}

// shellCmd runs a command line sy runs on code agents wrote (a verify
// command, an after_merge or after_task hook) in dir, with env (NAME=value)
// added: in the sandbox when agents write in one, through the system shell
// otherwise. done must be called with the command's error and output once
// it ended; it returns why the container could not start, or that the
// command changed a submodule's .git ("" when all is well). A sandbox that
// is configured but cannot start is an error: the command never runs on
// this machine instead.
func shellCmd(ctx context.Context, cfg *config.Config, dir, line string, env []string) (cmd *exec.Cmd, done func(err error, out []byte) string, err error) {
	sb, on := cfg.CheckSandbox()
	if !on {
		cmd = proc.Shell(ctx, line)
		cmd.Dir = dir
		cmd.Env = append(cmd.Env, env...)
		return cmd, func(error, []byte) string { return "" }, nil
	}
	var pass []string
	for _, kv := range env {
		// The folder is /work in the container.
		if strings.HasPrefix(kv, "SY_DIR=") {
			kv = "SY_DIR=" + sandbox.Work
		}
		pass = append(pass, kv)
	}
	cmd, box, err := sandbox.Command(ctx, sandbox.Spec{Cfg: sb, Dir: dir, Argv: []string{"sh", "-c", line},
		Env: append(sandbox.PassEnv(sb.Env, nil), pass...), Label: "check"})
	if err != nil {
		return nil, nil, err
	}
	return cmd, func(runErr error, out []byte) string {
		escape := box.Check()
		box.Close(runErr == nil && ctx.Err() == nil)
		if escape != nil {
			return escape.Error()
		}
		if runErr == nil || cmd.ProcessState == nil || ctx.Err() != nil {
			return ""
		}
		if e := box.Explain(cmd.ProcessState.ExitCode(), string(out)); e != nil {
			return e.Error()
		}
		return ""
	}, nil
}
