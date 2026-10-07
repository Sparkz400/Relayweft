package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/runner"
)

func TestPreflightBlocksImplementationAndCountsCost(t *testing.T) {
	for _, single := range []bool{false, true} {
		for _, proof := range []bool{false, true} {
			calls, probes := 0, 0
			set := both(func(s runner.Spec) runner.Result {
				if s.CheckOnly {
					probes++
					if strings.Contains(s.Prompt, "secret task") {
						t.Error("task leaked into preflight")
					}
					r := runner.Result{Final: "I ran all tests", Tokens: event.TokenUsage{Input: 100}}
					if proof {
						r.Commands = append([]string(nil), s.AllowedCommands...)
					}
					return r
				}
				calls++
				if single && (len(s.AllowedCommands) == 0 || !strings.Contains(s.Prompt, "go test")) {
					t.Error("single writer lacks configured commands")
				}
				return runner.Result{Final: "done"}
			})
			o, _ := newOrc(t, "", set, func(c *config.Config) {
				c.Verify.Preflight = []string{"go version", "gofmt -l ."}
				c.Verify.Commands = []string{"go test ./..."}
				// Planned, so the routes are unknown before the probe (a
				// one-step shortcut probes only its own provider:
				// TestAutoSinglePreflightsOnlyItsProvider).
				c.Orchestrator.SmallTaskWords = 0
			})
			var res TaskResult
			if single {
				res = o.RunSingle(context.Background(), "secret task", event.Claude, config.Route{Model: "sonnet"})
			} else {
				// A failed probe must precede the planner, regardless of task routing.
				if proof {
					continue
				}
				res = o.Run(context.Background(), "secret task")
			}
			if probes != 1 {
				t.Fatal("expected one Claude probe", probes)
			}
			if !proof && (res.OK || calls != 0 || !strings.Contains(res.Summary, "preflight")) {
				t.Fatalf("claim without tool evidence accepted: %+v calls=%d", res, calls)
			}
			if proof && (calls != 1 || !res.OK) {
				t.Fatalf("valid probe blocked writer: %+v calls=%d", res, calls)
			}
			if res.Cost.PerProvider[event.Claude].Total() != 100 {
				t.Fatal("probe quota omitted", res.Cost)
			}
		}
	}
}
