package orchestrator

import (
	"time"

	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// Team budget (budget.team): several machines share a day budget through
// a folder they all reach (OneDrive, a network share).
//
//   - After every task, rw writes this machine's usage of the last days
//     (the `rw stats --json` format, without task texts or paths) to
//     <folder>/<machine id>.json, atomically. It never writes another
//     machine's file.
//   - Before an agent starts, the budget check adds the other machines'
//     totals for today (their files, re-read at most every minute) to this
//     machine's own (its session logs, plus the running task). At a team
//     limit the usual budget flow applies: an attended task asks, an
//     unattended one stops.
//   - A file that is broken, stale, of an unknown version or written by
//     another machine than its name says is skipped with a warning (once
//     per file and problem); a folder that cannot be read counts as no
//     other machine. Neither ever stops a task by itself.
//   - "Today" is each machine's local date: machines in other time zones
//     count their own day.

// teamExportDays is how many days of usage this machine's team file holds
// (enough for a weekly `rw stats --merge <folder>`).
const teamExportDays = 7

// teamCache holds the other machines' totals for today.
type teamCache struct {
	date   string
	tokens int64
	usd    float64
	read   time.Time
}

// teamFolder is the configured team folder ("" when off or invalid; an
// invalid one is logged once).
func (o *Orchestrator) teamFolder(warn bool) string {
	if o.opts.Store == nil {
		return ""
	}
	d, err := o.opts.Store.Budget().Team.Folder()
	if err != nil {
		if warn {
			o.teamWarn("budget: " + err.Error() + "; the team budget counts only this machine")
		}
		return ""
	}
	return d
}

// teamWarn logs a team folder problem once per process.
func (o *Orchestrator) teamWarn(msg string) {
	o.mu.Lock()
	if o.teamWarned == nil {
		o.teamWarned = map[string]bool{}
	}
	seen := o.teamWarned[msg]
	o.teamWarned[msg] = true
	o.mu.Unlock()
	if !seen {
		o.logf("%s", msg)
	}
}

// teamTotals returns the other machines' totals for today: cached, re-read
// when the day changed or the cache is older than maxAge.
func (o *Orchestrator) teamTotals(now time.Time, maxAge time.Duration, warn bool) teamCache {
	date := now.Local().Format("2006-01-02")
	o.mu.Lock()
	tc := o.team
	o.mu.Unlock()
	if tc.date == date && now.Sub(tc.read) < maxAge {
		return tc
	}
	tc = teamCache{date: date, read: now}
	if dir := o.teamFolder(warn); dir != "" {
		me, err := sessionlog.MachineID()
		if err != nil && warn {
			o.teamWarn("budget: no machine id (" + err.Error() + "); the team folder is read without leaving out this machine's file")
		}
		exps, warns, err := sessionlog.ReadTeamDir(dir, me, now)
		if err != nil && warn {
			o.teamWarn("budget: cannot read the team folder " + dir + " (" + err.Error() + "); the team budget counts only this machine")
		}
		for _, w := range warns {
			if warn {
				o.teamWarn("budget: team folder " + dir + ": " + w)
			}
		}
		tc.tokens, tc.usd = sessionlog.TeamDay(exps, date)
	}
	o.mu.Lock()
	o.team = tc
	o.mu.Unlock()
	return tc
}

// writeTeam writes this machine's team file after a task (its task_end
// record is in the session log already).
func (o *Orchestrator) writeTeam() {
	dir := o.teamFolder(true)
	logDir := o.logDir()
	if dir == "" || logDir == "" {
		return
	}
	me, err := sessionlog.MachineID()
	if err != nil {
		o.teamWarn("budget: no machine id (" + err.Error() + "); this machine's usage is not written to the team folder")
		return
	}
	now := time.Now()
	since := sessionlog.DayStart(now).AddDate(0, 0, -(teamExportDays - 1))
	recs, rerr := sessionlog.ReadDirSince(logDir, since)
	if rerr != nil && len(recs) == 0 {
		o.teamWarn("budget: cannot read the session logs for the team folder: " + rerr.Error())
		return
	}
	e := sessionlog.BuildExport(recs, sessionlog.ExportOptions{Machine: me, Filter: sessionlog.Filter{Since: since}, Now: now})
	if err := sessionlog.WriteTeamFile(dir, e); err != nil {
		o.teamWarn("budget: cannot write this machine's usage to the team folder (" + err.Error() + ")")
	}
}
