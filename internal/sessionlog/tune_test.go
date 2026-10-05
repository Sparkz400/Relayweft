package sessionlog

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/event"
)

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// runs makes n agent_end records for a route, the first fails of them failed.
func runs(role, prov, model, effort string, n, fails int) []Record {
	var out []Record
	for i := 0; i < n; i++ {
		out = append(out, Record{Type: TypeAgentEnd, TS: t0, Session: "s", TaskID: fmt.Sprint(i), Step: "s1", Attempt: 1,
			Role: role, Provider: prov, Model: model, Effort: effort, OK: Bool(i >= fails),
			Tokens: &event.TokenUsage{Input: 5000, Output: 1000}})
	}
	return out
}

// decisions makes n decision records with the given rule/role/provider.
func decisions(n int, rule, role, prov, reason string) []Record {
	var out []Record
	for i := 0; i < n; i++ {
		out = append(out, Record{Type: TypeDecision, TS: t0, Session: "s", Role: role, Provider: prov, Model: "m", Rule: rule, Reason: reason, Attempt: 1})
	}
	return out
}

func titles(s []Suggestion) string {
	var t []string
	for _, x := range s {
		t = append(t, x.Severity+": "+x.Title+" "+strings.Join(x.Commands, ","))
	}
	return strings.Join(t, "\n")
}

func TestNextEffort(t *testing.T) {
	for _, tc := range []struct{ prov, in, want string }{
		{event.Codex, "medium", "high"},
		{event.Codex, "xhigh", ""},
		{event.Claude, "xhigh", "max"},
		{event.Claude, "max", ""},
		{event.Claude, "", "high"},
	} {
		if got := nextEffort(tc.prov, tc.in); got != tc.want {
			t.Errorf("nextEffort(%s,%q) = %q, want %q", tc.prov, tc.in, got, tc.want)
		}
	}
}

// The confidence floor needs a clear majority from few runs and little
// more than the floor from many (the counts in the thresholds' comment).
func TestClearlyAbove(t *testing.T) {
	for n, want := range map[int]int{5: 3, 10: 4, 20: 7, 50: 14} {
		least := -1
		for k := 0; k <= n; k++ {
			if clearlyAbove(k, n, failFloor) {
				least = k
				break
			}
		}
		if least != want {
			t.Errorf("%d runs: %d failures cross the floor, want %d", n, least, want)
		}
	}
	if clearlyAbove(4, 4, failFloor) {
		t.Error("4 runs are below minRuns")
	}
	if got := wilsonLower(0, 10); got != 0 {
		t.Errorf("wilsonLower(0, 10) = %v", got)
	}
	// More hits than samples (records that do not match their total)
	// count as n of n, never as NaN (found in review).
	if got, want := wilsonLower(7, 5), wilsonLower(5, 5); got != want || math.IsNaN(got) {
		t.Errorf("wilsonLower(7, 5) = %v, want %v", got, want)
	}
	if !clearlyAbove(7, 5, failFloor) {
		t.Error("7 of 5 is not clearly above")
	}
	if got := wilsonUpper(5, 5); got != 1 {
		t.Errorf("wilsonUpper(5, 5) = %v", got)
	}
}

// stepDecision is one decision of a step attempt.
func stepDecision(step, role string, attempt int, reason string) Record {
	return Record{Type: TypeDecision, TS: t0, Session: "s", TaskID: "t", Step: step, Attempt: attempt,
		Role: role, Provider: "codex", Model: "gpt", Rule: "default", Reason: reason}
}

// Escalations count steps, not records, and a step that started on
// another role counts in the role it escalated from (found in review).
func TestEscalationsCountSteps(t *testing.T) {
	esc := "same error twice: worker -> worker_high"
	var recs []Record
	for i := 0; i < 10; i++ {
		recs = append(recs, stepDecision(fmt.Sprint("s", i), "worker", 1, "default worker route"))
	}
	for a := 2; a <= 4; a++ {
		recs = append(recs, stepDecision("s0", "worker_high", a, esc))
	}
	if got := titles(escalations(recs)); got != "" {
		t.Errorf("one step repeating its error three times: %q", got)
	}

	recs = nil
	for i := 0; i < 5; i++ {
		recs = append(recs, stepDecision(fmt.Sprint("w", i), "worker", 1, "default worker route"))
	}
	for i := 0; i < 5; i++ {
		s := fmt.Sprint("h", i)
		recs = append(recs, stepDecision(s, "worker_high", 1, "touches 9 files"),
			stepDecision(s, "planner", 2, "same error twice: worker -> planner"))
	}
	got := escalations(recs)
	if len(got) != 1 || !strings.Contains(got[0].Detail, "5 of 10 worker steps") {
		t.Errorf("escalations from worker_high starts: %+v", got)
	}
}

func TestFailingRoutes(t *testing.T) {
	limit := runs("worker", "codex", "gpt", "medium", 5, 5)
	for i := range limit {
		limit[i].LimitHit = true
	}
	for _, tc := range []struct {
		name string
		recs []Record
		want string // substring of titles, "" = no suggestion
	}{
		{"4 of 10", runs("worker", "codex", "gpt", "medium", 10, 4), "medium: worker on codex:gpt fails often /route worker codex:gpt:high,/prefer worker claude"},
		{"50% is high", runs("worker", "claude", "sonnet", "max", 6, 3), "high: worker on claude:sonnet fails often /prefer worker codex"},
		{"3 of 10 is not clear yet", runs("worker", "codex", "gpt", "medium", 10, 3), ""},
		{"2 of 5: one bad afternoon", runs("worker", "codex", "gpt", "medium", 5, 2), ""},
		{"35% of 20", runs("worker", "codex", "gpt", "medium", 20, 7), "medium: worker on codex:gpt fails often"},
		{"30% of 20", runs("worker", "codex", "gpt", "medium", 20, 6), ""},
		{"below rate", runs("worker", "codex", "gpt", "medium", 10, 2), ""},
		{"too few runs", runs("worker", "codex", "gpt", "medium", 4, 4), ""},
		{"limit hits ignored", limit, ""},
	} {
		got := titles(failingRoutes(tc.recs))
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestEscalations(t *testing.T) {
	esc := "same error twice: worker -> worker_high"
	base := append(decisions(10, "default", "worker", "codex", "default worker route"),
		decisions(1, "large-or-sensitive", "worker_high", "codex", "")...)
	base[len(base)-1].Model, base[len(base)-1].Effort = "gpt", "xhigh"
	for _, tc := range []struct {
		name string
		recs []Record
		want string
	}{
		{"3 of 10", append(append([]Record{}, base...), decisions(3, "error-repeats", "worker_high", "codex", esc)...),
			"worker steps often escalate after repeated errors /route worker codex:gpt:xhigh,/prefer worker claude"},
		{"wrapped by limit fallback", append(append([]Record{}, base...), decisions(3, "limit-fallback", "worker_high", "claude", "codex at limit -> claude (error-repeats: "+esc+")")...),
			"worker steps often escalate"},
		{"only 2", append(append([]Record{}, base...), decisions(2, "error-repeats", "worker_high", "codex", esc)...), ""},
		{"rare", append(append(append([]Record{}, base...), decisions(10, "default", "worker", "codex", "")...), decisions(3, "error-repeats", "worker_high", "codex", esc)...), ""},
	} {
		got := titles(escalations(tc.recs))
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func reviews(n, rejected int) []Record {
	var out []Record
	for i := 0; i < n; i++ {
		out = append(out, Record{Type: TypeReview, TS: t0, Step: "final", OK: Bool(i >= rejected)})
	}
	return out
}

func TestFinalReviews(t *testing.T) {
	worker := runs("worker", "claude", "sonnet", "medium", 5, 0)
	plan := reviews(5, 5)
	for i := range plan {
		plan[i].Step = "plan"
	}
	for _, tc := range []struct {
		name string
		recs []Record
		want string
	}{
		{"8 of 20", append(reviews(20, 8), worker...), "medium: final reviews reject a lot of work /route worker claude:sonnet:high"},
		{"3 of 5 is high", reviews(5, 3), "high: final reviews"},
		{"2 of 5 is not clear yet", append(reviews(5, 2), worker...), ""},
		{"7 of 20", reviews(20, 7), ""},
		{"1 of 5", reviews(5, 1), ""},
		{"too few", reviews(4, 4), ""},
		{"plan reviews ignored", plan, ""},
	} {
		got := titles(finalReviews(tc.recs))
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// atTimes puts the records at from, from+step, from+2*step, ...
func atTimes(recs []Record, from time.Time, step time.Duration) []Record {
	for i := range recs {
		recs[i].TS = from.Add(time.Duration(i) * step)
	}
	return recs
}

// nextDay moves the first n records a day later.
func nextDay(recs []Record, n int) []Record {
	for i := 0; i < n; i++ {
		recs[i].TS = recs[i].TS.Add(24 * time.Hour)
	}
	return recs
}

func TestLimitPressure(t *testing.T) {
	cheap := append(decisions(3, "read-only", "explorer", "claude", ""), decisions(2, "forced", "judge", "claude", "")...)
	cheap = append(cheap, decisions(2, "read-only", "researcher", "codex", "")...)
	for _, tc := range []struct {
		name string
		recs []Record
		want string
	}{
		{"claude limited", append(append([]Record{}, cheap...), nextDay(decisions(5, "limit-fallback", "worker", "codex", ""), 2)...),
			"claude keeps running out of quota /prefer explorer codex,/prefer judge codex"},
		{"quota-preempt counts", append(append([]Record{}, cheap...), nextDay(decisions(5, "quota-preempt", "worker", "codex", ""), 1)...), "claude keeps running"},
		{"one bad afternoon", append(append([]Record{}, cheap...), decisions(20, "limit-fallback", "worker", "codex", "")...), ""},
		// One evening past midnight is one episode, not two days (found in review).
		{"one evening past midnight", append(append([]Record{}, cheap...), atTimes(decisions(6, "limit-fallback", "worker", "codex", ""),
			time.Date(2026, 9, 1, 23, 0, 0, 0, time.Local), 30*time.Minute)...), ""},
		{"13 hours apart", append(append([]Record{}, cheap...), atTimes(decisions(6, "limit-fallback", "worker", "codex", ""),
			time.Date(2026, 9, 1, 20, 0, 0, 0, time.Local), 13*time.Hour)...), "claude keeps running"},
		{"too few", append(append([]Record{}, cheap...), nextDay(decisions(4, "limit-fallback", "worker", "codex", ""), 2)...), ""},
		{"cheap roles already elsewhere", nextDay(decisions(5, "limit-fallback", "worker", "claude", ""), 2), ""},
	} {
		got := titles(limitPressure(tc.recs))
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
		if strings.Contains(got, "researcher") {
			t.Errorf("%s: researcher already on codex: %q", tc.name, got)
		}
	}
}

func TestCheaperReadOnly(t *testing.T) {
	big := runs("explorer", "claude", "opus", "", 5, 0)
	for i := range big {
		big[i].Tokens = &event.TokenUsage{Input: 50_000}
	}
	for _, tc := range []struct {
		name string
		recs []Record
		want string
	}{
		{"opus explorer", runs("explorer", "claude", "opus", "", 5, 0), "info: explorer could use a cheaper model than opus /route explorer claude:haiku"},
		{"codex researcher", runs("researcher", "codex", "gpt-6.1-sol", "high", 6, 0), "/route researcher codex:gpt-6-luna:low"},
		{"already cheap", runs("explorer", "claude", "haiku", "", 5, 0), ""},
		{"one failure", runs("explorer", "claude", "opus", "", 5, 1), ""},
		{"many tokens", big, ""},
		{"too few", runs("explorer", "claude", "opus", "", 4, 0), ""},
		{"worker is not read-only", runs("worker", "claude", "opus", "", 5, 0), ""},
	} {
		got := titles(cheaperReadOnly(tc.recs))
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// steps makes n linked decision+agent_end pairs, the first fails failed.
func steps(n, fails int, rule string, judged bool, task string) []Record {
	var out []Record
	for i := 0; i < n; i++ {
		k := Record{TS: t0, Session: "s", TaskID: task, Step: fmt.Sprint("s", i), Attempt: 1, Role: "worker", Provider: "codex", Model: "gpt"}
		d, e := k, k
		d.Type, d.Rule, d.Judged = TypeDecision, rule, judged
		e.Type, e.OK = TypeAgentEnd, Bool(i >= fails)
		out = append(out, d, e)
	}
	return out
}

func withTokens(recs []Record, n int64) []Record {
	for i := range recs {
		if recs[i].Type == TypeAgentEnd {
			recs[i].Tokens = &event.TokenUsage{Output: n}
		}
	}
	return recs
}

func judgeCalls(n int, tokens int64) []Record {
	var out []Record
	for i := 0; i < n; i++ {
		out = append(out, Record{TS: t0, Session: "s", TaskID: "b", Step: fmt.Sprint("s", i, "-judge"), Type: TypeAgentEnd, Role: "judge", OK: Bool(true), Tokens: &event.TokenUsage{Output: tokens}})
	}
	return out
}

func TestJudgeAdvice(t *testing.T) {
	for _, tc := range []struct {
		name string
		recs []Record
		want string
	}{
		{"default fails, judge off", steps(10, 4, "default", false, "a"), "medium: turn on the judge for unclear worker steps /judge on"},
		{"default ok", steps(10, 2, "default", false, "a"), ""},
		{"3 of 10 is not clear yet", steps(10, 3, "default", false, "a"), ""},
		{"judge already on", append(steps(10, 3, "default", false, "a"), steps(1, 0, "default", true, "b")...), ""},
		{"judge no better", append(steps(10, 2, "default", false, "a"), steps(10, 2, "default", true, "b")...), "info: the judge does not improve routing /judge off"},
		{"old logs: rule judge", append(steps(10, 2, "default", false, "a"), steps(10, 3, "judge", false, "b")...), "/judge off"},
		{"judge better, free", append(steps(10, 3, "default", false, "a"), steps(10, 1, "default", true, "b")...), "info: the judge pays off"},
		{"judge better but costly", append(withTokens(steps(10, 3, "default", false, "a"), 1000), append(withTokens(steps(10, 1, "default", true, "b"), 1000), judgeCalls(10, 5000)...)...), "info: the judge costs more than it saves /judge off"},
		{"judge better and cheap", append(withTokens(steps(10, 3, "default", false, "a"), 50000), append(withTokens(steps(10, 1, "default", true, "b"), 50000), judgeCalls(10, 500)...)...), "info: the judge pays off"},
		{"too few judged", append(steps(10, 2, "default", false, "a"), steps(9, 5, "default", true, "b")...), ""},
	} {
		got := titles(judgeAdvice(tc.recs))
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func tasks(mode string, n, ok int, viaStart bool) []Record {
	var out []Record
	for i := 0; i < n; i++ {
		id := fmt.Sprint(mode, i)
		end := Record{Type: TypeTaskEnd, TS: t0, Session: "s", TaskID: id, Mode: mode, OK: Bool(i < ok)}
		if viaStart {
			out = append(out, Record{Type: TypeTask, TS: t0, Session: "s", TaskID: id, Mode: mode})
			end.Mode = ""
		}
		out = append(out, end)
	}
	return out
}

func TestRoutedVsSingle(t *testing.T) {
	for _, tc := range []struct {
		name string
		recs []Record
		want string
	}{
		{"routed worse", append(tasks("routed", 10, 2, false), tasks("single", 10, 10, false)...), "high: routed tasks succeed less often"},
		{"mode from task_start", append(tasks("routed", 10, 2, true), tasks("single", 10, 10, true)...), "high: routed"},
		{"15 vs 20 of 20", append(tasks("routed", 20, 15, false), tasks("single", 20, 20, false)...), "high: routed"},
		{"one run apart is noise", append(tasks("routed", 5, 3, false), tasks("single", 5, 4, false)...), ""},
		// single is a sample too: a perfect baseline does not make one
		// routed failure an alarm (found in review).
		{"4 vs 5 of 5", append(tasks("routed", 5, 4, false), tasks("single", 5, 5, false)...), ""},
		{"9 vs 10 of 10", append(tasks("routed", 10, 9, false), tasks("single", 10, 10, false)...), ""},
		{"19 of 20 vs 5 of 5", append(tasks("routed", 20, 19, false), tasks("single", 5, 5, false)...), ""},
		{"16 vs 18 of 20", append(tasks("routed", 20, 16, false), tasks("single", 20, 18, false)...), ""},
		{"routed equal", append(tasks("routed", 5, 4, false), tasks("single", 5, 4, false)...), ""},
		{"too few single", append(tasks("routed", 5, 0, false), tasks("single", 4, 4, false)...), ""},
	} {
		got := titles(routedVsSingle(tc.recs))
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSuggestFilterAndOrder(t *testing.T) {
	recs := append(runs("explorer", "claude", "opus", "", 5, 0), runs("worker", "codex", "gpt", "medium", 10, 4)...)
	recs = append(recs, tasks("routed", 10, 2, false)...)
	recs = append(recs, tasks("single", 10, 10, false)...)
	got := Suggest(recs, Filter{})
	if len(got) != 3 || got[0].Severity != SevHigh || got[1].Severity != SevMedium || got[2].Severity != SevInfo {
		t.Fatalf("order:\n%s", titles(got))
	}
	if got := Suggest(recs, Filter{Since: t0.Add(time.Hour)}); len(got) != 0 {
		t.Errorf("since filter ignored:\n%s", titles(got))
	}
	for i := range recs {
		recs[i].Cwd = "/a"
	}
	if got := Suggest(recs, Filter{Cwd: "/b"}); len(got) != 0 {
		t.Errorf("cwd filter ignored:\n%s", titles(got))
	}
}
