package schedule

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/limits"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

var loc = time.FixedZone("test", 2*3600)

func at(y int, mo time.Month, d, h, mi int) time.Time { return time.Date(y, mo, d, h, mi, 0, 0, loc) }

func TestParseAt(t *testing.T) {
	now := at(2026, 10, 3, 14, 0)
	cases := []struct {
		in   string
		want time.Time
	}{
		{"23:00", at(2026, 10, 3, 23, 0)}, // later today
		{"02:30", at(2026, 10, 4, 2, 30)}, // past today: tomorrow
		{"14:00", at(2026, 10, 4, 14, 0)}, // exactly now: tomorrow
		{"2:30", at(2026, 10, 4, 2, 30)},  // one-digit hour
		{"3pm", at(2026, 10, 3, 15, 0)},   // 12-hour clock
		{"2026-10-04 02:30", at(2026, 10, 4, 2, 30)},
		{"2026-10-04T02:30", at(2026, 10, 4, 2, 30)},
		{"2026-10-05T01:00:00+02:00", at(2026, 10, 5, 1, 0)},
		{"2026-10-04T00:30:00Z", at(2026, 10, 4, 2, 30)}, // RFC3339 in UTC
	}
	for _, c := range cases {
		got, err := ParseAt(c.in, now)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("ParseAt(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "25:00", "tomorrow", "2026-10-02 10:00", "2026-10-03T10:00:00+02:00"} {
		if _, err := ParseAt(bad, now); err == nil {
			t.Errorf("ParseAt(%q) accepted", bad)
		}
	}
	if _, err := ParseAt("2026-10-02 10:00", now); err == nil || !strings.Contains(err.Error(), "past") {
		t.Errorf("past date: %v", err)
	}
}

func TestParseIn(t *testing.T) {
	for in, want := range map[string]time.Duration{"3h": 3 * time.Hour, "90m": 90 * time.Minute, "1h30m": 90 * time.Minute, "2d": 48 * time.Hour, "0s": 0} {
		if got, err := ParseIn(in); err != nil || got != want {
			t.Errorf("ParseIn(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "soon", "-1h", "xd"} {
		if _, err := ParseIn(bad); err == nil {
			t.Errorf("ParseIn(%q) accepted", bad)
		}
	}
}

func TestParseWords(t *testing.T) {
	now := at(2026, 10, 3, 14, 0)
	reset := func(p string) (time.Time, string) { return at(2026, 10, 3, 19, 0), p + " resets at 19:00" }
	cases := []struct {
		words string
		want  time.Time
		used  int
	}{
		{"in 2h fix it", now.Add(2 * time.Hour), 2},
		{"23:00 fix it", at(2026, 10, 3, 23, 0), 1},
		{"2026-10-04 02:30 fix it", at(2026, 10, 4, 2, 30), 2},
		{"3 pm fix it", at(2026, 10, 3, 15, 0), 2},
		{"reset claude fix it", at(2026, 10, 3, 19, 0), 2},
	}
	for _, c := range cases {
		got, used, _, err := ParseWords(strings.Fields(c.words), now, reset)
		if err != nil || !got.Equal(c.want) || used != c.used {
			t.Errorf("%q: %v used %d err %v; want %v used %d", c.words, got, used, err, c.want, c.used)
		}
	}
	for _, bad := range []string{"", "in", "in soon x", "reset bard x", "whenever x"} {
		if _, _, _, err := ParseWords(strings.Fields(bad), now, reset); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestResetTimeFromLogs(t *testing.T) {
	now := time.Now()
	quota := func(ago time.Duration, resets time.Time) sessionlog.Record {
		return sessionlog.Record{Type: sessionlog.TypeQuota, TS: now.Add(-ago), Provider: event.Claude,
			Quota: &event.QuotaInfo{Utilization: 0.97, Window: "five_hour", ResetsAt: resets}}
	}
	until := now.Add(3 * time.Hour)
	limit := sessionlog.Record{Type: sessionlog.TypeLimit, TS: now.Add(-time.Hour), Provider: event.Codex, Until: &until}

	// The newest Claude record wins.
	recs := []sessionlog.Record{quota(3*time.Hour, now.Add(-time.Hour)), quota(10*time.Minute, now.Add(90*time.Minute)), limit}
	got, note := ResetTime(event.Claude, nil, recs, now)
	if !got.Equal(now.Add(90*time.Minute)) || !strings.Contains(note, "claude resets at") {
		t.Errorf("claude = %v (%s)", got, note)
	}
	// A codex limit record carries its reset time.
	if got, _ := ResetTime(event.Codex, nil, recs, now); !got.Equal(until) {
		t.Errorf("codex = %v", got)
	}
	// any: the earlier of the two.
	if got, _ := ResetTime("any", nil, recs, now); !got.Equal(now.Add(90 * time.Minute)) {
		t.Errorf("any = %v", got)
	}
	// Past reset: start now and say so.
	got, note = ResetTime(event.Claude, nil, []sessionlog.Record{quota(6*time.Hour, now.Add(-time.Hour))}, now)
	if !got.IsZero() || !strings.Contains(note, "has passed") {
		t.Errorf("past reset = %v (%s)", got, note)
	}
	// Unknown: start now.
	got, note = ResetTime(event.Claude, nil, nil, now)
	if !got.IsZero() || !strings.Contains(note, "starting now") {
		t.Errorf("unknown = %v (%s)", got, note)
	}
	// any with one provider free: now.
	if got, _ := ResetTime("any", nil, []sessionlog.Record{limit}, now); !got.IsZero() {
		t.Errorf("any with claude free = %v", got)
	}
	// The live tracker comes before the logs.
	tr := limits.NewTracker()
	tr.MarkLimited(event.Claude, now.Add(4*time.Hour))
	if got, _ := ResetTime(event.Claude, tr, recs, now); !got.Equal(now.Add(4 * time.Hour)) {
		t.Errorf("tracker limit = %v", got)
	}
	tr.Clear(event.Claude)
	tr.SetQuota(event.Claude, event.QuotaInfo{Utilization: 0.5, ResetsAt: now.Add(30 * time.Minute)})
	if got, _ := ResetTime(event.Claude, tr, recs, now); !got.Equal(now.Add(30 * time.Minute)) {
		t.Errorf("tracker quota = %v", got)
	}
}

func TestWait(t *testing.T) {
	// A past or zero time returns at once, without a countdown.
	calls := 0
	if err := Wait(context.Background(), time.Now().Add(-time.Minute), time.Millisecond, func(time.Duration) { calls++ }); err != nil || calls != 0 {
		t.Fatalf("past: %v, %d reports", err, calls)
	}
	if err := Wait(context.Background(), time.Time{}, time.Millisecond, nil); err != nil {
		t.Fatal(err)
	}
	// Reports while waiting, then returns when the time comes.
	start := time.Now()
	if err := Wait(context.Background(), start.Add(60*time.Millisecond), 10*time.Millisecond, func(time.Duration) { calls++ }); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 50*time.Millisecond || calls < 2 {
		t.Errorf("returned after %v with %d reports", time.Since(start), calls)
	}
	// Cancelling ends the wait.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Wait(ctx, time.Now().Add(time.Hour), time.Minute, nil) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not end the wait")
	}
}

func TestLeftAndClock(t *testing.T) {
	if Left(2*time.Hour+13*time.Minute) != "2h13m" || Left(4*time.Minute) != "4m" || Left(30*time.Second) != "30s" {
		t.Error(Left(2*time.Hour+13*time.Minute), Left(4*time.Minute), Left(30*time.Second))
	}
	now := at(2026, 10, 3, 14, 0)
	if Clock(at(2026, 10, 3, 23, 0), now) != "23:00" || Clock(at(2026, 10, 4, 2, 30), now) != "Sun 02:30" {
		t.Error(Clock(at(2026, 10, 3, 23, 0), now), Clock(at(2026, 10, 4, 2, 30), now))
	}
}
