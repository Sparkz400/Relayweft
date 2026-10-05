package main

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/limits"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

func TestTermApproverBudget(t *testing.T) {
	r := orchestrator.BudgetRequest{Limit: orchestrator.LimitTaskUSD, Used: 2.04, Max: 2, Task: "t", Next: "start b (docs)"}
	for in, want := range map[string]bool{"maybe\ny\n": true, "yes\n": true, "\n": false, "n\n": false, "": false} {
		var out bytes.Buffer
		a := newTermApprover(strings.NewReader(in), &out)
		if got := a.ApproveBudget(context.Background(), r); got != want {
			t.Errorf("answer %q: %v, want %v\n%s", in, got, want, out.String())
		}
		if o := out.String(); !strings.Contains(o, "this task's cost $2.04 reached the budget of $2.00") || !strings.Contains(o, "--budget-task-usd") || !strings.Contains(o, "start b (docs)") {
			t.Errorf("prompt:\n%s", o)
		}
	}
}

func TestTermApproverConflict(t *testing.T) {
	q := orchestrator.ConflictQuestion{StepID: "b", With: "your uncommitted edits", Files: []string{"shared.txt"}, Yours: true}
	for in, want := range map[string]bool{"maybe\ny\n": true, "yes\n": true, "\n": false, "n\n": false, "": false} {
		var out bytes.Buffer
		a := newTermApprover(strings.NewReader(in), &out)
		if got := a.ApproveResolve(context.Background(), q); got != want {
			t.Errorf("answer %q: %v, want %v\n%s", in, got, want, out.String())
		}
		if o := out.String(); !strings.Contains(o, "Merge conflict: b conflicts with your uncommitted edits in shared.txt") || !strings.Contains(o, "never in your folder") {
			t.Errorf("prompt:\n%s", o)
		}
	}
}

func TestScheduleFlagsTarget(t *testing.T) {
	cfg := config.Default()
	cfg.LogDir = t.TempDir()
	now := time.Now()
	var sf scheduleFlags
	if at, _, err := sf.target(cfg, nil, now); err != nil || !at.IsZero() || sf.set() {
		t.Fatalf("no flags: %v %v", at, err)
	}
	sf = scheduleFlags{in: "3h"}
	if at, _, err := sf.target(cfg, nil, now); err != nil || !at.Equal(now.Add(3*time.Hour)) {
		t.Errorf("--in: %v %v", at, err)
	}
	sf = scheduleFlags{at: "2001-01-01 01:00"}
	if _, _, err := sf.target(cfg, nil, now); err == nil {
		t.Error("--at in the past accepted")
	}
	sf = scheduleFlags{at: "02:30", in: "1h"}
	if _, _, err := sf.target(cfg, nil, now); err == nil {
		t.Error("two start flags accepted")
	}
	sf = scheduleFlags{whenReset: "bard"}
	if _, _, err := sf.target(cfg, nil, now); err == nil {
		t.Error("unknown provider accepted")
	}
	// --when-reset reads the newest quota record from the session logs.
	w, err := sessionlog.Open(cfg.LogDir, "/p")
	if err != nil {
		t.Fatal(err)
	}
	resets := now.Add(2 * time.Hour).Truncate(time.Second)
	w.Write(sessionlog.Record{Type: sessionlog.TypeQuota, Provider: event.Claude, Quota: &event.QuotaInfo{Utilization: 1, Window: "five_hour", ResetsAt: resets}})
	w.Close()
	sf = scheduleFlags{whenReset: "claude"}
	if at, note, err := sf.target(cfg, limits.NewTracker(), now); err != nil || !at.Equal(resets) || note == "" {
		t.Errorf("--when-reset claude: %v %q %v", at, note, err)
	}
	sf = scheduleFlags{whenReset: "codex"}
	if at, note, err := sf.target(cfg, limits.NewTracker(), now); err != nil || !at.IsZero() || !strings.Contains(note, "starting now") {
		t.Errorf("--when-reset codex (unknown): %v %q %v", at, note, err)
	}
}

func TestWaitUntilDue(t *testing.T) {
	cfg := config.Default()
	cfg.LogDir = t.TempDir()
	old := waitEvery
	waitEvery = 10 * time.Millisecond
	defer func() { waitEvery = old }()

	// Unknown reset: starts at once and says so.
	var out bytes.Buffer
	sf := scheduleFlags{whenReset: "any"}
	release, err := waitUntilDue(context.Background(), &out, &sf, cfg, limits.NewTracker(), true, "the task")
	if err != nil || !strings.Contains(out.String(), "starting now") {
		t.Fatalf("%v\n%s", err, out.String())
	}
	release()

	// A short wait prints a countdown and returns.
	out.Reset()
	sf = scheduleFlags{in: "60ms"}
	release, err = waitUntilDue(context.Background(), &out, &sf, cfg, nil, true, "2 tasks")
	if err != nil || !strings.Contains(out.String(), "scheduled: 2 tasks starts at") || !strings.Contains(out.String(), "waiting: starts in") {
		t.Fatalf("%v\n%s", err, out.String())
	}
	release()

	// Ctrl+C cancels the wait.
	ctx, cancel := context.WithCancel(context.Background())
	sf = scheduleFlags{in: "1h"}
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	done := make(chan error, 1)
	go func() {
		_, err := waitUntilDue(ctx, &bytes.Buffer{}, &sf, cfg, nil, true, "the task")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "cancelled") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not end the wait")
	}
}

func TestPrintSchedule(t *testing.T) {
	now := time.Date(2026, 10, 3, 14, 0, 0, 0, time.Local)
	clock := time.Date(0, 1, 1, 2, 30, 0, 0, time.UTC)
	var w bytes.Buffer
	printSchedule(&w, "windows", `C:\Tools\rw.exe`, `C:\src\app`, `C:\src\app\tasks.txt`, clock, false, now)
	out := w.String()
	for _, want := range []string{`schtasks /create /tn "Relayweft 02:30" /sc once /sd 10/04/2026 /st 02:30`, `run --file \"C:\src\app\tasks.txt\"`, "schtasks /delete"} {
		if !strings.Contains(out, want) {
			t.Errorf("windows output misses %q:\n%s", want, out)
		}
	}
	w.Reset()
	printSchedule(&w, "linux", "/usr/local/bin/rw", "/home/me/my app", "/home/me/my app/tasks.txt", clock, true, now)
	if out := w.String(); !strings.Contains(out, "30 2 * * * cd '/home/me/my app' && /usr/local/bin/rw run --file '/home/me/my app/tasks.txt'") {
		t.Errorf("cron output:\n%s", out)
	}
}

// pasteToCmd is what an interactive cmd.exe hands the program for a line:
// it toggles quoting at every ", and outside quotes runs & | < > as
// operators (returned in ops) and removes the ^ escape.
func pasteToCmd(line string) (out, ops string) {
	var b, o strings.Builder
	q := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '"':
			q = !q
		case !q && c == '^' && i+1 < len(line):
			i++
			b.WriteByte(line[i])
			continue
		case !q && strings.IndexByte("&|<>", c) >= 0:
			o.WriteByte(c)
		}
		b.WriteByte(c)
	}
	return b.String(), o.String()
}

// msvcrtArgs splits a command line by the MSVCRT/UCRT rules (2n
// backslashes + " = n backslashes + toggle, 2n+1 = n + literal ", "" in
// quotes = literal ").
func msvcrtArgs(line string) []string {
	var args []string
	p := 0
	for {
		for p < len(line) && (line[p] == ' ' || line[p] == '\t') {
			p++
		}
		if p >= len(line) {
			return args
		}
		var b strings.Builder
		q := false
		for {
			copyChar, slashes := true, 0
			for p < len(line) && line[p] == '\\' {
				p++
				slashes++
			}
			if p < len(line) && line[p] == '"' {
				if slashes%2 == 0 {
					if q && p+1 < len(line) && line[p+1] == '"' {
						p++
					} else {
						copyChar, q = false, !q
					}
				}
				slashes /= 2
			}
			b.WriteString(strings.Repeat(`\`, slashes))
			if p >= len(line) || (!q && (line[p] == ' ' || line[p] == '\t')) {
				break
			}
			if copyChar {
				b.WriteByte(line[p])
			}
			p++
		}
		args = append(args, b.String())
	}
}

// The printed schtasks line works when pasted into cmd.exe for paths with
// & and a drive root: cmd runs no operator of it, schtasks gets the whole
// /tr command, and that command quotes every path for its own cmd /c.
func TestPrintScheduleWindowsQuoting(t *testing.T) {
	now := time.Date(2026, 10, 3, 14, 0, 0, 0, time.Local)
	clock := time.Date(0, 1, 1, 2, 30, 0, 0, time.UTC)
	for _, c := range []struct{ exe, dir, file string }{
		{`C:\Tools\rw.exe`, `C:\src\R&D`, `C:\src\R&D\tasks.txt`},
		{`C:\Program Files (x86)\rw\rw.exe`, `C:\`, `C:\tasks.txt`},
		{`C:\a^b\rw.exe`, `C:\x|y <z>`, `C:\x|y <z>\t (1).txt`},
	} {
		var w bytes.Buffer
		printSchedule(&w, "windows", c.exe, c.dir, c.file, clock, true, now)
		var line string
		for _, l := range strings.Split(w.String(), "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "schtasks /create") {
				line = strings.TrimSpace(l)
			}
		}
		got, ops := pasteToCmd(line)
		if ops != "" {
			t.Errorf("%s: cmd.exe runs %q of the pasted line:\n%s", c.dir, ops, line)
			continue
		}
		argv := msvcrtArgs(got)
		i := slices.Index(argv, "/tr")
		if i < 0 || i+2 != len(argv) {
			t.Errorf("%s: schtasks argv %q", c.dir, argv)
			continue
		}
		want := `cmd /c cd /d "` + c.dir + `" && "` + c.exe + `" run --file "` + c.file + `" > "` + filepath.Join(c.dir, "rw-scheduled.log") + `" 2>&1`
		if argv[i+1] != want {
			t.Errorf("%s: /tr = %s\nwant    %s", c.dir, argv[i+1], want)
		}
		if argv[3] != "Relayweft 02:30" {
			t.Errorf("/tn = %q", argv[3])
		}
	}
	// A path with % has no safe command line: none is printed.
	var w bytes.Buffer
	printSchedule(&w, "windows", `C:\Tools\rw.exe`, `C:\src\100%`, `C:\src\100%\tasks.txt`, clock, true, now)
	if out := w.String(); strings.Contains(out, "schtasks /create") || !strings.Contains(out, "contains %") {
		t.Errorf("%% path:\n%s", out)
	}
	if got := argvTrail(`C:\`); got != `C:\\` {
		t.Errorf("argvTrail = %s", got)
	}
}
