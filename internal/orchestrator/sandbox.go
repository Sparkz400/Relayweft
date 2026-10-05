package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/proc"
	"github.com/sparkz400/relayweft/internal/sandbox"
)

// checkCmd is one verify command rw runs in dir: in the sandbox when
// agents write in one (config.CheckSandbox, docs/sandbox.md), through the
// system shell otherwise.
func checkCmd(ctx context.Context, cfg *config.Config, dir, line string) (cmd *exec.Cmd, done func(err error, out []byte) string, err error) {
	return shellCmd(ctx, cfg, dir, line, nil)
}

// taskCfg is t's config, or the orchestrator's when the task has none of
// its own: the sandbox settings must never be skipped for lack of one.
func (o *Orchestrator) taskCfg(t *task) *config.Config {
	if t != nil && t.cfg != nil {
		return t.cfg
	}
	return o.opts.Store.Get()
}

// selectExec is how the narrowed verify run (package affected) runs the
// tools that pick the tests (go list, cargo metadata) in the agents'
// folder: nil (here) when no writing agent runs in a sandbox, otherwise in
// the sandbox with the folder read-only, since those tools read configs
// agents may have written.
func selectExec(cfg *config.Config) func(ctx context.Context, dir string, argv []string) ([]byte, error) {
	sb, on := cfg.CheckSandbox()
	if !on {
		return nil
	}
	return func(ctx context.Context, dir string, argv []string) ([]byte, error) {
		cmd, box, err := sandbox.Command(ctx, sandbox.Spec{Cfg: sb, Dir: dir, ReadOnly: true, Argv: argv,
			Env: sandbox.PassEnv(sb.Env, nil), Label: "select"})
		if err != nil {
			return nil, err
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		escape := box.Check()
		box.Close(err == nil && ctx.Err() == nil)
		switch {
		case escape != nil:
			return nil, escape
		case err != nil:
			if cmd.ProcessState != nil {
				if e := box.Explain(cmd.ProcessState.ExitCode(), stderr.String()); e != nil {
					return nil, e
				}
			}
			return nil, fmt.Errorf("%v: %s", err, clip(strings.TrimSpace(stderr.String()), 300))
		}
		return out, nil
	}
}

// RunCheck runs one command line on code agents wrote (rw bench's check)
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

// shellCmd runs a command line rw runs on code agents wrote (a verify
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
		if strings.HasPrefix(kv, "RW_DIR=") {
			kv = "RW_DIR=" + sandbox.Work
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
