package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/sparkz400/switchyard/internal/event"
)

// Fake is a scripted runner for `sy --demo` and tests. It understands the
// orchestrator's prompt markers, so it drives the full real pipeline:
// planning, plan review, parallel agents, a repeated error, a usage-limit
// fallback and the final review.
type Fake struct {
	Provider string
	// Speed scales all delays; 0 means no delays (tests).
	Speed float64
	// Scenario toggles.
	FailRepeat  bool // the first edit step fails twice with the same error
	LimitOnCall int  // the Nth call on this provider hits the usage limit (0 = never)
	RejectPlan  bool // the plan review asks for one revision
	RejectFinal bool // the final review asks for one fix round

	mu       sync.Mutex
	calls    int
	attempts map[string]int
	rejected map[string]bool
}

// NewFakeSet returns fake runners for both providers with the demo scenario.
func NewFakeSet(speed float64) Set {
	return Set{
		event.Codex:  &Fake{Provider: event.Codex, Speed: speed, FailRepeat: true, LimitOnCall: 5},
		event.Claude: &Fake{Provider: event.Claude, Speed: speed, RejectFinal: true},
	}
}

// Prompt markers the orchestrator writes and the fake recognises.
const (
	MarkerPlan        = "[SY:PLAN]"
	MarkerPlanReview  = "[SY:PLAN-REVIEW]"
	MarkerFinalReview = "[SY:FINAL-REVIEW]"
	MarkerErrorReview = "[SY:ERROR-REVIEW]"
	MarkerJudge       = "[SY:JUDGE]"
	MarkerStep        = "[SY:STEP]"
	MarkerFix         = "[SY:FIX]"
)

func (f *Fake) sleep(ctx context.Context, min, max time.Duration) bool {
	if f.Speed <= 0 {
		return ctx.Err() == nil
	}
	d := min
	if max > min {
		d += time.Duration(randInt63n(int64(max - min)))
	}
	d = time.Duration(float64(d) / f.Speed)
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

var (
	rngMu sync.Mutex
	rng   = rand.New(rand.NewSource(time.Now().UnixNano()))
)

func randIntn(n int) int {
	rngMu.Lock()
	defer rngMu.Unlock()
	return rng.Intn(n)
}

func randInt63n(n int64) int64 {
	rngMu.Lock()
	defer rngMu.Unlock()
	return rng.Int63n(n)
}

// Run implements Runner.
func (f *Fake) Run(ctx context.Context, s Spec, emit func(event.Event)) Result {
	start := time.Now()
	send := func(k event.Kind, text string) {
		emit(event.Event{AgentID: s.AgentID, ParentID: s.ParentID, Provider: f.Provider, Model: s.Model, Role: s.Role, Kind: k, Text: text}.Stamp())
	}
	f.mu.Lock()
	f.calls++
	call := f.calls
	if f.attempts == nil {
		f.attempts = map[string]int{}
		f.rejected = map[string]bool{}
	}
	f.attempts[s.StepID]++
	attempt := f.attempts[s.StepID]
	f.mu.Unlock()

	send(event.Started, "started "+s.Model)
	// A session id makes demo agents take follow-ups (@agent message).
	res := Result{SessionID: fmt.Sprintf("demo-%s-%d", s.AgentID, call)}
	finish := func() Result {
		res.Duration = time.Since(start)
		if ctx.Err() != nil {
			res.Killed = true
			res.Err = fmt.Errorf("killed")
		}
		if res.Tokens.Total() > 0 {
			emit(event.Event{AgentID: s.AgentID, ParentID: s.ParentID, Provider: f.Provider, Model: s.Model, Role: s.Role, Kind: event.Usage, Tokens: res.Tokens}.Stamp())
		}
		text := SummaryLine(res.Final)
		if res.Err != nil {
			text = res.Err.Error()
		}
		emit(event.Event{AgentID: s.AgentID, ParentID: s.ParentID, Provider: f.Provider, Model: s.Model, Role: s.Role, Kind: event.Done, OK: res.OK(), Tokens: res.Tokens, Text: text}.Stamp())
		return res
	}
	res.Tokens = event.TokenUsage{Input: int64(2000 + randIntn(18000)), Output: int64(200 + randIntn(3000))}

	if !f.sleep(ctx, 300*time.Millisecond, 900*time.Millisecond) {
		return finish()
	}
	if f.LimitOnCall > 0 && call == f.LimitOnCall {
		send(event.Thinking, "reading the task")
		f.sleep(ctx, 300*time.Millisecond, 600*time.Millisecond)
		msg := "You've hit your usage limit. Try again in 1h 0m."
		send(event.LimitHit, msg)
		res.LimitHit = true
		res.ResetAt = time.Now().Add(time.Hour)
		res.Err = fmt.Errorf("%s", msg)
		res.Tokens = event.TokenUsage{}
		return finish()
	}

	p := s.Prompt
	switch {
	case strings.Contains(p, MarkerJudge):
		res.Final = "B"
	case strings.Contains(p, MarkerPlanReview):
		f.script(ctx, send, []string{"Read plan", "Grep TODO"}, nil)
		if f.RejectPlan && !f.rejectedOnce("plan") {
			res.Final = verdictJSON(false, "Split the edit step so tests are written before the implementation.", []string{"tests missing from plan"})
		} else {
			res.Final = verdictJSON(true, "Plan looks sound. Keep the parser change and the CLI flag in separate steps; run the tests after both merge.", nil)
		}
	case strings.Contains(p, MarkerFinalReview):
		f.script(ctx, send, []string{"Read diff", "Grep error handling"}, nil)
		if f.RejectFinal && !f.rejectedOnce("final") {
			res.Final = verdictJSON(false, "The new flag is not documented in the README and one error path drops the wrapped error.", []string{"README not updated", "error not wrapped in parse.go"})
		} else {
			res.Final = verdictJSON(true, "Diff is consistent with the plan; tests cover the new path.", nil)
		}
	case strings.Contains(p, MarkerErrorReview):
		f.script(ctx, send, []string{"Read failing test", "Read parser"}, nil)
		res.Final = verdictJSON(false, "The test expects the trailing field to be kept: stop trimming the last separator in splitFields.", []string{"off-by-one in splitFields"})
	case strings.Contains(p, MarkerPlan):
		f.script(ctx, send, []string{"Glob **/*.go", "Read README.md", "Grep func main"}, nil)
		res.Final = "```json\n" + fakePlan(p) + "\n```"
	default: // step or fix
		ro := s.ReadOnly
		tools := []string{"Grep " + pick("parse", "config", "Run(", "TODO"), "Read " + pick("internal/parse.go", "cmd/main.go", "README.md", "go.mod")}
		var edits []string
		if !ro {
			tools = append(tools, "$ go test ./...")
			edits = []string{pick("internal/parse.go", "internal/flags.go"), pick("internal/parse_test.go", "README.md")}
		}
		f.script(ctx, send, tools, edits)
		if !ro && f.FailRepeat && strings.Contains(p, "first-edit") && attempt <= 2 {
			res.Err = fmt.Errorf("go test failed: TestSplitFields: expected 3 fields, got 2")
			send(event.Error, res.Err.Error())
			return finish()
		}
		res.Files = edits
		if ro {
			res.Final = "Found the relevant code in internal/parse.go (splitFields, parseLine) and cmd/main.go (flag setup)."
		} else {
			res.Final = "Implemented the change; go test ./... passes."
		}
	}
	return finish()
}

func (f *Fake) rejectedOnce(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	was := f.rejected[key]
	f.rejected[key] = true
	return was
}

func (f *Fake) script(ctx context.Context, send func(event.Kind, string), tools, edits []string) {
	send(event.Thinking, pick("Looking at the repository layout", "Reading the task carefully", "Checking how this is wired today"))
	for _, t := range tools {
		if !f.sleep(ctx, 400*time.Millisecond, 1200*time.Millisecond) {
			return
		}
		send(event.ToolCall, t)
	}
	for _, e := range edits {
		if !f.sleep(ctx, 500*time.Millisecond, 1400*time.Millisecond) {
			return
		}
		send(event.FileEdit, e)
	}
	if f.sleep(ctx, 300*time.Millisecond, 900*time.Millisecond) {
		send(event.Message, pick("Done with this part.", "That covers it.", "Ready to hand back."))
	}
}

func pick(xs ...string) string { return xs[randIntn(len(xs))] }

func verdictJSON(approve bool, advice string, issues []string) string {
	b, _ := json.Marshal(map[string]any{"approve": approve, "advice": advice, "issues": issues})
	return "```json\n" + string(b) + "\n```"
}

func fakePlan(prompt string) string {
	plan := map[string]any{
		"summary": "Explore the parser, check the docs, then change the parser and the CLI flag in parallel.",
		"subtasks": []map[string]any{
			{"id": "explore", "title": "Map parser code", "kind": "explore", "prompt": "Find where input lines are split into fields.", "files": []string{}},
			{"id": "docs", "title": "Read flag docs", "kind": "research", "prompt": "Summarize how CLI flags are documented.", "files": []string{}},
			{"id": "parser", "title": "Fix field splitting", "kind": "edit", "prompt": "first-edit: keep the trailing field in splitFields and add a test.", "files": []string{"internal/parse.go", "internal/parse_test.go"}, "depends_on": []string{"explore"}},
			{"id": "flag", "title": "Add --strict flag", "kind": "edit", "prompt": "Add a --strict flag that rejects malformed lines.", "files": []string{"internal/flags.go", "README.md"}, "depends_on": []string{"docs"}},
		},
	}
	b, _ := json.MarshalIndent(plan, "", "  ")
	return string(b)
}
