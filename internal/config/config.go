// Package config loads, validates, edits and saves switchyard.yaml.
package config

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sparkz400/switchyard/internal/event"
	"gopkg.in/yaml.v3"
)

//go:embed default.yaml
var defaultYAML []byte

// DefaultYAML returns the commented default config file.
func DefaultYAML() []byte { return append([]byte(nil), defaultYAML...) }

// FileName is the config file name looked up in the working directory.
const FileName = "switchyard.yaml"

// Prefer values.
const (
	PreferCodex  = "codex"
	PreferClaude = "claude"
	PreferOther  = "other"
	PreferAuto   = "auto"
)

// PreferOptions lists every valid prefer value.
var PreferOptions = []string{PreferCodex, PreferClaude, PreferOther, PreferAuto}

// Route is a model + reasoning effort on one provider.
type Route struct {
	Model  string `yaml:"model"`
	Effort string `yaml:"effort"`
}

// RoleCfg holds a role's route on both providers.
type RoleCfg struct {
	Prefer string `yaml:"prefer"`
	Codex  Route  `yaml:"codex"`
	Claude Route  `yaml:"claude"`
}

// For returns the role's route on a provider.
func (r RoleCfg) For(provider string) Route {
	if provider == event.Claude {
		return r.Claude
	}
	return r.Codex
}

// ModelInfo is one entry of a provider's model catalog.
type ModelInfo struct {
	ID    string `yaml:"id"`
	Label string `yaml:"label,omitempty"`
	Tier  string `yaml:"tier,omitempty"` // fast | standard | strong
}

// Duration is a time.Duration that reads and writes "1h30m" in YAML.
type Duration time.Duration

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	if s == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// ProviderCfg configures one CLI.
type ProviderCfg struct {
	Disabled            bool        `yaml:"disabled,omitempty"` // true hides the provider from routing
	Command             string      `yaml:"command"`
	TestedVersion       string      `yaml:"tested_version,omitempty"`
	WriteSandbox        string      `yaml:"write_sandbox,omitempty"`
	WritePermissionMode string      `yaml:"write_permission_mode,omitempty"`
	WriteAllowedTools   []string    `yaml:"write_allowed_tools,omitempty"`
	ExtraArgs           []string    `yaml:"extra_args"`
	LimitCooldown       Duration    `yaml:"limit_cooldown"`
	Efforts             []string    `yaml:"efforts"`
	Models              []ModelInfo `yaml:"models"`
}

// RoutingCfg tunes the rule router.
type RoutingCfg struct {
	MaxFilesBeforeHigh   int      `yaml:"max_files_before_high"`
	SensitivePaths       []string `yaml:"sensitive_paths"`
	Judge                bool     `yaml:"judge"`
	JudgeBelowConfidence float64  `yaml:"judge_below_confidence"`
	SwitchAtUtilization  float64  `yaml:"switch_at_utilization"`
}

// OrchestratorCfg tunes the task lifecycle.
type OrchestratorCfg struct {
	MaxThreads          int      `yaml:"max_threads"`
	Parallel            bool     `yaml:"parallel"`
	Worktrees           bool     `yaml:"worktrees"`
	WorktreeMaxFiles    int      `yaml:"worktree_max_files"`
	ReviewBeforePlan    bool     `yaml:"review_before_plan"`
	ReviewOnRepeatError bool     `yaml:"review_on_repeat_error"`
	ReviewBeforeDone    bool     `yaml:"review_before_done"`
	MaxPlanRevisions    int      `yaml:"max_plan_revisions"`
	MaxFixRounds        int      `yaml:"max_fix_rounds"`
	MaxAttempts         int      `yaml:"max_attempts"`
	AgentTimeout        Duration `yaml:"agent_timeout"`
	SmallTaskWords      int      `yaml:"small_task_words"`
	ApprovePlan         bool     `yaml:"approve_plan"`
	ReviewChanges       bool     `yaml:"review_changes"`
	Handoff             bool     `yaml:"handoff"`
	LowPriority         bool     `yaml:"low_priority"`
	MaxCPUPercent       int      `yaml:"max_cpu_percent"`
	MinFreeMemoryMB     int      `yaml:"min_free_memory_mb"`
	BusyMaxWait         Duration `yaml:"busy_max_wait"`
	MinFreeDiskGB       float64  `yaml:"min_free_disk_gb"`
	PoolWarnGB          float64  `yaml:"pool_warn_gb"`
	PoolMaxIdle         Duration `yaml:"pool_max_idle"`
}

// VerifyCfg lists the repo's own checks (tests, build, lint). Agents may
// run them without asking, and Switchyard runs them before the final review.
type VerifyCfg struct {
	Commands []string `yaml:"commands"`
	Timeout  Duration `yaml:"timeout"`
}

// HooksCfg runs your own commands around tasks. Each list runs in order in
// the project folder through the system shell, with SY_* environment
// variables describing the task (see README). A failing before_task hook
// stops the task; failures of the others are reported and ignored.
type HooksCfg struct {
	BeforeTask []string `yaml:"before_task,omitempty"`
	AfterMerge []string `yaml:"after_merge,omitempty"` // after each agent's changes land in your tree
	AfterTask  []string `yaml:"after_task,omitempty"`  // every end: done, failed or cancelled
	Timeout    Duration `yaml:"timeout,omitempty"`
}

// NotifyCfg controls desktop notifications.
type NotifyCfg struct {
	Enabled bool     `yaml:"enabled"`
	MinTask Duration `yaml:"min_task"` // only tasks that ran at least this long
}

// Config is the whole file.
type Config struct {
	Roles         map[string]RoleCfg     `yaml:"roles"`
	Providers     map[string]ProviderCfg `yaml:"providers"`
	Routing       RoutingCfg             `yaml:"routing"`
	Orchestrator  OrchestratorCfg        `yaml:"orchestrator"`
	Verify        VerifyCfg              `yaml:"verify"`
	Notify        NotifyCfg              `yaml:"notify"`
	Hooks         HooksCfg               `yaml:"hooks"`
	LimitPatterns []string               `yaml:"limit_patterns"`
	Theme         string                 `yaml:"theme"`
	LogDir        string                 `yaml:"log_dir"`
}

// Default returns the built-in configuration.
func Default() *Config {
	c, err := Parse(defaultYAML)
	if err != nil {
		panic("config: embedded default is invalid: " + err.Error())
	}
	return c
}

// Parse reads YAML on top of nothing (missing keys stay zero), then fills
// gaps from the default so partial user files work.
func Parse(data []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Load finds and reads the config. path may be empty, in which case
// ./switchyard.yaml and then <user config dir>/switchyard/switchyard.yaml are
// tried. The returned path is where Save writes; it is ./switchyard.yaml when
// no file was found.
func Load(path string) (*Config, string, error) {
	candidates := []string{}
	if path != "" {
		candidates = append(candidates, path)
	} else {
		candidates = append(candidates, FileName)
		if dir, err := os.UserConfigDir(); err == nil {
			candidates = append(candidates, filepath.Join(dir, "switchyard", FileName))
		}
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			if path != "" {
				return nil, "", fmt.Errorf("config %s not found", path)
			}
			continue
		}
		if err != nil {
			return nil, "", err
		}
		c := Default()
		if err := yaml.Unmarshal(data, c); err != nil {
			return nil, "", fmt.Errorf("%s: %w", p, err)
		}
		c.fillProviderDefaults(Default())
		if err := c.Validate(); err != nil {
			return nil, "", fmt.Errorf("%s: %w", p, err)
		}
		abs, _ := filepath.Abs(p)
		return c, abs, nil
	}
	abs, _ := filepath.Abs(FileName)
	return Default(), abs, nil
}

// fillProviderDefaults fills provider fields a partial user file left empty.
// (YAML decoding replaces a whole map entry, so a user who only sets
// `providers.codex.command` would otherwise lose the model catalog.)
func (c *Config) fillProviderDefaults(def *Config) {
	for k, v := range c.Providers {
		d, ok := def.Providers[k]
		if !ok {
			continue
		}
		if v.Command == "" {
			v.Command = d.Command
		}
		if len(v.Efforts) == 0 {
			v.Efforts = d.Efforts
		}
		if len(v.Models) == 0 {
			v.Models = d.Models
		}
		if v.LimitCooldown == 0 {
			v.LimitCooldown = d.LimitCooldown
		}
		if v.WriteSandbox == "" {
			v.WriteSandbox = d.WriteSandbox
		}
		if v.WritePermissionMode == "" {
			v.WritePermissionMode = d.WritePermissionMode
		}
		if v.TestedVersion == "" {
			v.TestedVersion = d.TestedVersion
		}
		c.Providers[k] = v
	}
}

// Validate checks the config for mistakes a user is likely to make.
func (c *Config) Validate() error {
	var errs []string
	for _, role := range event.Roles {
		rc, ok := c.Roles[role]
		if !ok {
			errs = append(errs, "missing role "+role)
			continue
		}
		if !contains(PreferOptions, rc.Prefer) {
			errs = append(errs, fmt.Sprintf("role %s: prefer must be one of %v, got %q", role, PreferOptions, rc.Prefer))
		}
		if rc.Codex.Model == "" && rc.Claude.Model == "" {
			errs = append(errs, fmt.Sprintf("role %s: needs a model on at least one provider", role))
		}
	}
	for _, p := range event.Providers {
		if _, ok := c.Providers[p]; !ok {
			errs = append(errs, "missing provider "+p)
		}
	}
	for _, pat := range c.LimitPatterns {
		if _, err := regexp.Compile("(?i)" + pat); err != nil {
			errs = append(errs, fmt.Sprintf("limit pattern %q: %v", pat, err))
		}
	}
	if c.Orchestrator.MaxThreads < 1 {
		errs = append(errs, "orchestrator.max_threads must be >= 1")
	}
	switch c.Theme {
	case "", "auto", "unicode", "ascii":
	default:
		errs = append(errs, "theme must be auto, unicode or ascii")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// Clone returns a deep copy.
func (c *Config) Clone() *Config {
	data, err := yaml.Marshal(c)
	if err != nil {
		panic(err)
	}
	out, err := Parse(data)
	if err != nil {
		panic(err)
	}
	return out
}

// Save writes the config to path.
func (c *Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	header := "# Switchyard configuration (saved by sy). See `sy init --print` for the commented default.\n"
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, append([]byte(header), data...), 0o644)
}

// SessionDir returns the directory for JSONL session logs.
func (c *Config) SessionDir() string {
	if c.LogDir != "" {
		return c.LogDir
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "switchyard", "sessions")
	}
	return filepath.Join(".switchyard", "sessions")
}

// ParseRouteSpec parses "provider:model[:effort]" (effort may be empty).
func ParseRouteSpec(s string) (provider string, r Route, err error) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) < 2 || parts[1] == "" {
		return "", r, fmt.Errorf("route %q: want provider:model[:effort]", s)
	}
	provider = strings.ToLower(parts[0])
	if provider != event.Codex && provider != event.Claude {
		return "", r, fmt.Errorf("route %q: provider must be codex or claude", s)
	}
	r.Model = parts[1]
	if len(parts) == 3 {
		r.Effort = parts[2]
	}
	return provider, r, nil
}

// Store is a concurrency-safe holder for the live config, edited by the TUI
// while the orchestrator reads it.
type Store struct {
	mu   sync.RWMutex
	cfg  *Config
	path string
	// base is the config without the repo file (what Save writes), nil
	// when no repo file was applied.
	base *Config
	repo RepoInfo
}

// ApplyRepo layers the project's .switchyard.yaml (if any) over the live
// config. Save keeps writing the user's file without it.
func (s *Store) ApplyRepo(dir string) (RepoInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := s.cfg.Clone()
	live := s.cfg.Clone()
	info, err := ApplyRepo(live, dir)
	if err != nil {
		return info, err
	}
	if info.Path != "" {
		s.base, s.cfg = base, live
	}
	s.repo = info
	return info, nil
}

// Repo describes the applied repo file.
func (s *Store) Repo() RepoInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.repo
}

// SaveRepo writes the shareable settings to the repo file (creating
// <dir>/.switchyard.yaml when there is none yet) and returns its path.
func (s *Store) SaveRepo(dir string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.repo.Path
	if p == "" {
		p = filepath.Join(dir, RepoFileName)
	}
	if err := SaveRepo(s.cfg, p); err != nil {
		return "", err
	}
	if s.base == nil {
		s.base = s.cfg.Clone()
	}
	s.repo = RepoInfo{Path: p, Trusted: true}
	return p, nil
}

// NewStore wraps a config and the path it is saved to.
func NewStore(c *Config, path string) *Store { return &Store{cfg: c, path: path} }

// Get returns a snapshot that is safe to read without locks.
func (s *Store) Get() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Clone()
}

// Path returns where Save writes.
func (s *Store) Path() string { return s.path }

// Update applies fn to the live config under the write lock and validates
// the result; on error the change is rolled back.
func (s *Store) Update(fn func(c *Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cfg.Clone()
	if err := fn(next); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if s.base != nil {
		// Changes made in the session also go to the user's file on Save.
		nb := s.base.Clone()
		if err := fn(nb); err == nil && nb.Validate() == nil {
			s.base = nb
		}
	}
	s.cfg = next
	return nil
}

// Save persists the live config.
func (s *Store) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.base != nil {
		return s.base.Save(s.path) // never bake the repo file into yours
	}
	return s.cfg.Save(s.path)
}

// SetRoute changes one role's route on one provider.
func (s *Store) SetRoute(role, provider string, r Route) error {
	return s.Update(func(c *Config) error {
		rc, ok := c.Roles[role]
		if !ok {
			return fmt.Errorf("unknown role %q (roles: %s)", role, strings.Join(event.Roles, ", "))
		}
		if provider == event.Claude {
			rc.Claude = r
		} else {
			rc.Codex = r
		}
		c.Roles[role] = rc
		return nil
	})
}

// SetPrefer changes which provider a role prefers.
func (s *Store) SetPrefer(role, prefer string) error {
	return s.Update(func(c *Config) error {
		rc, ok := c.Roles[role]
		if !ok {
			return fmt.Errorf("unknown role %q", role)
		}
		rc.Prefer = prefer
		c.Roles[role] = rc
		return nil
	})
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
