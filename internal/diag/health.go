package diag

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sparkz400/switchyard/internal/sysload"
)

// Health log: a small record of how every sy process went, kept next to the
// debug log as sy-health.log. The debug log rotates within days of heavy
// use; this one holds a few lines per process (start, load peaks every few
// minutes, hangs, panics, end) so `sy health` can look back weeks.
//
// A line is "<time> <kind> pid=<pid> key=value ...", values quoted when
// they contain spaces. A process that ends without an "end" line was
// killed, lost its window or died in a fatal runtime error; the last case
// leaves its output in fatal-<pid>-<time>.log (debug.SetCrashOutput).

const (
	healthFile    = "sy-health.log"
	healthMaxSize = 4 << 20

	watchEvery = 10 * time.Second
	loadEvery  = 5 * time.Minute
	// HangAfter is how long a watched loop may go without a beat before it
	// counts as hung.
	HangAfter = time.Minute
	// a watchdog tick this much late means the machine slept or froze
	pauseAfter = time.Minute
)

var (
	hf        *os.File // guarded by mu
	fatalPath string   // guarded by mu
	started   time.Time

	monOnce sync.Once
	peakMu  sync.Mutex
	window  Peaks // since the last load line
	session Peaks // whole process

	beatMu sync.Mutex
	beats  = map[string]*beat{}
)

func openHealth(d string) {
	if hf != nil {
		hf.Close()
		hf = nil
	}
	path := filepath.Join(d, healthFile)
	if st, err := os.Stat(path); err == nil && st.Size() > healthMaxSize {
		os.Rename(path, path+".1") // fails on Windows while another sy has it open: keep appending
	}
	if file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		hf = file
	}
}

// Health records one event in the health log (and the debug log).
// kv alternates keys and values.
func Health(kind string, kv ...any) {
	var b strings.Builder
	b.WriteString(kind)
	fmt.Fprintf(&b, " pid=%d", os.Getpid())
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, " %v=%s", kv[i], quoteValue(fmt.Sprint(kv[i+1])))
	}
	rest := b.String()
	Logf("health %s", rest)
	line := time.Now().Format("2006-01-02 15:04:05.000") + " " + rest + "\n"
	mu.Lock()
	defer mu.Unlock()
	if hf != nil {
		hf.WriteString(line)
	}
}

func quoteValue(v string) string {
	if v == "" || strings.ContainsAny(v, " \t\r\n\"=") {
		return strconv.Quote(v)
	}
	return v
}

// Start records the start of this process, arms the fatal-error file and
// starts the watchdog that samples load and checks watched loops.
func Start(cmd string) {
	mu.Lock()
	started = time.Now()
	d := dir
	mu.Unlock()
	Health("start", "ver", Version, "cmd", cmd)
	if d != "" {
		pruneFatal(d)
		path := filepath.Join(d, fmt.Sprintf("fatal-%d-%s.log", os.Getpid(), time.Now().Format("20060102-150405")))
		if f, err := os.Create(path); err == nil {
			if debug.SetCrashOutput(f, debug.CrashOptions{}) == nil {
				mu.Lock()
				fatalPath = path
				mu.Unlock()
			} else {
				defer os.Remove(path)
			}
			f.Close() // SetCrashOutput keeps its own copy
		}
	}
	monOnce.Do(func() { go monitor() })
}

// End records how this process ended and disarms the fatal-error file.
func End(err error) {
	mu.Lock()
	since := started
	path := fatalPath
	fatalPath = ""
	mu.Unlock()
	peakMu.Lock()
	p := session
	p.merge(window)
	peakMu.Unlock()
	kv := []any{"ok", err == nil}
	if !since.IsZero() {
		kv = append(kv, "after", time.Since(since).Round(time.Second))
	}
	kv = append(kv, p.kv()...)
	if err != nil {
		kv = append(kv, "err", clipLine(err.Error(), 200))
	}
	Health("end", kv...)
	if path != "" {
		debug.SetCrashOutput(nil, debug.CrashOptions{})
		os.Remove(path)
	}
	Sync()
}

func clipLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

// pruneFatal deletes empty fatal files older than a week: they only say a
// process ended without an end line, which the health log already has.
func pruneFatal(d string) {
	files, _ := filepath.Glob(filepath.Join(d, "fatal-*.log"))
	for _, p := range files {
		if st, err := os.Stat(p); err == nil && st.Size() == 0 && time.Since(st.ModTime()) > 7*24*time.Hour {
			os.Remove(p)
		}
	}
}

// Peaks are the worst load readings over a span.
type Peaks struct {
	CPU        float64 // highest machine CPU busy fraction
	CPUOK      bool
	MemFree    uint64 // lowest free RAM
	MemTotal   uint64
	MemOK      bool
	SyMem      uint64 // highest memory sy itself got from the OS
	Goroutines int
}

func (p *Peaks) add(s sysload.Sample, syMem uint64, goroutines int) {
	if s.CPUOK && (!p.CPUOK || s.CPU > p.CPU) {
		p.CPU, p.CPUOK = s.CPU, true
	}
	if s.MemOK && (!p.MemOK || s.MemFree < p.MemFree) {
		p.MemFree, p.MemTotal, p.MemOK = s.MemFree, s.MemTotal, true
	}
	p.SyMem = max(p.SyMem, syMem)
	p.Goroutines = max(p.Goroutines, goroutines)
}

func (p *Peaks) merge(o Peaks) {
	if o.CPUOK {
		p.add(sysload.Sample{CPU: o.CPU, CPUOK: true}, 0, 0)
	}
	if o.MemOK {
		p.add(sysload.Sample{MemFree: o.MemFree, MemTotal: o.MemTotal, MemOK: true}, 0, 0)
	}
	p.SyMem = max(p.SyMem, o.SyMem)
	p.Goroutines = max(p.Goroutines, o.Goroutines)
}

func (p Peaks) kv() []any {
	var kv []any
	if p.CPUOK {
		kv = append(kv, "cpu", int(p.CPU*100+0.5))
	}
	if p.MemOK {
		kv = append(kv, "memfree_mb", p.MemFree>>20, "memtotal_mb", p.MemTotal>>20)
	}
	if p.SyMem > 0 {
		kv = append(kv, "symem_mb", p.SyMem>>20, "goroutines", p.Goroutines)
	}
	return kv
}

// monitor samples load and checks watched loops until the process ends.
func monitor() {
	defer Recover("health monitor", nil)
	s := sysload.NewSampler(watchEvery)
	s.Get()
	t := time.NewTicker(watchEvery)
	last, lastLoad := time.Now(), time.Now()
	for now := range t.C {
		// Wall clock: the monotonic clock stops during sleep on Windows.
		if gap := now.Round(0).Sub(last.Round(0)) - watchEvery; gap > pauseAfter {
			Health("pause", "for", gap.Round(time.Second))
			resetBeats()
		}
		last = now
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		peakMu.Lock()
		window.add(s.Get(), ms.Sys, runtime.NumGoroutine())
		var flush Peaks
		due := now.Sub(lastLoad) >= loadEvery
		if due {
			flush = window
			session.merge(window)
			window = Peaks{}
			lastLoad = now
		}
		peakMu.Unlock()
		if due {
			Health("load", flush.kv()...)
		}
		checkBeats(time.Now())
	}
}

type beat struct {
	last   time.Time
	hungAt time.Time // set while a hang is reported
}

// Beat tells the watchdog that the loop called name is making progress. A
// loop that beats once and then stops for HangAfter is reported as a hang,
// with every goroutine's stack in hang-<time>.log. Unwatch ends the watch.
func Beat(name string) {
	now := time.Now()
	beatMu.Lock()
	b := beats[name]
	if b == nil {
		b = &beat{}
		beats[name] = b
	}
	b.last = now
	beatMu.Unlock()
}

// Unwatch stops watching name (the loop ended on purpose).
func Unwatch(name string) {
	beatMu.Lock()
	delete(beats, name)
	beatMu.Unlock()
}

func resetBeats() {
	now := time.Now()
	beatMu.Lock()
	for _, b := range beats {
		b.last = now
	}
	beatMu.Unlock()
}

func checkBeats(now time.Time) {
	type ev struct {
		name   string
		hung   bool
		stuck  time.Duration
		hungAt time.Time
	}
	var evs []ev
	beatMu.Lock()
	for name, b := range beats {
		stuck := now.Sub(b.last)
		switch {
		case b.hungAt.IsZero() && stuck > HangAfter:
			b.hungAt = b.last
			evs = append(evs, ev{name, true, stuck, b.hungAt})
		case !b.hungAt.IsZero() && stuck < HangAfter:
			evs = append(evs, ev{name, false, b.last.Sub(b.hungAt), b.hungAt})
			b.hungAt = time.Time{}
		}
	}
	beatMu.Unlock()
	sort.Slice(evs, func(i, j int) bool { return evs[i].name < evs[j].name })
	for _, e := range evs {
		if e.hung {
			Health("hang", "in", e.name, "stuck", e.stuck.Round(time.Second), "log", filepath.Base(dumpHang(e.name, e.stuck)))
		} else {
			Health("hang-end", "in", e.name, "after", e.stuck.Round(time.Second))
		}
	}
}

// dumpHang writes every goroutine's stack and the recent debug lines to
// hang-<time>.log and returns its path ("" if it could not be written).
func dumpHang(name string, stuck time.Duration) string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var b strings.Builder
	fmt.Fprintf(&b, "Switchyard %s: %s made no progress for %s (pid %d, %s)\n", Version, name, stuck.Round(time.Second), os.Getpid(), time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "os %s/%s, go %s\n\n--- goroutines ---\n%s\n", runtime.GOOS, runtime.GOARCH, runtime.Version(), buf)
	b.WriteString("\n--- recent debug log ---\n")
	for _, l := range Recent() {
		b.WriteString(l + "\n")
	}
	d := Dir()
	if d == "" {
		return ""
	}
	path := filepath.Join(d, "hang-"+time.Now().Format("20060102-150405.000")+".log")
	if os.WriteFile(path, []byte(b.String()), 0o644) != nil {
		return ""
	}
	return path
}

// Record is one parsed health log line.
type Record struct {
	Time   time.Time
	Kind   string
	PID    int
	Fields map[string]string
}

// ReadHealth returns the health log records in dir, oldest first (the
// rotated file included). A missing log is no error.
func ReadHealth(d string) ([]Record, error) {
	var out []Record
	for _, name := range []string{healthFile + ".1", healthFile} {
		f, err := os.Open(filepath.Join(d, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return out, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if r, ok := ParseRecord(sc.Text()); ok {
				out = append(out, r)
			}
		}
		f.Close()
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// ParseRecord parses one health log line.
func ParseRecord(line string) (Record, bool) {
	const layout = "2006-01-02 15:04:05.000"
	if len(line) < len(layout)+2 {
		return Record{}, false
	}
	t, err := time.ParseInLocation(layout, line[:len(layout)], time.Local)
	if err != nil {
		return Record{}, false
	}
	rest := strings.TrimSpace(line[len(layout):])
	kind, rest, _ := strings.Cut(rest, " ")
	if kind == "" {
		return Record{}, false
	}
	r := Record{Time: t, Kind: kind, Fields: map[string]string{}}
	for rest = strings.TrimSpace(rest); rest != ""; rest = strings.TrimSpace(rest) {
		key, after, ok := strings.Cut(rest, "=")
		if !ok || key == "" || strings.ContainsAny(key, " \"") {
			break
		}
		val := after
		if strings.HasPrefix(after, `"`) {
			q, err := strconv.QuotedPrefix(after)
			if err != nil {
				break
			}
			val, _ = strconv.Unquote(q)
			rest = after[len(q):]
		} else {
			val, rest, _ = strings.Cut(after, " ")
		}
		r.Fields[key] = val
	}
	r.PID, _ = strconv.Atoi(r.Fields["pid"])
	return r, r.PID > 0
}
