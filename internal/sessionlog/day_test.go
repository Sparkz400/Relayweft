package sessionlog

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/event"
)

func TestDayUsageAndReadDirSince(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, "/p")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cost := func(cx, cl int64, usd float64) *event.TaskCost {
		return &event.TaskCost{CostUSD: usd, PerProvider: map[string]event.TokenUsage{event.Codex: {Input: cx}, event.Claude: {Input: cl, CostUSD: usd}}}
	}
	w.Write(Record{Type: TypeTaskEnd, TS: now, Cost: cost(1000, 500, 0.40)})
	w.Write(Record{Type: TypeTaskEnd, TS: now, Cost: cost(0, 100, 0.10)})
	w.Write(Record{Type: TypeTaskEnd, TS: DayStart(now).Add(-time.Minute), Cost: cost(9999, 0, 9)}) // yesterday
	w.Write(Record{Type: TypeAgentEnd, TS: now, Tokens: &event.TokenUsage{Input: 7777}})            // not a task end
	w.Close()
	recs, err := ReadDirSince(dir, DayStart(now))
	if err != nil {
		t.Fatal(err)
	}
	tok, usd := DayUsage(recs, now)
	if tok != 1600 || usd < 0.499 || usd > 0.501 {
		t.Errorf("day usage = %d tokens, $%.3f", tok, usd)
	}
	// A file not written since the cutoff is skipped.
	if recs, _ := ReadDirSince(dir, now.Add(time.Hour)); len(recs) != 0 {
		t.Errorf("old file read: %d records", len(recs))
	}
}

// An unreadable log file is reported, and the other files still count.
func TestReadDirSinceSkipsUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, "/p")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	w.Write(Record{Type: TypeTaskEnd, TS: now, Cost: &event.TaskCost{CostUSD: 0.5}})
	w.Close()
	if err := os.Mkdir(filepath.Join(dir, "locked.jsonl"), 0o755); err != nil { // cannot be read as a file
		t.Fatal(err)
	}
	recs, err := ReadDirSince(dir, DayStart(now))
	if err == nil || !strings.Contains(err.Error(), "locked.jsonl") {
		t.Errorf("error = %v", err)
	}
	if _, usd := DayUsage(recs, now); usd < 0.499 || usd > 0.501 {
		t.Errorf("readable file not counted: $%.3f", usd)
	}
}

func TestQuotaChanged(t *testing.T) {
	r := time.Now().Add(time.Hour)
	q := event.QuotaInfo{Utilization: 0.5, Window: "five_hour", ResetsAt: r}
	if !QuotaChanged(nil, q) {
		t.Error("first reading not logged")
	}
	same := q
	same.Utilization = 0.52
	if QuotaChanged(&q, same) {
		t.Error("a 2-point move was logged")
	}
	moved := q
	moved.ResetsAt = r.Add(time.Hour)
	if !QuotaChanged(&q, moved) {
		t.Error("a new reset time was not logged")
	}
}

func TestStatsShowDailyBudget(t *testing.T) {
	today := time.Date(2026, 9, 10, 15, 0, 0, 0, time.Local)
	defer func(f func() time.Time) { now = f }(now)
	now = func() time.Time { return today }
	recs := []Record{{Type: TypeTaskEnd, TS: today, Session: "s", OK: Bool(true), Cost: &event.TaskCost{CostUSD: 1.5}}}
	s := Aggregate(recs, Filter{})
	s.DayLimitUSD = 2
	var buf bytes.Buffer
	s.Print(&buf)
	if out := buf.String(); !strings.Contains(out, "DAILY BUDGET") || !strings.Contains(out, "75% of $2.00") {
		t.Errorf("print:\n%s", out)
	}
	s.DayLimitUSD = 1
	buf.Reset()
	s.Print(&buf)
	if !strings.Contains(buf.String(), "150%! of $1.00") {
		t.Errorf("over budget not marked:\n%s", buf.String())
	}
}
