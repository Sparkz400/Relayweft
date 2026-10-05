// Package config loads, validates, edits and saves relayweft.yaml.
package config

import (
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/notify"
	"gopkg.in/yaml.v3"
)

//go:embed default.yaml
var defaultYAML []byte

// DefaultYAML returns the commented default config file.
func DefaultYAML() []byte { return append([]byte(nil), defaultYAML...) }

// FileName is the config file name looked up in the working directory.
const FileName = "relayweft.yaml"

// Prefer values. A role may also prefer any configured provider by name.
const (
	PreferCodex  = "codex"
	PreferClaude = "claude"
	PreferOther  = "other"
	PreferAuto   = "auto"
)

// PreferOptions lists the prefer values that are not provider names.
var PreferOptions = []string{PreferOther, PreferAuto}

// Route is a model + reasoning effort on one provider.
type Route struct {
	// Model is the model id given to the CLI ("" = this provider never
	// runs the role).
	Model string `yaml:"model"`
	// Effort is the reasoning effort, one of the provider's efforts (""
	// = the CLI's default).
	Effort string `yaml:"effort"`
}

// RoleCfg holds a role's route on each provider.
type RoleCfg struct {
	// Prefer picks the provider: codex, claude or any provider name
	// (always that one, unless it is at its usage limit), other (another
	// provider than the planner's, for review) or auto (whichever has used
	// fewer tokens this session).
	Prefer string `yaml:"prefer"`
	// The role's route on each provider: one entry per provider name
	// (codex:, claude:, gemini:, ...), each with model and effort.
	Codex  Route `yaml:"codex"`
	Claude Route `yaml:"claude"`
	// Extra are the routes on every other configured provider, keyed by
	// provider name (`gemini: {model: ...}` next to codex: and claude:).
	Extra map[string]Route `yaml:",inline"`
}

// For returns the role's route on a provider.
func (r RoleCfg) For(provider string) Route {
	switch provider {
	case event.Codex:
		return r.Codex
	case event.Claude:
		return r.Claude
	}
	return r.Extra[provider]
}

// With returns the role with its route on a provider replaced.
func (r RoleCfg) With(provider string, rt Route) RoleCfg {
	switch provider {
	case event.Codex:
		r.Codex = rt
	case event.Claude:
		r.Claude = rt
	default:
		// A copy, so the role it came from keeps its own map.
		extra := maps.Clone(r.Extra)
		if extra == nil {
			extra = map[string]Route{}
		}
		extra[provider] = rt
		r.Extra = extra
	}
	return r
}

// ModelInfo is one entry of a provider's model catalog.
type ModelInfo struct {
	ID    string `yaml:"id"`              // what the CLI is given
	Label string `yaml:"label,omitempty"` // shown in the model picker
	Tier  string `yaml:"tier,omitempty"`  // fast | standard | strong
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
	Disabled bool `yaml:"disabled,omitempty"` // true hides the provider from routing
	// OnlyPreferred: used only by roles that prefer it by name, never as a
	// fallback, for prefer: other or for prefer: auto (for example a slow
	// local model that should not silently take a strong model's job).
	OnlyPreferred bool `yaml:"only_preferred,omitempty"`
	// Kind is the CLI protocol: codex, claude, gemini, qwen or generic. Empty means
	// the provider's name, so only extra providers need it (for example
	// `ollama: {kind: claude, ...}` runs Claude Code against local models).
	Kind  string `yaml:"kind,omitempty"`
	Label string `yaml:"label,omitempty"` // display name (default: the provider name)
	// Env is added to the CLI's environment; ${VAR} is read from yours.
	// Values never go on the command line.
	Env map[string]string `yaml:"env,omitempty"`
	// AllowRepoSettings lets the CLI load a repo's own settings (.gemini,
	// .qwen, a .env for Gemini), which can run commands or change where it
	// connects. Off by default; only your own config can turn it on.
	AllowRepoSettings bool `yaml:"allow_repo_settings,omitempty"`
	// Standby lists roles this provider takes when every provider that
	// would otherwise run them is at or near its usage limit (a free local
	// model for cheap read-only work), even with only_preferred.
	Standby []string `yaml:"standby,omitempty"`
	// Generic describes the CLI for kind: generic (generic.go).
	Generic *GenericCfg `yaml:"generic,omitempty"`
	// Sandbox is added to the top-level sandbox section for this
	// provider's agents (sandbox.go).
	Sandbox *SandboxCfg `yaml:"sandbox,omitempty"`
	// InstallHint is what `rw doctor` suggests when the command is missing.
	InstallHint string `yaml:"install_hint,omitempty"`
	// Command is the CLI to run, found on PATH (with PATHEXT on Windows,
	// so codex.cmd works) or a full path.
	Command string `yaml:"command"`
	// TestedVersion is the CLI version rw was tested with; `rw doctor`
	// warns when yours differs.
	TestedVersion string `yaml:"tested_version,omitempty"`
	// WriteSandbox is Codex's sandbox for writing agents (read-only agents
	// always get read-only).
	WriteSandbox string `yaml:"write_sandbox,omitempty"`
	// WritePermissionMode is the permission mode of writing agents
	// (Claude Code: acceptEdits, auto, bypassPermissions or dontAsk;
	// Gemini: auto_edit; Qwen: auto-edit).
	WritePermissionMode string `yaml:"write_permission_mode,omitempty"`
	// WriteAllowedTools are tools Claude Code's writing agents may use
	// without asking, e.g. ["Bash(go test *)"] so workers can run tests.
	WriteAllowedTools []string `yaml:"write_allowed_tools,omitempty"`
	// ExtraArgs are appended to every run of the CLI.
	ExtraArgs []string `yaml:"extra_args"`
	// LimitCooldown is how long a provider stays "at limit" when the CLI
	// says no reset time.
	LimitCooldown Duration `yaml:"limit_cooldown"`
	// Efforts are the reasoning efforts the CLI accepts (empty: it has no
	// effort setting).
	Efforts []string `yaml:"efforts"`
	// Models is the catalog shown in the model picker (any other id can be
	// typed too).
	Models []ModelInfo `yaml:"models"`
}

// RoutingCfg tunes the rule router.
type RoutingCfg struct {
	MaxFilesBeforeHigh int `yaml:"max_files_before_high"`
	// SensitivePaths are path words that send a step to worker_high.
	SensitivePaths []string `yaml:"sensitive_paths"`
	Judge          bool     `yaml:"judge"`
	// JudgeBelowConfidence: with judge on, a rule decision less certain
	// than this asks the judge model.
	JudgeBelowConfidence float64 `yaml:"judge_below_confidence"`
	SwitchAtUtilization  float64 `yaml:"switch_at_utilization"`
	// ProviderOrder is the order providers are tried in when one is at its
	// limit (and what prefer: other picks first). Empty = codex, claude,
	// then the rest by name.
	ProviderOrder []string `yaml:"provider_order,omitempty"`
	// Learn controls the learned routes per repo: auto (refreshed daily
	// at task start), suggest (only `rw tune --apply`) or off ("" =
	// suggest).
	Learn           string `yaml:"learn"`
	LearnMinSamples int    `yaml:"learn_min_samples"` // runs (after the age decay) a route needs; 0 = 8
	// Tiers picks a work step's model from its difficulty and the quota
	// left (router/tiers.go): auto | off ("" = off).
	Tiers string `yaml:"tiers,omitempty"`
	// TiersSaveAt is the quota left (0..1) below which tiers step down to
	// save it; 0 = 0.5.
	TiersSaveAt float64 `yaml:"tiers_save_below,omitempty"`
	// BestOf runs writing steps on several routes and keeps the best
	// result (orchestrator/bestof.go).
	BestOf BestOfCfg `yaml:"best_of,omitempty"`
}

// BestOfCfg is routing.best_of: a writing step runs on several routes at
// once, each in its own pool worktree, the repo's checks run in each and
// the best result is kept.
type BestOfCfg struct {
	When string `yaml:"when,omitempty"` // off | hard | always ("" = off)
	N    int    `yaml:"n,omitempty"`    // candidates per step, 2 to MaxBestOf (0 = 2)
	// Routes are the candidates as provider[:model[:effort]] (a provider
	// alone means the step's role there). Empty: the step's own route plus
	// the same role on the next providers in provider_order.
	Routes []string `yaml:"routes,omitempty"`
}

// Best-of modes (routing.best_of.when) and the most candidates per step.
const (
	BestOfOff    = "off"
	BestOfHard   = "hard"
	BestOfAlways = "always"
	MaxBestOf    = 4
)

// Count is the number of candidates per step (n, default 2).
func (b BestOfCfg) Count() int {
	if b.N <= 0 {
		return 2
	}
	return min(b.N, MaxBestOf)
}

// On reports whether any step may run as best of N by config.
func (b BestOfCfg) On() bool { return b.When == BestOfHard || b.When == BestOfAlways }

// Tier modes (routing.tiers).
const (
	TiersAuto = "auto"
	TiersOff  = "off"
)

// DefaultTiersSaveBelow is routing.tiers_save_below when unset.
const DefaultTiersSaveBelow = 0.5

// TiersSaveBelow returns routing.tiers_save_below (default 0.5).
func (r RoutingCfg) TiersSaveBelow() float64 {
	if r.TiersSaveAt <= 0 {
		return DefaultTiersSaveBelow
	}
	return r.TiersSaveAt
}

// OrchestratorCfg tunes the task lifecycle.
type OrchestratorCfg struct {
	MaxThreads int `yaml:"max_threads"`
	// Parallel runs independent subtasks at the same time (false: one at
	// a time, like --no-parallel).
	Parallel         bool `yaml:"parallel"`
	Worktrees        bool `yaml:"worktrees"`
	WorktreeMaxFiles int  `yaml:"worktree_max_files"`
	// ReviewBeforePlan has the reviewer check the plan before it runs.
	ReviewBeforePlan     bool `yaml:"review_before_plan"`
	ReviewSingleStepPlan bool `yaml:"review_single_step_plan"`
	// ReviewOnRepeatError asks the reviewer when a subtask fails the same
	// way twice.
	ReviewOnRepeatError bool `yaml:"review_on_repeat_error"`
	// ReviewBeforeDone has the reviewer check the result before the task
	// ends; changes it asks for start a fix round.
	ReviewBeforeDone bool `yaml:"review_before_done"`
	// MaxPlanRevisions is how often the planner may revise a plan the
	// reviewer sent back.
	MaxPlanRevisions int `yaml:"max_plan_revisions"`
	// MaxFixRounds is how many fix rounds failed checks or the final
	// review may start.
	MaxFixRounds int `yaml:"max_fix_rounds"`
	MaxAttempts  int `yaml:"max_attempts"`
	// AgentTimeout stops an agent that runs longer than this.
	AgentTimeout      Duration `yaml:"agent_timeout"`
	SmallTaskWords    int      `yaml:"small_task_words"`
	ApprovePlan       bool     `yaml:"approve_plan"`
	ReviewChanges     bool     `yaml:"review_changes"`
	Handoff           bool     `yaml:"handoff"`
	LowPriority       bool     `yaml:"low_priority"`
	MaxCPUPercent     int      `yaml:"max_cpu_percent"`
	MinFreeMemoryMB   int      `yaml:"min_free_memory_mb"`
	BusyMaxWait       Duration `yaml:"busy_max_wait"`
	MinFreeDiskGB     float64  `yaml:"min_free_disk_gb"`
	PoolWarnGB        float64  `yaml:"pool_warn_gb"`
	PoolMaxIdle       Duration `yaml:"pool_max_idle"`
	SnapshotMaxFileMB int      `yaml:"snapshot_max_file_mb"`
}

// VerifyCfg lists the repo's own checks (tests, build, lint). Agents may
// run them without asking, and Relayweft runs them before the final review.
type VerifyCfg struct {
	// Commands are the checks, run through the system shell in the
	// project folder, e.g. ["go test ./...", "go vet ./..."].
	Commands []string `yaml:"commands"`
	// Timeout is the time limit for each command.
	Timeout Duration `yaml:"timeout"`
	// Affected: "auto" (or "") runs only the tests the changes affect
	// after a fix round, and the full checks before the final review;
	// "off" always runs the full checks.
	Affected string `yaml:"affected,omitempty"`
	// AffectedCommands maps a command to its narrowed form, with {files},
	// {packages} and {test_files}; "off" never narrows that command.
	AffectedCommands map[string]string `yaml:"affected_commands,omitempty"`
}

// HooksCfg runs your own commands around tasks. Each list runs in order in
// the project folder through the system shell, with RW_* environment
// variables describing the task (see README). A failing before_task hook
// stops the task; failures of the others are reported and ignored.
type HooksCfg struct {
	BeforeTask []string `yaml:"before_task,omitempty"`
	AfterMerge []string `yaml:"after_merge,omitempty"` // after each agent's changes land in your tree
	AfterTask  []string `yaml:"after_task,omitempty"`  // every end: done, failed or cancelled
	Timeout    Duration `yaml:"timeout,omitempty"`     // time limit for each hook
}

// WorkspaceCfg makes every task of the project a multi-repo task: Repos
// maps a short name to another git repository (a path relative to the
// project folder), e.g. {frontend: ../web}. Agents write to these folders,
// so a repo file's workspace applies only after `rw trust`.
type WorkspaceCfg struct {
	// Repos maps a short name to another git repository: a path relative
	// to the project folder (to the repo file's folder in a .relayweft.yaml).
	Repos map[string]string `yaml:"repos,omitempty"`
}

// NotifyCfg controls notifications: Enabled turns desktop notifications
// on; Webhooks (Slack, Discord, ntfy) are sent whenever listed. A repo
// file's webhooks apply only after `rw trust`: they say where rw sends
// what your tasks did.
type NotifyCfg struct {
	Enabled bool     `yaml:"enabled"`
	MinTask Duration `yaml:"min_task"` // only tasks that ran at least this long
	// Webhooks (Slack, Discord, ntfy or plain JSON) get done, failed,
	// limit, waiting and watch messages: overnight runs, scheduled tasks
	// and rw watch post here (rw notify --test sends a test message).
	Webhooks []notify.Webhook `yaml:"webhooks,omitempty"`
}

// Redacted returns a copy for display (rw bugreport): webhook URLs and
// tokens are secrets.
func (n NotifyCfg) Redacted() NotifyCfg {
	if n.Webhooks != nil {
		hooks := make([]notify.Webhook, len(n.Webhooks))
		for i, w := range n.Webhooks {
			hooks[i] = w.Redacted()
		}
		n.Webhooks = hooks
	}
	return n
}

// BudgetCfg caps what a task and a day may use (0 = off). Tokens are fresh
// tokens (both providers); usd is Claude's API-equivalent price. When a
// limit is reached, an attended task asks whether to go on; unattended
// tasks (queued, scheduled, --file) and tasks without anyone to ask stop.
type BudgetCfg struct {
	TaskTokens int64   `yaml:"task_tokens" json:"task_tokens"` // fresh tokens one task may use (0 = no limit)
	TaskUSD    float64 `yaml:"task_usd" json:"task_usd"`       // API-equivalent $ one task may cost (0 = no limit)
	DayTokens  int64   `yaml:"day_tokens" json:"day_tokens"`   // fresh tokens today's tasks may use (0 = no limit)
	DayUSD     float64 `yaml:"day_usd" json:"day_usd"`         // API-equivalent $ today's tasks may cost (0 = no limit)
	WarnAt     float64 `yaml:"warn_at" json:"warn_at"`         // warn once at this share of a limit (0 = no warning)
	// Team is a day budget several machines share through a folder.
	Team TeamBudgetCfg `yaml:"team" json:"team"`
}

// Any reports whether any budget limit is set.
func (b BudgetCfg) Any() bool {
	return b.TaskTokens > 0 || b.TaskUSD > 0 || b.DayTokens > 0 || b.DayUSD > 0 || b.Team.Limited()
}

// TeamBudgetCfg is a day budget shared by several machines: each one
// writes its own usage export (the `rw stats --json` format) to a shared
// folder (OneDrive, a network share) after every task, and checks the
// combined day total of every machine there before an agent starts. Dir
// is where rw writes, so a repo file's dir applies only after `rw trust`.
type TeamBudgetCfg struct {
	Dir       string  `yaml:"dir" json:"dir"`               // "" = off; ~ and environment variables ($X, %X%) are expanded
	DayTokens int64   `yaml:"day_tokens" json:"day_tokens"` // fresh tokens all machines together may use today (0 = no limit)
	DayUSD    float64 `yaml:"day_usd" json:"day_usd"`       // API-equivalent $ all machines together may spend today (0 = no limit)
}

// Limited reports whether the team folder is set and has a limit.
func (t TeamBudgetCfg) Limited() bool {
	return t.Dir != "" && (t.DayTokens > 0 || t.DayUSD > 0)
}

var reWinEnv = regexp.MustCompile(`%([A-Za-z_][A-Za-z0-9_()]*)%`)

// Folder is Dir with ~ and environment variables expanded ("" when off).
// It must be absolute: a relative folder would depend on where rw started.
func (t TeamBudgetCfg) Folder() (string, error) {
	d := strings.TrimSpace(t.Dir)
	if d == "" {
		return "", nil
	}
	d = reWinEnv.ReplaceAllStringFunc(d, func(m string) string {
		if v, ok := os.LookupEnv(m[1 : len(m)-1]); ok {
			return v
		}
		return m
	})
	d = os.ExpandEnv(d)
	if d == "~" || strings.HasPrefix(d, "~/") || strings.HasPrefix(d, `~\`) {
		if h, err := os.UserHomeDir(); err == nil {
			d = filepath.Join(h, d[1:])
		}
	}
	if !filepath.IsAbs(d) {
		return "", fmt.Errorf("budget.team.dir %q is not an absolute path", t.Dir)
	}
	return filepath.Clean(d), nil
}

// ContextCfg controls what the planner and the final reviewer learn from
// the repo's own documents (CONTRIBUTING, the PR template, CI workflows,
// AGENTS.md...). The summary is built without a model call and is shown
// as untrusted repo data: none of it becomes a command.
type ContextCfg struct {
	RepoDocs      bool `yaml:"repo_docs"`        // give the planner and the final reviewer the summary
	RepoDocsMaxKB int  `yaml:"repo_docs_max_kb"` // total size of the summary (0 = 8)
}

// WatchCfg controls rw watch: the pull requests rw opened get follow-up
// tasks for failed checks and review comments.
type WatchCfg struct {
	// MaxRounds caps the follow-up tasks per pull request (0 = none: rw
	// watch only reports).
	MaxRounds int `yaml:"max_rounds"`
}

// Config is the whole file.
type Config struct {
	// Roles are the jobs in a task (planner, worker, worker_high, explorer,
	// researcher, reviewer, judge), each with a route on every provider.
	Roles map[string]RoleCfg `yaml:"roles"`
	// Providers are the agent CLIs rw drives: codex and claude, and gemini,
	// deepseek, qwen, ollama and ollama-run (all off until you turn them
	// on), or any CLI you describe (docs/providers.md).
	Providers map[string]ProviderCfg `yaml:"providers"`
	// Routing tunes how each step's role and provider are picked.
	Routing RoutingCfg `yaml:"routing"`
	// Orchestrator tunes a task's lifecycle: parallel agents, reviews,
	// retries, and what keeps your machine responsive.
	Orchestrator OrchestratorCfg `yaml:"orchestrator"`
	Verify       VerifyCfg       `yaml:"verify"`
	Notify       NotifyCfg       `yaml:"notify"`
	Hooks        HooksCfg        `yaml:"hooks"`
	// MCP gives the agents MCP servers, passed to both CLIs on every run
	// of the listed roles. ${VAR} in any value is read from your
	// environment when the agent starts, so secrets stay out of the file.
	// `rw doctor` checks them.
	MCP MCPCfg `yaml:"mcp,omitempty"`
	// Workspace makes every task of the project a multi-repo task.
	Workspace WorkspaceCfg `yaml:"workspace,omitempty"`
	Sandbox   SandboxCfg   `yaml:"sandbox,omitempty"`
	Budget    BudgetCfg    `yaml:"budget"`
	Context   ContextCfg   `yaml:"context"`
	Watch     WatchCfg     `yaml:"watch"`
	// LimitPatterns are case-insensitive regular expressions: CLI output
	// that matches one means "usage limit reached".
	LimitPatterns []string `yaml:"limit_patterns"`
	// Theme is the TUI's look: auto, unicode or ascii (ascii is picked
	// automatically on the legacy console).
	Theme string `yaml:"theme"`
	// LogDir is where the session logs go ("" = <user config
	// dir>/relayweft/sessions).
	LogDir string `yaml:"log_dir"`
	// Learned are the learned routes in effect, per role (learned.go): set
	// by Store.ApplyLearned, read by the router for its reasons, never saved.
	Learned map[string]LearnedRoute `yaml:"-" json:"-"`
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
// ./relayweft.yaml and then <user config dir>/relayweft/relayweft.yaml are
// tried. The returned path is where Save writes; it is ./relayweft.yaml when
// no file was found.
//
// A ./relayweft.yaml found this way may have come with a cloned
// repository, so like a repo's .relayweft.yaml its settings that run
// commands apply only once trusted (see LoadInfo).
func Load(path string) (*Config, string, error) {
	c, p, _, err := LoadInfo(path)
	return c, p, err
}

// LoadInfo is Load, and also returns the settings of an untrusted
// ./relayweft.yaml that were ignored (verify, hooks, providers, ...: see
// commandKeys). Those come from the user config, or the defaults, instead,
// until `rw trust` (or rw itself saving the file) trusts what the file
// sets for them. An explicit path and the user config are yours and always
// apply in full.
func LoadInfo(path string) (*Config, string, []string, error) {
	candidates := []string{}
	if path != "" {
		candidates = append(candidates, path)
	} else {
		candidates = append(candidates, FileName)
		if dir, err := os.UserConfigDir(); err == nil {
			candidates = append(candidates, filepath.Join(dir, "relayweft", FileName))
		}
	}
	for i, p := range candidates {
		data, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			if path != "" {
				return nil, "", nil, fmt.Errorf("config %s not found", path)
			}
			continue
		}
		if err != nil {
			return nil, "", nil, err
		}
		c, err := parseFile(p, data)
		if err != nil {
			return nil, "", nil, err
		}
		var ignored []string
		if path == "" && i == 0 && !IsLocalTrusted(p, data) {
			ignored = guardLocal(c, data, candidates[1:])
		}
		if err := c.Validate(); err != nil {
			return nil, "", nil, fmt.Errorf("%s: %w", p, err)
		}
		abs, _ := filepath.Abs(p)
		return c, abs, ignored, nil
	}
	abs, _ := filepath.Abs(FileName)
	return Default(), abs, nil, nil
}

// parseFile reads a config file's content over the defaults.
func parseFile(p string, data []byte) (*Config, error) {
	c := Default()
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	// Before guardLocal: an untrusted file's provider entries are merged
	// here and then put back as a whole.
	if err := c.mergeProviderEntries(data); err != nil {
		return nil, fmt.Errorf("%s: providers: %w", p, err)
	}
	def := Default()
	c.fillProviderDefaults(def)
	c.fillRoleDefaults(def)
	return c, nil
}

// guardLocal puts back the settings that run commands in c, read from an
// untrusted ./relayweft.yaml (data), from the first of the user's own
// config files that loads, or the defaults; it returns those the file set
// to something else.
func guardLocal(c *Config, data []byte, own []string) []string {
	base := Default()
	for _, p := range own {
		if d, err := os.ReadFile(p); err == nil {
			if b, err := parseFile(p, d); err == nil && b.Validate() == nil {
				base = b
			}
			break
		}
	}
	// Restored whatever YAML reached them (merge keys, anchors); reported
	// only where the file itself sets them, since your own config differs
	// from the defaults the file was read over.
	changed := restoreCommandSettings(c, base)
	// Best of N multiplies what a step costs: like a repo file, the local
	// file may lower it, not raise it above your own config.
	var ignored []string
	if bestOfRaised(base.Routing.BestOf, c.Routing.BestOf) {
		c.Routing.BestOf = base.Routing.BestOf
		ignored = append(ignored, bestOfKey)
	}
	set, err := trustSubset(data)
	if err != nil {
		return append(changed, ignored...) // unreadable as a plain mapping: report all
	}
	for _, k := range changed {
		if _, ok := set[k]; ok {
			ignored = append(ignored, k)
		}
	}
	return ignored
}

// mergeProviderEntries decodes the file's provider entries field by field
// over the presets: YAML decoding replaces a whole map entry, so
// `qwen: {disabled: false}` would otherwise drop the preset's
// only_preferred, env and extra_args.
func (c *Config) mergeProviderEntries(data []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil // Unmarshal into the struct already reported real errors
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "providers" {
			continue
		}
		base := Default().Providers
		if err := mergeMap(root.Content[i+1], base); err != nil {
			return err
		}
		c.Providers = base
	}
	return nil
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
		if v.Kind == "" {
			v.Kind = d.Kind
		}
		if v.Label == "" {
			v.Label = d.Label
		}
		if v.Env == nil {
			v.Env = d.Env
		}
		if v.InstallHint == "" {
			v.InstallHint = d.InstallHint
		}
		if v.Generic == nil {
			v.Generic = d.Generic
		}
		if v.Sandbox == nil {
			v.Sandbox = d.Sandbox
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
		_ = rc // prefer and routes: validateProviders
	}
	errs = append(errs, c.validateProviders()...)
	for _, pat := range c.LimitPatterns {
		if _, err := regexp.Compile("(?i)" + pat); err != nil {
			errs = append(errs, fmt.Sprintf("limit pattern %q: %v", pat, err))
		}
	}
	if c.Orchestrator.MaxThreads < 1 {
		errs = append(errs, "orchestrator.max_threads must be >= 1")
	}
	errs = append(errs, c.MCP.validate()...)
	errs = append(errs, c.validateSandbox()...)
	if b := c.Budget; b.TaskTokens < 0 || b.TaskUSD < 0 || b.DayTokens < 0 || b.DayUSD < 0 {
		errs = append(errs, "budget limits must be >= 0 (0 = off)")
	}
	if tb := c.Budget.Team; tb.DayTokens < 0 || tb.DayUSD < 0 {
		errs = append(errs, "budget.team limits must be >= 0 (0 = off)")
	}
	if c.Context.RepoDocsMaxKB < 0 {
		errs = append(errs, "context.repo_docs_max_kb must be >= 0 (0 = 8)")
	}
	if c.Watch.MaxRounds < 0 {
		errs = append(errs, "watch.max_rounds must be >= 0")
	}
	for i, w := range c.Notify.Webhooks {
		if err := w.Validate(); err != nil {
			errs = append(errs, fmt.Sprintf("notify.webhooks[%d]: %v", i, err))
		}
	}
	if w := c.Budget.WarnAt; w < 0 || w > 1 {
		errs = append(errs, "budget.warn_at must be between 0 and 1")
	}
	switch c.Theme {
	case "", "auto", "unicode", "ascii":
	default:
		errs = append(errs, "theme must be auto, unicode or ascii")
	}
	switch c.Routing.Learn {
	case "", LearnAuto, LearnSuggest, LearnOff:
	default:
		errs = append(errs, "routing.learn must be auto, suggest or off")
	}
	switch c.Routing.Tiers {
	case "", TiersAuto, TiersOff:
	default:
		errs = append(errs, "routing.tiers must be auto or off")
	}
	switch c.Verify.Affected {
	case "", "auto", "off":
	default:
		errs = append(errs, "verify.affected must be auto or off")
	}
	for cmd, tmpl := range c.Verify.AffectedCommands {
		if tmpl != "off" && !strings.Contains(tmpl, "{files}") && !strings.Contains(tmpl, "{packages}") && !strings.Contains(tmpl, "{test_files}") {
			errs = append(errs, fmt.Sprintf("verify.affected_commands[%q] needs {files}, {packages} or {test_files} (or off)", cmd))
		}
	}
	if v := c.Routing.TiersSaveAt; v < 0 || v > 1 {
		errs = append(errs, "routing.tiers_save_below must be between 0 and 1 (0 = 0.5)")
	}
	if c.Routing.LearnMinSamples < 0 {
		errs = append(errs, "routing.learn_min_samples must be >= 0 (0 = 8)")
	}
	switch bo := c.Routing.BestOf; {
	case bo.When != "" && bo.When != BestOfOff && !bo.On():
		errs = append(errs, "routing.best_of.when must be off, hard or always")
	case bo.N != 0 && (bo.N < 2 || bo.N > MaxBestOf):
		errs = append(errs, fmt.Sprintf("routing.best_of.n must be between 2 and %d (0 = 2)", MaxBestOf))
	}
	for _, r := range c.Routing.BestOf.Routes {
		if p, _, _ := strings.Cut(strings.TrimSpace(r), ":"); p == "" {
			errs = append(errs, fmt.Sprintf("routing.best_of.routes: %q needs a provider (provider[:model[:effort]])", r))
		}
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
	out.Learned = cloneLearned(c.Learned) // not in the YAML
	return out
}

// Save writes the config to path.
func (c *Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	header := "# Relayweft configuration (saved by rw). See `rw init --print` for the commented default.\n"
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
		return filepath.Join(dir, "relayweft", "sessions")
	}
	return filepath.Join(".relayweft", "sessions")
}

// ParseRouteSpec parses "provider:model[:effort]" (effort may be empty).
// The model may itself contain a colon (Ollama tags such as qwen3.6:35b):
// a trailing part is the effort only when it is an effort word, or empty.
// Callers with a config use ParseRouteFor, which also checks the provider.
func ParseRouteSpec(s string) (provider string, r Route, err error) {
	return parseRoute(s, nil)
}

// ParseRouteFor parses a route like ParseRouteSpec and checks it against
// the config: the provider must be configured, and a trailing part is the
// effort only when it is one of that provider's efforts.
func (c *Config) ParseRouteFor(s string) (provider string, r Route, err error) {
	return parseRoute(s, c)
}

func parseRoute(s string, c *Config) (provider string, r Route, err error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) < 2 || parts[1] == "" {
		return "", r, fmt.Errorf("route %q: want provider:model[:effort]", s)
	}
	provider = strings.ToLower(parts[0])
	if !providerName.MatchString(provider) {
		return "", r, fmt.Errorf("route %q: bad provider name %q", s, parts[0])
	}
	efforts := effortWords
	if c != nil {
		pc, ok := c.Providers[provider]
		if !ok {
			return "", r, fmt.Errorf("route %q: unknown provider %q (providers: %s)", s, provider, strings.Join(c.ProviderNames(), ", "))
		}
		efforts = pc.Efforts
	}
	r.Model = parts[1]
	if i := strings.LastIndex(r.Model, ":"); i >= 0 {
		if last := r.Model[i+1:]; last == "" || contains(efforts, last) {
			r.Model, r.Effort = r.Model[:i], last
		}
	}
	if r.Model == "" || strings.HasPrefix(r.Model, ":") || strings.HasSuffix(r.Model, ":") {
		return "", r, fmt.Errorf("route %q: want provider:model[:effort]", s)
	}
	if msg := checkRoute(r); msg != "" {
		return "", r, fmt.Errorf("route %q: %s", s, msg)
	}
	if i := strings.LastIndex(r.Model, ":"); i >= 0 && r.Effort == "" && contains(efforts, r.Model[i+1:]) {
		// "m:high:" would print back as "m:high", which reads as model m.
		return "", r, fmt.Errorf("route %q: ambiguous - is %q the effort?", s, r.Model[i+1:])
	}
	return provider, r, nil
}

// effortWords are the efforts any built-in CLI knows.
var effortWords = []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// Store is a concurrency-safe holder for the live config, edited by the TUI
// while the orchestrator reads it.
type Store struct {
	mu   sync.RWMutex
	cfg  *Config
	path string
	// base is the config without the repo file and the learned routes
	// (what Save writes), nil when neither was applied.
	base *Config
	repo RepoInfo
	// learned are the learned routes (learned.go) and pinned the roles
	// set explicitly (repo file, flags, session edits), which they skip.
	learned *Learned
	pinned  map[string]bool
}

// ApplyRepo layers the project's .relayweft.yaml (if any) over the live
// config. Save keeps writing the user's file without it.
func (s *Store) ApplyRepo(dir string) (RepoInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := s.cfg.Clone()
	if s.base != nil {
		base = s.base.Clone() // without learned routes: they go back on below
	}
	live := base.Clone()
	info, err := ApplyRepo(live, dir)
	if err != nil {
		return info, err
	}
	if info.Path != "" || s.base != nil {
		s.base, s.cfg = base, live
		s.pinRepoRoles(base, live)
		s.layerLearned()
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
// <dir>/.relayweft.yaml when there is none yet) and returns its path.
func (s *Store) SaveRepo(dir string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.repo.Path
	if p == "" {
		p = filepath.Join(dir, RepoFileName)
	}
	if err := SaveRepo(s.unlearnedLocked(), p); err != nil { // learned routes stay yours
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

// Budget returns the live budget limits without cloning the whole config
// (UIs read it every frame).
func (s *Store) Budget() BudgetCfg {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Budget
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
	s.pinEdited(next) // an edited role wins over its learned route
	s.cfg = next
	return nil
}

// Save persists the live config. You saved it, so a ./relayweft.yaml
// written here is trusted as it now is.
func (s *Store) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.cfg
	if s.base != nil {
		c = s.base // never bake the repo file into yours
	}
	if err := c.Save(s.path); err != nil {
		return err
	}
	return TrustLocal(s.path)
}

// SetRoute changes one role's route on one provider.
func (s *Store) SetRoute(role, provider string, r Route) (err error) {
	defer s.pinIfOK(role, &err) // set explicitly, even to the value it had
	return s.Update(func(c *Config) error {
		rc, ok := c.Roles[role]
		if !ok {
			return fmt.Errorf("unknown role %q (roles: %s)", role, strings.Join(event.Roles, ", "))
		}
		if !c.IsProvider(provider) {
			return fmt.Errorf("unknown provider %q (providers: %s)", provider, strings.Join(c.ProviderNames(), ", "))
		}
		c.Roles[role] = rc.With(provider, r)
		return nil
	})
}

// SetPrefer changes which provider a role prefers.
func (s *Store) SetPrefer(role, prefer string) (err error) {
	defer s.pinIfOK(role, &err)
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
