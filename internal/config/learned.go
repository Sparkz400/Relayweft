package config

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"github.com/sparkz400/switchyard/internal/canon"
	"github.com/sparkz400/switchyard/internal/event"
)

// Learned routes: `sy tune --apply` (and, with routing.learn: auto, the
// first task of a day) works out per-role routes from this repo's session
// logs and bench runs (sessionlog/learn.go) and stores them in your state
// dir, keyed by the canonical repo root: never in the repo, so they need
// no `sy trust` and are never shared.
//
// The layers, lowest first:
//
//	built-in defaults < user config (switchyard.yaml) < learned routes
//	  < repo file (.switchyard.yaml) < flags and session edits
//
// Explicit settings always win: a role the repo file changes, or that a
// flag or a session edit sets (--route, --prefer, /route, /prefer, the
// model picker), keeps that setting whole and gets no learned route. A
// learned route only replaces what your own config (or the default) says.
//
// A learned route pins the role to one provider:model:effort. It applies
// only while that route is still configured and usable (RouteAvailable);
// otherwise the role keeps its configured route. Decisions that use one
// say so in their reason ("learned route: ..."), in the logs and reports.

// Learn modes (routing.learn).
const (
	LearnAuto    = "auto"    // apply learned routes and refresh them at most once a day at task start
	LearnSuggest = "suggest" // only `sy tune` suggestions; learned routes apply once `sy tune --apply` stored them
	LearnOff     = "off"     // ignore learned routes
)

// DefaultLearnMinSamples is routing.learn_min_samples when unset.
const DefaultLearnMinSamples = 8

// LearnMode returns routing.learn ("" means suggest).
func (c *Config) LearnMode() string {
	if c.Routing.Learn == "" {
		return LearnSuggest
	}
	return c.Routing.Learn
}

// LearnMin returns routing.learn_min_samples (default 8).
func (c *Config) LearnMin() int {
	if c.Routing.LearnMinSamples <= 0 {
		return DefaultLearnMinSamples
	}
	return c.Routing.LearnMinSamples
}

// LearnedRoute is one role's learned route with the evidence for it.
type LearnedRoute struct {
	Provider string          `json:"provider"`
	Model    string          `json:"model"`
	Effort   string          `json:"effort,omitempty"`
	Why      string          `json:"why"`   // one line, appended to routing reasons
	Since    time.Time       `json:"since"` // when it was learned
	Evidence []RouteEvidence `json:"evidence,omitempty"`
}

// Spec is the route as "provider:model[:effort]".
func (r LearnedRoute) Spec() string {
	return RouteSpec(r.Provider, Route{Model: r.Model, Effort: r.Effort})
}

// RouteEvidence summarizes one route's runs for a role.
type RouteEvidence struct {
	Route   string  `json:"route"`   // provider:model[:effort]
	Samples int     `json:"samples"` // runs
	Weight  float64 `json:"weight"`  // runs after the age decay
	Success float64 `json:"success"` // share of runs that succeeded, 0..1 (decayed)
	Tokens  int64   `json:"tokens"`  // mean fresh tokens per run
	WallMS  int64   `json:"wall_ms"` // mean wall time per run
}

// Learned is one repo's learned routes.
type Learned struct {
	Root    string                  `json:"root"`    // canonical repo root
	Updated time.Time               `json:"updated"` // last refresh, also when nothing changed
	Routes  map[string]LearnedRoute `json:"routes,omitempty"`
}

// RouteSpec formats "provider:model[:effort]" (ParseRouteSpec's input).
func RouteSpec(provider string, r Route) string {
	s := provider + ":" + r.Model
	if r.Effort != "" {
		s += ":" + r.Effort
	}
	return s
}

// learnedDir is where learned routes live; tests point it elsewhere via
// the user config dir.
func learnedDir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "switchyard", "learned")
}

// LearnedPath is the learned-routes file of the repo at root (any path to
// it: it is canonicalized, so /var and /private/var, C:\x and c:/X share
// one file).
func LearnedPath(root string) string {
	h := sha1.Sum([]byte(canon.Path(root)))
	return filepath.Join(learnedDir(), hex.EncodeToString(h[:])[:12]+".json")
}

// LoadLearned reads a repo's learned routes; a repo without any gets an
// empty set (Updated zero).
func LoadLearned(root string) (*Learned, error) {
	l := &Learned{Root: canon.Path(root)}
	data, err := os.ReadFile(LearnedPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal(data, l); err != nil {
		return &Learned{Root: canon.Path(root)}, fmt.Errorf("%s: %w", LearnedPath(root), err)
	}
	return l, nil
}

// SaveLearned writes a repo's learned routes atomically.
func SaveLearned(l *Learned) error {
	p := LearnedPath(l.Root)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	// A temporary file of its own (two sy writing at once must not share
	// one), in the same folder so the rename is atomic.
	f, err := os.CreateTemp(filepath.Dir(p), filepath.Base(p)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o644) // CreateTemp makes it 0600
	}
	if werr == nil {
		werr = os.Rename(tmp, p)
	}
	if werr != nil {
		os.Remove(tmp)
	}
	return werr
}

// ResetLearned forgets a repo's learned routes.
func ResetLearned(root string) error {
	err := os.Remove(LearnedPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// RouteAvailable reports whether a route may be used: the provider is
// configured and not disabled, the model is in its catalog or used by a
// role, and the effort is one of its efforts (or the CLI default).
func (c *Config) RouteAvailable(provider string, r Route) bool {
	pc, ok := c.Providers[provider]
	if !ok || pc.Disabled || r.Model == "" {
		return false
	}
	if r.Effort != "" && len(pc.Efforts) > 0 && !contains(pc.Efforts, r.Effort) {
		return false
	}
	for _, m := range pc.Models {
		if m.ID == r.Model {
			return true
		}
	}
	for _, rc := range c.Roles {
		if rc.For(provider).Model == r.Model {
			return true
		}
	}
	return false
}

// withLearned is a role's config with a learned route: that provider,
// with that model and effort.
func withLearned(rc RoleCfg, lr LearnedRoute) RoleCfg {
	rc.Prefer = lr.Provider
	if lr.Provider == event.Claude {
		rc.Claude = Route{Model: lr.Model, Effort: lr.Effort}
	} else {
		rc.Codex = Route{Model: lr.Model, Effort: lr.Effort}
	}
	return rc
}

func cloneLearned(m map[string]LearnedRoute) map[string]LearnedRoute {
	if m == nil {
		return nil
	}
	out := make(map[string]LearnedRoute, len(m))
	for k, v := range m {
		v.Evidence = append([]RouteEvidence(nil), v.Evidence...)
		out[k] = v
	}
	return out
}

// ApplyLearned layers a repo's learned routes over the user's config (nil
// or an empty set removes them). Roles set explicitly keep their setting,
// and a route that is no longer available is skipped. It returns the roles
// that now use a learned route.
func (s *Store) ApplyLearned(l *Learned) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.base == nil {
		s.base = s.cfg.Clone() // Save keeps writing your config without them
	}
	s.learned = l
	return s.layerLearned()
}

// layerLearned puts the stored learned routes on the live config: each
// learned role first goes back to its user-layer value, then gets its
// (new) learned route. Called with s.mu held.
func (s *Store) layerLearned() []string {
	if s.base == nil {
		return nil
	}
	next := s.cfg.Clone()
	for role := range next.Learned {
		if !s.pinned[role] {
			next.Roles[role] = s.base.Roles[role]
		}
	}
	next.Learned = nil
	var applied []string
	if s.learned != nil {
		roles := make([]string, 0, len(s.learned.Routes))
		for role := range s.learned.Routes {
			roles = append(roles, role)
		}
		sort.Strings(roles)
		for _, role := range roles {
			lr := s.learned.Routes[role]
			rc, ok := next.Roles[role]
			if !ok || s.pinned[role] || !next.RouteAvailable(lr.Provider, Route{Model: lr.Model, Effort: lr.Effort}) {
				continue
			}
			next.Roles[role] = withLearned(rc, lr)
			if next.Learned == nil {
				next.Learned = map[string]LearnedRoute{}
			}
			next.Learned[role] = lr
			applied = append(applied, role)
		}
	}
	if next.Validate() != nil {
		return nil
	}
	s.cfg = next
	return applied
}

// Learned returns the learned routes in effect, per role.
func (s *Store) Learned() map[string]LearnedRoute {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneLearned(s.cfg.Learned)
}

// Unlearned returns the live config without its learned routes: the
// routes a refresh compares them with.
func (s *Store) Unlearned() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.unlearnedLocked()
}

func (s *Store) unlearnedLocked() *Config {
	c := s.cfg.Clone()
	for role := range c.Learned {
		if s.base != nil {
			c.Roles[role] = s.base.Roles[role]
		}
	}
	c.Learned = nil
	return c
}

// Pin marks a role as set explicitly (a flag or a session edit): it keeps
// its setting, and a learned route it had falls back to your config.
func (s *Store) Pin(role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pinLocked(role)
}

func (s *Store) pinIfOK(role string, err *error) {
	if *err == nil {
		s.Pin(role)
	}
}

func (s *Store) pinLocked(role string) {
	if s.pinned == nil {
		s.pinned = map[string]bool{}
	}
	s.pinned[role] = true
	if _, ok := s.cfg.Learned[role]; ok {
		next := s.cfg.Clone()
		if s.base != nil {
			next.Roles[role] = s.base.Roles[role]
		}
		delete(next.Learned, role)
		if next.Validate() == nil {
			s.cfg = next
		}
	}
}

// pinEdited pins the roles an Update changed. A learned role that was
// edited takes your edited setting (Update applied the same edit to base)
// instead of the learned route with the edit on top. Called with s.mu held.
func (s *Store) pinEdited(next *Config) {
	for role, rc := range next.Roles {
		if reflect.DeepEqual(rc, s.cfg.Roles[role]) {
			continue
		}
		if s.pinned == nil {
			s.pinned = map[string]bool{}
		}
		s.pinned[role] = true
		if _, ok := next.Learned[role]; ok {
			if s.base != nil {
				next.Roles[role] = s.base.Roles[role]
			}
			delete(next.Learned, role)
		}
	}
}

// pinRepoRoles pins the roles a repo file changed (an entry that only
// repeats your config does not count: `sy init --repo` writes every role).
// Called with s.mu held.
func (s *Store) pinRepoRoles(before, after *Config) {
	for role, rc := range after.Roles {
		if !reflect.DeepEqual(rc, before.Roles[role]) {
			if s.pinned == nil {
				s.pinned = map[string]bool{}
			}
			s.pinned[role] = true
		}
	}
}

// Pinned reports whether a role was set explicitly (repo file, flag or
// session edit), so a learned route does not apply to it.
func (s *Store) Pinned(role string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pinned[role]
}
