package diag

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHealthRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	defer Close()
	Health("end", "ok", false, "err", `exited: "quoted" = x`, "after", 90*time.Second)
	recs, err := ReadHealth(dir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("records = %v, %v", recs, err)
	}
	r := recs[0]
	if r.Kind != "end" || r.PID != os.Getpid() || r.Fields["ok"] != "false" || r.Fields["err"] != `exited: "quoted" = x` || r.Fields["after"] != "1m30s" {
		t.Fatalf("record = %+v", r)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, debugFile)); !strings.Contains(string(data), "health end pid=") {
		t.Errorf("health event missing from the debug log: %q", data)
	}
}

func TestParseRecordRejectsJunk(t *testing.T) {
	for _, l := range []string{"", "garbage", "2026-10-04 12:00:00.000", "2026-10-04 12:00:00.000 start", "2026-10-04 12:00:00.000 start pid=x"} {
		if _, ok := ParseRecord(l); ok {
			t.Errorf("%q parsed", l)
		}
	}
	r, ok := ParseRecord(`2026-10-04 12:00:00.000 hang pid=7 in=tui stuck=1m5s log="hang x.log" broken="unterminated`)
	if !ok || r.Fields["in"] != "tui" || r.Fields["log"] != "hang x.log" || r.Fields["broken"] != "" {
		t.Fatalf("record = %+v ok=%v", r, ok)
	}
}

func TestStartEndArmsFatalFile(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	defer Close()
	Start("run")
	fatal, _ := filepath.Glob(filepath.Join(dir, "fatal-*.log"))
	if len(fatal) != 1 {
		t.Fatalf("fatal files while running = %v", fatal)
	}
	End(nil)
	if fatal, _ = filepath.Glob(filepath.Join(dir, "fatal-*.log")); len(fatal) != 0 {
		t.Fatalf("fatal file left after a clean end: %v", fatal)
	}
	recs, _ := ReadHealth(dir)
	if len(recs) != 2 || recs[0].Kind != "start" || recs[0].Fields["cmd"] != "run" || recs[1].Kind != "end" || recs[1].Fields["ok"] != "true" {
		t.Fatalf("records = %+v", recs)
	}
}

func TestHangDetected(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	defer Close()
	defer Unwatch("loop")
	Beat("loop")
	checkBeats(time.Now()) // fresh: nothing
	checkBeats(time.Now().Add(HangAfter + time.Second))
	checkBeats(time.Now().Add(HangAfter + 20*time.Second)) // reported once
	Beat("loop")
	checkBeats(time.Now())
	recs, _ := ReadHealth(dir)
	if len(recs) != 2 || recs[0].Kind != "hang" || recs[0].Fields["in"] != "loop" || recs[1].Kind != "hang-end" {
		t.Fatalf("records = %+v", recs)
	}
	dump, err := os.ReadFile(filepath.Join(dir, recs[0].Fields["log"]))
	if err != nil || !strings.Contains(string(dump), "TestHangDetected") {
		t.Fatalf("hang log without goroutine stacks: %v", err)
	}
}

func TestPeaks(t *testing.T) {
	var p Peaks
	p.merge(Peaks{CPU: 0.5, CPUOK: true, MemFree: 4 << 30, MemTotal: 16 << 30, MemOK: true, SyMem: 50 << 20, Goroutines: 10})
	p.merge(Peaks{CPU: 0.9, CPUOK: true, MemFree: 8 << 30, MemTotal: 16 << 30, MemOK: true, SyMem: 40 << 20, Goroutines: 30})
	kv := p.kv()
	want := []any{"cpu", 90, "memfree_mb", uint64(4096), "memtotal_mb", uint64(16384), "symem_mb", uint64(50), "goroutines", 30}
	if len(kv) != len(want) {
		t.Fatalf("kv = %v", kv)
	}
	for i := range want {
		if kv[i] != want[i] {
			t.Fatalf("kv = %v, want %v", kv, want)
		}
	}
}
