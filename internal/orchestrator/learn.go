package orchestrator

import (
	"fmt"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/router"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// Learned routes (config/learned.go, sessionlog/learn.go): this file finds
// the repo, runs an update and layers the result over the live config.

// learnEvery is how often routing.learn: auto refreshes the learned routes
// (at the start of a task).
const learnEvery = 24 * time.Hour

// LearnReport is the outcome of a learned-routes update.
type LearnReport struct {
	Root    string                 // the repo root
	Before  *config.Learned        // the learned routes before
	Learned *config.Learned        // after (saved unless it was a dry run)
	Result  sessionlog.LearnResult // changes and evidence
}

// LearnedRoot returns the git repo root learned routes of dir are kept for.
func LearnedRoot(dir string) (string, error) {
	if !isRepo(dir) {
		return "", fmt.Errorf("%s is not in a git repository: learned routes are kept per repo", dir)
	}
	return repoRoot(dir)
}

// ConfiguredRoutes resolves each learnable role to the route it uses in
// cfg (pass a config without learned routes).
func ConfiguredRoutes(cfg *config.Config) map[string]sessionlog.RouteKey {
	r := &router.Router{Cfg: func() *config.Config { return cfg }}
	out := map[string]sessionlog.RouteKey{}
	for _, role := range sessionlog.LearnRoles {
		if _, ok := cfg.Roles[role]; !ok {
			continue
		}
		d := r.Preview(role, "")
		out[role] = sessionlog.RouteKey{Provider: d.Provider, Model: d.Model, Effort: d.Effort}
	}
	return out
}

// UpdateLearned recomputes the learned routes of the repo containing dir
// from the session records. cfg is the config without learned routes (your
// config and the repo file): what they are compared with, and what is
// available. Unless dry, the result is saved (also when nothing changed:
// that is the daily refresh).
func UpdateLearned(dir string, cfg *config.Config, recs []sessionlog.Record, now time.Time, dry bool) (LearnReport, error) {
	root, err := LearnedRoot(dir)
	if err != nil {
		return LearnReport{}, err
	}
	before, err := config.LoadLearned(root)
	if err != nil {
		return LearnReport{}, err
	}
	res := sessionlog.Learn(recs, sessionlog.LearnInput{
		Root: root, Configured: ConfiguredRoutes(cfg), Learned: before.Routes,
		Available:  func(k sessionlog.RouteKey) bool { return cfg.RouteAvailable(k.Provider, k.Route()) },
		MinSamples: cfg.LearnMin(), Now: now,
	})
	after := &config.Learned{Root: before.Root, Updated: now, Routes: res.Routes}
	if !dry {
		if err := config.SaveLearned(after); err != nil {
			return LearnReport{}, err
		}
	}
	return LearnReport{Root: root, Before: before, Learned: after, Result: res}, nil
}

// ApplyLearned layers the stored learned routes of dir's repo over the
// live config (unless routing.learn is off) and returns the roles that use
// one. Outside a git repo there are none.
func ApplyLearned(store *config.Store, dir string) ([]string, error) {
	if store.Get().LearnMode() == config.LearnOff {
		return nil, nil
	}
	root, err := LearnedRoot(dir)
	if err != nil {
		return nil, nil
	}
	l, err := config.LoadLearned(root)
	if err != nil {
		return nil, err
	}
	return store.ApplyLearned(l), nil
}

// autoLearn refreshes the learned routes at task start when routing.learn
// is auto, at most once a day, and layers them over the live config. Bench
// and demo runs, and runs in a temporary checkout (NoAutoLearn), never
// change them.
func (o *Orchestrator) autoLearn() {
	if o.opts.Bench != "" || o.opts.Mode == "demo" || o.opts.NoGit || o.opts.NoAutoLearn || o.opts.Store.Get().LearnMode() != config.LearnAuto {
		return
	}
	dir := o.logDir()
	root, err := LearnedRoot(o.opts.Dir)
	if dir == "" || err != nil {
		return
	}
	if l, err := config.LoadLearned(root); err == nil && time.Since(l.Updated) < learnEvery {
		return
	}
	recs, err := sessionlog.ReadDir(dir)
	if err != nil {
		o.logf("learned routes: could not read the session logs (%v); kept as they are", err)
		return
	}
	rep, err := UpdateLearned(o.opts.Dir, o.opts.Store.Unlearned(), recs, time.Now(), false)
	if err != nil {
		o.logf("learned routes: %v; kept as they are", err)
		return
	}
	o.opts.Store.ApplyLearned(rep.Learned)
	for _, c := range rep.Result.Changes {
		to := c.To.String() + " (learned)"
		if c.Remove {
			to = c.To.String() + " (configured)"
		}
		o.logf("learned route for %s: %s -> %s: %s (sy tune --learned; routing.learn: suggest stops this)", c.Role, c.From, to, c.Why)
	}
}
