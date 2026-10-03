package sessionlog

import (
	"fmt"
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
		{"30% fails", runs("worker", "codex", "gpt", "medium", 10, 3), "medium: worker on codex:gpt fails often /route worker codex:gpt:high,/prefer worker claude"},
		{"50% is high", runs("worker", "claude", "sonnet", "max", 6, 3), "high: worker on claude:sonnet fails often /prefer worker codex"},
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
		{"2 of 5", append(reviews(5, 2), worker...), "medium: final reviews reject a lot of work /route worker claude:sonnet:high"},
		{"3 of 5 is high", reviews(5, 3), "high: final reviews"},
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

func TestLimitPressure(t *testing.T) {
	cheap := append(decisions(3, "read-only", "explorer", "claude", ""), decisions(2, "forced", "judge", "claude", "")...)
	cheap = append(cheap, decisions(2, "read-only", "researcher", "codex", "")...)
	for _, tc := range []struct {
		name string
		recs []Record
		want string
	}{
		{"claude limited", append(append([]Record{}, cheap...), decisions(5, "limit-fallback", "worker", "codex", "")...),
			"claude keeps running out of quota /prefer explorer codex,/prefer judge codex"},
		{"quota-preempt counts", append(append([]Record{}, cheap...), decisions(5, "quota-preempt", "worker", "codex", "")...), "claude keeps running"},
		{"too few", append(append([]Record{}, cheap...), decisions(4, "limit-fallback", "worker", "codex", "")...), ""},
		{"cheap roles already elsewhere", decisions(5, "limit-fallback", "worker", "claude", ""), ""},
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

func TestJudgeAdvice(t *testing.T) {
	for _, tc := range []struct {
		name string
		recs []Record
		want string
	}{
		{"default fails, judge off", steps(10, 3, "default", false, "a"), "medium: turn on the judge for unclear worker steps /judge on"},
		{"default ok", steps(10, 2, "default", false, "a"), ""},
		{"judge already on", append(steps(10, 3, "default", false, "a"), steps(1, 0, "default", true, "b")...), ""},
		{"judge no better", append(steps(10, 2, "default", false, "a"), steps(10, 2, "default", true, "b")...), "info: the judge does not improve routing /judge off"},
		{"old logs: rule judge", append(steps(10, 2, "default", false, "a"), steps(10, 3, "judge", false, "b")...), "/judge off"},
		{"judge better", append(steps(10, 3, "default", false, "a"), steps(10, 1, "default", true, "b")...), ""},
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
		{"routed worse", append(tasks("routed", 5, 3, false), tasks("single", 5, 4, false)...), "high: routed tasks succeed less often"},
		{"mode from task_start", append(tasks("routed", 5, 3, true), tasks("single", 5, 4, true)...), "high: routed"},
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
	recs := append(runs("explorer", "claude", "opus", "", 5, 0), runs("worker", "codex", "gpt", "medium", 10, 3)...)
	recs = append(recs, tasks("routed", 5, 3, false)...)
	recs = append(recs, tasks("single", 5, 5, false)...)
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
