// Package health turns Relayweft's logs into a reliability report: crashes,
// hangs, unclean exits, load peaks and leftovers over a window of days, and
// whether the Phase 1 exit criterion ("2 weeks of daily use with no crash
// and no hang") is met. `rw health` prints it; `rw web` shows it as a panel.
package health

import (
	"bufio"
	"bytes"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/diag"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/proc"
)

// Options selects the window and the criterion.
type Options struct {
	Dir         string    // log directory (default diag.DefaultDir())
	Days        int       // window and clean streak needed (default 14)
	MinUseDays  int       // days with use needed in the window (default 10)
	Now         time.Time // default time.Now()
	NoLeftovers bool      // skip the scan of pools and temp files (tests)
}

// Incident is one thing that went wrong.
type Incident struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"` // crash, fatal, hang, unclean, agent-timeout, pause, leftover
	PID    int       `json:"pid,omitempty"`
	Cmd    string    `json:"cmd,omitempty"` // the rw command of that process
	Detail string    `json:"detail,omitempty"`
	Log    string    `json:"log,omitempty"` // file with the details
}

// Load holds the worst readings in the window.
type Load struct {
	CPU         int       `json:"cpu"` // peak machine CPU %, -1 unknown
	CPUAt       time.Time `json:"cpu_at"`
	MemFreeMB   int64     `json:"mem_free_mb"` // lowest free RAM, -1 unknown
	MemTotalMB  int64     `json:"mem_total_mb"`
	MemAt       time.Time `json:"mem_at"`
	RwMemMB     int64     `json:"rw_mem_mb"` // most memory one rw process used
	RwMemAt     time.Time `json:"rw_mem_at"`
	Goroutines  int       `json:"goroutines"`
	Samples     int       `json:"samples"`         // 5-minute load readings
	HotSamples  int       `json:"hot_samples"`     // with CPU >= 95%
	LowMemFrees int       `json:"low_mem_samples"` // with under 10% RAM free
}

// Session is one rw process.
type Session struct {
	PID     int       `json:"pid"`
	Cmd     string    `json:"cmd"`
	Version string    `json:"version,omitempty"`
	Start   time.Time `json:"start"`
	Last    time.Time `json:"last"` // last record seen
	Ended   bool      `json:"ended"`
	OK      bool      `json:"ok"`
	Err     string    `json:"err,omitempty"`
	Running bool      `json:"running"`
	crashed bool
}

// Criterion is the Phase 1 exit criterion.
type Criterion struct {
	Met        bool      `json:"met"`
	CleanSince time.Time `json:"clean_since"` // last crash or hang, or the start of the records
	CleanDays  float64   `json:"clean_days"`
	NeedDays   int       `json:"need_days"`
	UseDays    int       `json:"use_days"` // days with use in the last NeedDays
	NeedUse    int       `json:"need_use_days"`
	Summary    string    `json:"summary"`
}

// Report is the whole picture.
type Report struct {
	Now              time.Time               `json:"now"`
	From             time.Time               `json:"from"`
	Days             int                     `json:"days"`
	Dir              string                  `json:"dir"`
	HealthSince      time.Time               `json:"health_since"` // first health log record (zero: none)
	DebugSince       time.Time               `json:"debug_since"`  // first debug log record used (zero: none)
	Sessions         int                     `json:"sessions"`
	UseDays          []string                `json:"use_days"` // 2006-01-02, days with use in the window
	Running          []Session               `json:"running"`
	Crashes          []Incident              `json:"crashes"`
	Hangs            []Incident              `json:"hangs"`
	Unclean          []Incident              `json:"unclean"`
	Timeouts         []Incident              `json:"agent_timeouts"`
	Pauses           []Incident              `json:"pauses"`
	Load             Load                    `json:"load"`
	Leftovers        []orchestrator.Leftover `json:"leftovers"`
	LeftoversChecked bool                    `json:"leftovers_checked"` // false: the scan was skipped (logs of another machine)
	LeftoverLogs     []Incident              `json:"leftover_events"`   // logged when found
	Criterion        Criterion               `json:"criterion"`
}

// notUse are commands that only look at rw; they do not count as a day of use.
var notUse = map[string]bool{
	"health": true, "doctor": true, "bugreport": true, "stats": true, "history": true,
	"report": true, "models": true, "update": true, "version": true, "help": true,
	"trust": true, "clean": true, "init": true,
}

// runningWithin: a process without an end line that is still alive and was
// seen this recently counts as running (older: its pid was reused).
const runningWithin = 24 * time.Hour

// Build reads the logs and returns the report.
func Build(o Options) (*Report, error) {
	if o.Dir == "" {
		o.Dir = diag.DefaultDir()
	}
	if o.Days <= 0 {
		o.Days = 14
	}
	if o.MinUseDays <= 0 {
		o.MinUseDays = min(10, o.Days)
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	r := &Report{Now: o.Now, Days: o.Days, Dir: o.Dir, From: o.Now.Add(-time.Duration(o.Days) * 24 * time.Hour),
		Load: Load{CPU: -1, MemFreeMB: -1}}
	recs, err := diag.ReadHealth(o.Dir)
	if err != nil {
		return nil, err
	}
	if len(recs) > 0 {
		r.HealthSince = recs[0].Time
	}
	useDays := map[string]bool{}
	var allIncidents []Incident // every crash and hang, also before the window

	// Older history from the debug log, for the time before the health log.
	before := r.HealthSince
	if before.IsZero() {
		before = o.Now.Add(time.Minute)
	}
	debugSessions, debugCrashes, debugTimeouts, debugSince := readDebug(o.Dir, before)
	r.DebugSince = debugSince
	for _, s := range debugSessions {
		if !s.Start.Before(r.From) {
			r.Sessions++
			if !notUse[s.Cmd] {
				useDays[s.Start.Format("2006-01-02")] = true
			}
		}
	}
	allIncidents = append(allIncidents, debugCrashes...)
	r.Timeouts = append(r.Timeouts, inWindow(debugTimeouts, r.From)...)

	// Sessions from the health log.
	open := map[int]*Session{}
	var sessions []*Session
	closeUnclean := func(s *Session) {
		if s != nil && !s.Ended {
			sessions = append(sessions, s)
		}
	}
	for _, rec := range recs {
		s := open[rec.PID]
		if rec.Kind == "start" {
			closeUnclean(s)
			s = &Session{PID: rec.PID, Cmd: rec.Fields["cmd"], Version: rec.Fields["ver"], Start: rec.Time, Last: rec.Time}
			open[rec.PID] = s
			continue
		}
		cmd := ""
		if s != nil {
			s.Last = rec.Time
			cmd = s.Cmd
		}
		inc := Incident{Time: rec.Time, PID: rec.PID, Cmd: cmd}
		switch rec.Kind {
		case "end":
			if s != nil {
				s.Ended, s.OK, s.Err = true, rec.Fields["ok"] == "true", rec.Fields["err"]
				sessions = append(sessions, s)
				delete(open, rec.PID)
			}
		case "panic":
			inc.Kind, inc.Detail, inc.Log = "crash", "panic in "+rec.Fields["in"], logPath(o.Dir, rec.Fields["log"])
			allIncidents = append(allIncidents, inc)
			if s != nil {
				s.crashed = true
			}
		case "hang":
			inc.Kind, inc.Log = "hang", logPath(o.Dir, rec.Fields["log"])
			inc.Detail = rec.Fields["in"] + " made no progress for " + rec.Fields["stuck"]
			allIncidents = append(allIncidents, inc)
		case "hang-end":
			// The hang was already counted; note how long it lasted.
			for i := len(allIncidents) - 1; i >= 0; i-- {
				if h := &allIncidents[i]; h.Kind == "hang" && h.PID == rec.PID && !strings.Contains(h.Detail, "recovered") {
					h.Detail += ", recovered after " + rec.Fields["after"]
					break
				}
			}
		case "agent-timeout":
			inc.Kind, inc.Detail = "agent-timeout", "agent "+rec.Fields["agent"]+" timed out after "+rec.Fields["after"]
			if !rec.Time.Before(r.From) {
				r.Timeouts = append(r.Timeouts, inc)
			}
		case "pause":
			inc.Kind, inc.Detail = "pause", "machine slept or froze for "+rec.Fields["for"]
			if !rec.Time.Before(r.From) {
				r.Pauses = append(r.Pauses, inc)
			}
		case "leftover":
			inc.Kind, inc.Detail = "leftover", rec.Fields["what"]+": "+rec.Fields["path"]
			if !rec.Time.Before(r.From) {
				r.LeftoverLogs = append(r.LeftoverLogs, inc)
			}
		case "load":
			if !rec.Time.Before(r.From) {
				r.Load.addRecord(rec)
			}
		}
		if rec.Kind == "end" && !rec.Time.Before(r.From) {
			r.Load.addRecord(rec) // short processes never write a load line
		}
	}
	for _, s := range open {
		closeUnclean(s)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Start.Before(sessions[j].Start) })

	// Fatal runtime errors (unrecoverable: concurrent map writes, out of
	// memory, stack overflow) leave a non-empty fatal-<pid>-<time>.log.
	fatal := fatalFiles(o.Dir)

	self := os.Getpid()
	for _, s := range sessions {
		if s.Last.Before(r.From) {
			continue
		}
		r.Sessions++
		if !notUse[s.Cmd] {
			for d := dayStart(maxTime(s.Start, r.From)); !d.After(s.Last); d = d.AddDate(0, 0, 1) {
				useDays[d.Format("2006-01-02")] = true
			}
		}
		if s.Ended {
			continue
		}
		if s.PID == self || (proc.Alive(s.PID) && o.Now.Sub(s.Last) < runningWithin) {
			if s.PID != self {
				s.Running = true
				r.Running = append(r.Running, *s)
			}
			continue
		}
		if f, ok := fatal.match(s); ok {
			allIncidents = append(allIncidents, Incident{Time: s.Last, Kind: "fatal", PID: s.PID, Cmd: s.Cmd, Detail: f.detail, Log: f.path})
			continue
		}
		if s.crashed {
			continue // the panic is counted already
		}
		r.Unclean = append(r.Unclean, Incident{Time: s.Last, Kind: "unclean", PID: s.PID, Cmd: s.Cmd,
			Detail: "ended without a trace after " + s.Last.Sub(s.Start).Round(time.Second).String() + ": killed, window closed, power lost, or a crash that left no output"})
	}
	// Fatal files no session claimed (e.g. from before the health log).
	for _, f := range fatal {
		if !f.used && !f.time.Before(r.From) {
			allIncidents = append(allIncidents, Incident{Time: f.time, Kind: "fatal", PID: f.pid, Detail: f.detail, Log: f.path})
		}
	}
	// Crash logs no panic record points to (e.g. from before the health log).
	known := map[string]bool{}
	for _, in := range allIncidents {
		if in.Log != "" {
			known[filepath.Base(in.Log)] = true
		}
	}
	crashFiles, _ := filepath.Glob(filepath.Join(o.Dir, "crash-*.log"))
	for _, p := range crashFiles {
		base := filepath.Base(p)
		if known[base] {
			continue
		}
		t, err := time.ParseInLocation("20060102-150405.000", strings.TrimSuffix(strings.TrimPrefix(base, "crash-"), ".log"), time.Local)
		if err != nil {
			continue
		}
		// a debug-log PANIC line for the same crash is replaced by the file
		dup := false
		for i, in := range allIncidents {
			if in.Kind == "crash" && in.Log == "" && absDur(in.Time.Sub(t)) < 5*time.Second {
				allIncidents[i].Log = p
				dup = true
				break
			}
		}
		if !dup && !fromTest(p) {
			allIncidents = append(allIncidents, Incident{Time: t, Kind: "crash", Detail: firstLine(p), Log: p})
		}
	}

	sort.Slice(allIncidents, func(i, j int) bool { return allIncidents[i].Time.Before(allIncidents[j].Time) })
	var last time.Time
	for _, in := range allIncidents {
		last = in.Time
		if in.Time.Before(r.From) {
			continue
		}
		if in.Kind == "hang" {
			r.Hangs = append(r.Hangs, in)
		} else {
			r.Crashes = append(r.Crashes, in)
		}
	}
	sortIncidents(r.Timeouts)
	for d := range useDays {
		r.UseDays = append(r.UseDays, d)
	}
	sort.Strings(r.UseDays)
	if !o.NoLeftovers {
		r.Leftovers = orchestrator.Leftovers()
		r.LeftoversChecked = true
	}
	r.Criterion = criterion(r, o, last)
	return r, nil
}

func criterion(r *Report, o Options, lastIncident time.Time) Criterion {
	c := Criterion{NeedDays: o.Days, NeedUse: o.MinUseDays, UseDays: len(r.UseDays)}
	first := r.HealthSince
	if !r.DebugSince.IsZero() && (first.IsZero() || r.DebugSince.Before(first)) {
		first = r.DebugSince
	}
	c.CleanSince = first
	if lastIncident.After(c.CleanSince) {
		c.CleanSince = lastIncident
	}
	if !c.CleanSince.IsZero() {
		c.CleanDays = r.Now.Sub(c.CleanSince).Hours() / 24
	}
	clean := c.CleanDays >= float64(o.Days)
	used := c.UseDays >= o.MinUseDays
	c.Met = clean && used
	switch {
	case c.CleanSince.IsZero():
		c.Summary = "no records yet: use rw for a while, then look again"
	case c.Met:
		c.Summary = "met: " + days(c.CleanDays) + " without a crash or hang, used on " + strconv.Itoa(c.UseDays) + " of the last " + strconv.Itoa(o.Days) + " days"
	default:
		var why []string
		if !clean {
			since := "since the records start"
			if !lastIncident.IsZero() && lastIncident.Equal(c.CleanSince) {
				since = "since the last crash or hang"
			}
			why = append(why, days(c.CleanDays)+" clean "+since+", "+days(math.Ceil(float64(o.Days)-c.CleanDays))+" to go")
		}
		if !used {
			why = append(why, "used on "+strconv.Itoa(c.UseDays)+" of the last "+strconv.Itoa(o.Days)+" days ("+strconv.Itoa(o.MinUseDays)+" needed)")
		}
		c.Summary = "not yet: " + strings.Join(why, "; ")
	}
	return c
}

func days(d float64) string {
	if d < 0 {
		d = 0
	}
	if d < 1 {
		h := int(d * 24)
		if h == 1 {
			return "1 hour"
		}
		return strconv.Itoa(h) + " hours"
	}
	if int(d) == 1 {
		return "1 day"
	}
	return strconv.Itoa(int(d)) + " days"
}

func (l *Load) addRecord(rec diag.Record) {
	f := rec.Fields
	if rec.Kind == "load" {
		l.Samples++
	}
	if v, err := strconv.Atoi(f["cpu"]); err == nil {
		if v > l.CPU {
			l.CPU, l.CPUAt = v, rec.Time
		}
		if rec.Kind == "load" && v >= 95 {
			l.HotSamples++
		}
	}
	if v, err := strconv.ParseInt(f["memfree_mb"], 10, 64); err == nil {
		total, _ := strconv.ParseInt(f["memtotal_mb"], 10, 64)
		if l.MemFreeMB < 0 || v < l.MemFreeMB {
			l.MemFreeMB, l.MemTotalMB, l.MemAt = v, total, rec.Time
		}
		if rec.Kind == "load" && total > 0 && v*10 < total {
			l.LowMemFrees++
		}
	}
	if v, err := strconv.ParseInt(f["symem_mb"], 10, 64); err == nil && v > l.RwMemMB {
		l.RwMemMB, l.RwMemAt = v, rec.Time
	}
	if v, err := strconv.Atoi(f["goroutines"]); err == nil && v > l.Goroutines {
		l.Goroutines = v
	}
}

type fatalFile struct {
	path   string
	pid    int
	time   time.Time
	detail string
	used   bool
}

type fatalSet []*fatalFile

func fatalFiles(dir string) fatalSet {
	files, _ := filepath.Glob(filepath.Join(dir, "fatal-*.log"))
	var out fatalSet
	for _, p := range files {
		st, err := os.Stat(p)
		if err != nil || st.Size() == 0 {
			continue // armed by a running or killed process, nothing written
		}
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "fatal-"), ".log")
		pidStr, ts, _ := strings.Cut(name, "-")
		pid, _ := strconv.Atoi(pidStr)
		t, err := time.ParseInLocation("20060102-150405", ts, time.Local)
		if err != nil {
			t = st.ModTime()
		}
		detail := firstLine(p)
		if detail == "" {
			continue
		}
		out = append(out, &fatalFile{path: p, pid: pid, time: t, detail: detail})
	}
	return out
}

func (fs fatalSet) match(s *Session) (*fatalFile, bool) {
	for _, f := range fs {
		if !f.used && f.pid == s.PID && absDur(f.time.Sub(s.Start)) < 2*time.Second {
			f.used = true
			return f, true
		}
	}
	return nil, false
}

// readDebug reads sessions, panics and agent timeouts from the debug log,
// for the time before the health log started.
func readDebug(dir string, before time.Time) (sessions []Session, crashes, timeouts []Incident, since time.Time) {
	const layout = "2006-01-02 15:04:05.000"
	for _, name := range []string{"rw-debug.log.1", "rw-debug.log"} {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			line := sc.Text()
			if len(line) < len(layout)+2 || line[0] == ' ' {
				continue
			}
			t, err := time.ParseInLocation(layout, line[:len(layout)], time.Local)
			if err != nil || !t.Before(before) {
				continue
			}
			if since.IsZero() || t.Before(since) {
				since = t
			}
			msg := line[len(layout)+1:]
			switch {
			case strings.HasPrefix(msg, "=== rw "):
				fs := strings.Fields(msg)
				s := Session{Start: t, Last: t}
				if len(fs) > 2 {
					s.Version = fs[2]
				}
				if len(fs) > 3 && !strings.HasPrefix(fs[3], "(") {
					s.Cmd = fs[3]
				}
				sessions = append(sessions, s)
			case strings.HasPrefix(msg, "PANIC in "):
				crashes = append(crashes, Incident{Time: t, Kind: "crash", Detail: "panic in " + strings.TrimPrefix(msg, "PANIC in ")})
			case strings.HasPrefix(msg, "exit agent=") && strings.Contains(msg, "err=timed out after"):
				agent, _, _ := strings.Cut(strings.TrimPrefix(msg, "exit agent="), " ")
				after := msg[strings.Index(msg, "err=timed out after ")+len("err=timed out after "):]
				after, _, _ = strings.Cut(after, " ")
				timeouts = append(timeouts, Incident{Time: t, Kind: "agent-timeout", Detail: "agent " + agent + " timed out after " + after})
			}
		}
		f.Close()
	}
	return
}

// fromTest reports whether a crash log was written by a Go test (it has a
// stack frame in a _test.go file): older versions put those among the
// user's real crash logs.
func fromTest(path string) bool {
	data, err := os.ReadFile(path)
	return err == nil && bytes.Contains(data, []byte("_test.go:"))
}

func logPath(dir, base string) string {
	if base == "" {
		return ""
	}
	return filepath.Join(dir, base)
}

func firstLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			if len(l) > 160 {
				l = l[:160] + "…"
			}
			return l
		}
	}
	return ""
}

func inWindow(in []Incident, from time.Time) []Incident {
	var out []Incident
	for _, x := range in {
		if !x.Time.Before(from) {
			out = append(out, x)
		}
	}
	return out
}

func sortIncidents(in []Incident) {
	sort.SliceStable(in, func(i, j int) bool { return in[i].Time.Before(in[j].Time) })
}

func dayStart(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
