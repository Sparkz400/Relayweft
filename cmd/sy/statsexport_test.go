package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

func captureStats(t *testing.T) *bytes.Buffer {
	var b bytes.Buffer
	old := statsOut
	statsOut = &b
	t.Cleanup(func() { statsOut = old })
	return &b
}

// logTask writes one finished task to the session logs of the isolated
// config dir.
func logTask(t *testing.T, task string, usd float64) {
	t.Helper()
	w, err := sessionlog.Open(config.Default().SessionDir(), "/work/private-repo")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.Write(sessionlog.Record{Type: sessionlog.TypeAgentEnd, Provider: event.Claude, Model: "sonnet", OK: sessionlog.Bool(true), Tokens: &event.TokenUsage{Input: 3000, Output: 100, CostUSD: usd}})
	w.Write(sessionlog.Record{Type: sessionlog.TypeTaskEnd, Task: task, OK: sessionlog.Bool(true),
		Cost: &event.TaskCost{CostUSD: usd, PerProvider: map[string]event.TokenUsage{event.Claude: {Input: 3000, Output: 100}}}})
}

// sy stats --json: numbers only by default; --with-tasks and --name add
// what they say; --out writes the file.
func TestStatsJSONExport(t *testing.T) {
	isolate(t)
	logTask(t, "fix the login of customer ACME-SECRET", 0.25)
	out := captureStats(t)
	if err := cmdStats([]string{"--json", "--since", "7d"}); err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"ACME-SECRET", "private-repo", "customer"} {
		if strings.Contains(out.String(), leak) {
			t.Errorf("export leaks %q:\n%s", leak, out)
		}
	}
	e, err := sessionlog.ParseExport(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Days) != 1 || e.Days[0].Tasks != 1 || e.Days[0].FreshTokens != 3100 || e.Name != "" || e.Since.IsZero() {
		t.Fatalf("export = %+v", e)
	}
	// The machine id stays the same.
	out.Reset()
	file := filepath.Join(t.TempDir(), "me.json")
	if err := cmdStats([]string{"--json", "--with-tasks", "--name", "laptop", "--out", file}); err != nil {
		t.Fatal(err)
	}
	e2, err := sessionlog.ReadExport(file)
	if err != nil {
		t.Fatal(err)
	}
	if e2.Machine != e.Machine || e2.Name != "laptop" || len(e2.Tasks) != 1 || !strings.Contains(e2.Tasks[0].Task, "ACME-SECRET") || out.Len() != 0 {
		t.Errorf("--with-tasks --name --out: %+v (stdout %q)", e2, out)
	}
	if err := cmdStats([]string{"--with-tasks"}); err == nil {
		t.Error("--with-tasks without --json was accepted")
	}
}

// sy stats --merge: the same file twice counts once, a folder works, and
// an export of an unknown major version is refused with a clear error.
func TestStatsMerge(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	now := time.Now()
	today := now.Local().Format("2006-01-02")
	mk := func(id, name string, usd float64) sessionlog.Export {
		return sessionlog.Export{Format: sessionlog.ExportFormat, Version: sessionlog.ExportVersion, Machine: id, Name: name, Generated: now,
			Days: []sessionlog.ExportDay{{Date: today, Tasks: 2, OK: 2, FreshTokens: 4000, USD: usd, Providers: map[string]int64{event.Claude: 4000},
				Models: []sessionlog.ExportModel{{Provider: event.Claude, Model: "opus", Calls: 3, OK: 3, FreshTokens: 4000, USD: usd}}}}}
	}
	if err := sessionlog.WriteTeamFile(dir, mk("aaaa000000000001", "alice-pc", 1.50)); err != nil {
		t.Fatal(err)
	}
	if err := sessionlog.WriteTeamFile(dir, mk("bbbb000000000002", "", 0.50)); err != nil {
		t.Fatal(err)
	}
	a := sessionlog.TeamFile(dir, "aaaa000000000001")
	out := captureStats(t)
	if err := cmdStats([]string{"--merge", a, dir, a, "--since", "7d"}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"2 machine(s)", "claude:opus", "2.00", "Per machine", "alice-pc", "bbbb000000000002"} {
		if !strings.Contains(s, want) {
			t.Errorf("merge output lacks %q:\n%s", want, s)
		}
	}
	// Day total: $1.50 + $0.50, not $3.50 for alice merged three times.
	if !strings.Contains(s, today) || strings.Contains(s, "3.50") {
		t.Errorf("merge counted a machine twice:\n%s", s)
	}
	future := filepath.Join(t.TempDir(), "future.json")
	os.WriteFile(future, []byte(`{"format":"switchyard-stats","version":"2.0","machine":"cccc000000000003","days":[]}`), 0o644)
	err := cmdStats([]string{"--merge", a, future})
	if err == nil || !strings.Contains(err.Error(), `version "2.0" is not supported`) || !strings.Contains(err.Error(), "future.json") {
		t.Errorf("unknown version: %v", err)
	}
	// In a folder, a broken file is skipped.
	os.WriteFile(filepath.Join(dir, "junk.json"), []byte("{"), 0o644)
	out.Reset()
	if err := cmdStats([]string{"--merge", dir}); err != nil || !strings.Contains(out.String(), "2 machine(s)") {
		t.Errorf("folder with a broken file: %v\n%s", err, out)
	}
	if err := cmdStats([]string{"--merge"}); err == nil {
		t.Error("--merge without files was accepted")
	}
}
