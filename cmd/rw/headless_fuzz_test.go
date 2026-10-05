package main

import (
	"strings"
	"testing"
)

// FuzzParseTaskFile (ROADMAP 1.8): any task file splits into non-empty,
// trimmed tasks without comment lines or separators, and never panics.
func FuzzParseTaskFile(f *testing.F) {
	f.Add("# nightly\nfix the parser\n\n  add tests  \r\n")
	f.Add("fix the parser\nso that it keeps fields\n---\n# skip\nadd tests\n---\n")
	f.Add("---\n---\n")
	f.Add("  ---  \r\n#\r\n")
	f.Add("a\r\r\nb\n")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		for i, task := range parseTaskFile(s) {
			if task == "" || task != strings.TrimSpace(task) {
				t.Fatalf("task %d is empty or untrimmed: %q", i, task)
			}
			for _, l := range strings.Split(task, "\n") {
				if strings.HasPrefix(strings.TrimSpace(l), "#") {
					t.Fatalf("task %d keeps a comment line: %q", i, task)
				}
				if strings.TrimSpace(l) == "---" {
					t.Fatalf("task %d keeps a separator: %q", i, task)
				}
			}
		}
	})
}
