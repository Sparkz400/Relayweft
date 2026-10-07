package morning

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/notify"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// ParseAt reads notify.morning: a time of day, "07:30".
func ParseAt(v string) (hour, minute int, err error) {
	t, err := time.Parse("15:04", strings.TrimSpace(v))
	if err != nil {
		return 0, 0, fmt.Errorf("notify.morning %q: want a time of day like 07:30", v)
	}
	return t.Hour(), t.Minute(), nil
}

// Next is the next time of day h:m after now.
func Next(now time.Time, h, m int) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// Collect builds the summary from this machine's task states and session
// logs (all projects).
func Collect(cfg *config.Config, o Options) Summary {
	recs, _ := sessionlog.ReadDirSince(cfg.SessionDir(), o.Since) // a file another rw holds is skipped
	return Build(orchestrator.History("", 0), recs, o)
}

// Deliver posts the summary to the webhooks that want "summary" and as a
// desktop notification (when notifications are on). source names the
// machine or folder in the message.
func Deliver(ctx context.Context, cfg *config.Config, s Summary, source string) error {
	title := "Relayweft: " + s.Title()
	var errs []error
	if hooks := cfg.Notify.Webhooks; notify.Wanted(hooks, notify.EventSummary) {
		errs = append(errs, notify.Broadcast(ctx, hooks, notify.Message{Event: notify.EventSummary, Title: title, Body: s.Text(), Source: source}))
	}
	if cfg.Notify.Enabled {
		errs = append(errs, notify.Send("Relayweft: overnight", s.Title()))
	}
	return errors.Join(errs...)
}

// claim takes the day's marker in dir, so only one rw posts the summary;
// ok is false when another one did. Markers older than a week go.
func claim(dir string, day time.Time) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return true // no folder to coordinate in: post rather than never
	}
	old, _ := filepath.Glob(filepath.Join(dir, "morning-*.sent"))
	for _, f := range old {
		if st, err := os.Stat(f); err == nil && time.Since(st.ModTime()) > 7*24*time.Hour {
			os.Remove(f)
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, "morning-"+day.Format("2006-01-02")+".sent"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// Daily posts the summary of the day's unattended work at notify.morning
// for as long as ctx lives (rw web, the TUI and long rw runs start it):
// the tasks since the same time the day before. A night without
// unattended tasks sends nothing. cfg is read at each send.
func Daily(ctx context.Context, cfg func() *config.Config, source string, onErr func(error)) {
	daily(ctx, cfg, source, onErr, time.Now, Collect, Deliver, time.Minute)
}

func daily(ctx context.Context, cfg func() *config.Config, source string, onErr func(error), now func() time.Time,
	collect func(*config.Config, Options) Summary, deliver func(context.Context, *config.Config, Summary, string) error, every time.Duration) {
	for {
		c := cfg()
		h, m, err := ParseAt(c.Notify.Morning)
		if c.Notify.Morning == "" || err != nil {
			// Off (or broken): look again later, the config may change.
			if !sleep(ctx, every) {
				return
			}
			continue
		}
		at := Next(now(), h, m)
		// Re-read the wall clock at least every interval: a machine that
		// slept must not post hours late, nor miss the time.
		for now().Before(at) {
			if !sleep(ctx, min(at.Sub(now()), every)) {
				return
			}
			if c2 := cfg(); c2.Notify.Morning != c.Notify.Morning {
				break // changed: start over
			}
		}
		if now().Before(at) {
			continue
		}
		if now().Sub(at) > time.Hour {
			continue // woke up long after the time: tomorrow
		}
		c = cfg()
		if !claim(c.SessionDir(), at) {
			continue
		}
		s := collect(c, Options{Since: at.AddDate(0, 0, -1), Until: at})
		if s.Empty() {
			continue
		}
		if err := deliver(ctx, c, s, source); err != nil && onErr != nil {
			onErr(err)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(max(d, time.Millisecond))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
