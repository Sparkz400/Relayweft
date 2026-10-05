package router

import (
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/event"
)

// FuzzParseJudge (ROADMAP 1.8): any judge reply maps to a known role or is
// rejected; it never panics.
func FuzzParseJudge(f *testing.F) {
	for _, s := range []string{" C) worker-high", "maybe", "A", "**B**", "`d.`", "", "\n", "é", "\xff"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, reply string) {
		role, ok := ParseJudge(reply)
		if !ok {
			if role != "" {
				t.Fatalf("rejected reply returned role %q", role)
			}
			return
		}
		switch role {
		case event.RoleExplorer, event.RoleWorker, event.RoleWorkerHigh, event.RolePlanner:
		default:
			t.Fatalf("unknown role %q for %q", role, reply)
		}
		if !strings.ContainsAny(reply, "ABCDabcd") {
			t.Fatalf("%q has no route letter but parsed to %s", reply, role)
		}
		_ = JudgePrompt(Step{Title: reply, Prompt: reply})
	})
}
