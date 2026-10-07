package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/orchestrator"
)

func TestTermApproverAsksForTests(t *testing.T) {
	var out bytes.Buffer
	a := newTermApprover(strings.NewReader("go test ./pkg -run TestNew\n"), &out)
	got := a.ApproveTests(context.Background(), orchestrator.TestsQuestion{Task: "x", Why: "no test framework"})
	if !got.OK || got.Command != "go test ./pkg -run TestNew" {
		t.Errorf("answer %+v", got)
	}
	if !strings.Contains(out.String(), "No acceptance tests were written: no test framework") {
		t.Errorf("output:\n%s", out.String())
	}
	a = newTermApprover(strings.NewReader("q\n"), &out)
	if got := a.ApproveTests(context.Background(), orchestrator.TestsQuestion{Why: "none"}); got.OK {
		t.Errorf("q went on: %+v", got)
	}
}

func TestTermApproverShowsTests(t *testing.T) {
	q := orchestrator.TestsQuestion{Task: "x", Command: "go test ./pkg", Red: "pass",
		Tests: []orchestrator.AcceptanceTest{{Requirement: "keeps the last page", File: "pkg/page_test.go", Name: "TestLastPage"}}}
	for in, want := range map[string]orchestrator.TestsAnswer{
		"\n":                          {OK: true},
		"maybe\ny\n":                  {OK: true},
		"n\n":                         {},
		"go test ./pkg -run TestLast": {OK: true, Command: "go test ./pkg -run TestLast"},
	} {
		var out bytes.Buffer
		a := newTermApprover(strings.NewReader(in), &out)
		if got := a.ApproveTests(context.Background(), q); got != want {
			t.Errorf("answer to %q = %+v, want %+v", in, got, want)
		}
		for _, s := range []string{"keeps the last page", "pkg/page_test.go TestLastPage", "Run with: go test ./pkg", "they already pass"} {
			if !strings.Contains(out.String(), s) {
				t.Errorf("output lacks %q:\n%s", s, out.String())
			}
		}
	}
}

func TestBenchModeTestsFirst(t *testing.T) {
	m, err := parseBenchMode("routed-tests-first")
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.store(config.NewStore(config.Default(), filepath.Join(t.TempDir(), "rw.yaml")))
	if err != nil || !st.Get().Orchestrator.TestsFirst {
		t.Errorf("store: %v, tests_first %v", err, err == nil && st.Get().Orchestrator.TestsFirst)
	}
}
