package health

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const deadPID = 999_999_001 // never a running process

func writeLog(t *testing.T, dir, name string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func stamp(t time.Time) string { return t.Format("2006-01-02 15:04:05.000") }

func TestBuild(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
	day := func(n int, h int) time.Time { return now.AddDate(0, 0, -n).Add(time.Duration(h-12) * time.Hour) }
	var lines []string
	add := func(at time.Time, pid int, rest string) {
		lines = append(lines, fmt.Sprintf("%s %s", stamp(at), strings.Replace(rest, "PID", fmt.Sprintf("pid=%d", pid), 1)))
	}
	// Daily use for the last 12 days, cleanly ended.
	for d := 12; d >= 1; d-- {
		pid := deadPID + d
		add(day(d, 9), pid, "start PID ver=dev cmd=run")
		add(day(d, 9).Add(5*time.Minute), pid, "load PID cpu=97 memfree_mb=900 memtotal_mb=16000 symem_mb=40 goroutines=20")
		add(day(d, 10), pid, "end PID ok=true after=1h0m0s cpu=50 memfree_mb=4000 memtotal_mb=16000 symem_mb=60 goroutines=25")
	}
	// Inspection commands are no use.
	add(day(13, 9), deadPID+100, "start PID ver=dev cmd=health")
	add(day(13, 9), deadPID+100, "end PID ok=true after=0s")
	// A hang 5 days ago that recovered.
	add(day(5, 9).Add(10*time.Minute), deadPID+5, `hang PID in=tui stuck=1m10s log=hang-x.log`)
	add(day(5, 9).Add(12*time.Minute), deadPID+5, `hang-end PID in=tui after=2m0s`)
	// An unclean exit 3 days ago (no end line).
	add(day(3, 15), deadPID+200, "start PID ver=dev cmd=")
	add(day(3, 16), deadPID+200, "load PID cpu=30")
	// A fatal runtime error 2 days ago.
	add(day(2, 15), deadPID+300, "start PID ver=dev cmd=run")
	add(day(2, 18), deadPID+400, "agent-timeout PID agent=a3 after=30m0s")
	add(day(1, 8), deadPID+400, "pause PID for=7h0m0s")
	add(day(1, 8), deadPID+400, `leftover PID what=orphan-agent path="C:\pool\1"`)
	writeLog(t, dir, "rw-health.log", lines...)
	writeLog(t, dir, fmt.Sprintf("fatal-%d-%s.log", deadPID+300, day(2, 15).Format("20060102-150405")), "fatal error: concurrent map writes", "", "goroutine 1 [running]:")
	writeLog(t, dir, fmt.Sprintf("fatal-%d-%s.log", deadPID+500, day(1, 15).Format("20060102-150405"))) // empty: killed, not fatal
	os.WriteFile(filepath.Join(dir, fmt.Sprintf("fatal-%d-x.log", deadPID+501)), nil, 0o644)
	// A crash log written by a Go test is not a crash of rw.
	writeLog(t, dir, "crash-"+day(4, 9).Format("20060102-150405.000")+".log", "Relayweft dev crashed in subtask boom", "\tD:/x/phase1_test.go:545 +0x42d")
	// Debug log from before the health log: one use day and a panic.
	writeLog(t, dir, "rw-debug.log",
		stamp(day(13, 7))+" === rw dev run (windows/amd64, 16 cpus) cwd=C:\\x args=[]",
		stamp(day(13, 7).Add(time.Minute))+" PANIC in task: boom",
		"    continuation line",
		stamp(day(13, 7).Add(2*time.Minute))+` exit agent=a1 pid=5 code=1 after 30m0s ok=false killed=false limit=false tokens=0 err=timed out after 30m0s stderr=""`,
	)

	r, err := Build(Options{Dir: dir, Now: now, NoLeftovers: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Crashes) != 2 || r.Crashes[0].Detail != "panic in task: boom" || r.Crashes[1].Kind != "fatal" ||
		r.Crashes[1].Detail != "fatal error: concurrent map writes" || r.Crashes[1].Cmd != "run" {
		t.Errorf("crashes = %+v", r.Crashes)
	}
	if len(r.Hangs) != 1 || !strings.Contains(r.Hangs[0].Detail, "tui made no progress for 1m10s, recovered after 2m0s") || filepath.Base(r.Hangs[0].Log) != "hang-x.log" {
		t.Errorf("hangs = %+v", r.Hangs)
	}
	if len(r.Unclean) != 1 || r.Unclean[0].PID != deadPID+200 || !r.Unclean[0].Time.Equal(day(3, 16)) {
		t.Errorf("unclean = %+v", r.Unclean)
	}
	if len(r.Timeouts) != 2 || r.Timeouts[0].Detail != "agent a1 timed out after 30m0s" {
		t.Errorf("timeouts = %+v", r.Timeouts)
	}
	if len(r.Pauses) != 1 || len(r.LeftoverLogs) != 1 {
		t.Errorf("pauses = %+v, leftovers = %+v", r.Pauses, r.LeftoverLogs)
	}
	if r.Load.CPU != 97 || r.Load.MemFreeMB != 900 || r.Load.RwMemMB != 60 || r.Load.Goroutines != 25 || r.Load.Samples != 13 || r.Load.HotSamples != 12 || r.Load.LowMemFrees != 12 {
		t.Errorf("load = %+v", r.Load)
	}
	// Use: days 13 (debug log), 12..1 (runs), 3 and 2 already counted.
	if len(r.UseDays) != 13 {
		t.Errorf("use days = %v", r.UseDays)
	}
	c := r.Criterion
	if c.Met || !c.CleanSince.Equal(day(2, 15)) || c.UseDays != 13 || !strings.Contains(c.Summary, "1 day clean since the last crash or hang, 13 days to go") {
		t.Errorf("criterion = %+v", c)
	}
}

func TestCriterionMet(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
	var lines []string
	for d := 20; d >= 1; d-- {
		at := now.AddDate(0, 0, -d)
		lines = append(lines,
			fmt.Sprintf("%s start pid=%d ver=dev cmd=run", stamp(at), deadPID+d),
			fmt.Sprintf("%s end pid=%d ok=true after=1h0m0s", stamp(at.Add(time.Hour)), deadPID+d))
	}
	writeLog(t, dir, "rw-health.log", lines...)
	r, err := Build(Options{Dir: dir, Now: now, NoLeftovers: true})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Criterion.Met || r.Criterion.UseDays != 14 || !strings.HasPrefix(r.Criterion.Summary, "met: 20 days without a crash or hang") {
		t.Fatalf("criterion = %+v", r.Criterion)
	}

	// Too few days of use.
	r, _ = Build(Options{Dir: dir, Now: now.AddDate(0, 0, 8), NoLeftovers: true})
	if r.Criterion.Met || r.Criterion.UseDays != 6 || !strings.Contains(r.Criterion.Summary, "used on 6 of the last 14 days (10 needed)") {
		t.Fatalf("criterion = %+v", r.Criterion)
	}
}

func TestNoRecords(t *testing.T) {
	r, err := Build(Options{Dir: t.TempDir(), NoLeftovers: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Criterion.Met || !r.Criterion.CleanSince.IsZero() || !strings.HasPrefix(r.Criterion.Summary, "no records yet") {
		t.Fatalf("criterion = %+v", r.Criterion)
	}
}

func TestRunningIsNotUnclean(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeLog(t, dir, "rw-health.log", fmt.Sprintf("%s start pid=%d ver=dev cmd=web", stamp(now.Add(-time.Hour)), os.Getpid()))
	r, err := Build(Options{Dir: dir, Now: now, NoLeftovers: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Unclean) != 0 || len(r.Running) != 0 { // this process: neither unclean nor listed
		t.Fatalf("unclean = %+v running = %+v", r.Unclean, r.Running)
	}
}
