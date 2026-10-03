package sessionlog

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sparkz400/switchyard/internal/event"
)

// Usage exports (`sy stats --json`) let a team add up what several
// machines used, and carry the team budget (budget.team):
//
//   - An export holds per-day totals of one machine: finished tasks, fresh
//     tokens per provider and per model, API-equivalent $ and limit hits.
//     It never holds task texts, prompts or paths unless asked for
//     (--with-tasks), and the machine is a random id, not its host or
//     user name (--name adds a label of your choice).
//   - The format is versioned "major.minor". Readers accept every minor of
//     the major they know (new fields only add) and reject another major
//     with a clear error.
//   - Merging keeps one entry per machine and day, the newest export's, so
//     merging the same file twice, or an older and a newer export of one
//     machine, counts each day once.

// Export format identifiers.
const (
	ExportFormat  = "switchyard-stats"
	ExportVersion = "1.0"
	exportMajor   = "1"
)

// Export is one machine's usage, the `sy stats --json` file.
type Export struct {
	Format    string    `json:"format"`  // always "switchyard-stats"
	Version   string    `json:"version"` // "major.minor", see ExportVersion
	Machine   string    `json:"machine"` // random id of this sy installation (MachineID)
	Name      string    `json:"name,omitempty"`
	Generated time.Time `json:"generated"`
	// Since is where the export starts (zero: every log there was).
	Since time.Time   `json:"since,omitempty"`
	Days  []ExportDay `json:"days"` // oldest first
	// Tasks are listed only with --with-tasks (they hold task texts and
	// project folders).
	Tasks []ExportTask `json:"tasks,omitempty"`
}

// ExportDay is one local calendar day of one machine.
type ExportDay struct {
	Date  string `json:"date"`  // YYYY-MM-DD in the machine's local time
	Tasks int    `json:"tasks"` // finished tasks
	OK    int    `json:"ok"`
	// FreshTokens and USD total the finished tasks (what budget.day_tokens
	// and budget.day_usd count); Providers splits the tokens.
	FreshTokens int64            `json:"fresh_tokens"`
	USD         float64          `json:"usd"`
	Providers   map[string]int64 `json:"providers,omitempty"`
	LimitHits   int              `json:"limit_hits"`
	// Models are the agent runs of the day per provider and model (runs
	// of tasks still going count here before their task does).
	Models []ExportModel `json:"models,omitempty"`
}

// ExportModel is one provider:model's agent runs on one day.
type ExportModel struct {
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	Calls       int     `json:"calls"`
	OK          int     `json:"ok"`
	FreshTokens int64   `json:"fresh_tokens"`
	USD         float64 `json:"usd"`
	LimitHits   int     `json:"limit_hits"`
}

// ExportTask is one finished task (only with --with-tasks).
type ExportTask struct {
	TS          time.Time `json:"ts"`
	ID          string    `json:"id,omitempty"`
	Task        string    `json:"task"`
	Dir         string    `json:"dir,omitempty"`
	Mode        string    `json:"mode,omitempty"`
	OK          bool      `json:"ok"`
	FreshTokens int64     `json:"fresh_tokens"`
	USD         float64   `json:"usd"`
}

// ExportOptions select what BuildExport puts in.
type ExportOptions struct {
	Machine   string
	Name      string
	Filter    Filter
	WithTasks bool
	Now       time.Time // Generated (zero: now)
}

// BuildExport totals records per local day.
func BuildExport(recs []Record, o ExportOptions) Export {
	e := Export{Format: ExportFormat, Version: ExportVersion, Machine: o.Machine, Name: o.Name, Generated: o.Now, Since: o.Filter.Since}
	if e.Generated.IsZero() {
		e.Generated = time.Now()
	}
	type modelKey struct{ date, provider, model string }
	days := map[string]*ExportDay{}
	models := map[modelKey]*ExportModel{}
	day := func(ts time.Time) *ExportDay {
		date := ts.Local().Format("2006-01-02")
		d := days[date]
		if d == nil {
			d = &ExportDay{Date: date, Providers: map[string]int64{}}
			days[date] = d
		}
		return d
	}
	for _, r := range recs {
		if !o.Filter.keep(r) {
			continue
		}
		switch r.Type {
		case TypeAgentEnd:
			d := day(r.TS)
			key := modelKey{d.Date, r.Provider, r.Model}
			m := models[key]
			if m == nil {
				m = &ExportModel{Provider: r.Provider, Model: r.Model}
				models[key] = m
			}
			m.Calls++
			if r.OK != nil && *r.OK {
				m.OK++
			}
			if r.LimitHit {
				m.LimitHits++
				d.LimitHits++
			}
			if r.Tokens != nil {
				m.FreshTokens += r.Tokens.Total()
				m.USD += r.Tokens.CostUSD
			}
		case TypeTaskEnd:
			d := day(r.TS)
			d.Tasks++
			ok := r.OK != nil && *r.OK
			if ok {
				d.OK++
			}
			tokens, usd := taskUsage(r)
			d.FreshTokens += tokens
			d.USD += usd
			if r.Cost != nil {
				for p, u := range r.Cost.PerProvider {
					d.Providers[p] += u.Total()
				}
			}
			if o.WithTasks {
				e.Tasks = append(e.Tasks, ExportTask{TS: r.TS, ID: r.TaskID, Task: r.Task, Dir: r.Cwd, Mode: r.Mode, OK: ok, FreshTokens: tokens, USD: usd})
			}
		}
	}
	for key, m := range models {
		days[key.date].Models = append(days[key.date].Models, *m)
	}
	for _, d := range days {
		sortModels(d.Models)
		if len(d.Providers) == 0 {
			d.Providers = nil
		}
		e.Days = append(e.Days, *d)
	}
	sort.Slice(e.Days, func(i, j int) bool { return e.Days[i].Date < e.Days[j].Date })
	sort.Slice(e.Tasks, func(i, j int) bool { return e.Tasks[i].TS.Before(e.Tasks[j].TS) })
	if e.Days == nil {
		e.Days = []ExportDay{}
	}
	return e
}

// taskUsage is a task_end record's fresh tokens and $ (as DayUsage counts
// them).
func taskUsage(r Record) (tokens int64, usd float64) {
	if r.Cost != nil {
		for _, u := range r.Cost.PerProvider {
			tokens += u.Total()
		}
		return tokens, r.Cost.CostUSD
	}
	if r.Tokens != nil {
		return r.Tokens.Total(), r.Tokens.CostUSD
	}
	return 0, 0
}

func sortModels(ms []ExportModel) {
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].Provider != ms[j].Provider {
			return ms[i].Provider < ms[j].Provider
		}
		return ms[i].Model < ms[j].Model
	})
}

// Marshal is the export as indented JSON.
func (e Export) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(e, "", "  ")
	return append(b, '\n'), err
}

// reMachine is what a machine id may look like: it names a file in the
// team folder, so no separators or dots.
var reMachine = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{3,63}$`)

// ParseExport reads an export. It rejects other files and another major
// version.
func ParseExport(data []byte) (Export, error) {
	var head struct {
		Format  string `json:"format"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return Export{}, fmt.Errorf("not a Switchyard stats export: %w", err)
	}
	if head.Format != ExportFormat {
		return Export{}, fmt.Errorf("not a Switchyard stats export (format %q)", head.Format)
	}
	if major, _, _ := strings.Cut(head.Version, "."); major != exportMajor {
		return Export{}, fmt.Errorf("export version %q is not supported: this sy reads version %s.x (a newer sy wrote it? update with sy update)", head.Version, exportMajor)
	}
	var e Export
	if err := json.Unmarshal(data, &e); err != nil {
		return Export{}, fmt.Errorf("broken Switchyard stats export: %w", err)
	}
	if !reMachine.MatchString(e.Machine) {
		return Export{}, fmt.Errorf("export has no valid machine id (%q)", e.Machine)
	}
	for _, d := range e.Days {
		if _, err := time.Parse("2006-01-02", d.Date); err != nil {
			return Export{}, fmt.Errorf("export of machine %s: bad date %q", e.Machine, d.Date)
		}
		if err := d.check(); err != nil {
			return Export{}, fmt.Errorf("export of machine %s, %s: %w", e.Machine, d.Date, err)
		}
	}
	for _, t := range e.Tasks {
		if t.FreshTokens < 0 || !validUSD(t.USD) {
			return Export{}, fmt.Errorf("export of machine %s: task at %s has negative or invalid usage", e.Machine, t.TS.Format(time.RFC3339))
		}
	}
	return e, nil
}

// check rejects negative counts and $ (and NaN or infinite $): another
// machine's file must not lower the team's totals below what was used.
func (d ExportDay) check() error {
	if d.Tasks < 0 || d.OK < 0 || d.FreshTokens < 0 || d.LimitHits < 0 || !validUSD(d.USD) {
		return errors.New("negative or invalid totals")
	}
	for p, n := range d.Providers {
		if n < 0 {
			return fmt.Errorf("negative tokens for provider %q", p)
		}
	}
	for _, m := range d.Models {
		if m.Calls < 0 || m.OK < 0 || m.FreshTokens < 0 || m.LimitHits < 0 || !validUSD(m.USD) {
			return fmt.Errorf("negative or invalid usage for model %q", m.Model)
		}
	}
	return nil
}

func validUSD(f float64) bool { return f >= 0 && !math.IsInf(f, 0) } // NaN >= 0 is false

// exportMaxSize caps what is read of one export (a year of days is far
// less; anything bigger is not ours).
const exportMaxSize = 8 << 20

// ReadExport reads and parses one export file. It must be a regular file:
// a FIFO or device (in a shared team folder, say) would block or never
// end. The open does not wait for a FIFO's writer (openNoBlock).
func ReadExport(path string) (Export, error) {
	f, err := openNoBlock(path)
	if err != nil {
		return Export{}, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil {
		return Export{}, fmt.Errorf("%s: %w", path, err)
	} else if !st.Mode().IsRegular() {
		return Export{}, fmt.Errorf("%s: not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, exportMaxSize+1))
	if err != nil {
		return Export{}, fmt.Errorf("%s: %w", path, err)
	}
	if len(data) > exportMaxSize {
		return Export{}, fmt.Errorf("%s: larger than %d MB, not a stats export", path, exportMaxSize>>20)
	}
	e, err := ParseExport(data)
	if err != nil {
		return Export{}, fmt.Errorf("%s: %w", path, err)
	}
	return e, nil
}

// MergeExports combines exports into one per machine: each day of a
// machine comes from the newest export that has it, so merging is
// idempotent. Machines are sorted by id, days oldest first.
func MergeExports(exps []Export) []Export {
	type dayFrom struct {
		day ExportDay
		gen time.Time
	}
	byMachine := map[string]*Export{}
	days := map[string]map[string]dayFrom{}
	tasks := map[string]map[string]ExportTask{}
	for _, e := range exps {
		m := byMachine[e.Machine]
		if m == nil {
			m = &Export{Format: ExportFormat, Version: ExportVersion, Machine: e.Machine}
			byMachine[e.Machine] = m
			days[e.Machine] = map[string]dayFrom{}
			tasks[e.Machine] = map[string]ExportTask{}
		}
		if e.Generated.After(m.Generated) {
			m.Generated = e.Generated
			if e.Name != "" {
				m.Name = e.Name
			}
		} else if m.Name == "" {
			m.Name = e.Name
		}
		if m.Since.IsZero() || (!e.Since.IsZero() && e.Since.Before(m.Since)) {
			m.Since = e.Since
		}
		for _, d := range e.Days {
			if prev, ok := days[e.Machine][d.Date]; !ok || e.Generated.After(prev.gen) {
				days[e.Machine][d.Date] = dayFrom{d, e.Generated}
			}
		}
		for _, t := range e.Tasks {
			tasks[e.Machine][t.TS.UTC().Format(time.RFC3339Nano)+"\x00"+t.ID+"\x00"+t.Task] = t
		}
	}
	var out []Export
	for id, m := range byMachine {
		for _, d := range days[id] {
			m.Days = append(m.Days, d.day)
		}
		sort.Slice(m.Days, func(i, j int) bool { return m.Days[i].Date < m.Days[j].Date })
		for _, t := range tasks[id] {
			m.Tasks = append(m.Tasks, t)
		}
		sort.Slice(m.Tasks, func(i, j int) bool { return m.Tasks[i].TS.Before(m.Tasks[j].TS) })
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Machine < out[j].Machine })
	return out
}

// DayTotal is one machine's finished tasks on date.
func (e Export) DayTotal(date string) (tokens int64, usd float64) {
	for _, d := range e.Days {
		if d.Date == date {
			tokens += d.FreshTokens
			usd += d.USD
		}
	}
	return tokens, usd
}

// MachineIDFile is where this installation's machine id lives (tests
// point it elsewhere).
var MachineIDFile = func() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "switchyard", "machine-id")
	}
	return filepath.Join(os.TempDir(), "switchyard-machine-id")
}

// MachineID returns this installation's random id, created on first use.
// It says nothing about the machine (no host or user name).
func MachineID() (string, error) {
	p := MachineIDFile()
	if data, err := os.ReadFile(p); err == nil {
		if id := strings.TrimSpace(string(data)); reMachine.MatchString(id) {
			return id, nil
		}
		// A damaged file: replace it.
		os.Remove(p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errors.Is(err, os.ErrExist) {
		// Another sy created it just now: use theirs.
		data, rerr := os.ReadFile(p)
		if id := strings.TrimSpace(string(data)); rerr == nil && reMachine.MatchString(id) {
			return id, nil
		}
		return "", fmt.Errorf("machine id %s: unreadable", p)
	}
	if err != nil {
		return "", err
	}
	_, werr := f.WriteString(id + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(p)
		return "", werr
	}
	return id, nil
}

// WriteTeamFile writes e as <dir>/<machine>.json atomically (a temporary
// file in the same folder, then a rename), so a reader on another machine
// never sees half a file. It writes only this machine's own file.
func WriteTeamFile(dir string, e Export) error {
	if !reMachine.MatchString(e.Machine) {
		return fmt.Errorf("invalid machine id %q", e.Machine)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := e.Marshal()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+e.Machine+".json.tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if serr := f.Sync(); werr == nil {
		werr = serr
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, TeamFile(dir, e.Machine))
	}
	if werr != nil {
		os.Remove(tmp)
	}
	return werr
}

// TeamFile is a machine's file in the team folder.
func TeamFile(dir, machine string) string { return filepath.Join(dir, machine+".json") }

// teamStaleAfter: a team file not written for this long is reported (a
// machine that left the team, or a sync that stopped).
const teamStaleAfter = 7 * 24 * time.Hour

// ReadTeamDir reads every machine's export in the team folder except
// skip's (this machine's own, which the caller knows better). Files that
// cannot be read, are not exports, name another machine than their file
// name, or are stale are left out, each with a warning: a broken file of
// one machine must not stop everyone's work.
func ReadTeamDir(dir, skip string, now time.Time) (exps []Export, warnings []string, err error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, nil, err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(files)
	for _, f := range files {
		base := strings.TrimSuffix(filepath.Base(f), ".json")
		if base == skip || strings.HasPrefix(base, ".") {
			continue
		}
		if err := RegularFile(f); err != nil {
			warnings = append(warnings, fmt.Sprintf("skipped %s: %v", filepath.Base(f), err))
			continue
		}
		e, err := ReadExport(f)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipped %s: %v", filepath.Base(f), trimPath(err, f)))
			continue
		}
		switch {
		case e.Machine != base:
			warnings = append(warnings, fmt.Sprintf("skipped %s: it holds machine %s (each machine writes only <its id>.json)", filepath.Base(f), e.Machine))
			continue
		case e.Machine == skip:
			continue
		case now.Sub(e.Generated) > teamStaleAfter:
			warnings = append(warnings, fmt.Sprintf("skipped %s: stale (last written %s)", filepath.Base(f), e.Generated.Local().Format("2006-01-02 15:04")))
			continue
		case e.Generated.Sub(now) > 24*time.Hour:
			warnings = append(warnings, fmt.Sprintf("skipped %s: written in the future (%s; a clock is wrong)", filepath.Base(f), e.Generated.Local().Format("2006-01-02 15:04")))
			continue
		}
		exps = append(exps, e)
	}
	return exps, warnings, nil
}

// RegularFile reports, without following a symbolic link, why path is
// not a plain file to read (nil when it is): files found in a folder are
// read only when they are regular, never through links, FIFOs or devices.
func RegularFile(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	switch m := st.Mode(); {
	case m.IsRegular():
		return nil
	case m&os.ModeSymlink != 0:
		return errors.New("a symbolic link, not a regular file")
	case m.IsDir():
		return errors.New("a folder, not a regular file")
	default:
		return fmt.Errorf("not a regular file (%s)", m.Type())
	}
}

// trimPath drops the file path an error repeats.
func trimPath(err error, path string) string {
	return strings.TrimPrefix(err.Error(), path+": ")
}

// TeamDay totals date over the exports.
func TeamDay(exps []Export, date string) (tokens int64, usd float64) {
	for _, e := range exps {
		t, u := e.DayTotal(date)
		tokens += t
		usd += u
	}
	return tokens, usd
}

// PrintMerged writes the combined tables of merged exports (MergeExports):
// usage per model, per day and per machine. dayUSD and dayTokens are the
// (team) daily budget for the per-day table (0 = none).
func PrintMerged(w io.Writer, exps []Export, dayUSD float64, dayTokens int64) {
	if len(exps) == 0 {
		fmt.Fprintln(w, "No usage in these exports.")
		return
	}
	first, last := "", ""
	for _, e := range exps {
		for _, d := range e.Days {
			if first == "" || d.Date < first {
				first = d.Date
			}
			if d.Date > last {
				last = d.Date
			}
		}
	}
	span := "no days"
	if first != "" {
		span = first + " to " + last
	}
	fmt.Fprintf(w, "Switchyard team stats - %d machine(s), %s\n\n", len(exps), span)

	// Usage per model over every machine and day.
	type route struct {
		key string
		m   ExportModel
	}
	routes := map[string]*route{}
	days := map[string]*DayStats{}
	for _, e := range exps {
		for _, d := range e.Days {
			for _, m := range d.Models {
				// Another machine's file: never print its text raw.
				k := printable(m.Provider) + ":" + printable(m.Model)
				r := routes[k]
				if r == nil {
					r = &route{key: k}
					routes[k] = r
				}
				r.m.Calls += m.Calls
				r.m.OK += m.OK
				r.m.LimitHits += m.LimitHits
				r.m.FreshTokens += m.FreshTokens
				r.m.USD += m.USD
			}
			ds := days[d.Date]
			if ds == nil {
				ds = &DayStats{Date: d.Date}
				days[d.Date] = ds
			}
			ds.Tasks += d.Tasks
			ds.OK += d.OK
			ds.Codex += d.Providers[event.Codex]
			ds.Claude += d.Providers[event.Claude]
			ds.USD += d.USD
		}
	}
	rs := make([]*route, 0, len(routes))
	for _, r := range routes {
		rs = append(rs, r)
	}
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].m.FreshTokens != rs[j].m.FreshTokens {
			return rs[i].m.FreshTokens > rs[j].m.FreshTokens
		}
		return rs[i].key < rs[j].key
	})
	fmt.Fprintln(w, "Usage per model")
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  ROUTE\tCALLS\tOK\tFAIL\tLIMIT\tFRESH\t$")
	for _, r := range rs {
		fmt.Fprintf(tw, "  %s\t%d\t%d\t%d\t%d\t%s\t%.2f\n", r.key, r.m.Calls, r.m.OK, r.m.Calls-r.m.OK, r.m.LimitHits, human(r.m.FreshTokens), r.m.USD)
	}
	tw.Flush()

	st := Stats{DayLimitUSD: dayUSD, DayLimitTokens: dayTokens, daysTitle: "Per day, all machines ($ is Claude's API-equivalent price)"}
	for _, d := range days {
		st.Days = append(st.Days, d)
	}
	sort.Slice(st.Days, func(i, j int) bool { return st.Days[i].Date > st.Days[j].Date })
	st.printDays(w)

	fmt.Fprintln(w, "\nPer machine")
	tw = tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  MACHINE\tNAME\tDAYS\tTASKS\tOK\tCODEX\tCLAUDE\t$\tLIMIT HITS\tLAST DAY\tEXPORTED")
	for _, e := range exps {
		var tasks, ok, hits int
		var cx, cl int64
		var usd float64
		lastDay := "-"
		for _, d := range e.Days {
			tasks += d.Tasks
			ok += d.OK
			hits += d.LimitHits
			cx += d.Providers[event.Codex]
			cl += d.Providers[event.Claude]
			usd += d.USD
			lastDay = d.Date
		}
		name := e.Name
		if name == "" {
			name = "-"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%d\t%d\t%d\t%s\t%s\t%.2f\t%d\t%s\t%s\n", e.Machine, oneLineName(name), len(e.Days), tasks, ok,
			human(cx), human(cl), usd, hits, lastDay, e.Generated.Local().Format("2006-01-02 15:04"))
	}
	tw.Flush()
}

// StripControl makes text from elsewhere (an export, a log) safe to print
// on a terminal: valid UTF-8 without C0 or C1 control characters (escape
// sequences, carriage returns, ...). Whitespace controls become spaces;
// other Unicode is kept.
func StripControl(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f' || r == 0x85:
			return ' '
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
}

// printable is s on one line without control characters.
func printable(s string) string { return strings.Join(strings.Fields(StripControl(s)), " ") }

// oneLineName keeps a machine label (chosen by whoever exported) on one
// short line.
func oneLineName(s string) string {
	s = printable(s)
	if r := []rune(s); len(r) > 30 {
		s = string(r[:30]) + "..."
	}
	return s
}
