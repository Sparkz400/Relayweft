package sessionlog

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/event"
)

// exportRecs is a day of work in a secret project, and one the day before.
func exportRecs(now time.Time) []Record {
	cost := func(cx, cl int64, usd float64) *event.TaskCost {
		return &event.TaskCost{CostUSD: usd, PerProvider: map[string]event.TokenUsage{event.Codex: {Input: cx}, event.Claude: {Input: cl, CostUSD: usd}}}
	}
	yesterday := DayStart(now).Add(-time.Hour)
	return []Record{
		{Type: TypeTask, TS: now, Cwd: "/home/alice/secret-project", Task: "rotate the SECRET-API-KEY in vault", TaskID: "t1"},
		{Type: TypeAgentEnd, TS: now, Cwd: "/home/alice/secret-project", Provider: event.Claude, Model: "opus", OK: Bool(true), Tokens: &event.TokenUsage{Input: 1200, Cached: 200, Output: 100, CostUSD: 0.30}, Text: "PROMPT TEXT"},
		{Type: TypeAgentEnd, TS: now, Cwd: "/home/alice/secret-project", Provider: event.Codex, Model: "gpt-6", OK: Bool(false), LimitHit: true, Tokens: &event.TokenUsage{Input: 500}},
		{Type: TypeTaskEnd, TS: now, Cwd: "/home/alice/secret-project", Task: "rotate the SECRET-API-KEY in vault", TaskID: "t1", OK: Bool(true), Text: "summary with /home/alice/secret-project/x.go", Cost: cost(500, 1100, 0.30)},
		{Type: TypeTaskEnd, TS: yesterday, Cwd: "/home/alice/secret-project", Task: "older task", OK: Bool(false), Cost: cost(0, 50, 0.01)},
	}
}

// By default an export holds numbers only: no task text, prompt, summary
// or path, and no host or user name.
func TestExportPrivacy(t *testing.T) {
	now := time.Now()
	e := BuildExport(exportRecs(now), ExportOptions{Machine: "0123456789abcdef", Now: now})
	data, err := e.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	for _, leak := range []string{"SECRET", "secret-project", "alice", "PROMPT", "summary", "older task"} {
		if bytes.Contains(data, []byte(leak)) {
			t.Errorf("export leaks %q:\n%s", leak, data)
		}
	}
	if host != "" && len(host) > 3 && bytes.Contains(data, []byte(host)) {
		t.Errorf("export holds the host name %q", host)
	}
	if len(e.Days) != 2 {
		t.Fatalf("days = %+v", e.Days)
	}
	d := e.Days[1]
	if d.Tasks != 1 || d.OK != 1 || d.FreshTokens != 1600 || d.USD != 0.30 || d.LimitHits != 1 ||
		d.Providers[event.Codex] != 500 || d.Providers[event.Claude] != 1100 || len(d.Models) != 2 {
		t.Fatalf("today = %+v", d)
	}
	if m := d.Models[0]; m.Provider != event.Claude || m.Model != "opus" || m.FreshTokens != 1100 || m.USD != 0.30 || m.Calls != 1 || m.OK != 1 {
		t.Errorf("claude model = %+v", m)
	}
	if m := d.Models[1]; m.Provider != event.Codex || m.LimitHits != 1 || m.OK != 0 {
		t.Errorf("codex model = %+v", m)
	}
	// --with-tasks adds them.
	e = BuildExport(exportRecs(now), ExportOptions{Machine: "0123456789abcdef", Now: now, WithTasks: true})
	if len(e.Tasks) != 2 || e.Tasks[1].Task != "rotate the SECRET-API-KEY in vault" || e.Tasks[1].Dir != "/home/alice/secret-project" {
		t.Errorf("with tasks: %+v", e.Tasks)
	}
	// It reads back.
	data, _ = e.Marshal()
	back, err := ParseExport(data)
	if err != nil || back.Machine != e.Machine || len(back.Days) != 2 || len(back.Tasks) != 2 {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

// Another major version is refused with a clear error; a newer minor reads.
func TestExportVersions(t *testing.T) {
	e := BuildExport(nil, ExportOptions{Machine: "0123456789abcdef"})
	data, _ := e.Marshal()
	for v, ok := range map[string]bool{"1.0": true, "1.7": true, "2.0": false, "": false, "0.9": false} {
		var m map[string]any
		json.Unmarshal(data, &m)
		m["version"] = v
		m["new_field"] = "from the future"
		b, _ := json.Marshal(m)
		_, err := ParseExport(b)
		if ok != (err == nil) {
			t.Errorf("version %q: err %v", v, err)
		}
		if !ok && (err == nil || !strings.Contains(err.Error(), "not supported")) {
			t.Errorf("version %q: unclear error %v", v, err)
		}
	}
	if _, err := ParseExport([]byte(`{"format":"something-else","version":"1.0"}`)); err == nil || !strings.Contains(err.Error(), "not a Switchyard stats export") {
		t.Errorf("foreign file: %v", err)
	}
	if _, err := ParseExport([]byte(`{"format":"switchyard-stats","version":"1.0","machine":"../../evil"}`)); err == nil {
		t.Error("a machine id with a path was accepted")
	}
}

// Merging the same file again changes nothing; for one machine and day the
// newest export wins.
func TestMergeIdempotent(t *testing.T) {
	now := time.Now()
	a := BuildExport(exportRecs(now), ExportOptions{Machine: "aaaa000000000001", Now: now})
	b := BuildExport(exportRecs(now)[:2], ExportOptions{Machine: "bbbb000000000002", Name: "build box", Now: now})
	once := MergeExports([]Export{a, b})
	twice := MergeExports([]Export{a, b, a, b, a})
	j1, _ := json.Marshal(once)
	j2, _ := json.Marshal(twice)
	if !bytes.Equal(j1, j2) {
		t.Fatalf("merge is not idempotent:\n%s\n%s", j1, j2)
	}
	if len(once) != 2 || once[1].Name != "build box" {
		t.Fatalf("merged = %+v", once)
	}
	today := now.Local().Format("2006-01-02")
	if tok, usd := TeamDay(once, today); tok != 1600 || usd < 0.299 || usd > 0.301 {
		t.Errorf("team day = %d $%.2f", tok, usd)
	}
	// A newer export of a replaces its days, not adds to them.
	a2 := a
	a2.Generated = now.Add(time.Hour)
	a2.Days = append([]ExportDay(nil), a.Days...)
	a2.Days[1].USD, a2.Days[1].FreshTokens = 1, 5000
	m := MergeExports([]Export{a, a2, a})
	if tok, usd := TeamDay(m, today); tok != 5000 || usd != 1 {
		t.Errorf("newest export should win: %d $%.2f", tok, usd)
	}
	var out bytes.Buffer
	PrintMerged(&out, once, 2, 0)
	for _, want := range []string{"2 machine(s)", "Usage per model", "claude:opus", "Per day, all machines", "DAILY BUDGET", "Per machine", "aaaa000000000001", "build box"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("merged report lacks %q:\n%s", want, out.String())
		}
	}
}

// The team folder: corrupt, foreign, stale and mislabelled files are
// skipped with a warning; this machine's own file is not read; writes are
// atomic and touch only this machine's file.
func TestTeamDir(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	me, other := "1111111111111111", "2222222222222222"
	recs := exportRecs(now)
	if err := WriteTeamFile(dir, BuildExport(recs, ExportOptions{Machine: other, Now: now})); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "3333333333333333.json"), []byte(`{"format":"switchyard-stats","version":"1.0","machine":`), 0o644)
	os.WriteFile(filepath.Join(dir, "4444444444444444.json"), []byte(`{"format":"switchyard-stats","version":"9.0","machine":"4444444444444444"}`), 0o644)
	stale := BuildExport(recs, ExportOptions{Machine: "5555555555555555", Now: now.Add(-30 * 24 * time.Hour)})
	WriteTeamFile(dir, stale)
	liar := BuildExport(recs, ExportOptions{Machine: other, Now: now})
	data, _ := liar.Marshal()
	os.WriteFile(filepath.Join(dir, "6666666666666666.json"), data, 0o644) // claims to be another machine
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o644)
	mine := BuildExport(recs, ExportOptions{Machine: me, Now: now})
	mine.Days[1].USD = 1000
	if err := WriteTeamFile(dir, mine); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	for _, f := range files {
		before[f], _ = os.ReadFile(f)
	}

	exps, warns, err := ReadTeamDir(dir, me, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(exps) != 1 || exps[0].Machine != other {
		t.Fatalf("read %+v", exps)
	}
	joined := strings.Join(warns, "\n")
	for _, want := range []string{"3333333333333333.json", "4444444444444444.json: export version", "5555555555555555.json: stale", "6666666666666666.json: it holds machine"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings lack %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, me) {
		t.Errorf("own file was read: %s", joined)
	}

	// Writing again replaces only this machine's file and leaves no
	// temporary files.
	mine.Days[1].USD = 2
	if err := WriteTeamFile(dir, mine); err != nil {
		t.Fatal(err)
	}
	after, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(after) != len(files) {
		t.Errorf("files before %v, after %v", files, after)
	}
	own := TeamFile(dir, me)
	for _, f := range after {
		data, _ := os.ReadFile(f)
		if f != own && !bytes.Equal(data, before[f]) {
			t.Errorf("%s changed", f)
		}
	}
	if e, err := ReadExport(own); err != nil || e.Days[1].USD != 2 {
		t.Errorf("own file: %+v %v", e, err)
	}
	if err := WriteTeamFile(dir, Export{Machine: "../x"}); err == nil {
		t.Error("a path as machine id was written")
	}
	if _, _, err := ReadTeamDir(filepath.Join(dir, "missing"), me, now); err == nil {
		t.Error("a missing team folder is not reported")
	}
}

func TestMachineID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "machine-id")
	old := MachineIDFile
	MachineIDFile = func() string { return p }
	defer func() { MachineIDFile = old }()
	a, err := MachineID()
	if err != nil || !reMachine.MatchString(a) || len(a) != 16 {
		t.Fatalf("id %q %v", a, err)
	}
	if b, _ := MachineID(); b != a {
		t.Errorf("id changed: %s -> %s", a, b)
	}
	os.WriteFile(p, []byte("../broken\n"), 0o644)
	if c, err := MachineID(); err != nil || c == a || !reMachine.MatchString(c) {
		t.Errorf("damaged id file: %q %v", c, err)
	}
}

// Negative counts or $ (which would lower a team's totals) are rejected.
func TestExportRejectsNegativeUsage(t *testing.T) {
	now := time.Now()
	good := BuildExport(exportRecs(now), ExportOptions{Machine: "0123456789abcdef", Now: now, WithTasks: true})
	data, _ := good.Marshal()
	if _, err := ParseExport(data); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]func(e *Export){
		"tasks":        func(e *Export) { e.Days[0].Tasks = -1 },
		"ok":           func(e *Export) { e.Days[0].OK = -1 },
		"fresh_tokens": func(e *Export) { e.Days[0].FreshTokens = -5000 },
		"usd":          func(e *Export) { e.Days[0].USD = -10 },
		"limit_hits":   func(e *Export) { e.Days[0].LimitHits = -1 },
		"providers":    func(e *Export) { e.Days[0].Providers = map[string]int64{"claude": -1} },
		"model calls":  func(e *Export) { e.Days[1].Models[0].Calls = -1 },
		"model ok":     func(e *Export) { e.Days[1].Models[0].OK = -1 },
		"model tokens": func(e *Export) { e.Days[1].Models[0].FreshTokens = -1 },
		"model usd":    func(e *Export) { e.Days[1].Models[0].USD = -0.5 },
		"model limit":  func(e *Export) { e.Days[1].Models[0].LimitHits = -1 },
		"task tokens":  func(e *Export) { e.Tasks[0].FreshTokens = -1 },
		"task usd":     func(e *Export) { e.Tasks[0].USD = -1 },
	} {
		e := good
		e.Days = append([]ExportDay(nil), good.Days...)
		for i := range e.Days {
			e.Days[i].Models = append([]ExportModel(nil), e.Days[i].Models...)
		}
		e.Tasks = append([]ExportTask(nil), good.Tasks...)
		bad(&e)
		data, _ := e.Marshal()
		if _, err := ParseExport(data); err == nil {
			t.Errorf("negative %s accepted", name)
		}
	}
	for _, v := range []float64{math.NaN(), math.Inf(1)} {
		if validUSD(v) {
			t.Errorf("validUSD(%v)", v)
		}
	}
}

// Text from another machine's export reaches the terminal without control
// characters (no escape sequences), keeping other Unicode.
func TestMergedReportStripsControls(t *testing.T) {
	now := time.Now()
	e := Export{Format: ExportFormat, Version: ExportVersion, Machine: "aaaa000000000001", Name: "bad\x1b[2Jbox\u009b31m ünï", Generated: now,
		Days: []ExportDay{{Date: now.Format("2006-01-02"), Tasks: 1, Models: []ExportModel{{Provider: "claude\x1b]0;x\x07", Model: "op\rus\x00", Calls: 1}}}}}
	var out bytes.Buffer
	PrintMerged(&out, []Export{e}, 0, 0)
	s := out.String()
	for _, bad := range []string{"\x1b", "\u009b", "\x07", "\r", "\x00"} {
		if strings.Contains(s, bad) {
			t.Errorf("report holds %q:\n%q", bad, s)
		}
	}
	if !strings.Contains(s, "bad[2Jbox31m ünï") || !strings.Contains(s, "claude]0;x:op us") {
		t.Errorf("report:\n%s", s)
	}
	if got := StripControl("a\tb\u0085c\x7fd\u00a0é\xff"); got != "a b cd\u00a0é?" {
		t.Errorf("StripControl = %q", got)
	}
}
