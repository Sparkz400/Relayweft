package router

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
)

// Cost-aware model tiers (routing.tiers: auto). The rules above still pick
// the step's role (worker, explorer, ...); the tiers then pick the model
// for a work step from how hard the step looks and how much quota is left,
// instead of always using that role's own route.
//
// The three tiers reuse routes you already configured, so a tier is a
// model plus effort on each provider:
//
//	fast     = the explorer route
//	standard = the worker route
//	strong   = the worker_high route
//
// What never moves: the planner, reviewer and judge; a role picked in plan
// approval; a role set explicitly (repo file, flag, /route, the model
// picker). Large, sensitive and repeating-error steps, and the judge's
// pick, never drop below the tier their rule chose. Read-only steps never
// go above standard.

// Tier names, the same words the model catalog uses (ModelInfo.Tier).
const (
	TierFast     = "fast"
	TierStandard = "standard"
	TierStrong   = "strong"
)

var tierOrder = []string{TierFast, TierStandard, TierStrong}

// tierRole is the role whose route a tier uses.
var tierRole = map[string]string{
	TierFast:     event.RoleExplorer,
	TierStandard: event.RoleWorker,
	TierStrong:   event.RoleWorkerHigh,
}

// roleTier is the tier a work role stands for. Roles not listed (planner,
// reviewer, judge) are never moved.
var roleTier = map[string]string{
	event.RoleExplorer:   TierFast,
	event.RoleResearcher: TierFast,
	event.RoleWorker:     TierStandard,
	event.RoleWorkerHigh: TierStrong,
}

// Difficulty score bounds: below fastBelow a step is fast, from strongFrom
// on it is strong. Each role starts at its tier's centre.
const (
	fastBelow  = 0.35
	strongFrom = 0.65
)

var tierCentre = map[string]float64{TierFast: 0.2, TierStandard: 0.5, TierStrong: 0.8}

// Words that make a step look routine or hard. Matched on whole words in
// the title and prompt; the first hit is named in the reason.
var (
	routineWords = regexp.MustCompile(`(?i)\b(typos?|spelling|comments?|docstrings?|rename|readme|changelog|wording|bump|reformat|format(ting)?|lint warnings?|log messages?|copy ?edit)\b`)
	hardWords    = regexp.MustCompile(`(?i)\b(concurren(t|cy)|race( condition)?s?|deadlocks?|thread[- ]safe(ty)?|lock[- ]free|architecture|redesign|algorithms?|optimi[sz]e|performance|memory leaks?|parser|protocol|distributed|consisten(t|cy)|cache invalidation|state machine|scheduler|transactions?)\b`)
)

// Assessment is the tier picked for one step and why.
type Assessment struct {
	Difficulty float64  // 0..1, before the quota shift
	QuotaLeft  float64  // 0..1, the tightest of the provider's limit and the budgets
	QuotaKnown bool     // false when nothing reported a limit or budget
	Shift      float64  // how far the quota pushed the score down
	Tier       string   // the tier picked
	From       string   // the tier the role stands for
	Signals    []string // what moved the score, for the reason
}

// Difficulty estimates how hard a step is from what the router knows
// before it runs: the role the rules picked, the files it names, the size
// of its prompt and the words in it. 0 is trivial, 1 is very hard.
func Difficulty(s Step, role string) (float64, []string) {
	score := tierCentre[roleTier[role]]
	var why []string
	add := func(v float64, w string) {
		score += v
		why = append(why, w)
	}
	switch n := len(s.Files); {
	case n == 1:
		add(-0.05, "1 file")
	case n >= 3:
		add(math.Min(0.05*float64(n-2), 0.15), fmt.Sprintf("%d files", n))
	}
	text := s.Title + " " + s.Prompt
	// A routine word moves a step down only when the step is about it: in
	// the title, or in a short prompt. Planners write long prompts that
	// mention the readme or the docstrings in passing (the tiers bench).
	routine := s.Title
	switch w := len(strings.Fields(text)); {
	case w < 25:
		add(-0.1, "short")
		routine = text
	case w > 250:
		add(0.1, fmt.Sprintf("long (%d words)", w))
	}
	if routine == "" {
		routine = text
	}
	if m := routineWords.FindString(routine); m != "" {
		add(-0.15, "routine: "+strings.ToLower(m))
	}
	if m := hardWords.FindString(text); m != "" {
		add(0.15, "hard: "+strings.ToLower(m))
	}
	if s.Kind == KindFix {
		add(0.05, "review fix")
	}
	return round2(math.Max(0, math.Min(1, score))), why
}

// round2 keeps scores on hundredths, so a step that lands on a tier
// boundary lands there exactly, not a float error to either side.
func round2(v float64) float64 { return math.Round(v*100) / 100 }

func tierOf(score float64) string {
	switch {
	case score < fastBelow:
		return TierFast
	case score >= strongFrom:
		return TierStrong
	}
	return TierStandard
}

func tierIndex(t string) int {
	for i, x := range tierOrder {
		if x == t {
			return i
		}
	}
	return 1
}

// quotaShift is how far the score moves down for the quota left: nothing
// while at least saveBelow is left, then up to 0.3 (about one tier) when
// nothing is left.
func quotaShift(left, saveBelow float64) float64 {
	if saveBelow <= 0 || left >= saveBelow {
		return 0
	}
	return round2(0.3 * (saveBelow - left) / saveBelow)
}

// quotaLeft is the tightest headroom the router knows of for a provider:
// its reported usage limit and the task's and day's budget.
func (r *Router) quotaLeft(provider string, s Step) (float64, bool) {
	left, known := 1.0, false
	if r.State != nil {
		if u, ok := r.State.Utilization(provider); ok {
			left, known = math.Min(left, 1-u), true
		}
	}
	if s.BudgetUsed > 0 {
		left, known = math.Min(left, 1-s.BudgetUsed), true
	}
	return math.Max(0, left), known
}

// Assess picks the tier for a step whose rules chose role and rule. ok is
// false when tiers do not apply to the step.
func (r *Router) Assess(cfg *config.Config, s Step, role, rule, provider string) (Assessment, bool) {
	if cfg.Routing.Tiers != config.TiersAuto || s.UserRole != "" {
		return Assessment{}, false
	}
	from, ok := roleTier[role]
	if !ok || (r.Pinned != nil && r.Pinned(role)) {
		return Assessment{}, false
	}
	a := Assessment{From: from}
	a.Difficulty, a.Signals = Difficulty(s, role)
	a.QuotaLeft, a.QuotaKnown = r.quotaLeft(provider, s)
	a.Shift = quotaShift(a.QuotaLeft, cfg.Routing.TiersSaveBelow())
	idx := tierIndex(tierOf(round2(a.Difficulty - a.Shift)))
	// A rule that saw risk (or the judge) sets a floor; read-only work
	// never needs the strong tier.
	switch rule {
	case RuleLargeDiff, RuleRepeatError, RuleJudge:
		idx = max(idx, tierIndex(from))
	}
	if s.Kind.ReadOnly() || role == event.RoleExplorer || role == event.RoleResearcher {
		idx = min(idx, tierIndex(TierStandard))
	}
	a.Tier = tierOrder[idx]
	return a, true
}

// Reason is the assessment as one line for the decision's reason.
func (a Assessment) Reason() string {
	s := fmt.Sprintf("tier %s (difficulty %.2f", a.Tier, a.Difficulty)
	if len(a.Signals) > 0 {
		s += ": " + strings.Join(a.Signals, ", ")
	}
	s += ")"
	if a.QuotaKnown {
		s += fmt.Sprintf(", quota left %.0f%%", a.QuotaLeft*100)
		if a.Shift > 0 {
			s += fmt.Sprintf(" -> %.2f lower", a.Shift)
		}
	}
	if a.Tier != a.From {
		s += fmt.Sprintf(", %s route instead of %s", tierRole[a.Tier], tierRole[a.From])
	}
	return s
}

// applyTier moves a decision to its tier's route on the same provider. The
// route must be configured there; otherwise the decision keeps its own.
func (r *Router) applyTier(cfg *config.Config, s Step, d event.Decision, rule string) event.Decision {
	a, ok := r.Assess(cfg, s, d.Role, rule, d.Provider)
	if !ok {
		return d
	}
	if a.Tier != a.From {
		route := cfg.Roles[tierRole[a.Tier]].For(d.Provider)
		if route.Model == "" {
			a.Signals = append(a.Signals, "no "+tierRole[a.Tier]+" route on "+d.Provider)
			a.Tier = a.From
		} else {
			d.Model, d.Effort = route.Model, route.Effort
		}
	}
	d.Tier = a.Tier
	d.Reason += "; " + a.Reason()
	return d
}
