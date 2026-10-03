// Package router picks provider, model and effort for every step. Rules are
// evaluated top to bottom and the first match wins; an optional LLM judge
// handles steps no rule matches confidently.
package router

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
)

// Kind classifies a step.
type Kind string

const (
	KindPlan     Kind = "plan"
	KindExplore  Kind = "explore"
	KindResearch Kind = "research"
	KindEdit     Kind = "edit"
	KindFix      Kind = "fix"
	KindReview   Kind = "review"
	KindJudge    Kind = "judge"
)

// ReadOnly reports whether steps of this kind never write files.
func (k Kind) ReadOnly() bool {
	return k == KindPlan || k == KindExplore || k == KindResearch || k == KindReview || k == KindJudge
}

// Step is the router's view of one unit of work.
type Step struct {
	ID           string
	Title        string
	Kind         Kind
	Prompt       string
	Files        []string
	RepeatError  bool   // the same error was seen twice
	Escalations  int    // how many tiers to escalate (grows with each repeat)
	MainProvider string // provider the planner used, for prefer: other
	ForceRole    string // set by the judge
	UserRole     string // chosen by the person in plan approval
}

// State is what the router needs to know about providers.
type State interface {
	Limited(provider string) bool
	Share(provider string) float64
	// Utilization is the provider-reported share of its usage limit in use
	// (0..1); ok is false when the provider has not reported one.
	Utilization(provider string) (float64, bool)
}

// Rule names (also used in the session log and the stats).
const (
	RuleLimit       = "limit-fallback"
	RuleQuota       = "quota-preempt"
	RuleReview      = "review-checkpoint"
	RulePlan        = "plan"
	RuleReadOnly    = "read-only"
	RuleRepeatError = "error-repeats"
	RuleLargeDiff   = "large-or-sensitive"
	RuleDefault     = "default"
	RuleJudge       = "judge"
	RuleForced      = "forced"
)

// Router applies the rules to the live config.
type Router struct {
	Cfg   func() *config.Config
	State State
	// ForceProvider, when set, pins every role to one provider (sy --provider).
	ForceProvider string
}

var readOnlyWords = regexp.MustCompile(`(?i)\b(where is|find|search|explain|summari[sz]e|describe|list|what does|how does|look up|read|overview|document how)\b`)
var writeWords = regexp.MustCompile(`(?i)\b(add|fix|implement|change|refactor|rename|write|create|delete|remove|update|edit|migrate|build)\b`)

// tiers is the escalation ladder used by the "error repeats" rule.
var tiers = []string{event.RoleExplorer, event.RoleWorker, event.RoleWorkerHigh, event.RolePlanner}

func escalate(role string, n int) string {
	for i, r := range tiers {
		if r == role {
			j := i + n
			if j >= len(tiers) {
				j = len(tiers) - 1
			}
			return tiers[j]
		}
	}
	return event.RoleWorkerHigh
}

// Route decides how to run a step.
func (r *Router) Route(s Step) event.Decision {
	cfg := r.Cfg()
	role, rule, reason, conf := r.classify(cfg, s)
	d := r.resolve(cfg, s, role)
	d.StepID, d.StepTitle, d.Confidence = s.ID, s.Title, conf
	if d.Rule == RuleQuota {
		d.Reason = fmt.Sprintf("%s (%s: %s)", d.Reason, rule, reason)
		d.Confidence = 1
		return d
	}
	d.Rule, d.Reason = rule, reason
	return Finalize(learned(cfg, d))
}

// learned notes in the reason when the route came from the role's learned
// route (config/learned.go), so logs and reports can be traced back to
// `sy tune --learned`. A fallback to the other provider is not learned.
func learned(cfg *config.Config, d event.Decision) event.Decision {
	lr, ok := cfg.Learned[d.Role]
	if !ok || d.Fallback || d.Provider != lr.Provider || d.Model != lr.Model || d.Effort != lr.Effort {
		return d
	}
	d.Reason += "; learned route " + lr.Spec() + ": " + lr.Why
	return d
}

// classify runs rules 2-6 (rule 1, limits, is applied in resolve because it
// only changes the provider, not the role).
func (r *Router) classify(cfg *config.Config, s Step) (role, rule, reason string, conf float64) {
	if s.UserRole != "" {
		return s.UserRole, RuleForced, "role chosen in plan approval", 1
	}
	if s.ForceRole != "" {
		return s.ForceRole, RuleJudge, "judge picked " + s.ForceRole, 0.75
	}
	switch s.Kind {
	case KindReview:
		return event.RoleReviewer, RuleReview, "review checkpoint", 1
	case KindJudge:
		return event.RoleJudge, RuleForced, "judge call", 1
	case KindPlan:
		return event.RolePlanner, RulePlan, "planning step", 1
	}
	base := event.RoleWorker
	if s.Kind == KindExplore || s.Kind == KindResearch || (s.Kind != KindFix && s.Kind != KindEdit && looksReadOnly(s.Prompt)) {
		if s.Kind == KindResearch {
			base = event.RoleResearcher
		} else {
			base = event.RoleExplorer
		}
		if !s.RepeatError {
			return base, RuleReadOnly, "read-only " + string(s.Kind) + " step", 0.9
		}
	}
	if s.RepeatError {
		n := s.Escalations
		if n < 1 {
			n = 1
		}
		to := escalate(base, n)
		return to, RuleRepeatError, fmt.Sprintf("same error twice: %s -> %s", base, to), 0.85
	}
	if n := len(s.Files); n > cfg.Routing.MaxFilesBeforeHigh && cfg.Routing.MaxFilesBeforeHigh > 0 {
		return event.RoleWorkerHigh, RuleLargeDiff, fmt.Sprintf("touches %d files (> %d)", n, cfg.Routing.MaxFilesBeforeHigh), 0.8
	}
	if p := sensitive(cfg.Routing.SensitivePaths, s.Files, s.Title+" "+s.Prompt); p != "" {
		return event.RoleWorkerHigh, RuleLargeDiff, "sensitive: " + p, 0.8
	}
	// A clear "add/fix/implement..." instruction is a confident worker step;
	// anything vaguer is where the optional judge helps.
	conf = 0.6
	if writeWords.MatchString(s.Title + " " + s.Prompt) {
		conf = 0.7
	}
	return event.RoleWorker, RuleDefault, "default worker route", conf
}

// resolve turns a role into provider/model/effort, applying prefer and rule 1.
func (r *Router) resolve(cfg *config.Config, s Step, role string) event.Decision {
	rc := cfg.Roles[role]
	pref := r.preferred(cfg, s, rc)
	d := event.Decision{Role: role, Provider: pref}
	usable := func(p string) bool {
		pc, ok := cfg.Providers[p]
		return ok && !pc.Disabled && rc.For(p).Model != ""
	}
	if !usable(pref) && usable(event.Other(pref)) {
		pref = event.Other(pref)
		d.Provider = pref
	}
	other := event.Other(pref)
	if r.State != nil && r.State.Limited(pref) && usable(other) && !r.State.Limited(other) {
		d.Provider = other
		d.Fallback = true
	} else if thr := cfg.Routing.SwitchAtUtilization; thr > 0 && r.State != nil && r.ForceProvider == "" &&
		usable(other) && !r.State.Limited(other) {
		// Switch before the limit hits, not after: once a provider reports
		// it is nearly out, send work to the other one if that has more room.
		if u, ok := r.State.Utilization(pref); ok && u >= thr {
			if ou, ok2 := r.State.Utilization(other); !ok2 || ou < u {
				d.Provider = other
				d.Fallback = true
				d.Rule = RuleQuota
				d.Reason = fmt.Sprintf("%s at %.0f%% of its limit (>= %.0f%%) -> %s", pref, u*100, thr*100, other)
			}
		}
	}
	route := rc.For(d.Provider)
	d.Model, d.Effort = route.Model, route.Effort
	return d
}

func (r *Router) preferred(cfg *config.Config, s Step, rc config.RoleCfg) string {
	if r.ForceProvider != "" {
		return r.ForceProvider
	}
	switch rc.Prefer {
	case config.PreferCodex, config.PreferClaude:
		return rc.Prefer
	case config.PreferOther:
		main := s.MainProvider
		if main == "" {
			// Before the planner ran: assume the planner's fixed provider,
			// else Codex. Never recurse (planner may itself be "other").
			main = event.Codex
			if p := cfg.Roles[event.RolePlanner].Prefer; p == config.PreferCodex || p == config.PreferClaude {
				main = p
			}
		}
		return event.Other(main)
	case config.PreferAuto:
		if r.State != nil && r.State.Share(event.Claude) < r.State.Share(event.Codex) {
			return event.Claude
		}
		if r.State != nil && r.State.Share(event.Codex) < r.State.Share(event.Claude) {
			return event.Codex
		}
		return event.Codex
	}
	return event.Codex
}

// Finalize applies rule 1 bookkeeping: when the decision fell back because
// of a limit, the rule shown is the limit rule (the role is kept).
func Finalize(d event.Decision) event.Decision {
	if d.Fallback && d.Rule != RuleQuota {
		d.Reason = fmt.Sprintf("%s at limit -> %s (%s: %s)", event.Other(d.Provider), d.Provider, d.Rule, d.Reason)
		d.Rule = RuleLimit
		d.Confidence = 1
	}
	return d
}

// NeedsJudge reports whether the judge should be consulted for a decision.
func (r *Router) NeedsJudge(d event.Decision) bool {
	cfg := r.Cfg()
	return cfg.Routing.Judge && d.Rule == RuleDefault && d.Confidence < cfg.Routing.JudgeBelowConfidence
}

// JudgePrompt is the closed question sent to the judge model.
func JudgePrompt(s Step) string {
	return "[SY:JUDGE] You route coding work. Pick one route for this step:\n" +
		"A) explorer (read-only, cheap)\nB) worker\nC) worker-high (hard or risky change)\nD) planner (needs re-planning)\n\n" +
		"Step: " + s.Title + "\n" + truncate(s.Prompt, 1500) + "\n\nReply with the letter only."
}

// ParseJudge maps the judge's reply to a role; ok is false if unreadable.
func ParseJudge(reply string) (string, bool) {
	reply = strings.TrimSpace(strings.Trim(reply, "`*. \n"))
	if reply == "" {
		return "", false
	}
	switch strings.ToUpper(reply[:1]) {
	case "A":
		return event.RoleExplorer, true
	case "B":
		return event.RoleWorker, true
	case "C":
		return event.RoleWorkerHigh, true
	case "D":
		return event.RolePlanner, true
	}
	return "", false
}

func looksReadOnly(prompt string) bool {
	return readOnlyWords.MatchString(prompt) && !writeWords.MatchString(prompt)
}

func sensitive(words, files []string, text string) string {
	hay := strings.ToLower(strings.Join(files, " "))
	for _, w := range words {
		w = strings.ToLower(w)
		if w == "" {
			continue
		}
		if strings.Contains(hay, w) {
			return w
		}
	}
	// Titles only count with a word boundary, so "author" does not hit "auth".
	for _, w := range words {
		if w == "" {
			continue
		}
		if regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(w) + `\b`).MatchString(text) {
			return w
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Preview resolves a role to the route it would use right now, for the model
// picker. mainProvider may be empty.
func (r *Router) Preview(role, mainProvider string) event.Decision {
	cfg := r.Cfg()
	d := r.resolve(cfg, Step{MainProvider: mainProvider}, role)
	d.Rule = "preview"
	return Finalize(d)
}
