package config

import (
	"strings"
	"testing"
)

func TestWithEnabled(t *testing.T) {
	on := map[string]bool{"codex": false, "claude": true, "qwen": true, "ollama-run": true}
	for _, crlf := range []bool{false, true} {
		src := DefaultYAML()
		if crlf { // a checkout with core.autocrlf
			src = []byte(strings.ReplaceAll(string(src), "\n", "\r\n"))
		}
		out := WithEnabled(src, on)
		c, err := parseFile("test.yaml", out)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		def := Default()
		for name, pc := range c.Providers {
			want := def.Providers[name].Disabled
			if e, ok := on[name]; ok {
				want = !e
			}
			if pc.Disabled != want {
				t.Errorf("crlf=%v: %s disabled = %v, want %v", crlf, name, pc.Disabled, want)
			}
		}
		// The comments stay.
		if !strings.Contains(string(out), "# true = never route to Codex") || !strings.Contains(string(out), "# Qwen Code, here on a local model") {
			t.Errorf("crlf=%v: comments lost", crlf)
		}
		if strings.Count(string(out), "\n") != strings.Count(string(src), "\n") {
			t.Errorf("crlf=%v: line count changed", crlf)
		}
	}
}
