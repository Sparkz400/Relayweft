package sessionlog

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/event"
)

func TestAggregateDays(t *testing.T) {
	today := time.Date(2026, 9, 10, 15, 0, 0, 0, time.Local)
	defer func(f func() time.Time) { now = f }(now)
	now = func() time.Time { return today }

	end := func(daysAgo int, ok bool, cx, cl int64, usd float64) Record {
		return Record{Type: TypeTaskEnd, TS: today.AddDate(0, 0, -daysAgo), Session: "s", OK: Bool(ok),
			Cost: &event.TaskCost{CostUSD: usd, PerProvider: map[string]event.TokenUsage{
				event.Codex: {Input: cx}, event.Claude: {Input: cl, Cached: 100},
			}}}
	}
	recs := []Record{
		end(0, true, 1000, 600, 0.5),
		end(0, false, 2000, 100, 0.25),
		end(2, true, 3000, 0, 0),
		end(6, true, 1, 0, 0),
		end(7, true, 1, 0, 0), // outside the 7-day window
		{Type: TypeTaskEnd, TS: today.AddDate(0, 0, -1), Session: "s"}, // no cost
	}
	s := Aggregate(recs, Filter{})
	if len(s.Days) != 4 {
		t.Fatalf("days = %d, want 4: %+v", len(s.Days), s.Days)
	}
	d := s.Days[0]
	if d.Date != "2026-09-10" || d.Tasks != 2 || d.OK != 1 || d.Codex != 3000 || d.Claude != 500 || d.USD != 0.75 {
		t.Errorf("today = %+v", *d)
	}
	for i, want := range []string{"2026-09-10", "2026-09-09", "2026-09-08", "2026-09-04"} {
		if s.Days[i].Date != want {
			t.Errorf("day %d = %s, want %s", i, s.Days[i].Date, want)
		}
	}
	var buf bytes.Buffer
	s.Print(&buf)
	if out := buf.String(); !strings.Contains(out, "Per day (last 7 days") || !strings.Contains(out, "2026-09-10  2      1   3.0k   500     0.75") {
		t.Errorf("print:\n%s", out)
	}
}
