package report

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// Routing explanation (rw explain): why a task ran as one agent or
// several, why each agent got its provider and model, what each run was
// estimated to use against what it used, and what escalated the work (a
// repeating error, a usage limit, a risky change, a hard step, a fix
// round).
//
// The estimates are recomputed from the runs logged before the task
// started, the way plan approval and budget reservations compute them
// (sessionlog.History): the same role, step kind and route, this repo's
// runs first.

// Explanation is everything rw explain shows.
type Explanation struct {
	ID, Task, Status, Mode string
	Created                time.Time
	Duration               time.Duration

	// Shape is how the task ran: "one agent", "planned", "resumed" or
	// "single" (rw run --single); ShapeWhy is why.
	Shape, ShapeWhy string
	Steps           int // planned steps
	Runs            []RunWhy
	Providers       []ProviderWhy
	Escalations     []Escalation
	Reviews         []Choice // final review: runs or skipped, and why
	Checks          []Choice // checks detected from the build files (verify.auto)
	// ReqTests are independent tests that no longer failed a round, and
	// the tests a fix agent disputed (orchestrator.independent_tests_gate).
	ReqTests []Choice

	// Totals over the runs that reported usage.
	EstTokens, EstUSD sessionlog.Spread
	Tokens            event.TokenUsage
	NoHistory         int    // runs estimated from fixed defaults
	CostLine          string // the task's cost summary (with quota before -> after)

	Notes []string
}

// RunWhy is one agent run: its route, why, and estimate against use.
type RunWhy struct {
	Route
	Estimate sessionlog.Estimate
}

// Used is the run's fresh tokens.
func (r RunWhy) Used() int64 { return r.Tokens.Total() }

// ProviderWhy is how many runs one provider got and by which rules.
type ProviderWhy struct {
	Provider string
	Runs     int
	Rules    []string // "default ×2", "limit-fallback from codex ×1"
	Tokens   int64
}

// Escalation is one thing that moved work to a stronger route, another
// provider or another round, and its cause.
type Escalation struct {
	Step    string
	Attempt int
	What    string
	Cause   string
}

// Choice is a task-level choice record.
type Choice struct {
	What, Outcome, Why string
	Round              int
}

// Explain builds the explanation of a task from its state and the session
// logs in o.SessionDir.
func Explain(st *orchestrator.TaskState, o Options) *Explanation {
	recs, parts := taskRecords(o.SessionDir, st)
	d := &Data{Mode: st.Mode}
	d.fromRecords(recs)
	d.fromPlan(st)
	return explain(st, o, d, recs, parts)
}

// explain builds the explanation from a task's records, already read into
// d (Build reuses its own).
func explain(st *orchestrator.TaskState, o Options, d *Data, recs []sessionlog.Record, parts int) *Explanation {
	e := &Explanation{ID: st.ID, Task: st.Task, Status: st.Status, Mode: st.Mode, Created: st.Created, CostLine: st.CostLine}
	if st.Status == "running" && st.Interrupted() {
		e.Status = "interrupted"
	}
	if parts == 0 {
		e.Notes = append(e.Notes, "no session log found for this task: nothing to explain beyond its plan")
	}
	e.Mode, e.Duration, e.Steps = d.Mode, d.Duration, len(d.Steps)
	if d.HasCost {
		e.CostLine = d.CostLine
	}
	if e.Duration == 0 && st.Updated.After(st.Created) {
		e.Duration = st.Updated.Sub(st.Created)
	}

	hist := priorHistory(o.SessionDir, st)
	for _, rt := range d.Routes {
		r := RunWhy{Route: rt}
		kind := router.Kind(rt.Kind)
		r.Estimate = hist.Estimate(rt.Role, rt.Kind, sessionlog.RouteKey{Provider: rt.Provider, Model: rt.Model, Effort: rt.Effort}, kind.ReadOnly())
		e.Runs = append(e.Runs, r)
		if !rt.Ran || rt.LimitHit {
			continue // nothing (or only a limit error) to compare
		}
		e.EstTokens, e.EstUSD = e.EstTokens.Add(r.Estimate.Tokens), e.EstUSD.Add(r.Estimate.USD)
		e.Tokens = e.Tokens.Add(rt.Tokens)
		if r.Estimate.Source == sessionlog.SourceNone {
			e.NoHistory++
		}
	}

	var choices []Choice
	for _, r := range recs {
		if r.Type == sessionlog.TypeChoice {
			choices = append(choices, Choice{What: r.Step, Outcome: r.Kind, Why: r.Reason, Round: r.Attempt})
		}
	}
	e.shape(choices, st)
	e.providers()
	e.escalations(recs, choices)
	return e
}

// LastEnded is the newest task in dir that is not running (an interrupted
// one counts): the task a "why" after a result means, also while the next
// task runs.
func LastEnded(dir string) (*orchestrator.TaskState, error) {
	for _, t := range orchestrator.History(dir, 10) {
		if t.Status != "running" || t.Interrupted() {
			return &t, nil
		}
	}
	return nil, errors.New("no finished task in this folder yet (rw history --all lists every task)")
}

// priorHistory indexes the runs logged before the task started.
func priorHistory(dir string, st *orchestrator.TaskState) *sessionlog.History {
	if dir == "" {
		return nil
	}
	all, _ := sessionlog.ReadDir(dir) // unreadable logs only make it less precise
	var before []sessionlog.Record
	for _, r := range all {
		if r.TS.Before(st.Created) {
			before = append(before, r)
		}
	}
	return sessionlog.NewHistory(before, st.Dir)
}

// shape says why the task ran as one agent or several.
func (e *Explanation) shape(choices []Choice, st *orchestrator.TaskState) {
	for _, c := range choices {
		switch c.What {
		case sessionlog.ChoiceShape:
			// A resume logs its own; the first one is the task's shape.
			if e.Shape == "" {
				e.Shape, e.ShapeWhy = shapeName(c.Outcome), c.Why
			}
		case sessionlog.ChoiceReview:
			e.Reviews = append(e.Reviews, c)
		case sessionlog.ChoiceChecks:
			e.Checks = append(e.Checks, c)
		case sessionlog.ChoiceReqTests:
			e.ReqTests = append(e.ReqTests, c)
		}
	}
	if e.Shape != "" {
		return
	}
	// Logs from before choice records: tell what can be told.
	planner := false
	for _, r := range e.Runs {
		planner = planner || r.Role == event.RolePlanner && r.Kind == string(router.KindPlan) || r.Step == "plan"
	}
	switch {
	case e.Mode == "single":
		e.Shape, e.ShapeWhy = "single", "rw run --single: one agent on the chosen provider, no planner or reviewer"
	case planner:
		e.Shape, e.ShapeWhy = "planned", fmt.Sprintf("the planner made %d %s (this log predates recorded reasons)", e.Steps, plural(e.Steps, "step"))
	case e.Steps == 1:
		e.Shape, e.ShapeWhy = "one agent", "no planner ran (this log predates recorded reasons: a small task, or auto_single)"
	case st.Plan != nil:
		e.Shape, e.ShapeWhy = "planned", "the reason was not recorded"
	}
}

func shapeName(outcome string) string {
	switch outcome {
	case sessionlog.ChoiceOne:
		return "one agent"
	case sessionlog.ChoicePlanned:
		return "planned"
	case sessionlog.ChoiceResumed:
		return "resumed"
	}
	return outcome
}

// providers groups the runs by provider and the rules that sent them there.
func (e *Explanation) providers() {
	by := map[string]*ProviderWhy{}
	rules := map[string]map[string]int{}
	for _, r := range e.Runs {
		p := by[r.Provider]
		if p == nil {
			p = &ProviderWhy{Provider: r.Provider}
			by[r.Provider], rules[r.Provider] = p, map[string]int{}
		}
		p.Runs++
		p.Tokens += r.Used()
		rule := r.Rule
		if rule == "" {
			rule = "not recorded"
		}
		if r.Fallback && r.From != "" {
			rule += " from " + r.From
		}
		rules[r.Provider][rule]++
	}
	for _, name := range event.ProvidersOf(by) {
		p := by[name]
		for rule, n := range rules[name] {
			p.Rules = append(p.Rules, fmt.Sprintf("%s ×%d", rule, n))
		}
		sort.Strings(p.Rules)
		e.Providers = append(e.Providers, *p)
	}
}

// reRoleMove reads "same error twice: worker -> worker_high".
var reRoleMove = regexp.MustCompile(`same error twice: (\S+) -> (\S+)`)

// escalations finds what moved work up: a route rule that saw risk or a
// repeating error, a fallback off a limited provider, a stronger tier, the
// judge, a retry and the fix rounds.
func (e *Explanation) escalations(recs []sessionlog.Record, choices []Choice) {
	lastErr := map[string]string{} // step -> the error of its last failed run
	limits := map[string]string{}  // provider -> its last limit message
	for _, r := range recs {
		if r.Type == sessionlog.TypeLimit {
			limits[r.Provider] = r.Text
		}
	}
	seen := map[[2]string]bool{} // step, what: a retry on the same route is no new escalation
	for _, r := range e.Runs {
		add := func(what, cause string) {
			if k := [2]string{r.Step, what}; !seen[k] {
				seen[k] = true
				e.Escalations = append(e.Escalations, Escalation{Step: r.Step, Attempt: r.Attempt, What: what, Cause: cause})
			}
		}
		prev := lastErr[r.Step]
		switch r.Rule {
		case router.RuleRepeatError:
			what := "stronger role"
			if m := reRoleMove.FindStringSubmatch(r.Reason); m != nil {
				what = m[1] + " → " + m[2]
			}
			add(what+" ("+r.Label()+")", "the same error twice"+quoteErr(prev))
		case router.RuleLargeDiff:
			add("worker_high ("+r.Label()+")", firstClause(r.Reason))
		case router.RuleQuota:
			add("moved to "+r.Provider, firstClause(r.Reason))
		case router.RuleStandby:
			add("standby route "+r.Label(), firstClause(r.Reason))
		}
		if r.Fallback && r.Rule != router.RuleQuota {
			from := r.From
			if from == "" {
				from = "its provider"
			}
			cause := from + " was at its usage limit or unavailable"
			if msg := limits[from]; msg != "" {
				cause += quoteErr(msg)
			}
			add(from+" → "+r.Provider, cause)
		}
		if r.Judged {
			add("judge set the role: "+r.Role, "the rules were not confident about this step")
		}
		if tc := tierClause(r.Reason); tc != "" && strings.Contains(tc, "route instead of") && r.Tier == "strong" {
			add("tier strong ("+r.Label()+")", tc)
		}
		if r.Attempt > 1 && r.Rule != router.RuleRepeatError && !r.Fallback && prev != "" {
			add(fmt.Sprintf("retry (attempt %d)", r.Attempt), "the previous attempt failed"+quoteErr(prev))
		}
		if r.Ran && !r.OK {
			lastErr[r.Step] = r.Error
			if r.Error == "" {
				lastErr[r.Step] = "failed"
			}
		} else if r.Ran {
			delete(lastErr, r.Step)
		}
	}
	for _, c := range choices {
		if c.What == sessionlog.ChoiceFix {
			e.Escalations = append(e.Escalations, Escalation{Step: "fix", Attempt: c.Round, What: fmt.Sprintf("fix round %d", c.Round), Cause: c.Why})
		}
	}
	// Older logs have no fix choices: the fix runs themselves still show.
	if !hasFixChoice(choices) {
		fixes := map[string]bool{}
		for _, r := range e.Runs {
			if strings.HasPrefix(r.Step, "fix-") && !fixes[r.Step] {
				fixes[r.Step] = true
				e.Escalations = append(e.Escalations, Escalation{Step: r.Step, Attempt: 1, What: "fix round " + strings.TrimPrefix(r.Step, "fix-"),
					Cause: "the checks failed or the reviewer asked for changes (this log predates recorded reasons)"})
			}
		}
	}
}

func hasFixChoice(cs []Choice) bool {
	for _, c := range cs {
		if c.What == sessionlog.ChoiceFix {
			return true
		}
	}
	return false
}

// firstClause is a decision reason without the tier and learned-route
// notes the router appends after "; ".
func firstClause(reason string) string {
	c, _, _ := strings.Cut(reason, "; ")
	return c
}

// tierClause is the "tier ..." note of a decision reason ("" without one).
func tierClause(reason string) string {
	for _, c := range strings.Split(reason, "; ") {
		if strings.HasPrefix(c, "tier ") {
			return c
		}
	}
	return ""
}

func quoteErr(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "failed" {
		return ""
	}
	if l, _, cut := strings.Cut(s, "\n"); cut {
		s = l + " …"
	}
	if len(s) > 160 {
		s = s[:157] + "…"
	}
	return ": " + s
}

func plural(n int, s string) string {
	if n == 1 {
		return s
	}
	return s + "s"
}

// Agents counts the runs per role, e.g. "planner 1, worker 2, reviewer 1".
func (e *Explanation) Agents() string {
	n := map[string]int{}
	for _, r := range e.Runs {
		n[r.Role]++
	}
	var parts []string
	for _, role := range event.Roles {
		if n[role] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", role, n[role]))
			delete(n, role)
		}
	}
	rest := make([]string, 0, len(n))
	for role := range n {
		rest = append(rest, role)
	}
	sort.Strings(rest)
	for _, role := range rest {
		name := role
		if name == "" {
			name = "unrecorded"
		}
		parts = append(parts, fmt.Sprintf("%s %d", name, n[role]))
	}
	return strings.Join(parts, ", ")
}

// Text writes the explanation for a terminal.
func (e *Explanation) Text(w io.Writer) error {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	p("%s  %s", e.ID, e.Status)
	if e.Duration > 0 {
		p(" · %s", e.Duration.Round(time.Second))
	}
	if e.Mode != "" && e.Mode != "routed" {
		p(" · %s", e.Mode)
	}
	p("\n%s\n\n", clipLine(e.Task, 200))

	p("Why %s\n", agentsHeadline(e))
	if e.Shape != "" {
		p("  %s: %s\n", e.Shape, e.ShapeWhy)
	}
	if len(e.Runs) > 0 {
		p("  %d agent %s: %s\n", len(e.Runs), plural(len(e.Runs), "run"), e.Agents())
	}
	for _, c := range e.Checks {
		p("  checks: %s\n", c.Why)
	}
	for _, c := range e.Reviews {
		p("  final review %s: %s\n", c.Outcome, c.Why)
	}
	for _, c := range e.ReqTests {
		p("  independent tests %s (round %d): %s\n", c.Outcome, c.Round, c.Why)
	}

	if len(e.Runs) > 0 {
		p("\nRuns\n")
		title := func(r RunWhy) string {
			if r.Attempt > 1 {
				return fmt.Sprintf("%s (attempt %d)", r.Step, r.Attempt)
			}
			return r.Step
		}
		width := 0
		for _, r := range e.Runs {
			width = max(width, len(title(r)))
		}
		for i, r := range e.Runs {
			p("%2d. %-*s  %-11s → %s   %s\n", i+1, min(width, 28), title(r), r.Role, r.Label(), runOutcome(r))
			why := r.Rule
			if r.Reason != "" {
				why += " — " + r.Reason
			}
			if r.Fallback && r.From != "" && !strings.Contains(r.Reason, r.From) {
				why += " (moved from " + r.From + ")"
			}
			p("    why:  %s\n", why)
			if r.Ran && !r.LimitHit {
				p("    used: %s\n", usedLine(r))
			}
		}
	}

	if len(e.Providers) > 0 {
		p("\nProviders\n")
		for _, pw := range e.Providers {
			p("  %-8s %d %s, %s fresh tokens · %s\n", pw.Provider, pw.Runs, plural(pw.Runs, "run"), event.HumanTokens(pw.Tokens), strings.Join(pw.Rules, ", "))
		}
	}

	if len(e.Runs) > 0 {
		p("\nEstimated vs actual\n")
		p("  tokens  est %s (%s–%s)  actual %s%s\n", event.HumanTokens(int64(e.EstTokens.Mid)), event.HumanTokens(int64(e.EstTokens.Low)),
			event.HumanTokens(int64(e.EstTokens.High)), event.HumanTokens(e.Tokens.Total()), delta(float64(e.Tokens.Total()), e.EstTokens))
		if e.EstUSD.Mid > 0 || e.Tokens.CostUSD > 0 {
			p("  $       est $%.2f ($%.2f–$%.2f)  actual $%.2f%s  (API-equivalent)\n", e.EstUSD.Mid, e.EstUSD.Low, e.EstUSD.High, e.Tokens.CostUSD, delta(e.Tokens.CostUSD, e.EstUSD))
		}
		if e.Tokens.Incomplete {
			p("  some runs ended without final accounting: actual is a lower bound\n")
		}
		if e.NoHistory > 0 {
			p("  %d of the estimates had no history and use fixed defaults\n", e.NoHistory)
		}
		if e.CostLine != "" {
			p("  task: %s\n", e.CostLine)
		}
	}

	p("\nEscalations\n")
	if len(e.Escalations) == 0 {
		p("  none: every run kept its first route\n")
	}
	for _, x := range e.Escalations {
		if where := x.Where(); where != "" {
			p("  %s: %s — %s\n", where, x.What, x.Cause)
		} else {
			p("  %s — %s\n", x.What, x.Cause)
		}
	}
	for _, n := range e.Notes {
		p("\nnote: %s\n", n)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Headline is "one agent", "several agents (3 steps)" and the like.
func (e *Explanation) Headline() string { return agentsHeadline(e) }

// TokenDelta compares the task's fresh tokens with the estimate.
func (e *Explanation) TokenDelta() string {
	return strings.TrimSpace(delta(float64(e.Tokens.Total()), e.EstTokens))
}

// USDDelta compares the task's API-equivalent $ with the estimate.
func (e *Explanation) USDDelta() string { return strings.TrimSpace(delta(e.Tokens.CostUSD, e.EstUSD)) }

// HasUSD reports whether there is a $ estimate or a $ figure to compare.
func (e *Explanation) HasUSD() bool { return e.EstUSD.Mid > 0 || e.Tokens.CostUSD > 0 }

// EstDelta compares a run's fresh tokens with its estimate ("" if it did
// not run to the end).
func (r RunWhy) EstDelta() string {
	if !r.Ran || r.LimitHit {
		return ""
	}
	return strings.TrimSpace(delta(float64(r.Used()), r.Estimate.Tokens))
}

// Where is the step (and attempt) an escalation happened at; "" for a fix
// round, which names itself.
func (x Escalation) Where() string {
	switch {
	case strings.HasPrefix(x.What, "fix round"):
		return ""
	case x.Attempt > 1 && !strings.HasPrefix(x.What, "retry"):
		return fmt.Sprintf("%s attempt %d", x.Step, x.Attempt)
	}
	return x.Step
}

func agentsHeadline(e *Explanation) string {
	writers := 0
	for _, r := range e.Runs {
		if r.Role == event.RoleWorker || r.Role == event.RoleWorkerHigh || r.Role == event.RoleExplorer || r.Role == event.RoleResearcher {
			writers++
		}
	}
	switch {
	case e.Shape == "one agent" || e.Shape == "single":
		return "one agent"
	case e.Steps > 1:
		return fmt.Sprintf("several agents (%d steps)", e.Steps)
	case writers > 1:
		return "several agents"
	}
	return "this shape"
}

func runOutcome(r RunWhy) string {
	switch {
	case !r.Ran:
		return "no result logged"
	case r.LimitHit:
		return "limit hit"
	case r.OK:
		return "ok · " + r.Duration.Round(time.Second).String()
	}
	return "failed · " + r.Duration.Round(time.Second).String()
}

func usedLine(r RunWhy) string {
	es := r.Estimate
	s := fmt.Sprintf("%s fresh tokens", event.HumanTokens(r.Used()))
	if r.Tokens.Incomplete {
		s = "at least " + s
	}
	s += fmt.Sprintf(" · est %s (%s–%s, %s", event.HumanTokens(int64(es.Tokens.Mid)), event.HumanTokens(int64(es.Tokens.Low)), event.HumanTokens(int64(es.Tokens.High)), es.Source)
	if es.Samples > 0 {
		s += fmt.Sprintf(", %d %s", es.Samples, plural(es.Samples, "run"))
	}
	s += ")" + delta(float64(r.Used()), es.Tokens)
	if r.Tokens.CostUSD > 0 || es.USD.Mid > 0 {
		s += fmt.Sprintf(" · $%.2f vs est $%.2f", r.Tokens.CostUSD, es.USD.Mid)
	}
	return s
}

// delta compares an actual value with an estimate: "+16%", and whether it
// fell outside the estimate's range.
func delta(actual float64, est sessionlog.Spread) string {
	if est.Mid <= 0 {
		return ""
	}
	s := fmt.Sprintf("  %+.0f%%", (actual-est.Mid)/est.Mid*100)
	switch {
	case actual > est.High:
		s += " (above the range)"
	case actual < est.Low:
		s += " (below the range)"
	}
	return s
}

func clipLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}
