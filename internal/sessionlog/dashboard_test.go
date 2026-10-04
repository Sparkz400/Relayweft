package sessionlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/event"
)

// writeLog writes records as one session file, the way Writer does.
func writeLog(t testing.TB, dir, name string, recs []Record) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	for _, r := range recs {
		b, _ := json.Marshal(r)
		w.Write(append(b, '\n'))
	}
	w.Flush()
	f.Close()
}

func TestBuildDashboard(t *testing.T) {
	now := time.Date(2026, 10, 4, 18, 0, 0, 0, time.Local)
	repo, other := filepath.Join(t.TempDir(), "repo"), filepath.Join(t.TempDir(), "other")
	at := func(daysAgo int, h int) time.Time {
		return DayStart(now).AddDate(0, 0, -daysAgo).Add(time.Duration(h) * time.Hour)
	}
	tok := func(in, out int64, usd float64) *event.TokenUsage {
		return &event.TokenUsage{Input: in, Output: out, CostUSD: usd}
	}
	var recs []Record
	add := func(r Record) {
		if r.Session == "" {
			r.Session = "s1"
		}
		if r.Cwd == "" {
			r.Cwd = repo
		}
		recs = append(recs, r)
	}
	end := func(task string, ts time.Time, ok bool, text string, codex, claude int64, usd float64) {
		add(Record{Type: TypeTaskEnd, TS: ts, TaskID: task, OK: Bool(ok), Text: text, DurationMS: 60_000,
			Cost: &event.TaskCost{CostUSD: usd, PerProvider: map[string]event.TokenUsage{event.Codex: {Input: codex}, event.Claude: {Input: claude}}}})
	}
	// t1 (today): a worker on codex fails once and is escalated to
	// worker_high on claude; the final review rejects the work once.
	add(Record{Type: TypeDecision, TS: at(0, 9), TaskID: "t1", Step: "s1", Attempt: 1, Role: "worker", Provider: "codex", Model: "gpt-x", Effort: "medium", Rule: "default", Judged: true})
	add(Record{Type: TypeAgentEnd, TS: at(0, 9), TaskID: "t1", Step: "s1", Attempt: 1, Role: "worker", Provider: "codex", Model: "gpt-x", Effort: "medium", OK: Bool(false), Tokens: tok(1000, 100, 0), DurationMS: 30_000})
	add(Record{Type: TypeDecision, TS: at(0, 10), TaskID: "t1", Step: "s1", Attempt: 2, Role: "worker_high", Provider: "claude", Model: "opus", Rule: "error-repeats",
		Reason: "same error twice: worker -> worker_high; learned route claude:opus: succeeded 90% of 9 runs vs 50% of 8 on codex:gpt-x"})
	add(Record{Type: TypeAgentEnd, TS: at(0, 10), TaskID: "t1", Step: "s1", Attempt: 2, Role: "worker_high", Provider: "claude", Model: "opus", OK: Bool(true), Tokens: tok(2000, 500, 0.40), DurationMS: 90_000})
	add(Record{Type: TypeReview, TS: at(0, 11), TaskID: "t1", Step: "final", OK: Bool(false)})
	add(Record{Type: TypeReview, TS: at(0, 11), TaskID: "t1", Step: "final", OK: Bool(true)})
	end("t1", at(0, 12), true, "done", 1100, 2500, 0.40)
	// t2 (2 days ago) failed, t3 (2 days ago) was cancelled.
	end("t2", at(2, 9), false, "step s1 failed", 500, 0, 0)
	end("t3", at(2, 10), false, "cancelled: stopped", 0, 300, 0.05)
	// A task in another repo: counted only for all repos.
	add(Record{Type: TypeTaskEnd, TS: at(1, 9), Session: "s2", Cwd: other, TaskID: "t4", OK: Bool(true), Cost: &event.TaskCost{PerProvider: map[string]event.TokenUsage{"qwen": {Input: 7000}}}})
	// Outside the 7-day range.
	end("old", at(9, 9), true, "done", 1, 1, 0)
	// Limits (in the other repo: they count for the account all the same).
	until := at(0, 15)
	add(Record{Type: TypeLimit, TS: at(1, 14), Session: "s2", Cwd: other, Provider: "claude", Model: "opus", Text: "Claude AI usage limit reached|123\n\x1b[31mred", Until: &until})
	add(Record{Type: TypeDecision, TS: at(1, 15), Session: "s2", Cwd: other, TaskID: "t4", Step: "a", Attempt: 1, Role: "explorer", Provider: "codex", Model: "luna", Rule: "quota-preempt", Fallback: true, From: "claude"})
	add(Record{Type: TypeDecision, TS: at(1, 16), Session: "s2", Cwd: other, TaskID: "t4", Step: "b", Attempt: 1, Role: "explorer", Provider: "codex", Model: "luna", Rule: "limit-fallback", Fallback: true})
	add(Record{Type: TypeQuota, TS: at(0, 8), Provider: "claude", Quota: &event.QuotaInfo{Utilization: 0.2, Window: "five_hour"}})
	add(Record{Type: TypeQuota, TS: at(0, 9), Provider: "claude", Quota: &event.QuotaInfo{Utilization: 0.7, Window: "seven_day", Windows: map[string]float64{"five_hour": 0.3, "seven_day": 0.7}}})
	// A learned route that changes: explorer on codex:luna, then claude:haiku.
	for i, spec := range []string{"codex:luna", "codex:luna", "claude:haiku"} {
		add(Record{Type: TypeDecision, TS: at(3-i, 9), TaskID: fmt.Sprintf("l%d", i), Step: "e", Attempt: 1, Role: "explorer", Rule: "read-only",
			Reason: "read-only step; learned route " + spec + ": as reliable with fewer tokens; tier fast"})
	}
	// A failing route, for the tune flag: 5 of 6 planner runs fail.
	for i := 0; i < 6; i++ {
		add(Record{Type: TypeAgentEnd, TS: at(4, i), TaskID: fmt.Sprintf("p%d", i), Step: "plan", Attempt: 1, Role: "planner", Provider: "codex", Model: "gpt-x", OK: Bool(i == 0)})
	}

	dir := t.TempDir()
	writeLog(t, dir, "20261004-090000-aaaa", recs)
	read, err := ReadDir(dir)
	if err != nil || len(read) != len(recs) {
		t.Fatalf("read %d of %d records: %v", len(read), len(recs), err)
	}

	d := BuildDashboard(read, DashboardOptions{Days: 7, Cwd: repo, Now: now})
	if d.From != "2026-09-28" || d.To != "2026-10-04" || len(d.Days) != 7 {
		t.Fatalf("range %s..%s with %d days", d.From, d.To, len(d.Days))
	}
	today, twoAgo, yesterday := d.Days[6], d.Days[4], d.Days[5]
	if today.Date != "2026-10-04" || today.Tasks != 1 || today.OK != 1 || today.Providers["claude"] != 2500 || today.USD != 0.40 || today.WallMS != 60_000 {
		t.Errorf("today = %+v", today)
	}
	if twoAgo.Tasks != 2 || twoAgo.Failed != 1 || twoAgo.Cancelled != 1 || twoAgo.OK != 0 {
		t.Errorf("two days ago = %+v", twoAgo)
	}
	if yesterday.Tasks != 0 {
		t.Errorf("the other repo's task counted here: %+v", yesterday)
	}
	if tt := d.Totals; tt.Tasks != 3 || tt.OK != 1 || tt.Failed != 1 || tt.Cancelled != 1 || tt.Tokens != 1100+2500+500+300 || tt.Providers["codex"] != 1600 {
		t.Errorf("totals = %+v", tt)
	}
	if fmt.Sprint(d.Providers) != "[codex claude]" {
		t.Errorf("providers = %v", d.Providers)
	}

	row := func(role, route string) *RouteRow {
		for _, r := range d.Routes {
			if r.Role == role && r.Route == route {
				return r
			}
		}
		t.Fatalf("no row %s %s in %+v", role, route, d.Routes)
		return nil
	}
	w := row("worker", "codex:gpt-x:medium")
	if w.Runs != 1 || w.Failed != 1 || w.Escalated != 1 || w.Reviews != 2 || w.Rejected != 1 || w.Decisions["judged"] != (FlagCount{1, 1}) {
		t.Errorf("worker row = %+v", w)
	}
	hi := row("worker_high", "claude:opus")
	if hi.Success != 1 || hi.AvgTokens != 2500 || hi.AvgMS != 90_000 || hi.AvgUSD != 0.40 || hi.Decisions["learned"].Runs != 1 || hi.Decisions["repeat_error"].Runs != 1 || hi.Escalated != 0 {
		t.Errorf("worker_high row = %+v", hi)
	}
	p := row("planner", "codex:gpt-x")
	if p.Runs != 6 || p.Failed != 5 || len(p.Suggested) != 1 || d.Suggestions[p.Suggested[0]].Title != "planner on codex:gpt-x fails often" {
		t.Errorf("planner row = %+v, suggestions %+v", p, d.Suggestions)
	}
	if d.Routes[0].Role != "planner" {
		t.Errorf("rows not in role order: first is %s", d.Routes[0].Role)
	}

	lc := d.Limits.PerProvider["claude"]
	if lc == nil || lc.Hits != 1 || lc.Preempts != 1 || lc.Fallbacks != 1 {
		t.Errorf("claude limits = %+v", lc)
	}
	if len(d.Limits.Hits) != 1 || d.Limits.Hits[0].Text != "Claude AI usage limit reached|123 [31mred" || d.Limits.Hits[0].Until == nil {
		t.Errorf("hits = %+v", d.Limits.Hits)
	}
	if len(d.Limits.Switches) != 2 || d.Limits.Switches[0].Rule != "limit-fallback" || d.Limits.Switches[1].Provider != "claude" || d.Limits.Switches[1].To != "codex" {
		t.Errorf("switches = %+v", d.Limits.Switches)
	}
	if q := d.Limits.Quota["claude"]; q.Quota.Windows["seven_day"] != 0.7 {
		t.Errorf("quota = %+v", q)
	}

	// Newest first: worker_high's learned route (t1), then explorer's
	// change from codex:luna to claude:haiku.
	if len(d.Learned) != 3 || d.Learned[0].Role != "worker_high" {
		t.Fatalf("learned events = %+v", d.Learned)
	}
	if e := d.Learned[1]; e.Role != "explorer" || e.From != "codex:luna" || e.To != "claude:haiku" || e.Why != "as reliable with fewer tokens" {
		t.Errorf("explorer change = %+v", e)
	}
	if e := d.Learned[2]; e.To != "codex:luna" || e.From != "" || e.Runs != 2 || !e.Last.Equal(at(2, 9)) {
		t.Errorf("first explorer event = %+v", e)
	}

	// All repos: the other repo's task and provider count.
	all := BuildDashboard(read, DashboardOptions{Days: 7, Now: now})
	if all.Totals.Tasks != 4 || fmt.Sprint(all.Providers) != "[codex claude qwen]" || all.Days[5].Providers["qwen"] != 7000 {
		t.Errorf("all repos: totals %+v, providers %v", all.Totals, all.Providers)
	}
	// 30 days: the old task is in.
	if m := BuildDashboard(read, DashboardOptions{Days: 30, Cwd: repo, Now: now}); len(m.Days) != 30 || m.Totals.Tasks != 4 {
		t.Errorf("30 days: %d days, %d tasks", len(m.Days), m.Totals.Tasks)
	}
}

// A fresh install: no records gives every day with zeros and empty lists
// (never null, so a page needs no special case).
func TestBuildDashboardEmpty(t *testing.T) {
	d := BuildDashboard(nil, DashboardOptions{Days: 7, Now: time.Now()})
	b, _ := json.Marshal(d)
	var m map[string]any
	json.Unmarshal(b, &m)
	for _, k := range []string{"days", "routes", "suggestions", "learned_events", "providers", "decision_flags"} {
		if v, ok := m[k].([]any); !ok || (k == "days" && len(v) != 7) {
			t.Errorf("%s = %v", k, m[k])
		}
	}
	lim := m["limits"].(map[string]any)
	for _, k := range []string{"hits", "switches"} {
		if _, ok := lim[k].([]any); !ok {
			t.Errorf("limits.%s = %v", k, lim[k])
		}
	}
}

// synthLog writes a log of n tasks over the last 90 days, shaped like real
// use: planner and work steps on both providers, escalations, reviews,
// limit hits, quota readings and learned routes. Sessions hold 25 tasks.
func synthLog(t testing.TB, dir string, n int, now time.Time) {
	t.Helper()
	rng := rand.New(rand.NewSource(1))
	routes := []struct{ role, prov, model, effort string }{
		{"worker", "codex", "gpt-6", "medium"}, {"worker", "claude", "sonnet", "high"}, {"explorer", "codex", "gpt-6-luna", "low"},
		{"explorer", "claude", "haiku", ""}, {"worker_high", "claude", "opus", "high"}, {"researcher", "claude", "sonnet", ""},
	}
	var recs []Record
	flush := func(i int) {
		if len(recs) > 0 {
			writeLog(t, dir, fmt.Sprintf("synth-%05d", i), recs)
			recs = recs[:0]
		}
	}
	for i := 0; i < n; i++ {
		if i%25 == 0 {
			flush(i)
		}
		sess := fmt.Sprintf("s%04d", i/25)
		ts := now.Add(-time.Duration(float64(90*24*time.Hour) * float64(n-i) / float64(n)))
		id := fmt.Sprintf("t%05d", i)
		cwd := "/repo/a"
		if i%3 == 0 {
			cwd = "/repo/b"
		}
		rec := func(r Record) {
			r.Session, r.Cwd, r.TaskID = sess, cwd, id
			ts = ts.Add(time.Duration(rng.Intn(90)) * time.Second)
			r.TS = ts
			recs = append(recs, r)
		}
		rec(Record{Type: TypeTask, Task: "synthetic task", Mode: "routed"})
		rec(Record{Type: TypeDecision, Step: "plan", Attempt: 1, Role: "planner", Provider: "claude", Model: "opus", Rule: "plan", Reason: "plan the task"})
		rec(Record{Type: TypeAgentEnd, Step: "plan", Attempt: 1, Role: "planner", Provider: "claude", Model: "opus", OK: Bool(true), Tokens: &event.TokenUsage{Input: 8000, Output: 900, CostUSD: 0.2}, DurationMS: 20_000})
		cost := event.TaskCost{PerProvider: map[string]event.TokenUsage{}}
		for s := 0; s < 3; s++ {
			r := routes[rng.Intn(len(routes))]
			step := fmt.Sprintf("s%d", s)
			reason := "default rule"
			if rng.Intn(4) == 0 {
				reason += "; learned route " + r.prov + ":" + r.model + ": succeeded 90% of 12 runs vs 60% of 10"
			}
			rec(Record{Type: TypeDecision, Step: step, Attempt: 1, Role: r.role, Provider: r.prov, Model: r.model, Effort: r.effort, Rule: "default", Reason: reason, Judged: rng.Intn(5) == 0})
			ok := rng.Intn(6) != 0
			tk := event.TokenUsage{Input: int64(2000 + rng.Intn(30000)), Cached: 500, Output: int64(200 + rng.Intn(3000))}
			if r.prov == "claude" {
				tk.CostUSD = float64(tk.Total()) / 1e5
			}
			rec(Record{Type: TypeAgentEnd, Step: step, Attempt: 1, Role: r.role, Provider: r.prov, Model: r.model, Effort: r.effort, OK: Bool(ok), Tokens: &tk, DurationMS: int64(5000 + rng.Intn(120_000))})
			u := cost.PerProvider[r.prov]
			cost.PerProvider[r.prov] = u.Add(tk)
			cost.CostUSD += tk.CostUSD
			if !ok && rng.Intn(2) == 0 {
				rec(Record{Type: TypeDecision, Step: step, Attempt: 2, Role: "worker_high", Provider: "claude", Model: "opus", Rule: "error-repeats", Reason: "same error twice: " + r.role + " -> worker_high"})
				rec(Record{Type: TypeAgentEnd, Step: step, Attempt: 2, Role: "worker_high", Provider: "claude", Model: "opus", OK: Bool(true), Tokens: &tk, DurationMS: 60_000})
			}
		}
		if i%40 == 0 {
			until := ts.Add(3 * time.Hour)
			rec(Record{Type: TypeLimit, Provider: "claude", Model: "opus", Text: "Claude AI usage limit reached", Until: &until})
			rec(Record{Type: TypeDecision, Step: "s9", Attempt: 1, Role: "explorer", Provider: "codex", Model: "gpt-6-luna", Rule: "quota-preempt", Fallback: true, From: "claude"})
		}
		if i%10 == 0 {
			rec(Record{Type: TypeQuota, Provider: "claude", Quota: &event.QuotaInfo{Utilization: rng.Float64(), Window: "five_hour", Windows: map[string]float64{"five_hour": rng.Float64(), "seven_day": rng.Float64()}}})
		}
		rec(Record{Type: TypeReview, Step: "final", Provider: "codex", Model: "gpt-6", OK: Bool(rng.Intn(4) != 0)})
		ok := rng.Intn(5) != 0
		text := "done"
		if !ok && rng.Intn(3) == 0 {
			text = "cancelled: stopped"
		}
		tk := event.TokenUsage{Input: 30000}
		rec(Record{Type: TypeTaskEnd, Task: "synthetic task", Mode: "routed", OK: Bool(ok), Text: text, Tokens: &tk, DurationMS: int64(60_000 + rng.Intn(600_000)), Cost: &cost})
	}
	flush(n)
}

// TestDashboardLargeLog checks a few thousand tasks stay quick to read and
// aggregate (the endpoint does both on every load).
func TestDashboardLargeLog(t *testing.T) {
	if testing.Short() {
		t.Skip("large log")
	}
	dir := t.TempDir()
	now := time.Now()
	synthLog(t, dir, 3000, now)
	began := time.Now()
	recs, err := ReadDirSince(dir, DashboardSince(now, 90))
	if err != nil {
		t.Fatal(err)
	}
	read := time.Since(began)
	d := BuildDashboard(recs, DashboardOptions{Days: 90, Now: now})
	took := time.Since(began)
	t.Logf("%d records: read %v, read+aggregate %v", len(recs), read, took)
	if d.Totals.Tasks < 2900 || len(d.Routes) < 6 || len(d.Limits.Hits) != dashEvents || len(d.Learned) == 0 {
		t.Errorf("totals %+v, %d routes, %d hits, %d learned", d.Totals, len(d.Routes), len(d.Limits.Hits), len(d.Learned))
	}
	if took > 20*time.Second { // generous: CI machines are slow and shared
		t.Errorf("took %v", took)
	}
}

func BenchmarkDashboard(b *testing.B) {
	dir := b.TempDir()
	now := time.Now()
	synthLog(b, dir, 3000, now)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		recs, _ := ReadDirSince(dir, DashboardSince(now, 90))
		BuildDashboard(recs, DashboardOptions{Days: 90, Now: now})
	}
}
