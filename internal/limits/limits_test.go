package limits

import (
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/event"
)

var patterns = []string{"usage limit", "hit your (usage )?limit", "rate.?limit", "\\b429\\b"}

func TestDetector(t *testing.T) {
	d := NewDetector(patterns)
	for _, s := range []string{
		"You've hit your usage limit. Try again at 3:05 PM.",
		"Claude AI usage limit reached|1791003600",
		"HTTP 429 Too Many Requests",
		"rate_limit_error",
	} {
		if !d.Match(s) {
			t.Errorf("not detected: %q", s)
		}
	}
	if d.Match("TestParse failed: expected 3 got 2") {
		t.Error("false positive")
	}
}

func TestParseReset(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.Local)
	cases := []struct {
		in   string
		want time.Time
	}{
		{"try again at 3:05 PM", time.Date(2026, 10, 2, 15, 5, 0, 0, time.Local)},
		{"Try again at 9:00 am", time.Date(2026, 10, 3, 9, 0, 0, 0, time.Local)},
		{"try again in 2h 10m", now.Add(2*time.Hour + 10*time.Minute)},
		{"try again in 45 minutes", now.Add(45 * time.Minute)},
		{"usage limit reached|1791003600", time.Unix(1791003600, 0)},
	}
	for _, c := range cases {
		got, ok := ParseReset(c.in, now)
		if !ok || !got.Equal(c.want) {
			t.Errorf("ParseReset(%q) = %v %v, want %v", c.in, got, ok, c.want)
		}
	}
	if _, ok := ParseReset("no time here", now); ok {
		t.Error("parsed nothing as a time")
	}
}

func TestTracker(t *testing.T) {
	tr := NewTracker()
	now := time.Now()
	tr.SetClock(func() time.Time { return now })
	tr.MarkLimited(event.Codex, now.Add(time.Minute))
	if !tr.Limited(event.Codex) || tr.Limited(event.Claude) {
		t.Fatal("limit state wrong")
	}
	now = now.Add(2 * time.Minute)
	if tr.Limited(event.Codex) {
		t.Fatal("limit did not expire")
	}
	tr.AddUsage(event.Codex, event.TokenUsage{Input: 300, Output: 100})
	tr.AddUsage(event.Claude, event.TokenUsage{Input: 100})
	if s := tr.Share(event.Claude); s != 0.2 {
		t.Errorf("share = %v", s)
	}
	if s := tr.Snapshot(event.Codex); s.Calls != 1 || s.LimitHits != 1 {
		t.Errorf("snapshot = %+v", s)
	}
}
