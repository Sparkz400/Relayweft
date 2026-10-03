package sessionlog

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/sparkz400/switchyard/internal/canon"
	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
)

// Learned routes (config/learned.go) come from these rules. They are
// conservative on purpose: a route changes only on clear evidence from
// this repo, and one bad afternoon must not rewrite the routing.
//
//   - Only this repo's runs count (its session logs, bench runs included),
//     and a run's weight halves every learnHalfLife, so old habits fade.
//   - A route needs routing.learn_min_samples runs (after the decay), and so
//     does the route it would replace: without both, nothing changes.
//   - It must beat the current route clearly: succeed learnRateMargin more
//     often, or succeed as often (within learnRateSlack, and at least
//     learnMinRate) with learnCostMargin fewer tokens.
//   - Only routes that are configured and usable are picked; a learned
//     route that no longer is goes back to the configured one.
//   - Each update changes at most one route per role.
//
// A run succeeds when its agent finished ok; a writing step of a bench run
// whose check failed counts as failed, whatever the agent said.

// Learning thresholds.
const (
	learnHalfLife   = 30 * 24 * time.Hour
	learnRateMargin = 0.15 // success rate a route must beat the current one by
	learnRateSlack  = 0.02 // "as successful" for the cheaper rule
	learnMinRate    = 0.80 // a cheaper route must still succeed this often
	learnCostMargin = 0.40 // share of tokens a cheaper route must save
)

// LearnRoles are the roles routes are learned for. The reviewer runs on
// the other provider on purpose and the judge is one tiny call: both keep
// their configured routes.
var LearnRoles = []string{event.RolePlanner, event.RoleWorker, event.RoleWorkerHigh, event.RoleExplorer, event.RoleResearcher}

// RouteKey is a provider:model:effort route.
type RouteKey struct {
	Provider, Model, Effort string
}

func (k RouteKey) String() string { return routeSpec(k.Provider, k.Model, k.Effort) }

// Route is the key as a config route.
func (k RouteKey) Route() config.Route { return config.Route{Model: k.Model, Effort: k.Effort} }

// LearnInput is what an update starts from.
type LearnInput struct {
	Root string // the repo root: only records from inside it count
	// Configured is each role's route without learned routes (your config
	// and the repo file), Learned the learned routes so far.
	Configured map[string]RouteKey
	Learned    map[string]config.LearnedRoute
	Available  func(RouteKey) bool // configured and usable now
	MinSamples int                 // 0 = config.DefaultLearnMinSamples
	Now        time.Time
}

// RouteChange is one role's change in an update.
type RouteChange struct {
	Role     string
	From, To RouteKey
	Remove   bool   // back to the configured route: the learned one is dropped
	Why      string // one line with the numbers
	Evidence []config.RouteEvidence
}

// LearnResult is the outcome of an update.
type LearnResult struct {
	Routes  map[string]config.LearnedRoute // the new learned routes
	Changes []RouteChange
	// Evidence lists every route of a role with runs here, the most
	// reliable first (for `sy tune --learned` and the dry run).
	Evidence map[string][]config.RouteEvidence
}

// routeAgg accumulates one role's runs on one route.
type routeAgg struct {
	key       RouteKey
	n         int
	w, okW    float64 // decayed runs and successes
	tokW, msW float64 // decayed sums
}

func (a *routeAgg) rate() float64 {
	if a.w == 0 {
		return 0
	}
	return a.okW / a.w
}

func (a *routeAgg) tokens() float64 {
	if a.w == 0 {
		return 0
	}
	return a.tokW / a.w
}

func (a *routeAgg) evidence() config.RouteEvidence {
	ms := 0.0
	if a.w > 0 {
		ms = a.msW / a.w
	}
	return config.RouteEvidence{Route: a.key.String(), Samples: a.n, Weight: round2(a.w), Success: round2(a.rate()),
		Tokens: int64(a.tokens()), WallMS: int64(ms)}
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// decay is a run's weight: 1 now, halving every learnHalfLife.
func decay(now, ts time.Time) float64 {
	age := now.Sub(ts)
	if age <= 0 {
		return 1
	}
	return math.Pow(0.5, float64(age)/float64(learnHalfLife))
}

// routedStep reports whether an agent run is a routed step: single-agent
// baselines and follow-ups are whole conversations, not steps.
func routedStep(r Record) bool {
	return r.Type == TypeAgentEnd && !r.LimitHit && r.Role != "" && r.Model != "" && r.Step != "single" && r.Step != "followup"
}

// repoFilter reports whether a record was written in the repo at root
// (never, for root ""). Answers are cached per folder: canonicalizing a
// path touches the disk, and logs repeat the same few folders.
func repoFilter(root string) func(Record) bool {
	seen := map[string]bool{}
	return func(r Record) bool {
		if root == "" || r.Cwd == "" {
			return false
		}
		in, ok := seen[r.Cwd]
		if !ok {
			in = canon.Within(root, r.Cwd)
			seen[r.Cwd] = in
		}
		return in
	}
}

// benchFailed returns the tasks (session/task id) of bench runs whose check
// failed. A bench record follows its task's task_end in the same session.
func benchFailed(recs []Record) map[string]bool {
	last := map[string]string{} // session|bench name -> task id
	out := map[string]bool{}
	for _, r := range recs {
		switch {
		case r.Type == TypeTaskEnd && r.Bench != "":
			last[r.Session+"|"+r.Bench] = r.TaskID
		case r.Type == "bench" && r.Passed != nil && !*r.Passed:
			if id, ok := last[r.Session+"|"+r.Bench]; ok {
				out[r.Session+"/"+id] = true
			}
		}
	}
	return out
}

// learnStats aggregates this repo's routed runs per role and route.
func learnStats(recs []Record, root string, now time.Time) map[string]map[RouteKey]*routeAgg {
	failedBench := benchFailed(recs)
	roles := map[string]bool{}
	for _, r := range LearnRoles {
		roles[r] = true
	}
	inRepo := repoFilter(root)
	out := map[string]map[RouteKey]*routeAgg{}
	for _, r := range recs {
		if !routedStep(r) || !roles[r.Role] || !inRepo(r) {
			continue
		}
		k := RouteKey{r.Provider, r.Model, r.Effort}
		if out[r.Role] == nil {
			out[r.Role] = map[RouteKey]*routeAgg{}
		}
		a := out[r.Role][k]
		if a == nil {
			a = &routeAgg{key: k}
			out[r.Role][k] = a
		}
		w := decay(now, r.TS)
		ok := !failed(r)
		if (r.Role == event.RoleWorker || r.Role == event.RoleWorkerHigh) && failedBench[r.Session+"/"+r.TaskID] {
			ok = false
		}
		a.n++
		a.w += w
		if ok {
			a.okW += w
		}
		if r.Tokens != nil {
			a.tokW += w * float64(r.Tokens.Total())
		}
		a.msW += w * float64(r.DurationMS)
	}
	return out
}

// Learn works out the learned routes from the records.
func Learn(recs []Record, in LearnInput) LearnResult {
	minW := float64(in.MinSamples)
	if minW <= 0 {
		minW = config.DefaultLearnMinSamples
	}
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	avail := in.Available
	if avail == nil {
		avail = func(RouteKey) bool { return true }
	}
	stats := learnStats(recs, in.Root, in.Now)
	res := LearnResult{Routes: map[string]config.LearnedRoute{}, Evidence: map[string][]config.RouteEvidence{}}
	for role, lr := range in.Learned {
		res.Routes[role] = lr
	}
	for _, role := range LearnRoles {
		var aggs []*routeAgg
		for _, a := range stats[role] {
			aggs = append(aggs, a)
		}
		sort.Slice(aggs, func(i, j int) bool { return better(aggs[i], aggs[j]) })
		for _, a := range aggs {
			res.Evidence[role] = append(res.Evidence[role], a.evidence())
		}
		conf, hasConf := in.Configured[role]
		lr, hasLearned := in.Learned[role]
		cur := conf
		if hasLearned {
			cur = RouteKey{lr.Provider, lr.Model, lr.Effort}
			if !avail(cur) {
				// Gone from the config (or its provider disabled): back
				// to the configured route, whatever the numbers say.
				delete(res.Routes, role)
				res.Changes = append(res.Changes, RouteChange{Role: role, From: cur, To: conf, Remove: true,
					Why: cur.String() + " is no longer configured or usable"})
				continue
			}
		} else if !hasConf {
			continue
		}
		ca := stats[role][cur]
		if ca == nil || ca.w < minW {
			continue // nothing to compare with
		}
		var pick *routeAgg
		why := ""
		for _, a := range aggs {
			if a.key == cur || a.w < minW || !avail(a.key) {
				continue
			}
			if w := beats(a, ca); w != "" {
				pick, why = a, w
				break // aggs is sorted: the first qualifying one is the best
			}
		}
		if pick == nil {
			continue
		}
		ch := RouteChange{Role: role, From: cur, To: pick.key, Why: why, Evidence: []config.RouteEvidence{pick.evidence(), ca.evidence()}}
		if hasLearned && hasConf && pick.key == conf {
			ch.Remove = true
			delete(res.Routes, role)
		} else {
			res.Routes[role] = config.LearnedRoute{Provider: pick.key.Provider, Model: pick.key.Model, Effort: pick.key.Effort,
				Why: why, Since: in.Now, Evidence: ch.Evidence}
		}
		res.Changes = append(res.Changes, ch)
	}
	return res
}

// better orders routes: more reliable first, then fewer tokens, then by
// name so the order is stable.
func better(a, b *routeAgg) bool {
	if ra, rb := a.rate(), b.rate(); ra != rb {
		return ra > rb
	}
	if ta, tb := a.tokens(), b.tokens(); ta != tb {
		return ta < tb
	}
	return a.key.String() < b.key.String()
}

// beats reports why route a clearly beats the current route c ("" if it
// does not).
func beats(a, c *routeAgg) string {
	ra, rc := a.rate(), c.rate()
	if ra >= rc+learnRateMargin {
		return fmt.Sprintf("succeeded %.0f%% of %d runs vs %.0f%% of %d on %s", ra*100, a.n, rc*100, c.n, c.key)
	}
	ta, tc := a.tokens(), c.tokens()
	if ra >= rc-learnRateSlack && ra >= learnMinRate && tc > 0 && ta <= tc*(1-learnCostMargin) {
		return fmt.Sprintf("as reliable (%.0f%% of %d runs vs %.0f%% of %d) with %.0f%% fewer tokens than %s (%s vs %s per run)",
			ra*100, a.n, rc*100, c.n, (1-ta/tc)*100, c.key, human(int64(ta)), human(int64(tc)))
	}
	return ""
}
