package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sparkz400/switchyard/internal/event"
)

func TestValidateGeneric(t *testing.T) {
	plain := func() *GenericCfg { return &GenericCfg{Args: []string{"run", "{model}"}, NoTools: true} }
	for _, c := range []struct {
		name string
		edit func(pc *ProviderCfg)
		want string // "" = valid
	}{
		{"plain model", func(pc *ProviderCfg) {}, ""},
		{"no section", func(pc *ProviderCfg) { pc.Generic = nil }, "needs a generic: section"},
		{"section on another kind", func(pc *ProviderCfg) { pc.Kind = event.Claude }, "needs kind: generic"},
		{"unknown placeholder", func(pc *ProviderCfg) { pc.Generic.Args = []string{"{prompt}"} }, "{prompt} is not known"},
		{"session outside resume", func(pc *ProviderCfg) { pc.Generic.Args = []string{"{session}"} }, "{session} is not known"},
		{"resume without session", func(pc *ProviderCfg) { pc.Generic.ResumeArgs = []string{"--continue"} }, "must contain {session}"},
		{"no tools but writes", func(pc *ProviderCfg) { pc.Generic.WriteArgs = []string{"--yolo"} }, "no_tools and write_args"},
		{"can run nothing", func(pc *ProviderCfg) { pc.Generic.NoTools = false }, "can run nothing"},
		{"bad output", func(pc *ProviderCfg) { pc.Generic.Output = "xml" }, "output must be text or jsonl"},
		{"bad regex", func(pc *ProviderCfg) { pc.Generic.Usage.Input = "(" }, "usage.input"},
		{"regex without group", func(pc *ProviderCfg) { pc.Generic.Usage.Output = `eval count: \d+` }, "needs a (group)"},
		{"session without resume", func(pc *ProviderCfg) { pc.Generic.Session = `id: (\S+)` }, "no resume_args"},
		{"jsonl without answer", func(pc *ProviderCfg) {
			pc.Generic.Output = OutputJSONL
			pc.Generic.JSON = []JSONRule{{Match: map[string]string{"type": "init"}, Session: "id"}}
		}, "needs a json rule with text or final"},
		{"each without rules", func(pc *ProviderCfg) {
			pc.Generic.Output = OutputJSONL
			pc.Generic.JSON = []JSONRule{{Each: "content", Text: "x"}}
		}, "each and rules go together"},
		{"rules on text output", func(pc *ProviderCfg) { pc.Generic.JSON = []JSONRule{{Text: "x"}} }, "need output: jsonl"},
		{"bad limit pattern", func(pc *ProviderCfg) { pc.Generic.LimitPatterns = []string{"[a"} }, "limit_patterns"},
		{"standby unknown role", func(pc *ProviderCfg) { pc.Standby = []string{"boss"} }, `unknown role "boss"`},
		{"standby writer on plain model", func(pc *ProviderCfg) { pc.Standby = []string{event.RoleWorker} }, "read-only work only"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := Default()
			pc := ProviderCfg{Kind: event.Generic, Command: "x", Generic: plain()}
			c.edit(&pc)
			cfg.Providers["local"] = pc
			err := cfg.Validate()
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want valid, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

// A role cannot prefer a provider that cannot do its kind of work.
func TestPreferNeedsCapableProvider(t *testing.T) {
	cfg := Default()
	pc := cfg.Providers["ollama-run"]
	pc.Disabled = false
	cfg.Providers["ollama-run"] = pc
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	rc := cfg.Roles[event.RoleWorker].With("ollama-run", Route{Model: "m"})
	rc.Prefer = "ollama-run"
	cfg.Roles[event.RoleWorker] = rc
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "role worker: prefers ollama-run, which takes read-only work only") {
		t.Fatalf("worker on a plain model: %v", err)
	}
}

// The generic section survives the config's YAML round trip (Clone) and a
// user file that only switches the preset on.
func TestGenericPresetMerges(t *testing.T) {
	c := Default().Clone()
	g := c.Providers["ollama-run"].Generic
	if g == nil || !g.NoTools || g.Usage.Input == "" || len(g.ModelArgs) == 0 {
		t.Fatalf("after clone: %+v", g)
	}
	path := filepath.Join(t.TempDir(), FileName)
	os.WriteFile(path, []byte("providers:\n  ollama-run: {disabled: false}\n"), 0o600)
	user, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	pc := user.Providers["ollama-run"]
	if pc.Disabled || pc.Generic == nil || !pc.Generic.NoTools || !pc.StandsBy(event.RoleJudge) || !pc.OnlyPreferred {
		t.Errorf("switching the preset on lost its description: %+v", pc)
	}
}
