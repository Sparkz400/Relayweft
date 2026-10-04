// Package schedule turns "02:30", "in 3h" and "when claude's limit resets"
// into start times, and waits for them. `sy run --at/--in/--when-reset`,
// the TUI's /schedule and the web UI's schedule share it.
package schedule

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/limits"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// ParseAt reads a start time in local time: a clock time "02:30" (today,
// or tomorrow when that time has already passed), a date and time
// "2026-10-04 02:30" (also with a T) or RFC3339. A date in the past is an
// error.
func ParseAt(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("empty time")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return future(t, now, s)
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, now.Location()); err == nil {
			return future(t, now, s)
		}
	}
	for _, layout := range []string{"15:04", "15:04:05", "3:04pm", "3:04 pm", "3pm", "3 pm"} {
		c, err := time.Parse(layout, strings.ToLower(s))
		if err != nil {
			continue
		}
		t := time.Date(now.Year(), now.Month(), now.Day(), c.Hour(), c.Minute(), c.Second(), 0, now.Location())
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("time %q: want HH:MM, \"2026-10-04 02:30\" or RFC3339", s)
}

func future(t, now time.Time, s string) (time.Time, error) {
	if !t.After(now) {
		return time.Time{}, fmt.Errorf("%s is in the past", s)
	}
	return t, nil
}

// ParseIn reads a delay: a Go duration ("3h", "90m", "1h30m") or days
// ("2d").
func ParseIn(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if n, ok := strings.CutSuffix(s, "d"); ok {
		if d, err := strconv.Atoi(n); err == nil && d >= 0 && d <= 366 {
			return time.Duration(d) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("delay %q: want e.g. 3h, 90m, 1h30m or 2d", s)
	}
	return d, nil
}

// ValidReset reports whether p is a --when-reset choice: "any" or one of
// the providers.
func ValidReset(p string, providers []string) bool {
	if p == "any" {
		return true
	}
	for _, c := range providers {
		if c == p {
			return true
		}
	}
	return false
}

// ResetTime is when a provider's usage limit resets, from what is known:
// this sy's limit tracker first, then the newest quota or limit record in
// the session logs. "any" is the earlier of the two providers (and now when
// either has no known future reset: that one is usable). A zero time means
// unknown or already past: start now. note says where the time came from.
func ResetTime(provider string, tr *limits.Tracker, recs []sessionlog.Record, now time.Time) (time.Time, string) {
	return ResetTimeIn(provider, []string{event.Claude, event.Codex}, tr, recs, now)
}

// ResetTimeIn is ResetTime with "any" meaning any of providers.
func ResetTimeIn(provider string, providers []string, tr *limits.Tracker, recs []sessionlog.Record, now time.Time) (time.Time, string) {
	if provider == "any" {
		var best time.Time
		for _, p := range providers {
			t, _ := ResetTimeIn(p, providers, tr, recs, now)
			if t.IsZero() {
				return time.Time{}, p + " has no known limit waiting to reset: starting now"
			}
			if best.IsZero() || t.Before(best) {
				best = t
			}
		}
		return best, "the first provider limit resets at " + Clock(best, now)
	}
	if tr != nil {
		s := tr.Snapshot(provider)
		if s.Limited(now) {
			return s.LimitedUntil, fmt.Sprintf("%s is at its limit until %s", provider, Clock(s.LimitedUntil, now))
		}
		if q := s.Quota; q != nil && q.ResetsAt.After(now) {
			return q.ResetsAt, fmt.Sprintf("%s's %s window resets at %s (%.0f%% used)", provider, windowName(q.Window), Clock(q.ResetsAt, now), q.Utilization*100)
		}
	}
	at, seen, ok := sessionlog.LatestReset(recs, provider)
	switch {
	case !ok:
		return time.Time{}, "no known " + provider + " limit reset in the logs: starting now"
	case !at.After(now):
		return time.Time{}, fmt.Sprintf("the last known %s reset (%s) has passed: starting now", provider, Clock(at, now))
	}
	return at, fmt.Sprintf("%s resets at %s (reported %s)", provider, Clock(at, now), Clock(seen, now))
}

func windowName(w string) string {
	switch w {
	case "five_hour":
		return "5h"
	case "seven_day":
		return "7-day"
	case "":
		return "usage"
	}
	return w
}

// Clock formats t as 15:04 when it is today, else with the date.
func Clock(t, now time.Time) string {
	t = t.In(now.Location())
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	if y1 == y2 && m1 == m2 && d1 == d2 {
		return t.Format("15:04")
	}
	if t.Sub(now) < 6*24*time.Hour && t.After(now) {
		return t.Format("Mon 15:04")
	}
	return t.Format("Jan 2 15:04")
}

// Left formats a wait: "2h13m", "4m", "30s".
func Left(d time.Duration) string {
	switch {
	case d >= time.Hour:
		d = d.Round(time.Minute)
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Round(time.Minute).Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
}

// ResetFunc looks up a provider's reset time (see ResetTime).
type ResetFunc func(provider string) (time.Time, string)

// ParseWords reads a start time from the front of a command
// ("/schedule in 2h fix it", "/schedule reset claude fix it",
// "/schedule 02:30 fix it", "/schedule 2026-10-04 02:30 fix it") and says
// how many words it used. A zero time means "now" (a reset that is unknown
// or past); note explains reset lookups. providers are the names "reset"
// accepts besides "any" (none given: claude and codex).
func ParseWords(words []string, now time.Time, reset ResetFunc, providers ...string) (at time.Time, used int, note string, err error) {
	if len(providers) == 0 {
		providers = []string{event.Claude, event.Codex}
	}
	if len(words) == 0 {
		return time.Time{}, 0, "", errors.New("when? e.g. 02:30, in 2h or reset claude")
	}
	switch strings.ToLower(words[0]) {
	case "in":
		if len(words) < 2 {
			return time.Time{}, 0, "", errors.New("in how long? e.g. in 2h")
		}
		d, err := ParseIn(words[1])
		if err != nil {
			return time.Time{}, 0, "", err
		}
		return now.Add(d), 2, "", nil
	case "reset", "when-reset":
		if len(words) < 2 || !ValidReset(strings.ToLower(words[1]), providers) {
			return time.Time{}, 0, "", fmt.Errorf("reset of which provider? reset %s|any", strings.Join(providers, "|"))
		}
		if reset == nil {
			return time.Time{}, 2, "reset time unknown: starting now", nil
		}
		t, note := reset(strings.ToLower(words[1]))
		return t, 2, note, nil
	}
	if len(words) >= 2 && len(words[0]) == len("2006-01-02") && strings.Count(words[0], "-") == 2 {
		if t, err := ParseAt(words[0]+" "+words[1], now); err == nil {
			return t, 2, "", nil
		}
	}
	if len(words) >= 2 {
		if l := strings.ToLower(words[1]); l == "am" || l == "pm" {
			if t, err := ParseAt(words[0]+" "+l, now); err == nil {
				return t, 2, "", nil
			}
		}
	}
	t, err := ParseAt(words[0], now)
	if err != nil {
		return time.Time{}, 0, "", err
	}
	return t, 1, "", nil
}

// Wait blocks until the wall clock reaches until (a zero or past time
// returns at once), calling report with the time left right away and then
// every interval. It returns ctx.Err() when ctx ends first.
func Wait(ctx context.Context, until time.Time, every time.Duration, report func(left time.Duration)) error {
	if every <= 0 {
		every = time.Minute
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		left := time.Until(until)
		if until.IsZero() || left <= 0 {
			return nil
		}
		if report != nil {
			report(left)
		}
		// Re-read the wall clock at least every interval: a machine that
		// slept anyway must not start hours late.
		t := time.NewTimer(min(left, every))
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}
