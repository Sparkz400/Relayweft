package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/sparkz400/switchyard/internal/notify"
	"gopkg.in/yaml.v3"
)

// Per-repo settings: a .switchyard.yaml committed in the repository root
// (or the project folder) is layered on top of the user's config:
//
//	built-in defaults < user config (switchyard.yaml) < learned routes
//	  < repo file < flags
//
// (learned routes: see learned.go). It holds only what it sets; maps
// (roles, providers) merge per key, so `roles: {worker: {prefer: claude}}`
// keeps the worker's routes.
//
// A repo file comes from whoever pushed to the repo, so the parts that run
// commands on your machine (verify commands, hooks, provider commands and
// arguments, the log directory, MCP servers) apply only after you trust that exact
// content with `sy trust`, and so do budget.team.dir (where sy writes its
// usage file) and notify.webhooks (where sy sends what your tasks did).
// Routes, preferences and toggles always apply.

// RepoFileName is the per-repo settings file.
const RepoFileName = ".switchyard.yaml"

// commandKeys are the top-level keys that need trust.
// workspace is here too: its repos are folders agents may write to, and
// a path (absolute, or a UNC share on Windows) must not come from a repo
// file nobody reviewed.
var commandKeys = []string{"verify", "hooks", "providers", "log_dir", "mcp", "workspace"}

// RepoInfo describes the repo file applied to a config.
type RepoInfo struct {
	Path    string   // "" when there is none
	Trusted bool     // the command-running parts were applied
	Ignored []string // keys skipped because the file is not trusted
}

// FindRepoFile returns the .switchyard.yaml for dir: in dir or the nearest
// parent that is a git repository root ("" if none).
func FindRepoFile(dir string) string {
	d, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		p := filepath.Join(d, RepoFileName)
		if _, err := os.Stat(p); err == nil {
			return p
		}
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return "" // the repo root has none
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// ApplyRepo layers the repo file for dir onto c.
func ApplyRepo(c *Config, dir string) (RepoInfo, error) {
	p := FindRepoFile(dir)
	if p == "" {
		return RepoInfo{}, nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return RepoInfo{}, err
	}
	info := RepoInfo{Path: p, Trusted: IsTrusted(p, data)}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return info, fmt.Errorf("%s: %w", p, err)
	}
	if len(doc.Content) == 0 {
		return info, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return info, fmt.Errorf("%s: want a mapping at the top level", p)
	}
	// Snapshot of everything that runs commands. An untrusted file gets
	// these restored after decoding, whatever YAML it used to reach them
	// (aliases, merge keys "<<: *x", anchors): the key check below is only
	// a first filter and must not be the only guard.
	guarded := c.Clone()
	rest := &yaml.Node{Kind: yaml.MappingNode}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if !info.Trusted && contains(commandKeys, k.Value) {
			info.Ignored = append(info.Ignored, k.Value)
			continue
		}
		switch k.Value {
		case "roles":
			if err := mergeMap(v, c.Roles); err != nil {
				return info, fmt.Errorf("%s: roles: %w", p, err)
			}
		case "providers":
			if err := mergeMap(v, c.Providers); err != nil {
				return info, fmt.Errorf("%s: providers: %w", p, err)
			}
		default:
			rest.Content = append(rest.Content, k, v)
		}
	}
	if len(rest.Content) > 0 {
		if err := rest.Decode(c); err != nil {
			return info, fmt.Errorf("%s: %w", p, err)
		}
	}
	if !info.Trusted {
		for _, k := range restoreCommandSettings(c, guarded) {
			if !contains(info.Ignored, k) {
				info.Ignored = append(info.Ignored, k)
			}
		}
	}
	// A repo file may tighten your budget, never loosen it (trusted or not).
	c.Budget = stricterBudget(guarded.Budget, c.Budget)
	// Likewise the follow-up rounds sy watch may run unattended.
	if c.Watch.MaxRounds > guarded.Watch.MaxRounds || c.Watch.MaxRounds < 0 {
		c.Watch.MaxRounds = guarded.Watch.MaxRounds
	}
	if err := c.Validate(); err != nil {
		return info, fmt.Errorf("%s: %w", p, err)
	}
	return info, nil
}

// restoreCommandSettings puts back the settings that run commands (the
// commandKeys) from before, and returns the ones the file had changed.
func restoreCommandSettings(c, before *Config) []string {
	var changed []string
	if !reflect.DeepEqual(c.Verify, before.Verify) {
		changed = append(changed, "verify")
	}
	if !reflect.DeepEqual(c.Hooks, before.Hooks) {
		changed = append(changed, "hooks")
	}
	if !reflect.DeepEqual(c.Providers, before.Providers) {
		changed = append(changed, "providers")
	}
	if c.LogDir != before.LogDir {
		changed = append(changed, "log_dir")
	}
	if !reflect.DeepEqual(c.MCP, before.MCP) {
		changed = append(changed, "mcp")
	}
	if !reflect.DeepEqual(c.Workspace, before.Workspace) {
		changed = append(changed, "workspace")
	}
	// The team folder is where sy writes its usage file: like log_dir, a
	// path nobody reviewed must not decide where sy writes.
	if c.Budget.Team.Dir != before.Budget.Team.Dir {
		changed = append(changed, teamDirKey)
	}
	// Webhooks send task summaries off the machine: a URL nobody reviewed
	// must not decide where they go.
	if !reflect.DeepEqual(c.Notify.Webhooks, before.Notify.Webhooks) {
		changed = append(changed, webhooksKey)
	}
	c.Notify.Webhooks = before.Notify.Webhooks
	c.Verify, c.Hooks, c.Providers, c.LogDir, c.MCP, c.Workspace = before.Verify, before.Hooks, before.Providers, before.LogDir, before.MCP, before.Workspace
	c.Budget.Team.Dir = before.Budget.Team.Dir
	dropUnknownProviders(c, before)
	return changed
}

// dropUnknownProviders removes what still names a provider the file added
// but restoreCommandSettings took out again (a route, prefer, the provider
// order): otherwise the whole config fails validation instead of loading
// without the untrusted provider.
func dropUnknownProviders(c, before *Config) {
	for role, rc := range c.Roles {
		if rc.Prefer != PreferOther && rc.Prefer != PreferAuto && !c.IsProvider(rc.Prefer) {
			rc.Prefer = before.Roles[role].Prefer
		}
		var extra map[string]Route
		for p, r := range rc.Extra {
			if c.IsProvider(p) {
				if extra == nil {
					extra = map[string]Route{}
				}
				extra[p] = r
			}
		}
		rc.Extra = extra
		c.Roles[role] = rc
	}
	var order []string
	for _, p := range c.Routing.ProviderOrder {
		if c.IsProvider(p) {
			order = append(order, p)
		}
	}
	c.Routing.ProviderOrder = order
}

// teamDirKey is the one budget setting that needs trust.
const teamDirKey = "budget.team.dir"

// webhooksKey is the one notify setting that needs trust.
const webhooksKey = "notify.webhooks"

// stricterBudget keeps the tighter of two budgets per limit (0 = no limit).
func stricterBudget(mine, repo BudgetCfg) BudgetCfg {
	tighterI := func(a, b int64) int64 {
		if a <= 0 || (b > 0 && b < a) {
			return b
		}
		return a
	}
	tighterF := func(a, b float64) float64 {
		if a <= 0 || (b > 0 && b < a) {
			return b
		}
		return a
	}
	out := mine
	out.TaskTokens = tighterI(mine.TaskTokens, repo.TaskTokens)
	out.DayTokens = tighterI(mine.DayTokens, repo.DayTokens)
	out.TaskUSD = tighterF(mine.TaskUSD, repo.TaskUSD)
	out.DayUSD = tighterF(mine.DayUSD, repo.DayUSD)
	if repo.WarnAt > 0 && (mine.WarnAt <= 0 || repo.WarnAt < mine.WarnAt) {
		out.WarnAt = repo.WarnAt
	}
	out.Team.DayTokens = tighterI(mine.Team.DayTokens, repo.Team.DayTokens)
	out.Team.DayUSD = tighterF(mine.Team.DayUSD, repo.Team.DayUSD)
	// The folder is not a limit: it is the repo file's only when trusted
	// (restoreCommandSettings put yours back otherwise).
	out.Team.Dir = repo.Team.Dir
	return out
}

// mergeMap decodes each entry of a mapping node on top of the existing
// value, so unset fields keep their value.
func mergeMap[V any](n *yaml.Node, m map[string]V) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("want a mapping")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i].Value
		v := m[key]
		if err := n.Content[i+1].Decode(&v); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		m[key] = v
	}
	return nil
}

// trustPath is the list of trusted repo files (path -> content hash).
func trustPath() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "switchyard", "trusted.json")
}

func loadTrust() map[string]string {
	m := map[string]string{}
	if data, err := os.ReadFile(trustPath()); err == nil {
		json.Unmarshal(data, &m)
	}
	return m
}

func contentHash(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func trustKey(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	// The same file through a symlinked folder (macOS: /var -> /private/var)
	// must keep its trust.
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return strings.ToLower(filepath.ToSlash(abs))
}

// trustSubset is what a local config file (./switchyard.yaml) sets among
// the settings that need trust, keyed like CommandSettings' lines.
func trustSubset(data []byte) (map[string]any, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, k := range commandKeys {
		if v, ok := raw[k]; ok {
			out[k] = v
		}
	}
	if b, ok := raw["budget"].(map[string]any); ok {
		if t, ok := b["team"].(map[string]any); ok {
			if d, ok := t["dir"]; ok {
				out[teamDirKey] = d
			}
		}
	}
	if n, ok := raw["notify"].(map[string]any); ok {
		if w, ok := n["webhooks"]; ok {
			out[webhooksKey] = w
		}
	}
	return out, nil
}

// localHash hashes only what a local config file sets among the settings
// that need trust (yaml.v3 writes map keys sorted), so editing its routes
// or toggles by hand keeps the trust; a change to what it runs does not.
func localHash(data []byte) (string, error) {
	set, err := trustSubset(data)
	if err != nil {
		return "", err
	}
	b, err := yaml.Marshal(set)
	if err != nil {
		return "", err
	}
	return "local:" + contentHash(b), nil
}

// IsLocalTrusted reports whether what the local config file at path (with
// content data) sets among the settings that run commands was trusted.
func IsLocalTrusted(path string, data []byte) bool {
	h, err := localHash(data)
	return err == nil && loadTrust()[trustKey(path)] == h
}

// TrustLocal trusts what the local config file at path now sets among the
// settings that run commands (a later change to them asks again). A
// missing file is not an error.
func TrustLocal(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	h, err := localHash(data)
	if err != nil {
		return err
	}
	m := loadTrust()
	if m[trustKey(path)] == h {
		return nil
	}
	m[trustKey(path)] = h
	return writeTrust(m)
}

// IsTrusted reports whether this exact content of path was trusted.
func IsTrusted(path string, data []byte) bool {
	return loadTrust()[trustKey(path)] == contentHash(data)
}

// Trust records the current content of a repo file as trusted (any later
// change needs a new `sy trust`).
func Trust(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	m := loadTrust()
	m[trustKey(path)] = contentHash(data)
	return writeTrust(m)
}

// Untrust forgets a repo file.
func Untrust(path string) error {
	m := loadTrust()
	delete(m, trustKey(path))
	return writeTrust(m)
}

func writeTrust(m map[string]string) error {
	p := trustPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// CommandSettings lists what the repo file would run, for `sy trust` to
// show before trusting.
func CommandSettings(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	var out []string
	for _, k := range commandKeys {
		if v, ok := raw[k]; ok {
			b, _ := yaml.Marshal(map[string]any{k: v})
			out = append(out, strings.TrimRight(string(b), "\n"))
		}
	}
	if b, ok := raw["budget"].(map[string]any); ok {
		if t, ok := b["team"].(map[string]any); ok {
			if d, ok := t["dir"]; ok {
				out = append(out, fmt.Sprintf("%s: %v (sy writes this machine's usage file there)", teamDirKey, d))
			}
		}
	}
	if n, ok := raw["notify"].(map[string]any); ok {
		if hooks, ok := n["webhooks"].([]any); ok && len(hooks) > 0 {
			var where []string
			for _, h := range hooks {
				if m, ok := h.(map[string]any); ok {
					w := notify.Webhook{URL: fmt.Sprint(m["url"])}
					if k, ok := m["kind"].(string); ok {
						w.Kind = k
					}
					where = append(where, w.Name())
				}
			}
			out = append(out, fmt.Sprintf("%s: %s (sy sends task results there)", webhooksKey, strings.Join(where, ", ")))
		}
	}
	sort.Strings(out)
	return out, nil
}

// SaveRepo writes the shareable parts of c (routes and preferences, verify
// commands, hooks, approval/review toggles) into the repo file at path,
// keeping whatever else it already sets.
func SaveRepo(c *Config, path string) error {
	var doc map[string]any
	if data, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}
	roles := map[string]any{}
	for name, r := range c.Roles {
		roles[name] = r
	}
	doc["roles"] = roles
	doc["verify"] = c.Verify
	doc["hooks"] = c.Hooks
	orch, _ := doc["orchestrator"].(map[string]any)
	if orch == nil {
		orch = map[string]any{}
	}
	orch["approve_plan"] = c.Orchestrator.ApprovePlan
	orch["review_changes"] = c.Orchestrator.ReviewChanges
	orch["handoff"] = c.Orchestrator.Handoff
	doc["orchestrator"] = orch
	data, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	header := "# Switchyard settings for this repository (sy merges them over your own config).\n# Commands here (verify, hooks) run only after each person runs `sy trust`.\n"
	if err := os.WriteFile(path, append([]byte(header), data...), 0o644); err != nil {
		return err
	}
	// You wrote it: trust it.
	return Trust(path)
}
