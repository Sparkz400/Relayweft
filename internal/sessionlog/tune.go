package sessionlog

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
)

// Suggestion is one data-backed change to the routing config, for `sy tune`.
type Suggestion struct {
	Severity string // "high", "medium" or "info"
	Title    string
	Detail   string   // the evidence, with numbers
	Commands []string // TUI slash commands that apply it, e.g. "/route worker claude:sonnet:high"
}

// Severities, most urgent first.
const (
	SevHigh   = "high"
	SevMedium = "medium"
	SevInfo   = "info"
)

// Thresholds. A suggestion needs enough samples that one bad afternoon does
// not rewrite the config. The rates are floors for the lower end of the
// rate's confidence interval (clearlyAbove), not for the rate itself: in
// the benchmarks and logs so far almost no run failed and no review
// rejected, so a few failures in a handful of runs say little. 5 runs need
// 3 failures to cross failFloor, 10 runs 4, 20 runs 7, 50 runs 14.
const (
	minRuns          = 5    // samples per group before any rate is trusted
	minJudged        = 10   // judged decisions before judging the judge
	failFloor        = 0.20 // route failure rate worth acting on
	escalateFloor    = 0.10 // share of a role's steps that needed error-repeats
	minEscalations   = 3
	rejectFloor      = 0.25 // final reviews rejected
	minLimitSwitches = 5    // limit-fallback/quota-preempt away from one provider
	minLimitDays     = 2    // ... on this many days: one limit hit moves many steps
	cheapTokens      = 30_000
)

// confZ is the z-score of the one-sided 90% bound clearlyAbove uses.
const confZ = 1.2816

// wilsonLower is the lower end of the Wilson score interval for k of n.
func wilsonLower(k, n int) float64 {
	if n <= 0 {
		return 0
	}
	nf, p, z2 := float64(n), float64(k)/float64(n), confZ*confZ
	centre := p + z2/(2*nf)
	margin := confZ * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf))
	return (centre - margin) / (1 + z2/nf)
}

// clearlyAbove reports whether k of n is above floor with 90% confidence:
// few samples need a clear majority, many need little more than floor.
func clearlyAbove(k, n int, floor float64) bool {
	return n >= minRuns && wilsonLower(k, n) >= floor
}

// cheapRoles are the roles that are fine on either provider, so they are
// the first to move when one provider keeps running out of quota.
var cheapRoles = []string{event.RoleExplorer, event.RoleResearcher, event.RoleJudge}

// Catalog is what the suggestions may propose: the fast route per
// provider, the effort ladders and which models are fast-tier. CatalogFrom
// reads it from the config; DefaultCatalog matches default.yaml.
type Catalog struct {
	Cheap   map[string]string   // provider -> "model[:effort]"
	Efforts map[string][]string // provider -> ladder, lowest first
	Fast    map[string]bool     // model ids of the fast tier
	// Order is where work may move, in routing order (the enabled
	// providers that may take over); empty = codex, claude.
	Order []string
}

// other is where a suggestion moves work away from p: the first other
// provider in routing order.
func (c Catalog) other(p string) string {
	order := c.Order
	if len(order) == 0 {
		order = []string{event.Codex, event.Claude}
	}
	for _, q := range order {
		if q != p {
			return q
		}
	}
	return p
}

// from is the provider a fallback decision moved away from (logs written
// before it was recorded had only two providers).
func (c Catalog) from(r Record) string {
	if r.From != "" {
		return r.From
	}
	return c.other(r.Provider)
}

// DefaultCatalog matches default.yaml. Its ladders stop below the premium
// levels (codex max/ultra) because a tuning hint should not quietly
// multiply quota use.
var DefaultCatalog = Catalog{
	Cheap: map[string]string{event.Codex: "gpt-6-luna:low", event.Claude: "haiku"},
	Efforts: map[string][]string{
		event.Codex:  {"low", "medium", "high", "xhigh"},
		event.Claude: {"low", "medium", "high", "xhigh", "max"},
	},
}

// premiumEfforts are left out of the ladders read from the config.
var premiumEfforts = map[string]bool{"max": true, "ultra": true}

// CatalogFrom builds the catalog from the user's config: the explorer
// route is the fast route, models marked tier: fast are fast, and each
// provider's efforts form its ladder.
func CatalogFrom(cfg *config.Config) Catalog {
	c := Catalog{Cheap: map[string]string{}, Efforts: map[string][]string{}, Fast: map[string]bool{}}
	for _, p := range cfg.Enabled() {
		if !cfg.Providers[p].OnlyPreferred {
			c.Order = append(c.Order, p)
		}
	}
	for name, p := range cfg.Providers {
		for _, m := range p.Models {
			if m.Tier == "fast" {
				c.Fast[m.ID] = true
			}
		}
		var ladder []string
		for _, e := range p.Efforts {
			if !premiumEfforts[e] || (name == event.Claude && e == "max") {
				ladder = append(ladder, e)
			}
		}
		if len(ladder) > 0 {
			c.Efforts[name] = ladder
		}
	}
	if ex, ok := cfg.Roles[event.RoleExplorer]; ok {
		for _, prov := range cfg.ProviderNames() {
			if r := ex.For(prov); r.Model != "" {
				c.Cheap[prov] = strings.TrimSuffix(r.Model+":"+r.Effort, ":")
			}
		}
	}
	for k, v := range DefaultCatalog.Cheap {
		if c.Cheap[k] == "" {
			c.Cheap[k] = v
		}
	}
	for k, v := range DefaultCatalog.Efforts {
		if len(c.Efforts[k]) == 0 {
			c.Efforts[k] = v
		}
	}
	return c
}

// Suggest reads the logs and proposes routing changes. Each heuristic is
// independent; results are sorted by severity.
func Suggest(recs []Record, f Filter) []Suggestion {
	return SuggestFor(recs, f, DefaultCatalog)
}

// SuggestFor is Suggest with the models and efforts of a config.
func SuggestFor(recs []Record, f Filter, cat Catalog) []Suggestion {
	var kept []Record
	for _, r := range recs {
		if f.keep(r) {
			kept = append(kept, r)
		}
	}
	var out []Suggestion
	for _, h := range []func([]Record) []Suggestion{
		routedVsSingle, cat.failingRoutes, cat.escalations, cat.finalReviews, cat.limitPressure, judgeAdvice, cat.cheaperReadOnly,
	} {
		out = append(out, h(kept)...)
	}
	rank := map[string]int{SevHigh: 0, SevMedium: 1, SevInfo: 2}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Severity] < rank[out[j].Severity] })
	return out
}

// failed reports whether an agent run counts as a failure. Limit hits are
// excluded: they say nothing about the route's quality.
func failed(r Record) bool { return r.OK == nil || !*r.OK }

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// nextEffort is one step up the provider's ladder, "high" for the CLI
// default, or "" when already at the top.
func nextEffort(provider, effort string) string { return DefaultCatalog.nextEffort(provider, effort) }

func (c Catalog) nextEffort(provider, effort string) string {
	ladder := c.Efforts[provider]
	if effort == "" {
		return "high"
	}
	for i, e := range ladder {
		if e == effort && i+1 < len(ladder) {
			return ladder[i+1]
		}
	}
	return ""
}

func routeSpec(provider, model, effort string) string {
	s := provider + ":" + model
	if effort != "" {
		s += ":" + effort
	}
	return s
}

// counter tracks the most common value, for "where does this role usually run".
type counter map[string]int

func (c counter) top() string {
	best, n := "", 0
	for k, v := range c {
		if v > n || (v == n && k < best) {
			best, n = k, v
		}
	}
	return best
}

// failingRoutes flags role+provider+model combinations that fail often.
func failingRoutes(recs []Record) []Suggestion { return DefaultCatalog.failingRoutes(recs) }

func (c Catalog) failingRoutes(recs []Record) []Suggestion {
	type agg struct {
		role, prov, model string
		runs, fails       int
		effort            counter
	}
	groups := map[string]*agg{}
	var keys []string
	for _, r := range recs {
		if r.Type != TypeAgentEnd || r.LimitHit || r.Role == "" {
			continue
		}
		k := r.Role + "|" + r.Provider + "|" + r.Model
		g := groups[k]
		if g == nil {
			g = &agg{role: r.Role, prov: r.Provider, model: r.Model, effort: counter{}}
			groups[k] = g
			keys = append(keys, k)
		}
		g.runs++
		g.effort[r.Effort]++
		if failed(r) {
			g.fails++
		}
	}
	sort.Strings(keys)
	var out []Suggestion
	for _, k := range keys {
		g := groups[k]
		rate := pct(g.fails, g.runs)
		if !clearlyAbove(g.fails, g.runs, failFloor) {
			continue
		}
		sev := SevMedium
		if rate >= 0.5 {
			sev = SevHigh
		}
		var cmds []string
		if e := c.nextEffort(g.prov, g.effort.top()); e != "" {
			cmds = append(cmds, fmt.Sprintf("/route %s %s", g.role, routeSpec(g.prov, g.model, e)))
		}
		cmds = append(cmds, fmt.Sprintf("/prefer %s %s", g.role, c.other(g.prov)))
		out = append(out, Suggestion{
			Severity: sev,
			Title:    fmt.Sprintf("%s on %s:%s fails often", g.role, g.prov, g.model),
			Detail: fmt.Sprintf("%d of %d runs failed (%.0f%%, limit hits excluded). Raise the effort or move %s to %s.",
				g.fails, g.runs, rate*100, g.role, c.other(g.prov)),
			Commands: cmds,
		})
	}
	return out
}

var escalateRe = regexp.MustCompile(`same error twice: (\S+) -> (\S+)`)

// escalations flags roles whose steps keep hitting the same error twice and
// get bumped up the ladder: the first attempt is wasted every time.
func escalations(recs []Record) []Suggestion { return DefaultCatalog.escalations(recs) }

func (c Catalog) escalations(recs []Record) []Suggestion {
	steps := map[string]int{}      // first-attempt decisions per role
	esc := map[string]int{}        // error-repeats escalations away from a role
	prov := map[string]counter{}   // where each role usually runs
	routes := map[string]counter{} // full route per role
	for _, r := range recs {
		if r.Type != TypeDecision || r.Role == "" {
			continue
		}
		if m := escalateRe.FindStringSubmatch(r.Reason); m != nil {
			esc[m[1]]++
			continue
		}
		if r.Attempt > 1 {
			continue
		}
		steps[r.Role]++
		if prov[r.Role] == nil {
			prov[r.Role], routes[r.Role] = counter{}, counter{}
		}
		if !r.Fallback {
			prov[r.Role][r.Provider]++
		}
		routes[r.Role][routeSpec(r.Provider, r.Model, r.Effort)]++
	}
	roles := make([]string, 0, len(esc))
	for role := range esc {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	var out []Suggestion
	for _, role := range roles {
		n, total := esc[role], steps[role]
		rate := pct(n, total)
		if n < minEscalations || !clearlyAbove(n, total, escalateFloor) {
			continue
		}
		var cmds []string
		if role == event.RoleWorker {
			if hi := routes[event.RoleWorkerHigh].top(); hi != "" {
				cmds = append(cmds, fmt.Sprintf("/route %s %s", role, hi))
			}
		}
		if p := prov[role].top(); p != "" {
			cmds = append(cmds, fmt.Sprintf("/prefer %s %s", role, c.other(p)))
		}
		out = append(out, Suggestion{
			Severity: SevMedium,
			Title:    fmt.Sprintf("%s steps often escalate after repeated errors", role),
			Detail: fmt.Sprintf("%d of %d %s steps hit the same error twice and were escalated (%.0f%%). Start them on the stronger route (worker_high) or the other provider.",
				n, total, role, rate*100),
			Commands: cmds,
		})
	}
	return out
}

// finalReviews flags a high rejection rate at the final review: the work
// reaches review unfinished, so either the worker is too weak or it needs
// more fix rounds.
func finalReviews(recs []Record) []Suggestion { return DefaultCatalog.finalReviews(recs) }

func (c Catalog) finalReviews(recs []Record) []Suggestion {
	n, rejected := 0, 0
	worker := counter{}
	for _, r := range recs {
		switch {
		case r.Type == TypeReview && r.Step == "final":
			n++
			if failed(r) {
				rejected++
			}
		case r.Type == TypeAgentEnd && r.Role == event.RoleWorker && !r.LimitHit:
			worker[r.Provider+"|"+r.Model+"|"+r.Effort]++
		}
	}
	rate := pct(rejected, n)
	if !clearlyAbove(rejected, n, rejectFloor) {
		return nil
	}
	sev := SevMedium
	if rate >= 0.6 {
		sev = SevHigh
	}
	var cmds []string
	if w := worker.top(); w != "" {
		p := strings.SplitN(w, "|", 3)
		if e := c.nextEffort(p[0], p[2]); e != "" {
			cmds = append(cmds, fmt.Sprintf("/route worker %s", routeSpec(p[0], p[1], e)))
		}
	}
	return []Suggestion{{
		Severity: sev,
		Title:    "final reviews reject a lot of work",
		Detail: fmt.Sprintf("%d of %d final reviews requested changes (%.0f%%). Use a stronger worker route, or raise orchestrator.max_fix_rounds in switchyard.yaml so rejected work gets another fix round.",
			rejected, n, rate*100),
		Commands: cmds,
	}}
}

// limitPressure flags a provider that keeps running out: the cheap roles
// still preferring it should move to the other provider so the scarce quota
// goes to the work that needs it.
func limitPressure(recs []Record) []Suggestion { return DefaultCatalog.limitPressure(recs) }

func (c Catalog) limitPressure(recs []Record) []Suggestion {
	away := map[string]int{}        // switches away from a provider
	days := map[string]counter{}    // ... per day
	cheapOn := map[string]counter{} // cheap role -> preferred provider (non-fallback decisions)
	for _, r := range recs {
		if r.Type != TypeDecision {
			continue
		}
		if r.Rule == "limit-fallback" || r.Rule == "quota-preempt" {
			from := c.from(r)
			away[from]++
			if days[from] == nil {
				days[from] = counter{}
			}
			days[from][DayStart(r.TS).Format("2006-01-02")]++
			continue
		}
		for _, c := range cheapRoles {
			if r.Role == c && !r.Fallback {
				if cheapOn[c] == nil {
					cheapOn[c] = counter{}
				}
				cheapOn[c][r.Provider]++
			}
		}
	}
	var out []Suggestion
	for _, from := range event.ProvidersOf(away) {
		// One limit hit moves every step until the reset: only a
		// provider that runs out on several days is short of quota.
		n := away[from]
		if n < minLimitSwitches || len(days[from]) < minLimitDays {
			continue
		}
		to := c.other(from)
		var cmds, moved []string
		for _, c := range cheapRoles {
			if cheapOn[c].top() == from {
				cmds = append(cmds, fmt.Sprintf("/prefer %s %s", c, to))
				moved = append(moved, c)
			}
		}
		if len(cmds) == 0 {
			continue
		}
		out = append(out, Suggestion{
			Severity: SevMedium,
			Title:    fmt.Sprintf("%s keeps running out of quota", from),
			Detail: fmt.Sprintf("%d steps on %d days were moved away from %s by limit-fallback/quota-preempt. Moving the cheap roles (%s) to %s saves %s's quota for planning and coding.",
				n, len(days[from]), from, strings.Join(moved, ", "), to, from),
			Commands: cmds,
		})
	}
	return out
}

// isCheapModel reports whether a model is already fast-tier, by name, since
// the log does not carry the config's tiers.
func isCheapModel(model string) bool {
	m := strings.ToLower(model)
	for _, w := range []string{"haiku", "luna", "mini", "nano", "flash"} {
		if strings.Contains(m, w) {
			return true
		}
	}
	return false
}

// cheaperReadOnly flags read-only roles that always succeed with little
// context on a strong model: a fast model would do the same for less.
func cheaperReadOnly(recs []Record) []Suggestion { return DefaultCatalog.cheaperReadOnly(recs) }

func (c Catalog) cheaperReadOnly(recs []Record) []Suggestion {
	type agg struct {
		role, prov, model string
		runs, fails       int
		tokens            int64
	}
	groups := map[string]*agg{}
	var keys []string
	for _, r := range recs {
		if r.Type != TypeAgentEnd || r.LimitHit || (r.Role != event.RoleExplorer && r.Role != event.RoleResearcher) {
			continue
		}
		k := r.Role + "|" + r.Provider + "|" + r.Model
		g := groups[k]
		if g == nil {
			g = &agg{role: r.Role, prov: r.Provider, model: r.Model}
			groups[k] = g
			keys = append(keys, k)
		}
		g.runs++
		if failed(r) {
			g.fails++
		}
		if r.Tokens != nil {
			g.tokens += r.Tokens.Total()
		}
	}
	sort.Strings(keys)
	var out []Suggestion
	for _, k := range keys {
		g := groups[k]
		cheap, ok := c.Cheap[g.prov]
		if g.runs < minRuns || g.fails > 0 || isCheapModel(g.model) || c.Fast[g.model] || !ok || strings.HasPrefix(cheap, g.model+":") || cheap == g.model {
			continue
		}
		avg := g.tokens / int64(g.runs)
		if avg >= cheapTokens {
			continue
		}
		out = append(out, Suggestion{
			Severity: SevInfo,
			Title:    fmt.Sprintf("%s could use a cheaper model than %s", g.role, g.model),
			Detail: fmt.Sprintf("%d %s runs on %s:%s, none failed, avg %s fresh tokens. A fast model is likely enough.",
				g.runs, g.role, g.prov, g.model, human(avg)),
			Commands: []string{fmt.Sprintf("/route %s %s:%s", g.role, g.prov, cheap)},
		})
	}
	return out
}

// judgeAdvice compares steps the judge routed with steps the default rule
// routed. It links each agent_end to the decision for the same step attempt.
func judgeAdvice(recs []Record) []Suggestion {
	key := func(r Record) string { return fmt.Sprintf("%s|%s|%s|%d", r.Session, r.TaskID, r.Step, r.Attempt) }
	dec := map[string]Record{}
	var jRuns, jFails, dRuns, dFails int
	var judgeTokens, workerTokens int64
	var judgeCalls, workerRuns int
	for _, r := range recs {
		switch r.Type {
		case TypeDecision:
			dec[key(r)] = r
		case TypeAgentEnd:
			if r.Tokens != nil {
				switch r.Role {
				case event.RoleJudge:
					judgeTokens += r.Tokens.Total()
					judgeCalls++
				case event.RoleWorker, event.RoleWorkerHigh:
					workerTokens += r.Tokens.Total()
					workerRuns++
				}
			}
			d, ok := dec[key(r)]
			if !ok || r.LimitHit {
				continue
			}
			switch {
			case d.Judged || d.Rule == "judge":
				jRuns++
				if failed(r) {
					jFails++
				}
			case d.Rule == "default" && d.Role == event.RoleWorker:
				dRuns++
				if failed(r) {
					dFails++
				}
			}
		}
	}
	dRate, jRate := pct(dFails, dRuns), pct(jFails, jRuns)
	// What the judge costs (its own calls) against what it saves: the
	// failures it avoided, each roughly one more worker run.
	cost := judgeTokens
	var saved int64
	if workerRuns > 0 && dRate > jRate {
		saved = int64((dRate - jRate) * float64(jRuns) * float64(workerTokens/int64(workerRuns)))
	}
	costLine := ""
	if judgeCalls > 0 {
		costLine = fmt.Sprintf(" The judge used %s fresh tokens in %d calls; the failures it avoided would have cost about %s.", human(cost), judgeCalls, human(saved))
	}
	switch {
	case jRuns == 0 && clearlyAbove(dFails, dRuns, failFloor):
		return []Suggestion{{
			Severity: SevMedium,
			Title:    "turn on the judge for unclear worker steps",
			Detail: fmt.Sprintf("%d of %d default-rule worker steps failed (%.0f%%) and the judge never ran. It can send hard steps to worker_high and simple ones to explorer.",
				dFails, dRuns, dRate*100),
			Commands: []string{"/judge on"},
		}}
	case jRuns >= minJudged && dRuns >= minRuns && jRate >= dRate:
		return []Suggestion{{
			Severity: SevInfo,
			Title:    "the judge does not improve routing",
			Detail: fmt.Sprintf("judged steps failed %d of %d (%.0f%%), default-rule worker steps %d of %d (%.0f%%). Turning it off saves a model call per unclear step.",
				jFails, jRuns, jRate*100, dFails, dRuns, dRate*100) + costLine,
			Commands: []string{"/judge off"},
		}}
	case jRuns >= minJudged && dRuns >= minRuns && cost > saved:
		return []Suggestion{{
			Severity: SevInfo,
			Title:    "the judge costs more than it saves",
			Detail: fmt.Sprintf("judged steps fail less (%.0f%% vs %.0f%%), but not by enough to pay for the judge.", jRate*100, dRate*100) + costLine +
				" Lower routing.judge_below_confidence so it runs less often, or turn it off.",
			Commands: []string{"/judge off"},
		}}
	case jRuns >= minJudged && dRuns >= minRuns:
		return []Suggestion{{
			Severity: SevInfo,
			Title:    "the judge pays off",
			Detail:   fmt.Sprintf("judged steps fail %.0f%% vs %.0f%% for default-rule steps.", jRate*100, dRate*100) + costLine + " Keep it on.",
		}}
	}
	return nil
}

// routedVsSingle compares whole-task success of routed runs against the
// single-agent baseline. Routing that loses to one agent is the first thing
// to fix.
func routedVsSingle(recs []Record) []Suggestion {
	mode := map[string]string{}
	tasks, ok := map[string]int{}, map[string]int{}
	for _, r := range recs {
		switch r.Type {
		case TypeTask:
			mode[r.Session+"/"+r.TaskID] = r.Mode
		case TypeTaskEnd:
			m := r.Mode
			if m == "" {
				m = mode[r.Session+"/"+r.TaskID]
			}
			tasks[m]++
			if !failed(r) {
				ok[m]++
			}
		}
	}
	rt, st := tasks["routed"], tasks["single"]
	if rt < minRuns || st < minRuns {
		return nil
	}
	rr, sr := pct(ok["routed"], rt), pct(ok["single"], st)
	// Clearly lower: even the top of routed's interval stays below the
	// baseline, so one failed run of five is not an alarm.
	if 1-wilsonLower(rt-ok["routed"], rt) >= sr {
		return nil
	}
	return []Suggestion{{
		Severity: SevHigh,
		Title:    "routed tasks succeed less often than the single-agent baseline",
		Detail: fmt.Sprintf("routed: %d of %d ok (%.0f%%), single: %d of %d ok (%.0f%%). Check the failing routes below, or compare with `sy stats`.",
			ok["routed"], rt, rr*100, ok["single"], st, sr*100),
	}}
}
