package orchestrator

import (
	"fmt"
	"strings"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// Routing choices: every agent's route is a decision record; the
// task-level choices behind them are choice records, so rw explain can say
// why a task used one agent or several, why the final review ran or not
// and what started each fix round.

// noteChoice logs one task-level routing choice.
func (o *Orchestrator) noteChoice(t *task, what, outcome, why string, round int) {
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeChoice, TaskID: t.id, Step: what, Kind: outcome, Reason: why, Attempt: round})
}

// noteShape logs how the task runs: as one agent, as the planner's plan
// or as the saved plan of a resume. why is shortcutPlan's reason for one
// agent ("" for a plan).
func (o *Orchestrator) noteShape(t *task, plan Plan, why string) {
	n := len(plan.Subtasks)
	switch {
	case t.resumed && t.state != nil && t.state.Plan != nil:
		o.noteChoice(t, sessionlog.ChoiceShape, sessionlog.ChoiceResumed, fmt.Sprintf("resumed with its saved plan of %d %s", n, plural(n, "step")), 0)
	case why != "":
		o.noteChoice(t, sessionlog.ChoiceShape, sessionlog.ChoiceOne, why, 0)
	default:
		r := strings.TrimPrefix(t.shape.why, "planned: ")
		if r == "" {
			r = "the task was planned"
		}
		r += fmt.Sprintf("; the planner made %d %s", n, plural(n, "step"))
		if n <= 1 && plan.Summary != "" && strings.Contains(plan.Summary, "single step") {
			r += " (" + plan.Summary + ")"
		}
		o.noteChoice(t, sessionlog.ChoiceShape, sessionlog.ChoicePlanned, r, 0)
	}
}

// fixWhy says what starts a fix round: the failing checks, the reviewer's
// request, or both.
func fixWhy(verified, reviewed, approve bool, checks []string, advice string) string {
	var why []string
	if !verified {
		why = append(why, "checks fail ("+strings.Join(checks, ", ")+")")
	}
	if reviewed && !approve {
		w := "the reviewer asked for changes"
		if a := strings.TrimSpace(advice); a != "" {
			w += ": " + clip(firstLine(a), 160)
		}
		why = append(why, w)
	}
	if len(why) == 0 {
		return "the round did not pass"
	}
	return strings.Join(why, "; ")
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

func plural(n int, s string) string {
	if n == 1 {
		return s
	}
	return s + "s"
}

// detectChecks fills in the checks of every repo of the task that has
// none, from its build files (verify.auto), so the checks can decide the
// task: they run before the final review, and with review_when: failing
// they are what decides whether a review runs at all.
func (o *Orchestrator) detectChecks(t *task) {
	for _, r := range t.allRepos() {
		vc := &r.cfg.Verify
		if len(vc.Commands) > 0 || !vc.Auto {
			continue
		}
		dir := r.dir
		if dir == "" {
			dir = o.opts.Dir
		}
		cmds := config.DetectVerify(dir)
		label := ""
		if r.repoName != "" {
			label = r.repoName + ": "
		}
		if len(cmds) == 0 {
			o.noteChoice(t, sessionlog.ChoiceChecks, "none", label+"no verify.commands and none detected from the build files", 0)
			continue
		}
		vc.Commands = cmds
		why := label + "no verify.commands: detected " + strings.Join(cmds, ", ") + " from the build files (verify.auto)"
		o.logf("checks: %s", why)
		o.noteChoice(t, sessionlog.ChoiceChecks, "detected", why, 0)
	}
}

// firstTests are the acceptance tests a tests-first task wrote (nil
// otherwise).
func (t *task) firstTests() []AcceptanceTest {
	if t.tests == nil {
		return nil
	}
	return t.tests.Tests
}
