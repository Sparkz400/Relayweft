package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sparkz400/relayweft/internal/diag"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/limits"
	"github.com/sparkz400/relayweft/internal/mcpserve"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/proc"
	"github.com/sparkz400/relayweft/internal/runner"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

const mcpHelp = `Usage: rw mcp [--dir <path>] [--repo name=path] [flags]

Runs rw as an MCP server on stdin/stdout, for a coding agent (Claude Code,
Codex) to hand multi-step tasks to rw from inside its own session. Its
tools: run_task, task_status, task_result, list_tasks, cancel_task,
resume_task, follow_up, approve_plan, edit_plan, apply, reject and undo.

rw works only in the folder it was started in (or --dir) and the repos
named with --repo; no tool takes a path. One task runs at a time. When the
client disconnects, the running task is cancelled; resume_task continues it
from the next rw mcp. Under one of rw's own agents it refuses to start
tasks (no recursion). Set-up for Claude Code and Codex: docs/mcp.md.

The flags are those of rw run (routes, providers, budgets).
`

// cmdMCP implements `rw mcp`.
func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("rw mcp", flag.ExitOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, mcpHelp+"\nFlags:\n")
		fs.PrintDefaults()
	}
	var c common
	c.register(fs)
	parseFlags(fs, args)
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (rw mcp takes its tasks from the MCP client)", fs.Arg(0))
	}
	// stdout is the protocol: anything else written there would break
	// it, so the rest of rw writes to stderr.
	out := os.Stdout
	os.Stdout = os.Stderr
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return serveMCP(ctx, &c, os.Stdin, out, mcpserve.NestedHere())
}

// serveMCP serves MCP on in/out until the client disconnects or ctx ends.
// nested (mcpserve.Nested) refuses every tool call.
func serveMCP(ctx context.Context, c *common, in io.Reader, out io.Writer, nested string) error {
	transport := &mcp.IOTransport{Reader: io.NopCloser(in), Writer: nopWriteCloser{out}}
	if nested != "" {
		fmt.Fprintln(os.Stderr, "rw mcp: refusing tasks: "+nested)
		diag.Logf("mcp: nested, refusing tasks: %s", nested)
		return ignoreClosed(mcpserve.NewNestedServer(nested, version).Run(ctx, transport))
	}
	if gone := mcpserve.DropCallerEnv(); len(gone) > 0 {
		diag.Logf("mcp: the caller's session variables are not passed on: %s", strings.Join(gone, ","))
	}
	e, err := startMCPEngine(c)
	if err != nil {
		return err
	}
	defer e.stop()
	diag.Logf("mcp: serving %s", e.dir)
	err = mcpserve.NewServer(e.engine, version).Run(ctx, transport)
	fmt.Fprintln(os.Stderr, "rw mcp: client gone, stopping")
	return ignoreClosed(err)
}

// mcpEngine is a running engine with its session log.
type mcpEngine struct {
	engine *mcpserve.Engine
	events chan event.Event
	log    *sessionlog.Writer
	dir    string
}

func startMCPEngine(c *common) (*mcpEngine, error) {
	store, dir, err := c.setup()
	if err != nil {
		return nil, err
	}
	_ = proc.Guard()
	cfg := store.Get()
	prunePoolsInBackground(cfg)
	m := &mcpEngine{events: make(chan event.Event, 4096), dir: dir}
	if m.log, err = sessionlog.Open(cfg.SessionDir(), dir); err != nil {
		fmt.Fprintln(os.Stderr, "warning: session log disabled:", err)
		m.log = nil
	}
	ap := mcpserve.NewApprover()
	orc := orchestrator.New(orchestrator.Options{
		Dir: dir, Store: store, Runners: mcpRunners, Tracker: limits.NewTracker(), Log: m.log,
		Events: m.events, ForceProvider: c.provider, Mode: "routed", Approver: ap,
		Repos: c.workspace, WorkspaceSkipped: c.workspaceSkipped,
	})
	reports := ""
	if d, err := os.UserConfigDir(); err == nil {
		reports = filepath.Join(d, "relayweft", "reports")
	}
	m.engine, err = mcpserve.New(mcpserve.Options{Orc: orc, Approver: ap, Events: m.events, Dir: dir,
		SessionDir: cfg.SessionDir(), Session: m.log.Session(), ReportDir: reports, Version: version})
	if err != nil {
		m.log.Close()
		return nil, err
	}
	return m, nil
}

// mcpRunners starts the agents (tests swap it).
var mcpRunners = runner.New

// stop cancels the running task and waits (bounded) for its agents.
func (m *mcpEngine) stop() {
	m.engine.Close()
	m.log.Close()
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// ignoreClosed treats the client hanging up as the normal end.
func ignoreClosed(err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || errors.Is(err, mcp.ErrConnectionClosed) {
		return nil
	}
	return err
}
