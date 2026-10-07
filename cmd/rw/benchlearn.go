package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// Bench results feed the learned routes (config/learned.go). A bench run
// logs every agent step with its role and route, and a failed check marks
// the run's writing steps as failed (sessionlog/learn.go). Two things turn
// that into evidence the learner can act on:
//
//   - Route variants: the mode "routed:worker=claude:sonnet:medium" runs
//     the routed pipeline with that role on another route, so there is an
//     alternative to compare the current route with. Single-agent runs are
//     whole conversations, not steps, and do not count.
//   - learn: true in the bench file (rw bench --from-history writes it), or
//     --learn, updates this repo's learned routes when the bench ends, like
//     `rw tune --apply`: the next tasks use whatever clearly won.

// benchMode is one mode of a bench file.
type benchMode struct {
	name, provider string       // provider set: a single-agent run
	route          config.Route // the single agent's route
	noHandoff      bool
	bestOf         bool // every writing step runs as best of N (routing.best_of.when: always)
	tiers          bool
	// classic plans every task with full-strength planner and reviewer
	// routes, as before auto_single, light_planning, review skipping and
	// budget fitting: to measure them.
	classic bool
	// reqTests turns on orchestrator.independent_tests (routed-tests).
	reqTests bool
	// review is the final review in place of the independent tests, the
	// default before tests replaced it (review_when: failing,
	// independent_tests off; routed-review): to measure the swap.
	review bool
	// testsFirst turns on orchestrator.tests_first (routed-tests-first).
	testsFirst bool
	routes     []roleRoute // route variant: these roles on these routes
}

// roleRoute is one role's route in a route variant.
type roleRoute struct {
	role, provider string
	route          config.Route
}

func (r roleRoute) spec() string { return r.role + "=" + config.RouteSpec(r.provider, r.route) }

// parseBenchMode reads routed, routed-classic, routed-review, routed-tests, routed-tests-first, routed-nohandoff, routed-bestof,
// routed:<role>=<route>[,...]
// and single:<provider>:<model>[:effort].
func parseBenchMode(m string) (benchMode, error) {
	const want = "want routed, routed-classic, routed-review, routed-tests, routed-tests-first, routed-nohandoff, routed-tiers, routed-bestof, routed:<role>=<provider>:<model>[:effort] or single:<provider>:<model>[:effort]"
	if m == "routed" || m == "routed-classic" || m == "routed-review" || m == "routed-tests" || m == "routed-tests-first" || m == "routed-nohandoff" || m == "routed-bestof" || m == "routed-tiers" {
		return benchMode{name: m, noHandoff: m == "routed-nohandoff", bestOf: m == "routed-bestof", tiers: m == "routed-tiers", classic: m == "routed-classic",
			review: m == "routed-review", reqTests: m == "routed-tests", testsFirst: m == "routed-tests-first"}, nil
	}
	if spec, ok := strings.CutPrefix(m, "routed:"); ok {
		bm := benchMode{name: m}
		seen := map[string]bool{}
		for _, part := range strings.Split(spec, ",") {
			role, rs, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok || role == "" {
				return benchMode{}, fmt.Errorf("mode %q: %s", m, want)
			}
			if !knownRole(role) {
				return benchMode{}, fmt.Errorf("mode %q: unknown role %q (roles: %s)", m, role, strings.Join(event.Roles, ", "))
			}
			if seen[role] {
				return benchMode{}, fmt.Errorf("mode %q: %s is set twice", m, role)
			}
			seen[role] = true
			prov, route, err := config.ParseRouteSpec(rs)
			if err != nil {
				return benchMode{}, fmt.Errorf("mode %q: %w", m, err)
			}
			bm.routes = append(bm.routes, roleRoute{role: role, provider: prov, route: route})
		}
		return bm, nil
	}
	spec, ok := strings.CutPrefix(m, "single:")
	if !ok {
		return benchMode{}, fmt.Errorf("mode %q: %s", m, want)
	}
	prov, route, err := config.ParseRouteSpec(spec)
	if err != nil {
		return benchMode{}, fmt.Errorf("mode %q: %w", m, err)
	}
	return benchMode{name: m, provider: prov, route: route}, nil
}

func knownRole(role string) bool {
	for _, r := range event.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// store returns the config a run of this mode uses: base itself, or a copy
// without the context hand-off, with best of N for every writing step, or
// with the variant's routes (set the way --route sets them, so a learned
// route of that role does not apply).
func (m benchMode) store(base *config.Store) (*config.Store, error) {
	if !m.noHandoff && !m.bestOf && !m.tiers && !m.classic && !m.review && !m.reqTests && !m.testsFirst && len(m.routes) == 0 {
		return base, nil
	}
	cfg := base.Get()
	if m.classic {
		cfg.Orchestrator.Classic()
	}
	if m.noHandoff {
		cfg.Orchestrator.Handoff = false
	}
	if m.review {
		cfg.Orchestrator.ReviewBeforeDone = true
		cfg.Orchestrator.ReviewWhen, cfg.Orchestrator.IndependentTests = config.ReviewFailing, false
	}
	if m.reqTests {
		cfg.Orchestrator.IndependentTests = true
	}
	if m.bestOf {
		cfg.Routing.BestOf.When = config.BestOfAlways
	}
	if m.tiers {
		cfg.Routing.Tiers = config.TiersAuto
	}
	if m.testsFirst {
		cfg.Orchestrator.TestsFirst = true
	}
	st := config.NewStore(cfg, base.Path())
	for _, r := range m.routes {
		if err := st.SetRoute(r.role, r.provider, r.route); err != nil {
			return nil, fmt.Errorf("mode %q: %w", m.name, err)
		}
		if err := st.SetPrefer(r.role, r.provider); err != nil {
			return nil, fmt.Errorf("mode %q: %w", m.name, err)
		}
	}
	return st, nil
}

// unlearnable lists the variant routes the learner will never pick: their
// provider is off or the model is not in its catalog (or used by a role).
func unlearnable(cfg *config.Config, modes []benchMode) []string {
	var out []string
	for _, m := range modes {
		for _, r := range m.routes {
			if !cfg.RouteAvailable(r.provider, r.route) {
				out = append(out, r.spec())
			}
		}
	}
	return out
}

// historyVariants are the route variants `rw bench --from-history` adds:
// the worker (the role a failed check counts against) on each other
// provider's configured route, when it is usable.
func historyVariants(cfg *config.Config) []string {
	cur, ok := orchestrator.ConfiguredRoutes(cfg)[event.RoleWorker]
	if !ok {
		return nil
	}
	rc := cfg.Roles[event.RoleWorker]
	var out []string
	for _, p := range []string{event.Codex, event.Claude} {
		r := rc.For(p)
		if p == cur.Provider || !cfg.RouteAvailable(p, r) {
			continue
		}
		out = append(out, "routed:"+roleRoute{role: event.RoleWorker, provider: p, route: r}.spec())
	}
	return out
}

// benchLearn updates the learned routes of dir's repo from the session logs,
// this bench's runs included, and prints what changed and why.
func benchLearn(w io.Writer, store *config.Store, dir string) error {
	cfg := store.Get()
	if cfg.LearnMode() == config.LearnOff {
		fmt.Fprintln(w, "learned routes: not updated, routing.learn is off")
		return nil
	}
	recs, err := sessionlog.ReadDir(cfg.SessionDir())
	if err != nil {
		return err
	}
	rep, err := orchestrator.UpdateLearned(dir, store.Unlearned(), recs, time.Now(), false)
	if err != nil {
		return err
	}
	printLearnDiff(w, rep, cfg.LearnMode(), true)
	return nil
}
