// Package workflow stores explicit, user-owned task recipes. Repositories cannot
// install or replace recipes implicitly, because recipes can run check commands.
package workflow

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/sparkz400/relayweft/internal/config"
	"gopkg.in/yaml.v3"
)

type Definition struct {
	Name          string   `yaml:"name" json:"name"`
	Description   string   `yaml:"description" json:"description"`
	Prompt        string   `yaml:"prompt" json:"prompt"`
	Checks        []string `yaml:"checks" json:"checks"`
	TaskTokens    int64    `yaml:"task_tokens" json:"task_tokens"`
	TaskUSD       float64  `yaml:"task_usd" json:"task_usd"`
	ApprovePlan   bool     `yaml:"approve_plan" json:"approve_plan"`
	ReviewChanges bool     `yaml:"review_changes" json:"review_changes"`
	RequireChecks bool     `yaml:"require_checks" json:"require_checks"`
}

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

func Dir() (string, error) {
	d, err := os.UserConfigDir()
	return filepath.Join(d, "relayweft", "workflows"), err
}

func Parse(data []byte) (Definition, error) {
	var d Definition
	if len(data) > 64<<10 {
		return d, errors.New("workflow exceeds 64 KiB")
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return d, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return d, errors.New("a workflow must contain one YAML document")
	}
	if !validName.MatchString(d.Name) || !strings.Contains(d.Prompt, "{{task}}") || strings.TrimSpace(d.Description) == "" {
		return d, errors.New("workflow needs a lowercase name, description and a prompt containing {{task}}")
	}
	if d.TaskTokens < 0 || d.TaskUSD < 0 || math.IsNaN(d.TaskUSD) || math.IsInf(d.TaskUSD, 0) {
		return d, errors.New("workflow budgets must be finite and nonnegative")
	}
	for _, c := range d.Checks {
		if strings.TrimSpace(c) == "" || strings.ContainsRune(c, 0) {
			return d, errors.New("workflow contains an empty or invalid check")
		}
	}
	return d, nil
}

func Load(name string) (Definition, error) {
	if !validName.MatchString(name) {
		return Definition{}, errors.New("invalid workflow name")
	}
	dir, err := Dir()
	if err != nil {
		return Definition{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, name+".yaml"))
	if err != nil {
		return Definition{}, err
	}
	d, err := Parse(data)
	if err == nil && d.Name != name {
		return d, errors.New("workflow name differs from its filename")
	}
	return d, err
}

func Save(data []byte, replace bool) error {
	d, err := Parse(data)
	if err != nil {
		return err
	}
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	p := filepath.Join(dir, d.Name+".yaml")
	if !replace {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
	f, err := os.CreateTemp(dir, ".workflow-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func List() ([]Definition, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	ds := []Definition{}
	for _, p := range files {
		d, err := Load(strings.TrimSuffix(filepath.Base(p), ".yaml"))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		ds = append(ds, d)
	}
	return ds, nil
}

func (d Definition) Render(task string) (string, error) {
	if strings.TrimSpace(task) == "" {
		return "", errors.New("describe the workflow's task")
	}
	return strings.ReplaceAll(d.Prompt, "{{task}}", task), nil
}

// Apply preserves the repo's checks and stricter budgets. It changes only the
// in-memory configuration for this run, never saved config or trusted files.
func (d Definition) Apply(c *config.Config) error {
	for _, check := range d.Checks {
		if !slices.Contains(c.Verify.Commands, check) {
			c.Verify.Commands = append(c.Verify.Commands, check)
		}
	}
	if d.RequireChecks && len(c.Verify.Commands) == 0 {
		return errors.New("workflow requires checks: run rw init or add checks to the workflow")
	}
	d.Tighten(&c.Budget)
	if d.ApprovePlan {
		c.Orchestrator.ApprovePlan = true
	}
	if d.ReviewChanges {
		c.Orchestrator.ReviewChanges = true
	}
	return nil
}

// Tighten lowers b's per-task limits to the workflow's; it never raises
// one, and a zero workflow budget leaves the limit as it is.
func (d Definition) Tighten(b *config.BudgetCfg) {
	if d.TaskTokens > 0 && (b.TaskTokens == 0 || d.TaskTokens < b.TaskTokens) {
		b.TaskTokens = d.TaskTokens
	}
	if d.TaskUSD > 0 && (b.TaskUSD == 0 || d.TaskUSD < b.TaskUSD) {
		b.TaskUSD = d.TaskUSD
	}
}

// Gated reports whether the workflow waits for a person (plan approval or
// change review). Its gates hold even when the task is queued or scheduled.
func (d Definition) Gated() bool { return d.ApprovePlan || d.ReviewChanges }

func Builtins() []Definition {
	return []Definition{
		{Name: "bugfix", Description: "Reproduce, fix and verify a bug", Prompt: "Fix this bug: {{task}}\nFirst reproduce it with a failing test. Make the smallest complete fix, rerun that test and the relevant suite, and report the cause and evidence.", TaskUSD: 10, TaskTokens: 200000, ApprovePlan: true, ReviewChanges: true, RequireChecks: true},
		{Name: "dependency-upgrade", Description: "Upgrade a dependency and verify compatibility", Prompt: "Upgrade this dependency: {{task}}\nInspect current usage, update the dependency and lockfile together, adapt affected APIs, run tests and build checks, and document any migration needed.", TaskUSD: 10, TaskTokens: 200000, ApprovePlan: true, ReviewChanges: true, RequireChecks: true},
		{Name: "review", Description: "Review code and produce evidence-backed findings", Prompt: "Review: {{task}}\nInspect behavior and failure paths. Reproduce suspected defects in isolated tests. Report actionable findings with file references and distinguish verified problems from uncertainties. Do not change production code.", TaskUSD: 5, TaskTokens: 100000, ApprovePlan: true, ReviewChanges: true},
		{Name: "release-prep", Description: "Prepare release notes and validate the release", Prompt: "Prepare this release: {{task}}\nCheck version consistency, changelog, migration notes and build/test results. Prepare the requested release artifacts and report unresolved gates. Do not publish, tag or deploy.", TaskUSD: 10, TaskTokens: 200000, ApprovePlan: true, ReviewChanges: true, RequireChecks: true},
	}
}
