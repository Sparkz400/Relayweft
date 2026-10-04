package sessionlog

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sparkz400/switchyard/internal/event"
)

// bestOfSteps makes n best-of steps of the worker between the own codex
// route and claude:opus; ownWins of them the codex route won, the rest
// claude won, by by.
func bestOfSteps(n, ownWins int, by string) []Record {
	var out []Record
	for i := 0; i < n; i++ {
		id := fmt.Sprint("t", i)
		own := i < ownWins
		out = append(out,
			Record{Type: TypeBestOf, TS: t0, Session: "s", TaskID: id, Step: "w", Agent: "w--codex", Role: event.RoleWorker,
				Provider: event.Codex, Model: "gpt-6.1-sol", Effort: "medium", Rule: "default", Reason: by, OK: Bool(own)},
			Record{Type: TypeBestOf, TS: t0, Session: "s", TaskID: id, Step: "w", Agent: "w--claude", Role: event.RoleWorker,
				Provider: event.Claude, Model: "opus", Effort: "high", Rule: "best-of", Reason: by, OK: Bool(!own)})
	}
	return out
}

func TestBestOfAdvice(t *testing.T) {
	for _, tc := range []struct {
		name string
		recs []Record
		want string
	}{
		{"own route always wins", bestOfSteps(10, 10, BestOfByChecks), "info: best of N rarely changes the result for worker steps"},
		{"other route keeps winning", bestOfSteps(10, 3, BestOfByReviewer), "medium: claude:opus:high beats the worker route in best of N /route worker claude:opus:high"},
		{"wins by the fixed order say nothing", bestOfSteps(10, 3, BestOfByOrder), ""},
		{"mixed", bestOfSteps(10, 6, BestOfByChecks), ""},
		{"too few", bestOfSteps(4, 4, BestOfByChecks), ""},
	} {
		got := titles(bestOfAdvice(tc.recs))
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	steps, own, _ := BestOfStats(bestOfSteps(7, 5, BestOfByChecks))
	if steps[event.RoleWorker] != 7 || own[event.RoleWorker] != 5 {
		t.Errorf("stats: %d steps, %d own wins", steps[event.RoleWorker], own[event.RoleWorker])
	}
}
