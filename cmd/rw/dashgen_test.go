package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/runner"
	"gopkg.in/yaml.v3"
)

// TestDashboardMatchesCLI writes realistic session logs with the real rw
// (rw web --client, scripted Claude and Codex CLIs) and checks that the
// dashboard of `rw web` says what `rw stats`, `rw tune` and `rw health` say
// about the same logs, and what the test knows happened.
//
// The scripted CLIs plan, review and edit; per step they also fail with the
// same error until the step moves to worker_high (an escalation), hit a
// usage limit (a fallback), report a nearly full quota (a pre-emptive
// switch), hang until the task is cancelled, or are logged out. Tasks run
// in four rw sessions: the default routes; workers on Claude with the judge
// on; after `rw tune --apply` learned a cheaper worker route, with best of
// 2; and a second project with model tiers. RW_DASHGEN_KEEP=<dir> keeps the
// profile (logs, health log, learned routes) there for a look in a browser.

const (
	dashgenCLIArg   = "--rw-dashgen-cli="   // claude or codex: the protocol to speak
	dashgenStateArg = "--rw-dashgen-state=" // folder for markers between runs
)

func init() {
	kind, state := "", ""
	for _, a := range os.Args[1:] {
		switch {
		case strings.HasPrefix(a, dashgenCLIArg):
			kind = strings.TrimPrefix(a, dashgenCLIArg)
		case strings.HasPrefix(a, dashgenStateArg):
			state = strings.TrimPrefix(a, dashgenStateArg)
		}
	}
	if kind != "" {
		os.Exit(dashgenAgent(kind, state))
	}
}

var (
	reDashSpec  = regexp.MustCompile(`\[spec ([^\]]*)\]`)
	reDashTask  = regexp.MustCompile(`dashboard check task (\d+)`)
	reDashWrite = regexp.MustCompile(`write <<([^>]+)>> containing <<([^>]+)>>(?: \{(\w+)\})?`)
)

// dashOut writes one CLI protocol: Claude Code's stream-json or codex
// exec --json.
type dashOut struct {
	kind  string
	enc   *json.Encoder
	sid   string
	wd    string
	usage [3]int64 // fresh input, cached input, output
	usd   float64
}

func (o *dashOut) start(model string) {
	if o.kind == "codex" {
		o.enc.Encode(map[string]any{"type": "thread.started", "thread_id": o.sid})
		o.enc.Encode(map[string]any{"type": "turn.started"})
		return
	}
	o.enc.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": o.sid, "model": model})
}

// quota is a Claude rate_limit_event (Codex sends none).
func (o *dashOut) quota(status string, five, seven float64, resets time.Time) {
	if o.kind != "claude" {
		return
	}
	o.enc.Encode(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{
		"status": status, "resetsAt": resets.Unix(), "rateLimitType": "five_hour", "utilization": five,
		"unifiedWindows": map[string]any{
			"five_hour": map[string]any{"utilization": five, "resetsAt": resets.Unix()},
			"seven_day": map[string]any{"utilization": seven, "resetsAt": resets.Add(72 * time.Hour).Unix()},
		}}})
}

func (o *dashOut) wrote(path string) {
	abs := filepath.Join(o.wd, filepath.FromSlash(path))
	if o.kind == "codex" {
		o.enc.Encode(map[string]any{"type": "item.completed", "item": map[string]any{"id": "item_1", "type": "file_change",
			"changes": []any{map[string]any{"path": abs, "kind": "add"}}, "status": "completed"}})
		return
	}
	o.enc.Encode(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "id": "tu1", "name": "Write", "input": map[string]any{"file_path": abs}}}}})
}

func (o *dashOut) ok(text string) int {
	if o.kind == "codex" {
		o.enc.Encode(map[string]any{"type": "item.completed", "item": map[string]any{"id": "item_2", "type": "agent_message", "text": text}})
		o.enc.Encode(map[string]any{"type": "turn.completed", "usage": map[string]any{
			"input_tokens": o.usage[0] + o.usage[1], "cached_input_tokens": o.usage[1], "output_tokens": o.usage[2]}})
		return 0
	}
	o.enc.Encode(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text, "session_id": o.sid,
		"total_cost_usd": o.usd, "usage": map[string]any{"input_tokens": o.usage[0], "cache_read_input_tokens": o.usage[1], "output_tokens": o.usage[2]}})
	return 0
}

func (o *dashOut) fail(msg string) int {
	if o.kind == "codex" {
		o.enc.Encode(map[string]any{"type": "error", "message": msg})
		o.enc.Encode(map[string]any{"type": "turn.failed", "error": map[string]any{"message": msg}})
		return 1
	}
	o.enc.Encode(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": msg, "session_id": o.sid,
		"total_cost_usd": o.usd / 2, "usage": map[string]any{"input_tokens": o.usage[0] / 2, "output_tokens": o.usage[2] / 2}})
	return 1
}

// dashgenAgent is the scripted CLI. It returns the exit code.
func dashgenAgent(kind, state string) int {
	args := os.Args[1:]
	model, effort := "", ""
	for i, a := range args {
		if i+1 >= len(args) {
			break
		}
		switch {
		case a == "--model" || a == "-m":
			model = args[i+1]
		case a == "--effort":
			effort = args[i+1]
		case a == "-c" && strings.HasPrefix(args[i+1], "model_reasoning_effort="):
			effort = strings.TrimPrefix(args[i+1], "model_reasoning_effort=")
		}
	}
	for _, a := range args {
		switch a {
		case "--version":
			fmt.Println(config.Default().Providers[kind].TestedVersion)
			return 0
		case "status":
			fmt.Println(`{"loggedIn": true}`)
			return 0
		}
	}
	in, _ := io.ReadAll(os.Stdin)
	prompt := string(in)
	wd, _ := os.Getwd()
	o := &dashOut{kind: kind, enc: json.NewEncoder(os.Stdout), sid: fmt.Sprintf("dash-%s-%d-%d", kind, os.Getpid(), time.Now().UnixNano()), wd: wd}
	o.start(model)
	// Tokens and $ by model: a strong model reads more and costs more, so
	// Claude's Sonnet worker is the clearly cheaper route.
	switch {
	case kind == "codex" && (effort == "xhigh" || effort == "high"):
		o.usage = [3]int64{30000, 12000, 3000}
	case kind == "codex" && effort == "low":
		o.usage = [3]int64{4000, 2000, 300}
	case kind == "codex":
		o.usage = [3]int64{22000, 9000, 2000}
	case model == "opus":
		o.usage, o.usd = [3]int64{16000, 20000, 1800}, 0.42
	case model == "haiku":
		o.usage, o.usd = [3]int64{2500, 4000, 200}, 0.01
	default:
		o.usage, o.usd = [3]int64{7000, 9000, 700}, 0.07
	}
	now := time.Now()
	o.quota("allowed", 0.31, 0.12, now.Add(3*time.Hour))
	n := "0"
	if m := reDashTask.FindStringSubmatch(prompt); m != nil {
		n = m[1]
	}
	mark := func(name string) bool { // true the first time
		p := filepath.Join(state, strings.NewReplacer("/", "_", ".", "_").Replace(name)+"-"+n)
		if _, err := os.Stat(p); err == nil {
			return false
		}
		os.WriteFile(p, []byte(kind), 0o644)
		return true
	}
	verdict := func(ok bool, advice string, issues ...string) string {
		b, _ := json.Marshal(map[string]any{"approve": ok, "advice": advice, "issues": issues})
		return string(b)
	}
	switch {
	case strings.Contains(prompt, runner.MarkerFinalReview):
		if strings.Contains(prompt, "final=reject") && mark("final") {
			return o.ok(verdict(false, "Add a summary line to the notes file.", "notes file has no summary line"))
		}
		return o.ok(verdict(true, "Looks right."))
	case strings.Contains(prompt, runner.MarkerPlanReview), strings.Contains(prompt, runner.MarkerErrorReview):
		return o.ok(verdict(true, "ok"))
	case strings.Contains(prompt, runner.MarkerBestOf):
		return o.ok("```json\n{\"pick\": \"A\", \"why\": \"the smaller change\"}\n```")
	case strings.Contains(prompt, runner.MarkerJudge):
		return o.ok("B")
	case strings.Contains(prompt, runner.MarkerPlan):
		return o.ok(dashPlan(prompt, n))
	case strings.Contains(prompt, runner.MarkerFix):
		os.WriteFile(filepath.Join(wd, "fix-"+n+".txt"), []byte("summary\n"), 0o644)
		o.wrote("fix-" + n + ".txt")
		return o.ok("added the summary line")
	}
	// A step: only its own line counts (the prompt also holds the task).
	own := prompt
	if i := strings.Index(prompt, "YOUR SUBTASK"); i >= 0 {
		own = prompt[i:]
		if j := strings.Index(own, "\n"); j >= 0 {
			own = own[j+1:]
		}
		if j := strings.Index(own, "\n"); j >= 0 {
			own = own[:j]
		}
	}
	m := reDashWrite.FindStringSubmatch(own)
	if m == nil {
		return o.ok("The repository has a README and a src folder.")
	}
	file, content, how := m[1], m[2], m[3]
	high := (kind == "claude" && model == "opus") || (kind == "codex" && effort == "xhigh")
	switch how {
	case "repeat":
		if !high {
			return o.fail("go test failed: TestDash" + n + ": want 1, got 2")
		}
	case "broken":
		return o.fail("go test failed: TestBroken" + n + ": want 1, got 2")
	case "limit":
		if mark("limit-" + file) {
			o.usage, o.usd = [3]int64{}, 0 // as the real CLIs report a limit
			if kind == "claude" {
				reset := now.Add(3 * time.Second)
				o.quota("rejected", 1, 0.5, reset)
				return o.fail(fmt.Sprintf("Claude AI usage limit reached|%d", reset.Unix()))
			}
			return o.fail("You've hit your usage limit. Upgrade to Plus to continue using Codex.")
		}
	case "quota":
		o.quota("allowed", 0.95, 0.6, now.Add(4*time.Second))
	case "logout":
		if kind == "claude" {
			o.usage, o.usd = [3]int64{}, 0
			return o.fail("Not logged in · Please run /login")
		}
	case "hang":
		os.WriteFile(filepath.Join(state, "hang-"+n), []byte(wd), 0o644)
		time.Sleep(10 * time.Minute)
		return o.fail("dashgen agent: was not cancelled within 10 minutes")
	}
	p := filepath.Join(wd, filepath.FromSlash(file))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content+"\n"), 0o644); err != nil {
		return o.fail(err.Error())
	}
	o.wrote(file)
	return o.ok("wrote " + file)
}

// dashPlan turns a task's [spec ...] into a plan: "explore" adds a
// read-only step, name=how an edit step (how: ok, repeat, broken, limit,
// quota, logout, hang), "chain" makes each edit step wait for the one
// before it.
func dashPlan(prompt, n string) string {
	var spec []string
	if m := reDashSpec.FindStringSubmatch(prompt); m != nil {
		spec = strings.Fields(m[1])
	}
	chain := false
	for _, s := range spec {
		chain = chain || s == "chain"
	}
	var subs []map[string]any
	prev := ""
	for _, s := range spec {
		name, how, ok := strings.Cut(s, "=")
		switch {
		case s == "explore":
			subs = append(subs, map[string]any{"id": "look", "title": "look around", "kind": "explore", "prompt": "List the top-level files.", "files": []string{}})
		case ok && name != "final":
			file := fmt.Sprintf("notes/%s-%s.txt", name, n)
			p := fmt.Sprintf("write <<%s>> containing <<%s-%s>>", file, name, n)
			if how != "ok" {
				p += " {" + how + "}"
			}
			st := map[string]any{"id": name, "title": "write " + name, "kind": "edit", "prompt": p, "files": []string{file}}
			if chain && prev != "" {
				st["depends_on"] = []string{prev}
			}
			prev = name
			subs = append(subs, st)
		}
	}
	b, _ := json.Marshal(map[string]any{"summary": "Write the notes files.", "subtasks": subs})
	return "```json\n" + string(b) + "\n```"
}

// dashTask is one task of the generated history and what must come of it.
type dashTask struct {
	spec   string
	want   string // ok, failed, cancelled
	single string // provider:model: a single-agent baseline
}

// dashWeb is a running `rw web --client`.
type dashWeb struct {
	t     *testing.T
	cmd   *exec.Cmd
	stdin io.WriteCloser
	url   string
	sess  string
}

func startDashWeb(t *testing.T, bin, cfgPath, dir string, env []string) *dashWeb {
	t.Helper()
	cmd := exec.Command(bin, "web", "--client", "--config", cfgPath, "--dir", dir)
	cmd.Dir = dir
	cmd.Env = env
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w := &dashWeb{t: t, cmd: cmd, stdin: stdin}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("rw web --client: no hello: %v\n%s", err, stderr.String())
	}
	go io.Copy(io.Discard, stdout)
	var hello struct {
		URL       string `json:"url"`
		Bootstrap string `json:"bootstrap"`
	}
	if err := json.Unmarshal([]byte(line), &hello); err != nil {
		t.Fatalf("hello %q: %v", line, err)
	}
	w.url = hello.URL
	var s struct {
		Session string `json:"session"`
	}
	w.do("POST", "/api/session", map[string]string{"bootstrap": hello.Bootstrap}, &s)
	w.sess = s.Session
	return w
}

func (w *dashWeb) do(method, path string, body, out any) int {
	w.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, w.url+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if w.sess != "" {
		req.Header.Set("X-Relayweft-Session", w.sess)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		w.t.Fatalf("%s %s: %s: %s", method, path, resp.Status, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			w.t.Fatalf("%s %s: %v: %s", method, path, err, data)
		}
	}
	return resp.StatusCode
}

type dashState struct {
	Running bool `json:"running"`
	Queue   []any
	Last    *struct {
		OK   bool   `json:"ok"`
		Text string `json:"text"`
	} `json:"last"`
}

// run submits a task and waits until it ended; with cancel, it cancels the
// task once its hanging agent runs.
func (w *dashWeb) run(text string, single string, cancel func() bool) (ok bool, summary string) {
	w.t.Helper()
	body := map[string]string{"text": text}
	if single != "" {
		body["single"] = single
	}
	w.do("POST", "/api/task", body, nil)
	deadline := time.Now().Add(3 * time.Minute)
	cancelled := false
	for started := false; ; {
		var st dashState
		w.do("GET", "/api/state", nil, &st)
		started = started || st.Running
		if started && !st.Running && len(st.Queue) == 0 && st.Last != nil {
			return st.Last.OK, st.Last.Text
		}
		if cancel != nil && !cancelled && cancel() {
			w.do("POST", "/api/cancel", map[string]any{}, nil)
			cancelled = true
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("task %q did not end", text)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stop closes stdin: rw stops as on Ctrl+C and logs a clean exit.
func (w *dashWeb) stop() {
	w.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- w.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		w.cmd.Process.Kill()
		<-done
		w.t.Errorf("rw web did not stop within 30s of stdin closing")
	}
}

// dashView is the part of /api/dashboard this test reads.
type dashView struct {
	Days []struct {
		Date      string           `json:"date"`
		Tasks     int              `json:"tasks"`
		OK        int              `json:"ok"`
		Failed    int              `json:"failed"`
		Cancelled int              `json:"cancelled"`
		Providers map[string]int64 `json:"providers"`
		Tokens    int64            `json:"fresh_tokens"`
		USD       float64          `json:"usd"`
	} `json:"days"`
	Totals struct {
		Tasks     int              `json:"tasks"`
		OK        int              `json:"ok"`
		Failed    int              `json:"failed"`
		Cancelled int              `json:"cancelled"`
		Providers map[string]int64 `json:"providers"`
		Tokens    int64            `json:"fresh_tokens"`
		USD       float64          `json:"usd"`
	} `json:"totals"`
	Routes []struct {
		Role        string `json:"role"`
		Provider    string `json:"provider"`
		Model       string `json:"model"`
		Route       string `json:"route"`
		Runs        int    `json:"runs"`
		OK          int    `json:"ok"`
		Failed      int    `json:"failed"`
		LimitHits   int    `json:"limit_hits"`
		Unavailable int    `json:"unavailable"`
		Tokens      int64  `json:"fresh_tokens"`
		Escalated   int    `json:"escalated"`
		Reviews     int    `json:"reviews"`
		Rejected    int    `json:"rejected"`
		Decisions   map[string]struct {
			Runs int `json:"runs"`
		} `json:"decisions"`
	} `json:"routes"`
	Suggestions []struct {
		Title string `json:"Title"`
	} `json:"suggestions"`
	Limits struct {
		PerProvider map[string]struct {
			Hits        int `json:"hits"`
			Preempts    int `json:"preempts"`
			Fallbacks   int `json:"fallbacks"`
			Unavailable int `json:"unavailable"`
		} `json:"per_provider"`
		Hits        []any `json:"hits"`
		Unavailable []any `json:"unavailable"`
		Switches    []struct {
			Unavailable bool `json:"unavailable"`
		} `json:"switches"`
		Quota map[string]any
	} `json:"limits"`
	Quota   map[string]any `json:"quota"`
	Learned struct {
		Routes []struct {
			Role  string `json:"role"`
			Route string `json:"route"`
			InUse bool   `json:"in_use"`
		} `json:"routes"`
		Pending []any `json:"pending"`
	} `json:"learned"`
	LearnedEvents []struct {
		Role string `json:"role"`
		To   string `json:"to"`
		Runs int    `json:"runs"`
	} `json:"learned_events"`
	Health *struct {
		Criterion json.RawMessage `json:"criterion"`
		UseDays   []string        `json:"use_days"`
		BadDays   map[string]int  `json:"bad_days"`
	} `json:"health"`
	Warnings  []string `json:"warnings"`
	ElapsedMS int64    `json:"elapsed_ms"`
}

func TestDashboardMatchesCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("builds rw and runs whole tasks")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	work := t.TempDir()
	bin := filepath.Join(work, "rw")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(work, "profile")
	state := filepath.Join(work, "state")
	for _, d := range []string{profile, state} {
		os.MkdirAll(d, 0o755)
	}
	env := profileEnv(profile)
	if keep := os.Getenv("RW_DASHGEN_KEEP"); keep != "" {
		t.Cleanup(func() { // also after a failure
			if err := os.CopyFS(keep, os.DirFS(profile)); err != nil {
				t.Logf("keep %s: %v", keep, err)
			}
		})
	}
	if runtime.GOOS == "windows" {
		os.MkdirAll(filepath.Join(profile, "AppData", "Local", "Temp"), 0o755)
	}
	repo1, repo2 := dashRepo(t, filepath.Join(work, "app")), dashRepo(t, filepath.Join(work, "lib"))

	writeCfg := func(name string, edit func(map[string]any)) string {
		prov := func(kind string) map[string]any {
			return map[string]any{"command": self, "extra_args": []string{dashgenCLIArg + kind, dashgenStateArg + state}, "limit_cooldown": "3s"}
		}
		cfg := map[string]any{
			"providers":    map[string]any{"claude": prov("claude"), "codex": prov("codex")},
			"orchestrator": map[string]any{"approve_plan": false, "min_free_disk_gb": 0, "max_cpu_percent": 0, "min_free_memory_mb": 0},
			"notify":       map[string]any{"enabled": false},
			"routing":      map[string]any{"learn": "suggest"},
		}
		if edit != nil {
			edit(cfg)
		}
		data, _ := yaml.Marshal(cfg)
		p := filepath.Join(work, name+".yaml")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := writeCfg("base", nil)
	claudeWorkers := writeCfg("claude-workers", func(c map[string]any) {
		c["roles"] = map[string]any{"worker": map[string]any{"prefer": "claude", // a role is replaced whole
			"codex": map[string]any{"model": "gpt-6.1-sol", "effort": "medium"}, "claude": map[string]any{"model": "sonnet", "effort": "medium"}}}
		c["routing"] = map[string]any{"learn": "suggest", "judge": true, "judge_below_confidence": 0.75}
	})
	tiers := writeCfg("tiers", func(c map[string]any) {
		c["routing"] = map[string]any{"learn": "suggest", "tiers": "auto"}
	})
	bestOf := writeCfg("best-of", func(c map[string]any) {
		c["routing"] = map[string]any{"learn": "suggest", "best_of": map[string]any{"when": "always", "n": 2}}
	})

	var want struct{ ok, failed, cancelled, tasks int }
	n := 0
	phase := func(cfgPath, dir string, tasks []dashTask) {
		t.Helper()
		w := startDashWeb(t, bin, cfgPath, dir, env)
		defer w.stop()
		for _, tk := range tasks {
			n++
			text := fmt.Sprintf("Relayweft dashboard check task %d: write the notes files exactly as the plan says [spec %s]", n, tk.spec)
			var cancel func() bool
			if tk.want == "cancelled" {
				marker := filepath.Join(state, fmt.Sprintf("hang-%d", n))
				cancel = func() bool { _, err := os.Stat(marker); return err == nil }
			}
			ok, summary := w.run(text, tk.single, cancel)
			got := "failed"
			switch {
			case ok:
				got = "ok"
			case strings.HasPrefix(summary, "cancelled"):
				got = "cancelled"
			}
			if got != tk.want {
				t.Fatalf("task %d [%s]: %s (%q), want %s; its log:\n%s", n, tk.spec, got, summary, tk.want, dashTaskLog(profile, n))
			}
			want.tasks++
			switch got {
			case "ok":
				want.ok++
			case "failed":
				want.failed++
			default:
				want.cancelled++
			}
		}
	}

	// Session 1, the default routes (workers on Codex): plain tasks, an
	// escalation after a repeated error, a broken step, Codex and Claude
	// limits with fallbacks, a rejected final review, a cancel, and a
	// single-agent baseline.
	phase(base, repo1, []dashTask{
		{spec: "explore a=ok b=ok", want: "ok"},
		{spec: "a=ok b=ok", want: "ok"},
		{spec: "explore a=repeat b=ok", want: "ok"},
		{spec: "a=ok b=limit", want: "ok"},
		{spec: "a=ok b=ok final=reject", want: "ok"},
		{spec: "a=broken b=ok", want: "failed"},
		{spec: "a=ok b=hang", want: "cancelled"},
		{spec: "a=ok b=ok", want: "ok"},
		{spec: "explore a=ok b=ok", want: "ok"},
		{spec: "a=ok", want: "ok", single: "claude:sonnet"},
		{spec: "a=ok b=ok", want: "ok"},
	})
	// Session 2, workers on Claude and the judge on: enough runs for a
	// learned route, a nearly full quota that moves the next step away
	// before the limit, and a Claude limit. Claude logged out ends it (rw
	// then routes around Claude for 12 hours).
	phase(claudeWorkers, repo1, []dashTask{
		{spec: "a=ok b=ok", want: "ok"},
		{spec: "explore a=ok b=ok", want: "ok"},
		{spec: "chain a=quota b=ok", want: "ok"},
		{spec: "a=ok b=ok", want: "ok"},
		{spec: "a=limit b=ok", want: "ok"},
		{spec: "a=ok b=ok", want: "ok"},
		{spec: "a=ok b=ok final=reject", want: "ok"},
		{spec: "a=ok b=logout", want: "ok"},
	})
	// What `rw tune --apply` learns: Claude's Sonnet succeeds as often as
	// Codex with far fewer tokens.
	out := runRW(t, bin, env, repo1, "tune", "--config", base, "--apply")
	if !strings.Contains(out, "worker") || !strings.Contains(out, "claude:sonnet") {
		t.Fatalf("rw tune --apply learned no worker route:\n%s", out)
	}
	// Session 3: the learned route in use, best of 2.
	phase(bestOf, repo1, []dashTask{
		{spec: "a=ok", want: "ok"},
		{spec: "explore a=ok b=ok", want: "ok"},
		{spec: "a=ok b=ok", want: "ok"},
	})
	// Session 4: another project, with model tiers.
	phase(tiers, repo2, []dashTask{
		{spec: "a=ok b=ok", want: "ok"},
		{spec: "a=broken", want: "failed"},
	})

	// The dashboard, built fresh, over 7 days and for this project only.
	w := startDashWeb(t, bin, base, repo1, env)
	var all, here dashView
	w.do("GET", "/api/dashboard?days=7&fresh=1", nil, &all)
	w.do("GET", "/api/dashboard?days=7&here=1&fresh=1", nil, &here)
	w.stop()
	t.Logf("dashboard built in %d ms (%d tasks)", all.ElapsedMS, all.Totals.Tasks)
	if len(all.Warnings) > 0 {
		t.Errorf("dashboard warnings: %v", all.Warnings)
	}

	// What happened.
	tt := all.Totals
	if tt.Tasks != want.tasks || tt.OK != want.ok || tt.Failed != want.failed || tt.Cancelled != want.cancelled {
		t.Errorf("dashboard totals %d tasks (%d ok, %d failed, %d cancelled), want %d (%d, %d, %d)",
			tt.Tasks, tt.OK, tt.Failed, tt.Cancelled, want.tasks, want.ok, want.failed, want.cancelled)
	}

	// rw stats --json: tasks, tokens and $ per day, runs per model.
	compareStats(t, "all", all, runRW(t, bin, env, repo1, "stats", "--config", base, "--json"))
	compareStats(t, "here", here, runRW(t, bin, env, repo1, "stats", "--config", base, "--json", "--here"))
	statsText := runRW(t, bin, env, repo1, "stats", "--config", base)
	// rw stats counts both kinds of switch as "limit fallbacks".
	switches := 0
	for _, c := range all.Limits.PerProvider {
		switches += c.Fallbacks + c.Preempts
	}
	if !strings.Contains(statsText, fmt.Sprintf("limit fallbacks: %d ", switches)) {
		t.Errorf("dashboard: %d fallbacks and pre-emptive switches; rw stats:\n%s", switches, statsText)
	}

	// rw tune: the same suggestions, in the same order.
	tune := runRW(t, bin, env, repo1, "tune", "--config", base, "--since", "7d")
	var titles []string
	for _, line := range strings.Split(tune, "\n") {
		if len(line) > 4 && line[0] == '[' && line[3] == ']' { // "[! ] title"
			titles = append(titles, strings.TrimSpace(line[4:]))
		}
	}
	var dtitles []string
	for _, s := range all.Suggestions {
		dtitles = append(dtitles, s.Title)
	}
	if strings.Join(titles, "\n") != strings.Join(dtitles, "\n") {
		t.Errorf("suggestions differ:\nrw tune:   %q\ndashboard: %q", titles, dtitles)
	}

	// rw tune --learned: the learned routes, in use.
	learned := runRW(t, bin, env, repo1, "tune", "--config", base, "--learned")
	if len(all.Learned.Routes) == 0 {
		t.Errorf("dashboard shows no learned routes; rw tune --learned:\n%s", learned)
	}
	for _, r := range all.Learned.Routes {
		if !strings.Contains(learned, r.Route) || !r.InUse {
			t.Errorf("learned %s %s (in use %v); rw tune --learned:\n%s", r.Role, r.Route, r.InUse, learned)
		}
	}
	if len(all.Learned.Pending) != 0 {
		t.Errorf("dashboard: rw tune --apply would change %v right after it ran", all.Learned.Pending)
	}

	// rw health --json: the same criterion and days.
	var health struct {
		Criterion json.RawMessage `json:"criterion"`
		UseDays   []string        `json:"use_days"`
	}
	if err := json.Unmarshal([]byte(runRW(t, bin, env, repo1, "health", "--json")), &health); err != nil {
		t.Fatal(err)
	}
	if all.Health == nil {
		t.Fatal("dashboard: no health")
	}
	if !sameCriterion(all.Health.Criterion, health.Criterion) || strings.Join(all.Health.UseDays, ",") != strings.Join(health.UseDays, ",") {
		t.Errorf("health differs:\ndashboard: %s %v\nrw health: %s %v", all.Health.Criterion, all.Health.UseDays, health.Criterion, health.UseDays)
	}
	if len(health.UseDays) == 0 {
		t.Errorf("rw health: no day of use after %d tasks", want.tasks)
	}

	// The generated history has every kind of record the panels show.
	var esc, rejected, reviews int
	flags := map[string]int{}
	provs := map[string]bool{}
	for _, r := range all.Routes {
		esc += r.Escalated
		rejected += r.Rejected
		reviews += r.Reviews
		provs[r.Provider] = true
		for f, c := range r.Decisions {
			flags[f] += c.Runs
		}
	}
	for _, f := range []string{"judged", "learned", "tier", "repeat_error", "fallback", "preempt"} {
		if flags[f] == 0 {
			t.Errorf("no decision with flag %s: %v", f, flags)
		}
	}
	if esc == 0 || rejected == 0 || reviews <= rejected || len(provs) < 2 {
		t.Errorf("escalated %d, rejected %d of %d final reviews, providers %v", esc, rejected, reviews, provs)
	}
	lp := all.Limits.PerProvider
	if lp["codex"].Hits == 0 || lp["claude"].Hits == 0 || lp["claude"].Preempts == 0 {
		t.Errorf("limits: %+v", lp)
	}
	if len(all.LearnedEvents) == 0 || all.Limits.Quota["claude"] == nil {
		t.Errorf("learned events %v, quota readings %v", all.LearnedEvents, all.Limits.Quota)
	}
	// A logged-out Claude is not a usage limit: rw routes around it, and
	// the dashboard says so instead of "hit its limit".
	if lp["claude"].Unavailable != 1 || len(all.Limits.Unavailable) != 1 || lp["claude"].Hits != 1 {
		t.Errorf("claude: %d limit hits, %d unavailable (%d listed); want 1 and 1", lp["claude"].Hits, lp["claude"].Unavailable, len(all.Limits.Unavailable))
	}
	away := 0
	for _, sw := range all.Limits.Switches {
		if sw.Unavailable {
			away++
		}
	}
	if away == 0 {
		t.Errorf("no fallback marked as moving away from the logged-out Claude")
	}
	if here.Totals.Tasks >= all.Totals.Tasks || here.Totals.Tasks == 0 {
		t.Errorf("this project: %d tasks of %d", here.Totals.Tasks, all.Totals.Tasks)
	}
}

// compareStats checks the dashboard against `rw stats --json` (an export:
// per day tasks, tokens, $ and limit hits, and runs per provider:model).
func compareStats(t *testing.T, what string, d dashView, export string) {
	t.Helper()
	var e struct {
		Days []struct {
			Date      string           `json:"date"`
			Tasks     int              `json:"tasks"`
			OK        int              `json:"ok"`
			Fresh     int64            `json:"fresh_tokens"`
			USD       float64          `json:"usd"`
			Providers map[string]int64 `json:"providers"`
			LimitHits int              `json:"limit_hits"`
			Models    []struct {
				Provider  string  `json:"provider"`
				Model     string  `json:"model"`
				Calls     int     `json:"calls"`
				OK        int     `json:"ok"`
				Fresh     int64   `json:"fresh_tokens"`
				USD       float64 `json:"usd"`
				LimitHits int     `json:"limit_hits"`
				Unavail   int     `json:"unavailable"`
			} `json:"models"`
		} `json:"days"`
	}
	if err := json.Unmarshal([]byte(export), &e); err != nil {
		t.Fatalf("%s: rw stats --json: %v\n%s", what, err, export)
	}
	days := map[string]int{}
	for i, dd := range d.Days {
		days[dd.Date] = i
	}
	type model struct {
		calls, ok, limit, unavail int
		fresh                     int64
	}
	em, dm := map[string]*model{}, map[string]*model{}
	for _, ed := range e.Days {
		i, ok := days[ed.Date]
		if !ok {
			t.Errorf("%s: rw stats has %s, the dashboard has no such day", what, ed.Date)
			continue
		}
		dd := d.Days[i]
		if dd.Tasks != ed.Tasks || dd.OK != ed.OK || dd.Tokens != ed.Fresh || math.Abs(dd.USD-ed.USD) > 0.005 || !mapsEqual(dd.Providers, ed.Providers) {
			t.Errorf("%s %s: dashboard %d tasks, %d ok, %d tokens %v, $%.2f; rw stats %d, %d, %d %v, $%.2f",
				what, ed.Date, dd.Tasks, dd.OK, dd.Tokens, dd.Providers, dd.USD, ed.Tasks, ed.OK, ed.Fresh, ed.Providers, ed.USD)
		}
		for _, m := range ed.Models {
			k := m.Provider + ":" + m.Model
			if em[k] == nil {
				em[k] = &model{}
			}
			em[k].calls += m.Calls
			em[k].ok += m.OK
			em[k].limit += m.LimitHits
			em[k].unavail += m.Unavail
			em[k].fresh += m.Fresh
		}
	}
	for _, r := range d.Routes {
		k := r.Provider + ":" + r.Model
		if dm[k] == nil {
			dm[k] = &model{}
		}
		// The export counts every run; the dashboard keeps limit hits and
		// unavailable CLIs apart.
		dm[k].calls += r.Runs + r.LimitHits + r.Unavailable
		dm[k].ok += r.OK
		dm[k].limit += r.LimitHits
		dm[k].unavail += r.Unavailable
		dm[k].fresh += r.Tokens
	}
	var keys []string
	for k := range em {
		keys = append(keys, k)
	}
	for k := range dm {
		if em[k] == nil {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		a, b := dm[k], em[k]
		if a == nil || b == nil || *a != *b {
			t.Errorf("%s %s: dashboard routes %+v, rw stats %+v", what, k, a, b)
		}
	}
}

func mapsEqual(a, b map[string]int64) bool {
	for k, v := range a {
		if v != 0 && b[k] != v {
			return false
		}
	}
	for k, v := range b {
		if v != 0 && a[k] != v {
			return false
		}
	}
	return true
}

// sameCriterion compares two health criteria; the clean days and the
// summary that says them grow while the test runs.
func sameCriterion(a, b json.RawMessage) bool {
	var x, y map[string]any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	cx, cy := x["clean_days"].(float64), y["clean_days"].(float64)
	if math.Abs(cx-cy) > 0.01 {
		return false
	}
	for _, m := range []map[string]any{x, y} {
		delete(m, "clean_days")
		delete(m, "summary")
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}

// dashTaskLog is the session log records of task n, shortened.
func dashTaskLog(profile string, n int) string {
	var b strings.Builder
	filepath.WalkDir(profile, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".jsonl" || filepath.Base(filepath.Dir(p)) != "sessions" {
			return nil
		}
		data, _ := os.ReadFile(p)
		id := ""
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, fmt.Sprintf("dashboard check task %d:", n)) && strings.Contains(line, `"type":"task_start"`) {
				var r struct {
					TaskID string `json:"task_id"`
				}
				json.Unmarshal([]byte(line), &r)
				id = r.TaskID
			}
			if id != "" && strings.Contains(line, `"task_id":"`+id+`"`) {
				if len(line) > 400 {
					line = line[:400] + "..."
				}
				b.WriteString(line + "\n")
			}
		}
		return nil
	})
	return b.String()
}

// runRW runs the built rw in the test profile and returns its stdout.
func runRW(t *testing.T, bin string, env []string, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rw %s: %v\n%s%s", strings.Join(args, " "), err, out, stderr.String())
	}
	return string(out)
}

// dashRepo makes a small git repo.
func dashRepo(t *testing.T, dir string) string {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# dashboard check\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "core.autocrlf", "false"}, {"add", "-A"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "init"}} {
		run(t, dir, args...)
	}
	return dir
}
