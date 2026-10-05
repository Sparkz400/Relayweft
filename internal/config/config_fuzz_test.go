package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// FuzzParse (ROADMAP 1.8): a hand-edited relayweft.yaml must never crash
// rw. Parsing, the Load path (defaults + partial file + validation) and,
// for a valid config, Clone must not panic.
func FuzzParse(f *testing.F) {
	f.Add(DefaultYAML())
	f.Add([]byte("orchestrator:\n  max_threads: 3\n  agent_timeout: 90m\n"))
	f.Add([]byte("providers:\n  codex:\n    command: codex\n"))
	f.Add([]byte("orchestrator:\n  agent_timeout: forever\n"))
	f.Add([]byte("limit_patterns: ['(']\n"))
	f.Add([]byte("roles: {worker: {prefer: other}}\n"))
	f.Add([]byte("a: &a [*a]\n"))
	f.Add([]byte("- 1\n- 2\n"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, data []byte) {
		if _, err := Parse(data); err != nil {
			return
		}
		c := Default()
		if yaml.Unmarshal(data, c) != nil {
			return
		}
		c.fillProviderDefaults(Default())
		if c.Validate() != nil {
			return
		}
		cl := c.Clone()
		if err := cl.Validate(); err != nil {
			t.Fatalf("a valid config is invalid after Clone: %v", err)
		}
		_ = c.SessionDir()
	})
}

// FuzzParseRouteSpec: accepted specs name a valid provider and a model, and
// print back to a spec that parses to the same route.
func FuzzParseRouteSpec(f *testing.F) {
	for _, s := range []string{"claude:opus:high", "codex:gpt-6.1-sol", "opus", "gemini:x", "claude:", "CLAUDE:m:", "codex:m:a:b",
		"ollama:qwen3.6:35b-a3b-coding", "ollama:qwen3.6:35b:low", "codex:m:high:", ":", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		p, r, err := ParseRouteSpec(s)
		if err != nil {
			return
		}
		if !providerName.MatchString(p) {
			t.Fatalf("provider %q accepted", p)
		}
		if r.Model == "" {
			t.Fatalf("%q accepted without a model", s)
		}
		spec := p + ":" + r.Model
		if r.Effort != "" {
			spec += ":" + r.Effort
		}
		p2, r2, err := ParseRouteSpec(spec)
		if err != nil || p2 != p || r2 != r {
			t.Fatalf("%q -> %q does not round-trip: %s %+v %v", s, spec, p2, r2, err)
		}
	})
}
