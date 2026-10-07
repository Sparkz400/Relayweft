package orchestrator

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// Task shape: before any agent runs, rw guesses from the task's text how
// big the task is, with the router's difficulty estimate (tiers.go) and
// the dry-run estimates (estimate.go):
//
//   - orchestrator.auto_single runs a task that does not look multi-file,
//     multi-part, broad or hard as one worker step, without the planner:
//     on the bench a single agent was 2-3x faster on such tasks, and the
//     planner and reviewer were most of a routed task's cost. A task the
//     budget cannot fund planning for runs as one step too.
//   - orchestrator.light_planning has the planner and reviewer of a task
//     that does not look hard or sensitive use the worker route.
//   - orchestrator.fit_budget tells the planner what the budget has left
//     and shrinks a plan estimated over it (fitPlan).
//   - orchestrator.review_skip_max_lines skips the final review of a small
//     change whose checks pass (skipFinalReview).

// Limits of a task that still runs as one step.
const (
	singleMaxFiles = 2   // more files named: multi-file
	singleMaxParts = 2   // more list items: multi-part
	singleMaxWords = 150 // longer: probably several things
)

// taskShape is what rw tells about a task from its text.
type taskShape struct {
	single     bool   // run as one worker step without the planner
	light      bool   // planner and reviewer use the worker route
	why        string // why single (or why not), for the log
	difficulty float64
	files      []string
}

var (
	// reFile finds file paths named in a task: a word with a letter-led
	// extension, optionally with directories (internal/x/y.go, README.md,
	// @src/app.ts). Version numbers (1.2.3) and "e.g." do not match.
	reFile = regexp.MustCompile(`(?:^|[\s(\x60'"@])((?:[\w.-]+[/\\])*[\w-]+\.[A-Za-z][A-Za-z0-9]{0,7})\b`)
	// reListItem finds bullet and numbered list lines.
	reListItem = regexp.MustCompile(`(?m)^\s*(?:[-*\x{2022}]|\d{1,2}[.)])\s+\S`)
	// reBroad finds words for work that spans the code base.
	reBroad = regexp.MustCompile(`(?i)\b(across|throughout|everywhere|codebase|code base|whole (repo|repository|project|app)|all (the )?(files|packages|modules|callers|call sites|places|uses|usages|endpoints|commands|tests)|every (file|package|module|caller|call site|endpoint|command)|end[- ]to[- ]end|migrate|migration|overhaul|rewrite|refactor)\b`)
	// reSteps finds words that order several pieces of work.
	reSteps = regexp.MustCompile(`(?i)\b(then|in parallel|after that|afterwards|followed by|step \d|finally)\b`)
	// notFile are matches of reFile that are no file names.
	notFile = map[string]bool{"e.g": true, "i.e": true, "etc": true, "vs": true}
)

// mentionedFiles lists the distinct file paths a text names.
func mentionedFiles(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range reFile.FindAllStringSubmatch(text, -1) {
		f := strings.TrimRight(m[1], ".")
		low := strings.ToLower(f)
		if notFile[low] || strings.HasPrefix(low, "http") || seen[low] || !strings.Contains(f, ".") {
			continue
		}
		seen[low] = true
		out = append(out, f)
	}
	return out
}

// listItems counts a text's bullet and numbered list lines.
func listItems(text string) int { return len(reListItem.FindAllString(text, -1)) }

// shapeTask decides how a new task runs. It never applies to a resumed or
// multi-repo task, nor with a forced plan (rw run --plan).
func (o *Orchestrator) shapeTask(t *task) taskShape {
	oc := t.cfg.Orchestrator
	files := mentionedFiles(t.text)
	step := router.Step{ID: "work", Kind: router.KindEdit, Prompt: t.text, Files: files}
	diff, signals := router.Difficulty(step, event.RoleWorker)
	s := taskShape{difficulty: diff, files: files}
	hard := router.IsHard(diff)
	sens := o.router.Sensitive(files, t.text)
	s.light = oc.LightPlanning && !hard && sens == ""

	var not []string
	if len(t.repos) > 0 {
		not = append(not, "several repos")
	}
	if n := len(files); n > singleMaxFiles {
		not = append(not, fmt.Sprintf("%d files named", n))
	}
	if n := listItems(t.text); n > singleMaxParts {
		not = append(not, fmt.Sprintf("%d list items", n))
	}
	if m := reSteps.FindString(t.text); m != "" {
		not = append(not, "several steps: "+strings.ToLower(m))
	}
	if m := reBroad.FindString(t.text); m != "" {
		not = append(not, "broad: "+strings.ToLower(m))
	}
	if w := len(strings.Fields(t.text)); w > singleMaxWords {
		not = append(not, fmt.Sprintf("%d words", w))
	}
	if hard {
		not = append(not, fmt.Sprintf("difficulty %.2f (%s)", diff, strings.Join(signals, ", ")))
	}
	if sens != "" {
		not = append(not, "sensitive: "+sens)
	}
	switch {
	case t.forcePlan || (t.resumed && t.state != nil && t.state.Plan != nil):
		s.why = "planned on request"
	case oc.SingleWorker && len(t.repos) == 0:
		s.single = true
		s.why = "one worker: orchestrator.single_worker (use --plan for a planned task)"
	case len(not) == 0 && oc.AutoSingle:
		s.single = true
		s.why = fmt.Sprintf("one step: no sign of a multi-file task (difficulty %.2f)", diff)
	case len(not) == 0:
		s.why = "auto_single is off"
	case len(t.repos) == 0 && oc.FitBudget && !o.planFits(t):
		// Planning would leave too little for the work itself.
		s.single = true
		s.why = "one step: the budget left cannot fund planning, the work and the review (" + strings.Join(not, ", ") + ")"
	default:
		s.why = "planned: " + strings.Join(not, ", ")
	}
	return s
}

// shortcutPlan is the one-step plan of a task that skips the planner: a
// small task (orchestrator.small_task_words) or one shapeTask runs as one
// step. ok is false when the task is planned; small is true for a small
// task, which also skips plan approval.
func (o *Orchestrator) shortcutPlan(t *task) (p Plan, why string, ok, small bool) {
	oc := t.cfg.Orchestrator
	one := func(summary string) Plan {
		p := Plan{Summary: summary, Subtasks: []Subtask{{ID: "work", Title: firstWords(t.text, 6), Kind: router.KindEdit, Prompt: t.text}}}
		// "Where is X? Write the answer to ANSWER.md" writes a file.
		if looksRead(t.text) && !router.MentionsWrite(t.text) {
			p.Subtasks[0].Kind, p.Subtasks[0].ID = router.KindExplore, "explore"
		}
		return p
	}
	if words := len(strings.Fields(t.text)); oc.SmallTaskWords > 0 && words < oc.SmallTaskWords && len(t.repos) == 0 && !t.forcePlan {
		return one("small task: one worker step"), fmt.Sprintf("small task (%d words): skipping the planner", words), true, true
	}
	if !t.shape.single {
		return Plan{}, "", false, false
	}
	p = one("one worker step: " + t.shape.why)
	p.Subtasks[0].Files = t.shape.files
	return p, t.shape.why + "; skipping the planner (rw run --plan plans it)", true, false
}

// preflightOnly is the one provider whose permission preflight a task
// needs when it runs as one step without the planner: the provider that
// step routes to (a Claude probe before a Codex worker only spends quota).
// "" probes every Claude writing provider (planned and resumed tasks).
func (o *Orchestrator) preflightOnly(t *task) string {
	if t.resumed && t.state != nil && t.state.Plan != nil {
		return ""
	}
	if t.testsFirst {
		return "" // the test writer runs on another provider (testsfirst.go)
	}
	p, _, ok, _ := o.shortcutPlan(t)
	if !ok || p.Subtasks[0].Kind.ReadOnly() {
		return ""
	}
	return o.router.Route(routerStep(t, p.Subtasks[0])).Provider
}

// estimateStep estimates one step on the route the router picks for it
// now, from the budget's history (fixed defaults in bench runs and
// without history, like the reservation in admitAgent).
func (o *Orchestrator) estimateStep(t *task, step router.Step) sessionlog.Estimate {
	d := o.router.Route(step)
	var hist *sessionlog.History
	if t.budget != nil {
		hist = t.budget.history
	}
	return hist.Estimate(d.Role, string(step.Kind), sessionlog.RouteKey{Provider: d.Provider, Model: d.Model, Effort: d.Effort}, step.Kind.ReadOnly())
}

// budgetRoom is what is left of the tightest token and $ budget limits,
// with the estimates of running agents held back; ok is false when no
// limit is set.
type budgetRoom struct {
	tokens, usd        float64
	tokenMax, usdMax   float64 // the limits these are left of (0 = none)
	tokenName, usdName string
}

func (r budgetRoom) any() bool { return r.tokenMax > 0 || r.usdMax > 0 }

// fits reports whether an estimate (tokens and $) fits the room.
func (r budgetRoom) fits(tokens, usd float64) bool {
	return (r.tokenMax <= 0 || tokens <= r.tokens) && (r.usdMax <= 0 || usd <= r.usd)
}

func (o *Orchestrator) budgetRoom(t *task) budgetRoom {
	var r budgetRoom
	if t.budget == nil {
		return r
	}
	cfg := o.budgetLimits(t)
	if !cfg.Any() {
		return r
	}
	t.budget.mu.Lock()
	held := t.budget.reserved.Add(t.budget.unknown)
	lims := o.budgetUse(t, cfg, false)
	t.budget.mu.Unlock()
	r.tokens, r.usd = math.Inf(1), math.Inf(1)
	for _, l := range lims {
		if l.max <= 0 {
			continue
		}
		if (BudgetRequest{Limit: l.name}).USD() {
			if left := max(0, l.max-l.used-held.CostUSD); left < r.usd {
				r.usd, r.usdMax, r.usdName = left, l.max, l.name
			}
			continue
		}
		if left := max(0, l.max-l.used-float64(held.Total())); left < r.tokens {
			r.tokens, r.tokenMax, r.tokenName = left, l.max, l.name
		}
	}
	return r
}

// finishEstimate is the final review, or the independent test writer that
// takes its place, plus one fix round: what a plan must leave room for.
func (o *Orchestrator) finishEstimate(t *task) (tokens, usd float64) {
	oc := t.cfg.Orchestrator
	if reviewExpected(t, oc) {
		e := o.estimateStep(t, router.Step{ID: FinalReviewID, Title: "final review", Kind: router.KindReview, MainProvider: t.mainProv, Light: t.shape.light})
		tokens, usd = tokens+e.Tokens.Mid, usd+e.USD.Mid
	}
	if reqTestsExpected(t, Plan{Subtasks: []Subtask{{Kind: router.KindEdit}}}) {
		d := o.testerRoute(t, "")
		e := o.estimateStep(t, router.Step{ID: ReqTestsID, Title: reqTestsTitle, Kind: router.KindEdit, Pin: &d})
		tokens, usd = tokens+e.Tokens.Mid, usd+e.USD.Mid
	}
	if oc.MaxFixRounds > 0 && (oc.ReviewBeforeDone || t.verifying()) {
		e := o.estimateStep(t, router.Step{ID: "fix", Title: "fix round", Kind: router.KindFix, Prompt: t.text})
		tokens, usd = tokens+e.Tokens.Mid, usd+e.USD.Mid
	}
	return tokens, usd
}

// planFits reports whether the budget can fund the planner, one writing
// step and the finish before anything runs (no limit: it always can).
func (o *Orchestrator) planFits(t *task) bool {
	room := o.budgetRoom(t)
	if !room.any() {
		return true
	}
	p := o.estimateStep(t, router.Step{ID: "plan", Kind: router.KindPlan, Prompt: t.text, Light: t.shape.light})
	w := o.estimateStep(t, router.Step{ID: "work", Kind: router.KindEdit, Prompt: t.text, Files: t.shape.files})
	ft, fu := o.finishEstimate(t)
	return room.fits(p.Tokens.Mid+w.Tokens.Mid+ft, p.USD.Mid+w.USD.Mid+fu)
}

// planBudgetHint tells the planner what the budget has left ("" without
// limits or orchestrator.fit_budget).
func (o *Orchestrator) planBudgetHint(t *task) string {
	if !t.cfg.Orchestrator.FitBudget {
		return ""
	}
	room := o.budgetRoom(t)
	if !room.any() || room.tokenMax <= 0 {
		return ""
	}
	w := o.estimateStep(t, router.Step{ID: "work", Kind: router.KindEdit, Prompt: t.text})
	r := o.estimateStep(t, router.Step{ID: "explore", Kind: router.KindExplore, Prompt: t.text})
	ft, _ := o.finishEstimate(t)
	work := room.tokens - ft
	n := 1
	if w.Tokens.Mid > 0 {
		n = max(1, int(work/w.Tokens.Mid))
	}
	return fmt.Sprintf("\nBUDGET: about %s fresh tokens are left for this whole task. An edit subtask usually takes about %s, "+
		"a read-only one about %s, and the review and one fix round about %s. Plan at most %d subtask(s) so the whole task fits: "+
		"prefer fewer, larger edit subtasks, and add explore or research subtasks only when the edits need their findings. "+
		"Never leave required work out to fit.\n",
		tok(room.tokens), tok(w.Tokens.Mid), tok(r.Tokens.Mid), tok(ft), min(n, 6))
}

// planEstimate is a plan's median tokens and $ on the budget's history.
func (o *Orchestrator) planEstimate(t *task, p Plan) (tokens, usd float64) {
	for _, st := range p.Subtasks {
		step := routerStep(t, st)
		n := 1
		if on, _ := o.wantBestOf(t, st); on {
			if routes, _ := o.bestOfRoutes(t, step); len(routes) >= 2 {
				n = len(routes)
			}
		}
		e := o.estimateStep(t, step)
		tokens, usd = tokens+float64(n)*e.Tokens.Mid, usd+float64(n)*e.USD.Mid
	}
	return tokens, usd
}

// fitPlan makes a plan fit what the budget has left, for the work and the
// finish (final review and one fix round). In order, until it fits: skip
// the plan review, run best-of steps once, then merge single-repo steps
// into one step that still has every step's work. Multi-repo assignments
// and dependencies stay intact. Required work is never dropped:
// a one-step plan that still does not fit runs, and the budget admission
// decides. reviewPlan is false when the plan review should be skipped.
func (o *Orchestrator) fitPlan(t *task, p Plan, reviewPlan bool) (Plan, bool) {
	if !t.cfg.Orchestrator.FitBudget {
		return p, reviewPlan
	}
	room := o.budgetRoom(t)
	if !room.any() {
		return p, reviewPlan
	}
	ft, fu := o.finishEstimate(t)
	need := func(p Plan, review bool) (float64, float64) {
		pt, pu := o.planEstimate(t, p)
		pt, pu = pt+ft, pu+fu
		if review {
			e := o.estimateStep(t, router.Step{ID: "review-plan", Kind: router.KindReview, MainProvider: t.mainProv, Light: t.shape.light})
			pt, pu = pt+e.Tokens.Mid, pu+e.USD.Mid
		}
		return pt, pu
	}
	left := func() string {
		var parts []string
		if room.tokenMax > 0 {
			parts = append(parts, tok(room.tokens)+" tokens")
		}
		if room.usdMax > 0 {
			parts = append(parts, usd(room.usd))
		}
		return strings.Join(parts, " and ")
	}
	if nt, nu := need(p, reviewPlan); room.fits(nt, nu) {
		return p, reviewPlan
	}
	if reviewPlan {
		reviewPlan = false
		if nt, nu := need(p, false); room.fits(nt, nu) {
			o.logf("budget: skipping the plan review so the plan fits (%s left, plan ~%s)", left(), tok(nt))
			return p, false
		}
	}
	if planBestOf(p) || t.cfg.Routing.BestOf.On() {
		q := p
		q.Subtasks = append([]Subtask(nil), p.Subtasks...)
		for i := range q.Subtasks {
			if !q.Subtasks[i].Kind.ReadOnly() {
				q.Subtasks[i].BestOf = BestOfOff
			}
		}
		p = q
		if nt, nu := need(p, false); room.fits(nt, nu) {
			o.logf("budget: running best-of steps once so the plan fits (%s left, plan ~%s)", left(), tok(nt))
			return p, false
		}
	}
	if len(p.Subtasks) > 1 {
		nt, _ := need(p, false)
		if len(t.repos) > 0 {
			o.logf("budget: the multi-repo plan (~%s with review and one fix round) may not fit %s; keeping repository assignments and dependencies, and the budget admission decides", tok(nt), left())
			return p, false
		}
		m := mergePlan(t.text, p)
		mt, _ := need(m, false)
		o.logf("budget: the %d-step plan (~%s with review and one fix round) does not fit %s; merged into one step (~%s)", len(p.Subtasks), tok(nt), left(), tok(mt))
		return m, false
	}
	nt, _ := need(p, false)
	o.logf("budget: even one step (~%s with review and one fix round) may not fit %s; it runs, and the budget admission decides", tok(nt), left())
	return p, false
}

// mergePlan turns a plan into one edit step that carries every step's
// work, so one agent does it all without the per-step overhead.
func mergePlan(task string, p Plan) Plan {
	var b strings.Builder
	b.WriteString(task + "\n\nWork through this plan in order, all of it:\n")
	var files []string
	seen := map[string]bool{}
	for i, st := range p.Subtasks {
		fmt.Fprintf(&b, "%d. %s (%s): %s\n", i+1, st.Title, st.Kind, strings.TrimSpace(st.Prompt))
		for _, f := range st.Files {
			if !seen[f] {
				seen[f] = true
				files = append(files, f)
			}
		}
	}
	st := Subtask{ID: "work", Title: firstWords(task, 6), Kind: router.KindEdit, Prompt: b.String(), Files: files, Repo: p.Subtasks[0].Repo}
	if !hasEdits(p) {
		st.Kind, st.ID = router.KindExplore, "explore"
	}
	return Plan{Summary: p.Summary + " (merged into one step to fit the budget)", Subtasks: []Subtask{st}, Repos: p.Repos}
}

// changedLines counts the added and removed lines in a unified diff.
func changedLines(diff string) int {
	n := 0
	for _, l := range strings.Split(diff, "\n") {
		if strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---") {
			continue
		}
		if strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") {
			n++
		}
	}
	return n
}

// reviewExpected reports whether a task's final review is expected to run,
// for its estimate: with review_when: untested only a task without checks
// gets one.
func reviewExpected(t *task, oc config.OrchestratorCfg) bool {
	return oc.ReviewBeforeDone && (oc.FinalReview() != config.ReviewUntested || !t.verifying())
}

// skipFinalReview reports whether this round's final review is skipped,
// per orchestrator.review_when; why says what decided, for the log and rw
// explain. verified is the round's checks passing; tests is what its
// independent tests did (reqTestsNone, reqTestsPass, reqTestsAdvise or
// reqTestsFail); last is the round no fix round follows.
//
// With review_when: untested (the default) tests take the review's place:
// it runs only on a task without checks, where nothing else can decide.
// With checks, the checks and the independent tests decide, and their
// failing output advises the fix round. On the bench the final review
// approved 7 of 10 failing results and never changed a verdict; tests
// written from the task text alone caught 4 of the 7 it approved.
//
// With review_when: failing the checks decide too, but a review still
// runs to advise a fix round after they fail (or a step failed). A review
// after the last round would advise nothing.
func (o *Orchestrator) skipFinalReview(t *task, verifying, verified bool, tests string, allOK, reviewAsked, last bool) (bool, string) {
	checksOK := verified && tests != reqTestsFail
	switch t.cfg.Orchestrator.FinalReview() {
	case config.ReviewAlways:
		return false, "review_when: always"
	case config.ReviewUntested:
		switch {
		case !verifying:
			return false, "no checks or tests to decide, so the reviewer does"
		case checksOK && allOK:
			return true, map[string]string{reqTestsNone: "the checks pass", reqTestsPass: "the checks and independent tests pass",
				reqTestsAdvise: "the checks pass; the independent tests that fail only advise"}[tests]
		case checksOK:
			return true, "a step failed, so the task fails whatever a review says (review_when: untested)"
		case !verified && tests == reqTestsFail:
			return true, "the checks and independent tests fail: their output advises the fix round (review_when: untested)"
		case !verified:
			return true, "the checks fail: their output advises the fix round (review_when: untested)"
		}
		return true, "the independent tests fail: their output advises the fix round (review_when: untested)"
	case config.ReviewFailing:
		switch {
		case !verifying:
			return false, "no checks to decide, so the reviewer does"
		case checksOK && allOK:
			return true, "the checks pass (review_when: failing)"
		case last:
			return true, "no fix round is left for a review to advise"
		case !checksOK:
			return false, "the checks fail: the reviewer advises the fix round"
		}
		return false, "a step failed: the reviewer advises the fix round"
	}
	return o.skipSmallReview(t, verifying, checksOK, allOK, reviewAsked)
}

// skipSmallReview is review_when: large: the final review is skipped when
// the checks ran and pass, every step succeeded, no earlier review asked
// for changes, nothing is kept on a conflict branch, the change is at most
// orchestrator.review_skip_max_lines lines and touches no sensitive file.
func (o *Orchestrator) skipSmallReview(t *task, verifying, verified, allOK, reviewAsked bool) (bool, string) {
	limit := t.cfg.Orchestrator.ReviewSkipMaxLines
	switch {
	case limit <= 0:
		return false, "review_skip_max_lines is off"
	case !verifying:
		return false, "no checks ran to stand in for it"
	case !verified:
		return false, "the checks fail"
	case !allOK:
		return false, "a step failed"
	case reviewAsked:
		return false, "an earlier review asked for changes"
	case len(t.kept) > 0:
		return false, "work is kept on a conflict branch"
	case len(ParseCriteria(t.text)) > 0:
		return false, "the task lists acceptance criteria, which only the final review verifies"
	}
	const most = 200_000
	_, diff := t.workspaceDiff(most)
	if len(diff) >= most {
		return false, "the diff is too large to count"
	}
	if strings.Contains(diff, "\nBinary files ") || strings.HasPrefix(diff, "Binary files ") {
		return false, "the change includes binary files"
	}
	n := changedLines(diff)
	if n == 0 {
		return false, "no changed lines to count"
	}
	if n > limit {
		return false, fmt.Sprintf("the diff has %d lines (> %d)", n, limit)
	}
	if s := o.router.Sensitive(diffFiles(diff), ""); s != "" {
		return false, "the change touches a sensitive path: " + s
	}
	return true, fmt.Sprintf("checks pass and the diff has %d lines (<= review_skip_max_lines %d)", n, limit)
}

// What a round's independent tests did, for skipFinalReview.
const (
	reqTestsNone   = ""       // the task has none
	reqTestsPass   = "pass"   // they pass
	reqTestsAdvise = "advise" // they fail, but only advise (reqGate)
	reqTestsFail   = "fail"   // they fail the round
)

// diffFiles lists the files a unified diff changes (new files included,
// which the snapshot diff has and git diff against the work tree has not).
func diffFiles(diff string) []string {
	var out []string
	for _, l := range strings.Split(diff, "\n") {
		if rest, ok := strings.CutPrefix(l, "diff --git a/"); ok {
			if a, b, ok := strings.Cut(rest, " b/"); ok {
				out = append(out, a)
				if b != a {
					out = append(out, b)
				}
			}
		}
	}
	return out
}
