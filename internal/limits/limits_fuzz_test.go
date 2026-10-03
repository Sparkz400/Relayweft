package limits

import (
	"testing"
	"time"
)

// FuzzParseReset (ROADMAP 1.8): a limit message is CLI or model text, so
// any input must parse without panicking, and a parsed reset time must be
// usable: never in the past for "try again in/at", and at most a day ahead
// for a clock time.
func FuzzParseReset(f *testing.F) {
	for _, s := range []string{
		"try again at 3:05 PM",
		"Try again at 9:00 am",
		"try again in 2h 10m",
		"try again in 45 minutes",
		"usage limit reached|1791003600",
		"You've hit your usage limit. Try again in 1h 0m.",
		"Claude AI usage limit reached|1791003600",
		"resets at 12:00 am",
		"try again in 0h 0m",
		"no time here",
		"",
	} {
		f.Add(s, int64(1790950000))
	}
	f.Fuzz(func(t *testing.T, text string, nowSec int64) {
		// Keep "now" in a sane range (years 1970-2200).
		if nowSec < 0 || nowSec > 7258118400 {
			return
		}
		now := time.Unix(nowSec, 0)
		got, ok := ParseReset(text, now)
		if !ok {
			if !got.IsZero() {
				t.Fatalf("not ok but returned %v", got)
			}
			return
		}
		if got.IsZero() {
			t.Fatal("ok with a zero time")
		}
		if reEpoch.MatchString(text) {
			return // an absolute timestamp from the CLI is taken as given
		}
		if got.Before(now) {
			t.Fatalf("ParseReset(%q) = %v, before now %v", text, got, now)
		}
		if m := reTryIn.FindStringSubmatch(text); m != nil && (m[1] != "" || m[2] != "") {
			return // relative times have no fixed upper bound
		}
		if got.Sub(now) > 24*time.Hour {
			t.Fatalf("clock time %q parsed to %v, more than a day after %v", text, got, now)
		}
	})
}

// FuzzDetector: user-supplied patterns and arbitrary text never panic.
func FuzzDetector(f *testing.F) {
	f.Add("usage limit", "You've hit your usage limit.")
	f.Add("(", "x")
	f.Add("\\b429\\b", "HTTP 429")
	f.Fuzz(func(t *testing.T, pattern, text string) {
		_ = NewDetector([]string{pattern}).Match(text)
	})
}
