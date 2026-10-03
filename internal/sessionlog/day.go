package sessionlog

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/sparkz400/switchyard/internal/event"
)

// DayStart is local midnight of t's day.
func DayStart(t time.Time) time.Time {
	t = t.Local()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
}

// ReadDirSince is ReadDir limited to log files written to since a time (a
// file's records are never newer than the file), so the budget check does
// not parse months of logs. A file that cannot be read is skipped: the
// records of the others are returned together with the error.
func ReadDirSince(dir string, since time.Time) ([]Record, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	var out []Record
	var errs []error
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil || st.ModTime().Before(since) {
			continue
		}
		recs, err := readFile(f)
		if err != nil {
			// One unreadable file (locked by another process on Windows,
			// say) must not hide the others: keep going, report it.
			errs = append(errs, err)
			continue
		}
		out = append(out, recs...)
	}
	return out, errors.Join(errs...)
}

// DayUsage totals the finished tasks (task_end records) of the local day
// that contains now: fresh tokens on both providers and Claude's
// API-equivalent price.
func DayUsage(recs []Record, now time.Time) (tokens int64, usd float64) {
	start := DayStart(now)
	end := start.AddDate(0, 0, 1)
	for _, r := range recs {
		if r.Type != TypeTaskEnd || r.TS.Before(start) || !r.TS.Before(end) {
			continue
		}
		if r.Cost != nil {
			for _, u := range r.Cost.PerProvider {
				tokens += u.Total()
			}
			usd += r.Cost.CostUSD
		} else if r.Tokens != nil {
			tokens += r.Tokens.Total()
			usd += r.Tokens.CostUSD
		}
	}
	return tokens, usd
}

// LatestReset finds when a provider's usage limit resets according to the
// newest record that says so: a quota record (resets_at) or a limit record
// (until). ok is false when no record carries a reset time.
func LatestReset(recs []Record, provider string) (at time.Time, seen time.Time, ok bool) {
	for _, r := range recs {
		if r.Provider != provider || r.TS.Before(seen) {
			continue
		}
		var t time.Time
		switch {
		case r.Type == TypeQuota && r.Quota != nil:
			t = r.Quota.ResetsAt
		case r.Type == TypeLimit && r.Until != nil:
			t = *r.Until
		}
		if t.IsZero() {
			continue
		}
		at, seen, ok = t, r.TS, true
	}
	return at, seen, ok
}

// QuotaChanged reports whether a new quota reading is worth a log record:
// the window, its reset time or the status changed, or the usage moved by
// at least 5 points.
func QuotaChanged(prev *event.QuotaInfo, q event.QuotaInfo) bool {
	if prev == nil {
		return true
	}
	d := prev.Utilization - q.Utilization
	return prev.Window != q.Window || prev.Status != q.Status || !prev.ResetsAt.Equal(q.ResetsAt) || d >= 0.05 || d <= -0.05
}
