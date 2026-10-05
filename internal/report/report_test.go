package report

import (
	"bytes"
	"context"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/limits"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/runner"
	"github.com/sparkz400/relayweft/internal/sessionlog"
	"github.com/sparkz400/relayweft/internal/sysload"
)

const hostile = "<script>alert(1)</script> \"quoted\" & 'single' <img src=x onerror=alert(2)> ``` | --> ]]> </details></pre>"

func hostileData() *Data {
	ok := true
	_ = ok
	return &Data{
		ID: "20260101-120000-abcd-task-1", Task: hostile, Status: "done", Mode: "routed", Dir: `C:\Users\Nico Schu\repo`,
		Summary: hostile, Created: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), Duration: 90 * time.Second, Version: "v1<b>",
		PlanSummary: hostile,
		Steps: []Step{{ID: "a<x>", Title: hostile, Kind: "edit", Role: "worker", Route: "codex:m", DependsOn: []string{"<b>"},
			Result: "ok", Final: hostile, Err: hostile}},
		Routes: []Route{{Agent: "w1", Step: "a", Attempt: 2, Role: "worker", Provider: "codex", Model: "m<i>", Effort: "high",
			Rule: "rule\" onmouseover=\"x", Reason: hostile, Confidence: 0.8, Ran: true, OK: true, Final: hostile, Error: hostile}},
		Reviews: []Review{{Checkpoint: "final", Provider: "claude", Model: "opus", Approve: false, Advice: hostile}},
		Checks:  []Check{{Kind: "verify", Command: "go test ./... | tee `x`", OK: false, Duration: time.Second, Scope: "affected", Why: hostile}},
		Limits:  []Limit{{Agent: "w2", Provider: "codex", Model: "m", Text: hostile}},
		HasCost: true,
		Cost: event.TaskCost{PerProvider: map[string]event.TokenUsage{"claude": {Input: 12000, Cached: 2000, Output: 3000}},
			CostUSD: 0.42, QuotaBefore: map[string]float64{"claude": 0.61}, QuotaAfter: map[string]float64{"claude": 0.64}},
		Diff: &Diff{Before: "aaa", After: "bbb", Add: 1, Del: 1, Files: []FileDiff{{Path: "x<y>.go", Status: "M", Add: 1, Del: 1, Lang: "c",
			Lines: []Line{diffLine("@@ -1 +1 @@", "c"), diffLine(`-	s := "</div><script>alert(3)</script>"`, "c"), diffLine("+	return `<!--` // ```", "c")}}}},
		UndoCmd: `rw undo --dir "C:\Users\Nico Schu\repo" k`,
	}
}

func TestHTMLEscapesEverything(t *testing.T) {
	var buf bytes.Buffer
	if err := hostileData().HTML(&buf); err != nil {
		t.Fatal(err)
	}
	page := buf.String()
	for _, bad := range []string{"<script>alert", "<img src=x", "</details></pre>", `onmouseover="x`, "m<i>", "v1<b>", "<x>", "<y>"} {
		if strings.Contains(page, bad) {
			t.Errorf("unescaped %q in page", bad)
		}
	}
	if n := strings.Count(page, "<script"); n != 1 {
		t.Errorf("%d script tags, want only the print helper", n)
	}
	if !strings.Contains(page, `http-equiv="Content-Security-Policy"`) || !strings.Contains(html.UnescapeString(page), "script-src 'sha256-") {
		t.Error("no CSP")
	}
	// The CSP hash matches the inline script exactly.
	if !strings.Contains(page, "<script>"+printScript+"</script>") || !strings.Contains(page, scriptHash()) {
		t.Error("CSP hash does not cover the inline script")
	}
	for _, want := range []string{"&lt;script&gt;alert(1)&lt;/script&gt;", "codex", "0.80", "$0.42", "61%", "64%", "changes requested",
		`class="dl del"`, `<span class="kw">return</span>`, `<span class="com">// `, "C:\\Users\\Nico Schu\\repo", "13k", "12k"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// No external resources at all.
	if regexp.MustCompile(`(?i)(src|href)\s*=\s*"?(https?:)?//`).MatchString(page) {
		t.Error("page loads something external")
	}
}

func TestMarkdownEscapes(t *testing.T) {
	var buf bytes.Buffer
	if err := hostileData().Markdown(&buf); err != nil {
		t.Fatal(err)
	}
	md := buf.String()
	// Outside fenced blocks there is no raw HTML from the data.
	inFence := ""
	for _, l := range strings.Split(md, "\n") {
		if m := strings.TrimSpace(regexp.MustCompile("^ *`{3,}").FindString(l)); m != "" {
			switch {
			case inFence == "":
				inFence = m
				continue
			case strings.TrimSpace(l) == inFence:
				inFence = ""
				continue
			}
		}
		if inFence != "" {
			continue
		}
		for _, bad := range []string{"<script", "<img", "</details></pre>", "<b>", "<x>"} {
			if strings.Contains(l, bad) {
				t.Errorf("raw %q outside a fence: %s", bad, l)
			}
		}
	}
	if inFence != "" {
		t.Error("a fence was never closed")
	}
	// The task is in a fence longer than its own backtick run.
	if !strings.Contains(md, "````text\n"+hostile+"\n````") {
		t.Errorf("task fence:\n%s", md)
	}
	for _, want := range []string{"## Routing decisions", `| w1 | a \#2 |`, "rule\" onmouseover=\"x", "## Cost", "| claude | 13k |", "61%", "```diff\n@@ -1 +1 @@", "rw undo --dir"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q", want)
		}
	}
	if !strings.Contains(md, "| affected: ") {
		t.Error("markdown lacks which tests a check ran")
	}
	// Table cells cannot break out of their row.
	if !strings.Contains(md, `\| tee`) {
		t.Error("pipe in a table cell not escaped")
	}
}

func TestHighlightKeepsText(t *testing.T) {
	for _, s := range []string{`x := "a\"b" // c`, "def f(x): # hi 'q'", "x = 'unterminated \\", "naïve := 12.5e3", "--", ""} {
		for _, lang := range []string{"c", "hash", "dash", ""} {
			var b strings.Builder
			for _, tk := range highlight(s, lang) {
				b.WriteString(tk.Text)
			}
			if b.String() != s {
				t.Errorf("highlight(%q, %s) changed the text: %q", s, lang, b.String())
			}
		}
	}
}

// --- a real task in a temp repo, with scripted runners ---

type scripted struct {
	provider string
	fn       func(s runner.Spec) runner.Result
}

func (x scripted) Run(ctx context.Context, s runner.Spec, emit func(event.Event)) runner.Result {
	emit(event.Event{AgentID: s.AgentID, Provider: x.provider, Model: s.Model, Kind: event.Started}.Stamp())
	r := x.fn(s)
	emit(event.Event{AgentID: s.AgentID, Provider: x.provider, Kind: event.Done, OK: r.OK()}.Stamp())
	return r
}

func isolate(t *testing.T) string {
	home := t.TempDir()
	for _, k := range []string{"XDG_CONFIG_HOME", "AppData", "APPDATA"} {
		t.Setenv(k, filepath.Join(home, "config"))
	}
	for _, k := range []string{"XDG_CACHE_HOME", "LocalAppData", "LOCALAPPDATA"} {
		t.Setenv(k, filepath.Join(home, "cache"))
	}
	t.Setenv("HOME", home)
	return home
}

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if !orchestrator.SupportsMergeTree() {
		t.Skip("git < 2.38")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "core.autocrlf", "false")
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	return dir
}

func TestBuildFromRealTask(t *testing.T) {
	isolate(t)
	dir := gitRepo(t)
	t.Setenv("RW_REPORT_TEST_SECRET", "do-not-leak-me")
	task := "Add a greeting <script>alert(1)</script> to main.go and a notes file, with tests and documentation for everything please"
	fn := func(s runner.Spec) runner.Result {
		switch {
		case strings.Contains(s.Prompt, runner.MarkerPlan):
			return runner.Result{Final: "```json\n" + `{"summary":"two steps","subtasks":[` +
				`{"id":"greet","title":"greeting","kind":"edit","prompt":"edit main.go","files":["main.go"]},` +
				`{"id":"notes","title":"notes","kind":"edit","prompt":"write notes","files":["notes.md"],"depends_on":["greet"]}]}` + "\n```"}
		case strings.Contains(s.Prompt, runner.MarkerPlanReview), strings.Contains(s.Prompt, runner.MarkerFinalReview):
			return runner.Result{Final: `{"approve": true, "advice": "looks <b>good</b>"}`}
		case s.StepID == "greet":
			os.WriteFile(filepath.Join(s.Dir, "main.go"), []byte("package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hi <there>\") }\n"), 0o644)
			return runner.Result{Final: "added the greeting", Files: []string{"main.go"}, Tokens: event.TokenUsage{Input: 5000, Output: 700}}
		case s.StepID == "notes":
			os.WriteFile(filepath.Join(s.Dir, "notes.md"), []byte("# Notes\n"), 0o644)
			return runner.Result{Final: "wrote notes", Files: []string{"notes.md"}, Tokens: event.TokenUsage{Input: 3000, Output: 300}}
		}
		return runner.Result{Final: "done"}
	}
	set := runner.Set{event.Codex: scripted{event.Codex, fn}, event.Claude: scripted{event.Claude, fn}}
	cfg := config.Default()
	check := "git --version"
	cfg.Verify.Commands = []string{check}
	logDir := t.TempDir()
	log, err := sessionlog.Open(logDir, dir)
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan event.Event, 256)
	go func() {
		for range ch {
		}
	}()
	o := orchestrator.New(orchestrator.Options{
		Dir: dir, Store: config.NewStore(cfg, filepath.Join(t.TempDir(), "rw.yaml")), Mode: "routed",
		Runners: func(*config.Config) runner.Set { return set }, Tracker: limits.NewTracker(),
		Log: log, Events: ch, Load: func() sysload.Sample { return sysload.Sample{} },
	})
	res := o.Run(context.Background(), task)
	log.Close()
	close(ch)
	if !res.OK {
		t.Fatalf("task failed: %+v", res)
	}
	hist := orchestrator.History(dir, 1)
	if len(hist) != 1 {
		t.Fatalf("history = %+v", hist)
	}
	st, err := orchestrator.LoadTask(hist[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	d := Build(st, Options{SessionDir: logDir, Version: "test"})
	if d.Task != task || d.Status != "done" || d.Mode != "routed" || d.Duration <= 0 {
		t.Errorf("header: %+v", d)
	}
	if len(d.Notes) > 0 {
		t.Errorf("notes: %v", d.Notes)
	}
	if len(d.Steps) != 2 || d.Steps[0].Result != "ok" || d.Steps[1].DependsOn[0] != "greet" || d.Steps[0].Route == "" || d.Steps[0].Final != "added the greeting" {
		t.Errorf("steps = %+v", d.Steps)
	}
	var rules []string
	for _, r := range d.Routes {
		rules = append(rules, r.Rule)
		if r.Rule == "" || r.Reason == "" || r.Confidence == 0 || !r.Ran {
			t.Errorf("route without rule/reason/confidence/run: %+v", r)
		}
	}
	if len(d.Routes) < 4 { // planner, plan review, 2 workers, final review
		t.Errorf("routes = %v", rules)
	}
	if len(d.Reviews) < 1 || !d.Reviews[len(d.Reviews)-1].Approve {
		t.Errorf("reviews = %+v", d.Reviews)
	}
	if len(d.Checks) != 1 || d.Checks[0].Command != check || !d.Checks[0].OK || d.Checks[0].Tests() != "full: the full checks before the final review" {
		t.Errorf("checks = %+v", d.Checks)
	}
	if !d.HasCost || d.Tokens.Total() == 0 {
		t.Errorf("cost = %+v tokens %+v", d.Cost, d.Tokens)
	}
	if d.Diff == nil || len(d.Diff.Files) != 2 || d.Diff.Files[0].Path != "main.go" || d.Diff.Files[1].Status != "A" || d.Diff.Add == 0 {
		t.Fatalf("diff = %+v", d.Diff)
	}
	if !strings.Contains(d.UndoCmd, "rw undo --dir ") || !strings.HasSuffix(d.UndoCmd, st.UndoKey) {
		t.Errorf("undo = %q", d.UndoCmd)
	}
	var page, md bytes.Buffer
	if err := d.HTML(&page); err != nil {
		t.Fatal(err)
	}
	if err := d.Markdown(&md); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{page.String(), md.String()} {
		if strings.Contains(out, "do-not-leak-me") {
			t.Error("environment value in the report")
		}
		if !strings.Contains(out, "notes.md") || !strings.Contains(out, "greet") {
			t.Error("report lacks the plan or the diff")
		}
	}
	if strings.Contains(page.String(), "<script>alert(1)") || !strings.Contains(page.String(), "hi &lt;there&gt;") {
		t.Error("diff or task not escaped")
	}

	// The diff is size-capped.
	small := Build(st, Options{SessionDir: logDir, MaxFileLines: 2})
	if !small.Diff.Truncated || len(small.Diff.Files[0].Lines) != 2 {
		t.Errorf("cap: %+v", small.Diff)
	}
	// No session log: still a report, with a note.
	bare := Build(st, Options{SessionDir: t.TempDir()})
	if len(bare.Notes) == 0 || len(bare.Steps) != 2 || bare.Diff == nil {
		t.Errorf("bare = %+v", bare)
	}
	if runtime.GOOS == "windows" && !strings.Contains(d.UndoCmd, `"`) && strings.Contains(dir, " ") {
		t.Error("dir with spaces not quoted")
	}
}

// A multi-repo task's report lists the files of every repo it changed.
func TestBuildMultiRepoTask(t *testing.T) {
	isolate(t)
	api, web := gitRepo(t), gitRepo(t)
	fn := func(s runner.Spec) runner.Result {
		switch {
		case strings.Contains(s.Prompt, runner.MarkerPlan):
			return runner.Result{Final: "```json\n" + `{"summary":"two repos","subtasks":[` +
				`{"id":"a","title":"api","kind":"edit","prompt":"api change","files":["api.txt"]},` +
				`{"id":"b","title":"web","kind":"edit","prompt":"web change","files":["web.txt"],"repo":"web"}]}` + "\n```"}
		case strings.Contains(s.Prompt, runner.MarkerPlanReview), strings.Contains(s.Prompt, runner.MarkerFinalReview):
			return runner.Result{Final: `{"approve": true}`}
		}
		name := map[string]string{"a": "api.txt", "b": "web.txt"}[s.StepID]
		os.WriteFile(filepath.Join(s.Dir, name), []byte(s.StepID+"\n"), 0o644)
		return runner.Result{Final: "wrote " + name, Files: []string{name}}
	}
	set := runner.Set{event.Codex: scripted{event.Codex, fn}, event.Claude: scripted{event.Claude, fn}}
	ch := make(chan event.Event, 256)
	go func() {
		for range ch {
		}
	}()
	o := orchestrator.New(orchestrator.Options{
		Dir: api, Store: config.NewStore(config.Default(), filepath.Join(t.TempDir(), "rw.yaml")), Mode: "routed",
		Runners: func(*config.Config) runner.Set { return set }, Tracker: limits.NewTracker(),
		Events: ch, Load: func() sysload.Sample { return sysload.Sample{} },
		Repos: []orchestrator.Repo{{Name: "web", Dir: web}},
	})
	res := o.Run(context.Background(), "Change the api and the web repo together, each in its own step, keeping both in sync please")
	close(ch)
	if !res.OK {
		t.Fatalf("task failed: %+v", res)
	}
	st, err := orchestrator.LoadTask(orchestrator.History(api, 1)[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	d := Build(st, Options{Version: "test"})
	var paths []string
	if d.Diff != nil {
		for _, f := range d.Diff.Files {
			paths = append(paths, f.Path)
		}
	}
	if strings.Join(paths, ",") != "api.txt,[web] web.txt" {
		t.Fatalf("report files = %v, notes %v", paths, d.Notes)
	}
}

// A best-of step shows the kept candidate's route and how it was picked.
func TestBuildBestOfTask(t *testing.T) {
	isolate(t)
	dir := gitRepo(t)
	fn := func(s runner.Spec) runner.Result {
		switch {
		case strings.Contains(s.Prompt, runner.MarkerBestOf):
			// Candidates' names are shuffled: pick Claude's by its diff.
			i := strings.Index(s.Prompt, "+claude")
			a := strings.LastIndex(s.Prompt[:i], "### Candidate ")
			return runner.Result{Final: `{"pick": "` + s.Prompt[a+14:a+15] + `", "why": "clearer"}`}
		case strings.Contains(s.Prompt, runner.MarkerFinalReview):
			return runner.Result{Final: `{"approve": true}`}
		}
		os.WriteFile(filepath.Join(s.Dir, "greet.txt"), []byte(s.Provider+"\n"), 0o644)
		return runner.Result{Final: "changed it"}
	}
	set := runner.Set{event.Codex: scripted{event.Codex, fn}, event.Claude: scripted{event.Claude, fn}}
	cfg := config.Default()
	cfg.Routing.BestOf.When = config.BestOfAlways
	logDir := t.TempDir()
	log, err := sessionlog.Open(logDir, dir)
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan event.Event, 256)
	go func() {
		for range ch {
		}
	}()
	o := orchestrator.New(orchestrator.Options{
		Dir: dir, Store: config.NewStore(cfg, filepath.Join(t.TempDir(), "rw.yaml")), Mode: "routed",
		Runners: func(*config.Config) runner.Set { return set }, Tracker: limits.NewTracker(),
		Log: log, Events: ch, Load: func() sysload.Sample { return sysload.Sample{} },
	})
	res := o.Run(context.Background(), "fix the greeting")
	log.Close()
	close(ch)
	if !res.OK {
		t.Fatalf("task failed: %+v", res)
	}
	st, err := orchestrator.LoadTask(orchestrator.History(dir, 1)[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	d := Build(st, Options{SessionDir: logDir, Version: "test"})
	if len(d.Steps) != 1 || !strings.HasPrefix(d.Steps[0].Route, "claude:") || !strings.Contains(d.Steps[0].BestOf, "kept work--claude") {
		t.Fatalf("steps = %+v", d.Steps)
	}
	var md bytes.Buffer
	if err := d.Markdown(&md); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md.String(), "best of 2: kept work--claude") || !strings.Contains(md.String(), "work--codex") {
		t.Errorf("markdown lacks the best-of outcome:\n%s", md.String())
	}
}
