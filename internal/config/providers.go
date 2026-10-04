package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/sparkz400/switchyard/internal/event"
)

// providerName is what a provider may be called: it appears in routes
// ("name:model:effort"), /prefer and the logs.
var providerName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// reservedNames cannot name a provider: they are prefer values or
// command words.
var reservedNames = []string{PreferOther, PreferAuto, "all", "any", "none", "prefer"}

// modelID and effortName are what a route may hold. Both reach the CLI's
// command line, and the CLIs are often npm .cmd shims that cmd.exe parses,
// so shell characters (& | < > ^ % " and spaces) are never allowed, and
// nothing may start with "-" (it would read as a flag). Brackets stay
// allowed for ids like "deepseek-flash[1m]".
var (
	modelID    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+\[\]-]{0,127}$`)
	effortName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
)

// checkRoute reports what is wrong with a route's model or effort ("" = ok).
func checkRoute(r Route) string {
	if r.Model != "" && !modelID.MatchString(r.Model) {
		return fmt.Sprintf("model %q may use only letters, digits and . _ : / @ + [ ] - (and must start with a letter or digit)", r.Model)
	}
	if r.Effort != "" && !effortName.MatchString(r.Effort) {
		return fmt.Sprintf("effort %q may use only lower-case letters, digits, _ and -", r.Effort)
	}
	return ""
}

// KindOf returns the CLI protocol of a provider: its kind, or its name for
// the built-in ones (codex, claude, gemini, qwen).
func (p ProviderCfg) KindOf(name string) string {
	if p.Kind != "" {
		return strings.ToLower(p.Kind)
	}
	return name
}

// Kind returns a configured provider's CLI protocol ("" when unknown).
func (c *Config) Kind(provider string) string {
	pc, ok := c.Providers[provider]
	if !ok {
		return ""
	}
	return pc.KindOf(provider)
}

// ProviderNames returns every configured provider, disabled ones included,
// in routing order: routing.provider_order first, then codex, claude and the
// rest by name.
func (c *Config) ProviderNames() []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if _, ok := c.Providers[p]; ok && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range c.Routing.ProviderOrder {
		add(p)
	}
	add(event.Codex)
	add(event.Claude)
	var rest []string
	for p := range c.Providers {
		if !seen[p] {
			rest = append(rest, p)
		}
	}
	sort.Strings(rest)
	for _, p := range rest {
		add(p)
	}
	return out
}

// Enabled returns the providers routing may use, in routing order.
func (c *Config) Enabled() []string {
	var out []string
	for _, p := range c.ProviderNames() {
		if !c.Providers[p].Disabled {
			out = append(out, p)
		}
	}
	return out
}

// IsProvider reports whether p is a configured provider.
func (c *Config) IsProvider(p string) bool {
	_, ok := c.Providers[p]
	return ok
}

// Alternatives returns the enabled providers other than p, in routing order:
// where work goes when p is at its limit, and what "prefer: other" means.
func (c *Config) Alternatives(p string) []string {
	var out []string
	for _, q := range c.Enabled() {
		if q != p {
			out = append(out, q)
		}
	}
	return out
}

// Enabled returns the live enabled providers without cloning the config
// (UIs read it every frame).
func (s *Store) Enabled() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Enabled()
}

// ProviderNames returns the live provider names without cloning the config.
func (s *Store) ProviderNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.ProviderNames()
}

// ProviderLabel is the provider's display name.
func (c *Config) ProviderLabel(p string) string {
	if l := c.Providers[p].Label; l != "" {
		return l
	}
	return p
}

// WritingRole reports whether a role's steps normally write files.
func WritingRole(role string) bool { return role == event.RoleWorker || role == event.RoleWorkerHigh }

// SessionPerDir reports whether the provider's CLI keeps its sessions per
// folder, so a session started in a pool worktree cannot be resumed from
// the main tree (Claude Code and the CLIs built on it).
func (c *Config) SessionPerDir(provider string) bool {
	switch c.Kind(provider) {
	case event.Claude, event.Gemini, event.Qwen, event.Generic:
		return true // generic: assume so, a fresh agent is always safe
	}
	return false
}

// SupportsMCP reports whether sy can hand MCP servers to the provider's
// CLI on the command line. Gemini CLI reads them only from its own
// settings.json.
func (c *Config) SupportsMCP(provider string) bool {
	switch c.Kind(provider) {
	case event.Codex, event.Claude, event.Qwen:
		return true
	}
	return false
}

// EnvFor returns the provider's extra environment as NAME=value, with
// ${VAR} taken from lookup (nil = the process environment), and the names
// of the variables that were not set.
func (p ProviderCfg) EnvFor(lookup func(string) (string, bool)) (env []string, missing []string) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	names := make([]string, 0, len(p.Env))
	for k := range p.Env {
		names = append(names, k)
	}
	sort.Strings(names)
	miss := map[string]bool{}
	for _, k := range names {
		v, m := ExpandEnv(p.Env[k], lookup)
		for _, n := range m {
			miss[n] = true
		}
		env = append(env, k+"="+v)
	}
	for m := range miss {
		missing = append(missing, m)
	}
	sort.Strings(missing)
	return env, missing
}

// RedactEnv hides every env value that is not only ${VAR} references
// (those name where a secret comes from, never the secret).
func RedactEnv(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		if v == "" || strings.TrimSpace(envRef.ReplaceAllString(v, "")) == "" {
			out[k] = v
		} else {
			out[k] = "<hidden>"
		}
	}
	return out
}

// envKey is a valid environment variable name.
var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateProviders checks provider names, kinds and environments, and that
// every role route and prefer names a configured provider.
func (c *Config) validateProviders() []string {
	var errs []string
	for _, p := range []string{event.Codex, event.Claude} {
		if _, ok := c.Providers[p]; !ok {
			errs = append(errs, "missing provider "+p)
		}
	}
	for _, name := range c.ProviderNames() {
		pc := c.Providers[name]
		if !providerName.MatchString(name) || contains(reservedNames, name) {
			errs = append(errs, fmt.Sprintf("provider %q: a name is lower-case letters, digits, - and _ (not %s)", name, strings.Join(reservedNames, ", ")))
		}
		if k := pc.KindOf(name); !contains(event.Kinds, k) {
			if pc.Kind == "" {
				errs = append(errs, fmt.Sprintf("provider %s: set kind to one of %s (the CLI it runs)", name, strings.Join(event.Kinds, ", ")))
			} else {
				errs = append(errs, fmt.Sprintf("provider %s: kind must be one of %s, got %q", name, strings.Join(event.Kinds, ", "), pc.Kind))
			}
		}
		if !pc.Disabled && pc.Command == "" {
			errs = append(errs, fmt.Sprintf("provider %s: needs a command", name))
		}
		for k := range pc.Env {
			if !envKey.MatchString(k) {
				errs = append(errs, fmt.Sprintf("provider %s: env name %q may use only letters, digits and _", name, k))
			}
		}
		errs = append(errs, validateGeneric(name, pc)...)
		for _, role := range pc.Standby {
			switch {
			case !contains(event.Roles, role):
				errs = append(errs, fmt.Sprintf("provider %s: standby: unknown role %q (roles: %s)", name, role, strings.Join(event.Roles, ", ")))
			case WritingRole(role) && !pc.CanWrite(name):
				errs = append(errs, fmt.Sprintf("provider %s: standby: %s writes files, and this provider takes read-only work only (no write_args)", name, role))
			case !WritingRole(role) && !pc.CanReadOnly(name):
				errs = append(errs, fmt.Sprintf("provider %s: standby: %s is read-only, and this provider has no read_only_args (and is not no_tools)", name, role))
			}
		}
	}
	for _, p := range c.Routing.ProviderOrder {
		if !c.IsProvider(p) {
			errs = append(errs, fmt.Sprintf("routing.provider_order: unknown provider %q", p))
		}
	}
	for _, role := range event.Roles {
		rc, ok := c.Roles[role]
		if !ok {
			continue // reported by Validate
		}
		for p := range rc.Extra {
			if !c.IsProvider(p) {
				errs = append(errs, fmt.Sprintf("role %s: route for unknown provider %q (add it under providers:)", role, p))
			}
		}
		if rc.Prefer != PreferOther && rc.Prefer != PreferAuto && !c.IsProvider(rc.Prefer) {
			errs = append(errs, fmt.Sprintf("role %s: prefer must be other, auto or a provider (%s), got %q", role, strings.Join(c.ProviderNames(), ", "), rc.Prefer))
		} else if pc, ok := c.Providers[rc.Prefer]; ok && !pc.Disabled {
			if WritingRole(role) && !pc.CanWrite(rc.Prefer) {
				errs = append(errs, fmt.Sprintf("role %s: prefers %s, which takes read-only work only (no write_args)", role, rc.Prefer))
			} else if !WritingRole(role) && !pc.CanReadOnly(rc.Prefer) {
				errs = append(errs, fmt.Sprintf("role %s: prefers %s, which has no read_only_args, so sy cannot keep it read-only", role, rc.Prefer))
			}
		}
		// Every route is checked, also those on disabled providers: a
		// repo file could set one and a later enable would use it.
		for _, p := range append([]string{event.Codex, event.Claude}, sortedKeys(rc.Extra)...) {
			if msg := checkRoute(rc.For(p)); msg != "" {
				errs = append(errs, fmt.Sprintf("role %s on %s: %s", role, p, msg))
			}
		}
		// A model on codex or claude (as before), or on an enabled extra
		// provider: the presets' routes on disabled providers do not count.
		has := rc.Codex.Model != "" || rc.Claude.Model != ""
		for _, p := range c.Enabled() {
			if rc.For(p).Model != "" {
				has = true
			}
		}
		if !has {
			errs = append(errs, fmt.Sprintf("role %s: needs a model on at least one provider", role))
		}
	}
	for name, s := range c.MCP.Servers {
		for _, p := range s.Providers {
			switch {
			case !c.IsProvider(p):
				errs = append(errs, fmt.Sprintf("mcp server %s: unknown provider %q", name, p))
			case !c.SupportsMCP(p):
				errs = append(errs, fmt.Sprintf("mcp server %s: provider %s (%s) takes MCP servers only from its own settings, not from sy", name, p, c.Kind(p)))
			}
		}
	}
	return errs
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// fillRoleDefaults gives a role the default's routes on providers the user
// file did not set for it. (YAML decoding replaces a whole role, so a user
// file written before a provider existed would leave it without a route.)
func (c *Config) fillRoleDefaults(def *Config) {
	for role, rc := range c.Roles {
		d, ok := def.Roles[role]
		if !ok {
			continue
		}
		for p, r := range d.Extra {
			if _, set := rc.Extra[p]; !set {
				if rc.Extra == nil {
					rc.Extra = map[string]Route{}
				}
				rc.Extra[p] = r
			}
		}
		c.Roles[role] = rc
	}
}
