package orchestrator

import (
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sparkz400/switchyard/internal/router"
	"github.com/sparkz400/switchyard/internal/runner"
)

// Fuzz targets for plan, verdict and diff parsing (ROADMAP 1.8).
//
//	go test -run='^$' -fuzz=FuzzParsePlan -fuzztime=60s ./internal/orchestrator

var reSafeID = regexp.MustCompile(`^[a-z0-9_-]+$`)

// checkPlan asserts what the scheduler relies on for an accepted plan.
func checkPlan(t *testing.T, p Plan) {
	t.Helper()
	if n := len(p.Subtasks); n == 0 || n > maxSubtasks {
		t.Fatalf("accepted plan has %d subtasks", n)
	}
	ids := map[string]bool{}
	for _, st := range p.Subtasks {
		if st.ID == "" || !reSafeID.MatchString(st.ID) {
			t.Fatalf("unsafe id %q", st.ID)
		}
		switch st.ID {
		case AgentMain, "reviewer", "judge":
			t.Fatalf("subtask uses the reserved id %q", st.ID)
		}
		if ids[st.ID] {
			t.Fatalf("duplicate id %q", st.ID)
		}
		ids[st.ID] = true
		switch st.Kind {
		case router.KindExplore, router.KindResearch, router.KindEdit:
		default:
			t.Fatalf("subtask %s has unknown kind %q", st.ID, st.Kind)
		}
	}
	for _, st := range p.Subtasks {
		for _, d := range st.DependsOn {
			if !ids[d] {
				t.Fatalf("subtask %s depends on %q, which is not a subtask (ids %v)", st.ID, d, ids)
			}
			if d == st.ID {
				t.Fatalf("subtask %s depends on itself", st.ID)
			}
		}
	}
	if hasCycle(p.Subtasks) {
		t.Fatalf("accepted plan has a dependency cycle: %+v", p.Subtasks)
	}
	// Every step must be schedulable: repeatedly start the ready ones, as
	// the orchestrator's dispatch loop does.
	done := map[string]bool{}
	for progress := true; progress; {
		progress = false
		for _, st := range p.Subtasks {
			if done[st.ID] {
				continue
			}
			ready := true
			for _, d := range st.DependsOn {
				ready = ready && done[d]
			}
			if ready {
				done[st.ID], progress = true, true
			}
		}
	}
	if len(done) != len(p.Subtasks) {
		t.Fatalf("only %d of %d subtasks can ever be scheduled: %+v", len(done), len(p.Subtasks), p.Subtasks)
	}
	// Prompts built from the plan must not panic either.
	_ = planReviewPrompt("task", p)
	_ = stepPrompt("task", p.Subtasks[0], nil, "", "", false)
}

func FuzzParsePlan(f *testing.F) {
	f.Add("Here you go:\n```json\n{\"summary\":\"s\",\"subtasks\":[{\"id\":\"A b\",\"kind\":\"Explorer\",\"prompt\":\"find x\"},{\"id\":\"a-b\",\"kind\":\"weird\",\"prompt\":\"do y\",\"depends_on\":[\"A b\",\"ghost\"]}]}\n```\nthanks")
	f.Add(`{"subtasks":[{"id":"x","prompt":"1","depends_on":["y"]},{"id":"y","prompt":"2","depends_on":["x"]}]}`)
	f.Add(`{"subtasks":[{"id":"main","prompt":"1"},{"id":"main","prompt":"2"},{"id":"t2","prompt":"3"}]}`)
	f.Add(`{"subtasks":[{"id":"!!!"},{"id":""},{"id":"t1"},{"id":"t2"},{"id":"t3"},{"id":"t4"},{"id":"t5"},{"id":"t6"},{"id":"t7"},{"id":"t8"}]}`)
	f.Add("```json\n{\"subtasks\": []}\n```")
	f.Add("```\nnot json\n```\n{\"summary\":\"x\"}")
	f.Add("no json here")
	f.Add("{")
	f.Add("}{")
	f.Add(runner.MarkerPlan)
	// The demo planner's real reply.
	f.Add("```json\n" + `{"summary":"Explore the parser, check the docs, then change the parser and the CLI flag in parallel.","subtasks":[{"id":"explore","title":"Map parser code","kind":"explore","prompt":"Find where input lines are split into fields.","files":[]},{"id":"docs","title":"Read flag docs","kind":"research","prompt":"Summarize how CLI flags are documented.","files":[]},{"id":"parser","title":"Fix field splitting","kind":"edit","prompt":"first-edit","files":["internal/parse.go"],"depends_on":["explore"]},{"id":"flag","title":"Add --strict flag","kind":"edit","prompt":"Add a --strict flag","files":["internal/flags.go","README.md"],"depends_on":["docs"]}]}` + "\n```")
	f.Fuzz(func(t *testing.T, reply string) {
		p, err := ParsePlan(reply)
		if err != nil {
			if err.Error() == "" {
				t.Fatal("error without a message")
			}
			return
		}
		checkPlan(t, p)
		// Normalizing twice changes nothing (the TUI re-normalizes edited plans).
		again, err := NormalizePlan(clonePlan(p))
		if err != nil {
			t.Fatalf("normalized plan rejected on the second pass: %v", err)
		}
		if !reflect.DeepEqual(again, p) {
			t.Fatalf("NormalizePlan is not idempotent:\n%+v\n%+v", p.Subtasks, again.Subtasks)
		}
	})
}

func clonePlan(p Plan) Plan {
	c := p
	c.Subtasks = make([]Subtask, len(p.Subtasks))
	for i, st := range p.Subtasks {
		st.Files = slices.Clone(st.Files)
		st.DependsOn = slices.Clone(st.DependsOn)
		c.Subtasks[i] = st
	}
	return c
}

// FuzzNormalizePlan builds plans from a compact text form so the fuzzer can
// reach id collisions and dependency graphs quickly:
// "id|kind|dep,dep|title|prompt" per subtask, subtasks separated by ";".
func FuzzNormalizePlan(f *testing.F) {
	f.Add("a|edit||A|do a;b|explore|a|B|look;c|research|a,b|C|read")
	f.Add("x||y||;y||x||")
	f.Add("t2|||;|||;t2|||;t1||t2,t3|")
	f.Add("a||b||;b||c||;c||a||;d||||")
	f.Add("")
	f.Fuzz(func(t *testing.T, spec string) {
		var p Plan
		for _, part := range strings.Split(spec, ";") {
			fs := strings.SplitN(part, "|", 5)
			for len(fs) < 5 {
				fs = append(fs, "")
			}
			st := Subtask{ID: fs[0], Kind: router.Kind(fs[1]), Title: fs[3], Prompt: fs[4]}
			if fs[2] != "" {
				st.DependsOn = strings.Split(fs[2], ",")
			}
			p.Subtasks = append(p.Subtasks, st)
		}
		got, err := NormalizePlan(p)
		if err != nil {
			return
		}
		checkPlan(t, got)
	})
}

func FuzzParseVerdict(f *testing.F) {
	f.Add("```json\n{\"approve\": false, \"advice\": \"fix x\", \"issues\": [\"a\"]}\n```")
	f.Add("Looks fine to me.")
	f.Add(`{"approve": true, "advice": "ok"}`)
	f.Add("```json\n{\"approve\": \"yes\"}\n```\n```json\n{\"approve\": false}\n```")
	f.Add("{\"approve\": false, \"issues\": null} trailing } text")
	f.Add("```json\n\n```")
	f.Add("")
	f.Fuzz(func(t *testing.T, reply string) {
		v := ParseVerdict(reply)
		var probe Verdict
		if extractJSON(reply, &probe) != nil {
			// An unreadable reply must never block the task.
			if !v.Approve || v.Advice != strings.TrimSpace(reply) {
				t.Fatalf("unreadable reply did not approve: %+v", v)
			}
		}
		_ = fixPrompt("task", v)
		_ = runner.SummaryLine(reply)
	})
}

func FuzzSplitHunks(f *testing.F) {
	f.Add("diff --git a/f b/f\nindex 1..2 100644\n--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n-a\n+b\n c\n@@ -10,2 +10,2 @@\n-x\n+y\n z\n")
	f.Add("diff --git a/f b/f\nBinary files differ\n")
	f.Add("@@ only a hunk")
	f.Add("@@ -1 +1 @@\n-a\n+b\n\n... (diff truncated) ...")
	f.Add("x\n@@@ not a hunk\n @@ indented\n@@ -1 +1 @@")
	f.Add("")
	f.Add("\r\n@@ -1 +1 @@\r\n-a\r\n+b\r\n")
	f.Fuzz(func(t *testing.T, patch string) {
		header, hunks := SplitHunks(patch)
		if got := header + strings.Join(hunks, ""); got != patch {
			t.Fatalf("header+hunks != input:\n%q\n%q", got, patch)
		}
		for i, h := range hunks {
			if !strings.HasPrefix(h, "@@ ") {
				t.Fatalf("hunk %d does not start with @@: %q", i, h)
			}
			if strings.Contains(h[1:], "\n@@ ") {
				t.Fatalf("hunk %d holds another hunk: %q", i, h)
			}
		}
		if strings.HasPrefix(header, "@@ ") || strings.Contains(header, "\n@@ ") {
			t.Fatalf("header holds a hunk: %q", header)
		}
		for _, st := range []string{"M", "A", "D", ""} {
			_ = FileChange{Status: st, Patch: patch}.Splittable()
		}
		// splitPatch must keep every byte as well.
		if parts := splitPatch(patch); strings.Join(parts, "") != patch {
			t.Fatalf("splitPatch lost bytes: %q", parts)
		} else {
			for i, p := range parts {
				if p == "" {
					t.Fatalf("splitPatch produced an empty part %d of %q", i, patch)
				}
				if i > 0 && !strings.HasPrefix(p, "diff --git ") {
					t.Fatalf("part %d does not start at a file header: %q", i, p)
				}
			}
		}
	})
}

// FuzzParseChangeList feeds `git diff --name-status -z` / `--numstat -z`
// style output.
func FuzzParseChangeList(f *testing.F) {
	f.Add("M\x00a.txt\x00A\x00dir/b c.txt\x00D\x00gone\x00", "1\t2\ta.txt\x003\t0\tdir/b c.txt\x00-\t-\tgone\x00")
	f.Add("M\x00tab\there\x00", "1\t1\ttab\there\x00")
	f.Add("", "")
	f.Fuzz(func(t *testing.T, status, nums string) {
		for _, fc := range parseChangeList(status, nums) {
			if fc.Path == "" {
				t.Fatalf("change without a path: %+v", fc)
			}
			if len(fc.Status) != 1 {
				t.Fatalf("status %q of %s is not one letter", fc.Status, fc.Path)
			}
		}
	})
}
