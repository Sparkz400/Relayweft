package orchestrator

import (
	"context"
	"fmt"
	"math"
	"sync"

	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// admitAgent atomically accounts for in-flight estimates. If another agent may
// free enough room, wait for its accounting before deciding. Optional best-of
// candidates can be declined without cancelling work already paid for.
// The completion function converts the reservation into actual usage under the
// same lock, and must be called even on errors. It also handles legacy budgets.
func (o *Orchestrator) admitAgent(ctx context.Context, t *task, step router.Step, d event.Decision, agent string) (func(event.TokenUsage), bool) {
	b := t.budget
	if b == nil || !o.budgetLimits(t).Reserve || !o.budgetLimits(t).Any() {
		if !o.checkBudget(ctx, t, "start "+agent+" ("+step.Title+")") {
			return nil, false
		}
		var once sync.Once
		return func(u event.TokenUsage) { once.Do(func() { t.addTokens(d.Provider, u) }) }, true
	}
	est := b.history.Estimate(d.Role, string(step.Kind), sessionlog.RouteKey{Provider: d.Provider, Model: d.Model, Effort: d.Effort}, step.Kind.ReadOnly())
	reservation := event.TokenUsage{Input: int64(math.Ceil(est.Tokens.Mid)), CostUSD: est.USD.Mid}
	finishing := step.Kind == router.KindReview || step.Kind == router.KindFix || step.ID == "single" || step.ID == "followup"
	waited := false
	for {
		if ctx.Err() != nil {
			return nil, false
		}
		cfg := o.budgetLimits(t)
		b.mu.Lock()
		if b.stopped != "" {
			b.mu.Unlock()
			return nil, false
		}
		var why string
		for _, l := range o.budgetUse(t, cfg, true) {
			if l.max <= 0 || b.allowed[l.name] {
				continue
			}
			usd := (BudgetRequest{Limit: l.name}).USD()
			pending, ask := float64(b.reserved.Total()+b.unknown.Total()), float64(reservation.Total())
			if usd {
				pending, ask = b.reserved.CostUSD+b.unknown.CostUSD, reservation.CostUSD
			}
			finish := 0.0
			if !finishing && t.cfg.Orchestrator.ReviewBeforeDone {
				finish = l.max * 0.2
			}
			if l.used >= l.max || l.used+pending+ask+finish > l.max {
				why = fmt.Sprintf("%s: reported %.2f + running/unknown %.2f + next estimate %.2f + review/fix reserve %.2f exceeds %.2f", l.name, l.used, pending, ask, finish, l.max)
				break
			}
		}
		if why == "" {
			b.active++
			b.reserved = b.reserved.Add(reservation)
			held := b.reserved.Add(b.unknown)
			b.view.Store(&held)
			b.mu.Unlock()
			o.logf("budget: reserved %s fresh tokens and $%.2f API-equivalent for %s (%s)", event.HumanTokens(reservation.Total()), reservation.CostUSD, agent, est.Source)
			var once sync.Once
			return func(u event.TokenUsage) {
				once.Do(func() {
					b.mu.Lock()
					t.addTokens(d.Provider, u)
					b.active--
					b.reserved.Input -= reservation.Input
					b.reserved.CostUSD = max(0, b.reserved.CostUSD-reservation.CostUSD)
					if u.Incomplete {
						b.unknown.Input += max(0, reservation.Total()-u.Total())
						b.unknown.CostUSD += max(0, reservation.CostUSD-u.CostUSD)
					}
					held := b.reserved.Add(b.unknown)
					b.view.Store(&held)
					close(b.changed)
					b.changed = make(chan struct{})
					b.mu.Unlock()
				})
			}, true
		}
		if b.active > 0 {
			changed := b.changed
			b.mu.Unlock()
			if !waited {
				o.logf("budget: holding %s until running agents finish accounting: %s", agent, why)
				waited = true
			}
			select {
			case <-changed:
			case <-ctx.Done():
				return nil, false
			}
			continue
		}
		if step.Pin != nil {
			b.mu.Unlock()
			o.logf("budget: skipping best-of candidate %s: %s", agent, why)
			return nil, false
		}
		b.stopped = "reservation cannot fit " + agent + ": " + why
		b.cancel()
		b.mu.Unlock()
		o.logf("budget: %s; no further agent starts", why)
		return nil, false
	}
}
