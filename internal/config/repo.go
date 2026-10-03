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

	"gopkg.in/yaml.v3"
)

// Per-repo settings: a .switchyard.yaml committed in the repository root
// (or the project folder) is layered on top of the user's config:
//
//	built-in defaults < user config (switchyard.yaml) < repo file < flags
//
// It holds only what it sets; maps (roles, providers) merge per key, so
// `roles: {worker: {prefer: claude}}` keeps the worker's routes.
//
// A repo file comes from whoever pushed to the repo, so the parts that run
// commands on your machine (verify commands, hooks, provider commands and
// arguments, the log directory, MCP servers) apply only after you trust that exact
// content with `sy trust`. Routes, preferences and toggles always apply.

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
	c.Verify, c.Hooks, c.Providers, c.LogDir, c.MCP, c.Workspace = before.Verify, before.Hooks, before.Providers, before.LogDir, before.MCP, before.Workspace
	return changed
}

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
