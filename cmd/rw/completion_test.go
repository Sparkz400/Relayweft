package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/orchestrator"
)

// Every command in the table stops at parseFlags when its flags are read:
// none runs, and none parses its flags some other way.
func TestCommandTableProbes(t *testing.T) {
	// Should one run after all, it runs here.
	isolate(t)
	chdir(t, t.TempDir())
	seen := map[string]bool{}
	for _, c := range commands {
		if seen[c.name] {
			t.Errorf("command %q twice in the table", c.name)
		}
		seen[c.name] = true
		fs, ok := commandFlags(c)
		if c.builtin {
			if ok {
				t.Errorf("builtin %q has flags", c.name)
			}
			continue
		}
		if !ok || fs == nil {
			t.Errorf("rw %s did not reach parseFlags", c.name)
		}
	}
	if flagProbe != nil {
		t.Error("flagProbe left set")
	}
}

// A command that parses its flags without parseFlags would hide them
// from completion (and run when completion reads them).
func TestFlagsParseThroughParseFlags(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	re := regexp.MustCompile(`\.Parse\((args|rest)\b`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "commands.go" {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if loc := re.FindIndex(data); loc != nil {
			line := bytes.Count(data[:loc[0]], []byte("\n")) + 1
			t.Errorf("%s:%d parses flags directly: use parseFlags(fs, args)", f, line)
		}
	}
}

// The usage text names every command.
func TestUsageNamesEveryCommand(t *testing.T) {
	out, _ := captureStdout(t, func() error { usage(); return nil })
	for _, c := range commands {
		if c.name != "" && c.name != "help" && !strings.Contains(out, "rw "+c.name) {
			t.Errorf("usage does not mention rw %s", c.name)
		}
	}
}

func values(res completion) []string {
	var out []string
	for _, c := range res.cands {
		out = append(out, c.value)
	}
	return out
}

// Completion offers every subcommand and every flag of each: it reads
// them from the same table main dispatches from.
func TestCompletionHasEveryCommandAndFlag(t *testing.T) {
	subs := values(complete([]string{""}))
	for _, c := range commands {
		if c.name != "" && !slices.Contains(subs, c.name) {
			t.Errorf("rw <Tab> misses %s", c.name)
		}
		fs, ok := commandFlags(c)
		if !ok {
			continue
		}
		words := []string{"--"}
		if c.name != "" {
			words = []string{c.name, "--"}
		}
		got := values(complete(words))
		for _, name := range flagNames(fs) {
			if !slices.Contains(got, "--"+name) {
				t.Errorf("rw %s --<Tab> misses --%s", c.name, name)
			}
		}
	}
	if slices.Contains(subs, completeCmd) || slices.Contains(subs, selftestAgentCmd) {
		t.Error("hidden commands offered")
	}
}

// Every flag that takes a value was looked at: its values are completed
// (flagValues) or free text (completeFreeFlags). Both lists name only
// flags that exist.
func TestCompletionValueFlagsClassified(t *testing.T) {
	used := map[string]bool{}
	for _, c := range commands {
		fs, ok := commandFlags(c)
		if !ok {
			continue
		}
		fs.VisitAll(func(f *flag.Flag) {
			if isBoolFlag(f) {
				return
			}
			key := c.name + " " + f.Name
			switch {
			case flagValues[key].kind != valNone:
				used[key] = true
			case flagValues[f.Name].kind != valNone:
				used[f.Name] = true
			case slices.Contains(completeFreeFlags, f.Name):
				used[f.Name] = true
			default:
				t.Errorf("rw %s --%s takes a value: add it to flagValues or completeFreeFlags", c.name, f.Name)
			}
		})
	}
	for k := range flagValues {
		if !used[k] {
			t.Errorf("flagValues has %q, which no command's value flag uses", k)
		}
	}
	for _, k := range completeFreeFlags {
		if !used[k] {
			t.Errorf("completeFreeFlags has %q, which no command's value flag uses", k)
		}
	}
}

// writeTaskState saves a task state the way the orchestrator does.
func writeTaskState(t *testing.T, s orchestrator.TaskState) {
	t.Helper()
	cfg, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg, "relayweft", "tasks")
	os.MkdirAll(dir, 0o755)
	data, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(dir, s.ID+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCompletionCases(t *testing.T) {
	isolate(t)
	project := t.TempDir()
	chdir(t, project)
	now := time.Now()
	writeTaskState(t, orchestrator.TaskState{ID: "20261005-1-aaaa-task-1", Task: "fix the parser", Dir: project, Status: "running", UndoKey: "20261005-1-aaaa-task-1", Created: now})
	writeTaskState(t, orchestrator.TaskState{ID: "20261004-1-bbbb-task-1", Task: "elsewhere", Dir: t.TempDir(), Status: "done", UndoKey: "20261004-1-bbbb-task-1", Created: now.Add(-time.Hour)})

	cases := []struct {
		words     []string
		directive string
		want      []string // all of these, in any order
		not       []string
	}{
		{words: []string{"re"}, want: []string{"review", "resume", "report"}, not: []string{"run"}},
		{words: []string{"-"}, want: []string{"--dir", "--demo", "--route"}},
		{words: []string{"run", "--pre"}, want: []string{"--prefer"}, not: []string{"--provider"}},
		{words: []string{"run", "-qu"}, want: []string{"-quiet"}},
		{words: []string{"run", "--provider", ""}, want: []string{"codex", "claude", "gemini"}},
		{words: []string{"run", "--provider=cl"}, want: []string{"--provider=claude"}, not: []string{"--provider=codex"}},
		{words: []string{"run", "--prefer", "wor"}, directive: "nospace", want: []string{"worker=", "worker_high="}},
		{words: []string{"run", "--prefer", "worker=c"}, want: []string{"worker=codex", "worker=claude"}, not: []string{"worker=other"}},
		{words: []string{"run", "--prefer", "all="}, want: []string{"all=auto", "all=other", "all=gemini"}},
		{words: []string{"run", "--route", "worker=cla"}, directive: "nospace", want: []string{"worker=claude:"}},
		{words: []string{"run", "--route", "worker=claude:"}, want: []string{"worker=claude:opus", "worker=claude:sonnet"}},
		{words: []string{"run", "--route", "worker=claude:opus:h"}, want: []string{"worker=claude:opus:high"}},
		{words: []string{"run", "--single", "codex:gpt-6.1"}, want: []string{"codex:gpt-6.1-sol"}},
		{words: []string{"run", "--when-reset", ""}, want: []string{"any", "codex"}},
		{words: []string{"run", "--config", ""}, directive: "files"},
		{words: []string{"run", "--dir", ""}, directive: "dirs"},
		{words: []string{"--dir", ""}, directive: "dirs"},
		{words: []string{"run", "--in", ""}},                         // a delay: free text
		{words: []string{"selftest", "--in", ""}, directive: "dirs"}, // a folder
		{words: []string{"selftest", "--sandbox", "o"}, want: []string{"off", "only"}},
		{words: []string{"schedule", "--os", ""}, want: []string{"windows", "darwin", "linux"}},
		{words: []string{"run", "--quiet", ""}}, // a bool flag takes no value: the task text
		{words: []string{"resume", ""}, want: []string{"20261005-1-aaaa-task-1"}, not: []string{"20261004-1-bbbb-task-1"}},
		{words: []string{"report", "--md", "2026"}, want: []string{"20261005-1-aaaa-task-1"}},
		{words: []string{"report", "20261005-1-aaaa-task-1", ""}}, // one id only
		{words: []string{"undo", ""}, want: []string{"20261005-1-aaaa-task-1"}},
		{words: []string{"stats", "--merge", ""}, directive: "files"},
		{words: []string{"completion", ""}, want: []string{"bash", "zsh", "fish", "powershell"}},
		{words: []string{"completion", "bash", ""}},
		{words: []string{"nosuch", ""}},
		{words: []string{"version", "-"}},
		{words: []string{"run", "--", "--pr"}}, // after --: the task text
		{words: nil, want: []string{"run", "web"}},
	}
	for _, c := range cases {
		res := complete(c.words)
		got := values(res)
		if res.directive != c.directive {
			t.Errorf("%q: directive %q, want %q", c.words, res.directive, c.directive)
		}
		for _, w := range c.want {
			if !slices.Contains(got, w) {
				t.Errorf("%q: misses %q (got %q)", c.words, w, got)
			}
		}
		for _, w := range c.not {
			if slices.Contains(got, w) {
				t.Errorf("%q: offers %q", c.words, w)
			}
		}
		if len(c.want) == 0 && len(got) > 0 {
			t.Errorf("%q: offers %q, want nothing", c.words, got)
		}
	}

	// Tasks of another folder when --dir names it; none of this one's.
	elsewhere := complete([]string{"undo", "--dir", t.TempDir(), ""})
	if len(elsewhere.cands) != 0 {
		t.Errorf("undo in an empty folder offers %v", values(elsewhere))
	}
}

// A config that does not load still completes, from the defaults.
func TestCompletionBadConfig(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	chdir(t, dir)
	os.WriteFile(filepath.Join(dir, "relayweft.yaml"), []byte("roles: [not, a, map\n"), 0o644)
	got := values(complete([]string{"run", "--provider", ""}))
	if !slices.Contains(got, "claude") {
		t.Errorf("got %q", got)
	}
	got = values(complete([]string{"run", "--config", "missing.yaml", "--provider", ""}))
	if !slices.Contains(got, "codex") {
		t.Errorf("got %q", got)
	}
}

// The wire format the scripts read: directive, then value<TAB>desc lines,
// with no stray tabs or newlines from descriptions.
func TestCompletionOutput(t *testing.T) {
	var b bytes.Buffer
	w := bufio.NewWriter(&b)
	writeCompletion(w, completion{cands: []candidate{{"a", "line\none\ttab"}, {"b", ""}}})
	w.Flush()
	if got, want := b.String(), "default\na\tline one tab\nb\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// PowerShell passes the words through the environment.
func TestCompleteFromEnv(t *testing.T) {
	t.Setenv(completeWordsEnv, "run"+completeWordsSep+"--pre"+completeWordsSep)
	out, _ := captureStdout(t, func() error { cmdComplete(nil); return nil })
	if !strings.HasPrefix(out, "default\n--prefer\t") {
		t.Errorf("got %q", out)
	}
	t.Setenv(completeWordsEnv, completeWordsSep) // rw <Tab>: one empty word
	out, _ = captureStdout(t, func() error { cmdComplete(nil); return nil })
	if !strings.Contains(out, "\nrun\t") {
		t.Errorf("got %q", out)
	}
}

func TestCompletionScripts(t *testing.T) {
	for shell := range completionShells {
		s, err := completionScript(shell)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(s, "\r") {
			t.Errorf("%s script has CR", shell)
		}
		if !strings.Contains(s, "rw __complete") {
			t.Errorf("%s script does not call rw __complete", shell)
		}
	}
	if _, err := completionScript("tcsh"); err == nil {
		t.Error("tcsh accepted")
	}
	out, err := captureStdout(t, func() error { return cmdCompletion([]string{"PowerShell"}) })
	if err != nil || !strings.Contains(out, "Register-ArgumentCompleter") {
		t.Errorf("rw completion PowerShell: %v %q", err, out)
	}
	if err := cmdCompletion(nil); err == nil {
		t.Error("no shell accepted")
	}
}
