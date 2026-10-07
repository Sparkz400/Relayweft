package main

import (
	"os"
	"strings"
	"testing"
)

// A known bad scoring contract must stop before setup or real agent calls.
func TestBenchQuarantinedTaskRefused(t *testing.T) {
	isolate(t)
	chdir(t, t.TempDir())
	if err := os.WriteFile("bench.yaml", []byte("tasks:\n- name: broken\n  prompt: fix it\n  check: git --version\n  disabled: hidden test requires an unspecified internal API\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--yes"}, {"--yes", "--only", "broken"}} {
		out, err := captureStdout(t, func() error { return cmdBench(args) })
		if err == nil || !strings.Contains(out+err.Error(), "hidden test requires an unspecified internal API") {
			t.Fatalf("bad scoring contract was not refused: %v\n%s", err, out)
		}
	}
}
