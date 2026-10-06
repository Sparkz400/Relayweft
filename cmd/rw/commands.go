package main

import (
	"flag"
	"fmt"
	"sort"
)

// command is one rw subcommand. The commands table is the one list of
// them: main dispatches from it and shell completion (completion.go)
// reads names, flags and argument kinds from it, so the two cannot drift.
type command struct {
	name string // "" is rw itself: the TUI
	run  func(args []string) error
	desc string // one line, for completion menus
	args argKind
	// builtin commands parse no flags (version, help).
	builtin bool
}

// argKind says what a command's positional arguments are, for completion.
type argKind int

const (
	argNone    argKind = iota
	argTask            // a task id (rw history)
	argUndoKey         // a task rw undo recorded in this directory
	argFiles
	argShell // a shell name (rw completion)
)

// commands is filled in init: some commands (completion) read the table,
// which a package-level initializer could not refer to.
var commands []command

func init() {
	commands = []command{
		{name: "", run: cmdTUI, desc: "start the TUI"},
		{name: "run", run: cmdRun, desc: "run one task headless"},
		{name: "web", run: cmdWeb, desc: "the browser UI"},
		{name: "app", run: cmdApp, desc: "the browser UI in its own window"},
		{name: "mcp", run: cmdMCP, desc: "MCP server: Claude Code or Codex hand tasks to rw"},
		{name: "pr", run: cmdPR, desc: "pull request from a finished task", args: argTask},
		{name: "watch", run: cmdWatch, desc: "follow up on the PRs rw opened"},
		{name: "review", run: cmdReview, desc: "agent review of a pull request"},
		{name: "schedule", run: cmdSchedule, desc: "print a Task Scheduler / cron command"},
		{name: "notify", run: cmdNotify, desc: "where notifications go"},
		{name: "morning", run: cmdMorning, desc: "what ran unattended overnight, and what needs you"},
		{name: "history", run: cmdHistory, desc: "recent tasks"},
		{name: "resume", run: cmdResume, desc: "continue an interrupted task", args: argTask},
		{name: "report", run: cmdReport, desc: "a shareable page about a task", args: argTask},
		{name: "stats", run: cmdStats, desc: "usage per model and route", args: argFiles},
		{name: "tune", run: cmdTune, desc: "routing suggestions and learned routes"},
		{name: "models", run: cmdModels, desc: "routes and model catalogs"},
		{name: "setup", run: cmdSetup, desc: "guided first run"},
		{name: "doctor", run: cmdDoctor, desc: "check CLIs, git and terminal"},
		{name: "init", run: cmdInit, desc: "write the default config"},
		{name: "trust", run: cmdTrust, desc: "trust this repo's .relayweft.yaml"},
		{name: "clean", run: cmdClean, desc: "remove pooled worktrees"},
		{name: "undo", run: cmdUndo, desc: "revert or re-apply a task's changes", args: argUndoKey},
		{name: "bench", run: cmdBench, desc: "routed vs single agents on your tasks"},
		{name: "bugreport", run: cmdBugreport, desc: "zip logs and diagnostics"},
		{name: "selftest", run: cmdSelftest, desc: "automated Windows checks"},
		{name: "health", run: cmdHealth, desc: "crashes, hangs and leftovers from the logs"},
		{name: "update", run: cmdUpdate, desc: "update rw to the latest release"},
		{name: "completion", run: cmdCompletion, desc: "print a shell completion script", args: argShell},
		{name: "version", run: cmdVersion, desc: "print the version", builtin: true},
		{name: "help", run: cmdHelp, desc: "show the usage", builtin: true},
	}
}

func findCommand(name string) (command, bool) {
	for _, c := range commands {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

func cmdVersion([]string) error {
	fmt.Println("relayweft", version)
	return nil
}

func cmdHelp([]string) error {
	usage()
	return nil
}

// flagProbe, when set, receives a command's flag set in place of parsing
// it: commandFlags uses it to read a command's flags without running it.
var flagProbe func(*flag.FlagSet)

// probed stops a command at parseFlags while its flags are read.
type probed struct{}

// parseFlags parses a command's flags. Every command parses through it
// (a test checks), before it does anything else, so commandFlags sees
// every flag. Its FlagSets use flag.ExitOnError: a bad flag exits, so
// there is no error to return. rw update (ContinueOnError) uses
// parseFlagsErr.
func parseFlags(fs *flag.FlagSet, args []string) {
	_ = parseFlagsErr(fs, args)
}

// parseFlagsErr is parseFlags for a FlagSet that returns its errors.
func parseFlagsErr(fs *flag.FlagSet, args []string) error {
	if flagProbe != nil {
		flagProbe(fs)
		panic(probed{})
	}
	return fs.Parse(args)
}

// commandFlags returns the flags a command defines, read by starting it
// with flagProbe set: it stops at parseFlags, before it does anything.
// ok is false for a builtin, or a command that never reached parseFlags.
func commandFlags(c command) (fs *flag.FlagSet, ok bool) {
	if c.builtin || c.run == nil {
		return nil, false
	}
	defer func() {
		flagProbe = nil
		if r := recover(); r != nil {
			if _, stopped := r.(probed); !stopped {
				panic(r)
			}
		}
	}()
	flagProbe = func(f *flag.FlagSet) { fs = f; ok = true }
	// The command panics at parseFlags; it never returns normally (a test
	// checks every command in the table).
	_ = c.run(nil)
	return nil, false
}

// flagNames lists a flag set's flags, sorted.
func flagNames(fs *flag.FlagSet) []string {
	var out []string
	fs.VisitAll(func(f *flag.Flag) { out = append(out, f.Name) })
	sort.Strings(out)
	return out
}

// isBoolFlag reports whether a flag takes no value.
func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}
