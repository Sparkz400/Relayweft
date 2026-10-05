package sessionlog

import (
	"fmt"
	"sort"
)

// Best-of thresholds for `sy tune`.
const (
	bestOfOwnWins   = 0.90 // the step's own route wins this often: the extra runs buy nothing
	bestOfOtherWins = 0.60 // another route beats the own route this often: make it the role's route
)

// BestOfRoute is one route's record as a best-of candidate.
type BestOfRoute struct {
	Role, Route string
	Own         bool // it was the step's own route (not an extra candidate)
	Runs, Wins  int
	Merit       int // wins on checks or by the reviewer
}

// BestOfStats sums up the best-of records: per role, the best-of steps and
// how often the step's own route won, and per route its runs and wins.
func BestOfStats(recs []Record) (steps, ownWins map[string]int, routes []BestOfRoute) {
	type stepKey struct{ session, task, step string }
	steps, ownWins = map[string]int{}, map[string]int{}
	seen := map[stepKey]bool{}
	agg := map[string]*BestOfRoute{}
	for _, r := range recs {
		if r.Type != TypeBestOf || r.Role == "" {
			continue
		}
		own := r.Rule != "best-of"
		won := r.OK != nil && *r.OK
		if k := (stepKey{r.Session, r.TaskID, r.Step}); !seen[k] && r.Reason != BestOfNone {
			seen[k] = true
			steps[r.Role]++
		}
		if own && won {
			ownWins[r.Role]++
		}
		key := r.Role + "|" + routeSpec(r.Provider, r.Model, r.Effort)
		a := agg[key]
		if a == nil {
			a = &BestOfRoute{Role: r.Role, Route: routeSpec(r.Provider, r.Model, r.Effort)}
			agg[key] = a
		}
		a.Own = a.Own || own
		a.Runs++
		if won {
			a.Wins++
			if r.Reason == BestOfByChecks || r.Reason == BestOfByReviewer {
				a.Merit++
			}
		}
	}
	for _, a := range agg {
		routes = append(routes, *a)
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Role != routes[j].Role {
			return routes[i].Role < routes[j].Role
		}
		return routes[i].Route < routes[j].Route
	})
	return steps, ownWins, routes
}

// bestOfAdvice flags best of N that buys nothing (the step's own route
// nearly always wins) and a route that keeps beating the role's route.
func bestOfAdvice(recs []Record) []Suggestion {
	steps, ownWins, routes := BestOfStats(recs)
	var out []Suggestion
	var roles []string
	for role := range steps {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		n, own := steps[role], ownWins[role]
		if n >= minRuns && pct(own, n) >= bestOfOwnWins {
			out = append(out, Suggestion{
				Severity: SevInfo,
				Title:    fmt.Sprintf("best of N rarely changes the result for %s steps", role),
				Detail: fmt.Sprintf("The step's own route won %d of %d best-of steps (%.0f%%). Each costs about N times a single run: set routing.best_of.when to hard (or off) in switchyard.yaml.",
					own, n, pct(own, n)*100),
			})
		}
	}
	for _, r := range routes {
		if r.Own || r.Runs < minRuns || pct(r.Merit, r.Runs) < bestOfOtherWins {
			continue
		}
		out = append(out, Suggestion{
			Severity: SevMedium,
			Title:    fmt.Sprintf("%s beats the %s route in best of N", r.Route, r.Role),
			Detail: fmt.Sprintf("As an extra candidate it won %d of %d best-of steps on checks or by the reviewer (%.0f%%). Make it the %s route.",
				r.Merit, r.Runs, pct(r.Merit, r.Runs)*100, r.Role),
			Commands: []string{fmt.Sprintf("/route %s %s", r.Role, r.Route)},
		})
	}
	return out
}
