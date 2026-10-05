package main

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/router"
)

func TestParseTaskFile(t *testing.T) {
	got := parseTaskFile("# nightly\nfix the parser\n\n  add tests  \r\n")
	if want := []string{"fix the parser", "add tests"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lines: %q", got)
	}
	got = parseTaskFile("fix the parser\nso that it keeps fields\n---\n# skip\nadd tests\n---\n")
	if want := []string{"fix the parser\nso that it keeps fields", "add tests"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("blocks: %q", got)
	}
}

func testPlan() orchestrator.Plan {
	return orchestrator.Plan{Summary: "s", Subtasks: []orchestrator.Subtask{
		{ID: "a", Title: "look", Kind: router.KindExplore, Prompt: "look around"},
		{ID: "b", Title: "change", Kind: router.KindEdit, Prompt: "change it", DependsOn: []string{"a"}},
	}}
}

func TestTermApproverPlan(t *testing.T) {
	var out bytes.Buffer
	a := newTermApprover(strings.NewReader("r 2 worker_high\nd 1\np 1 do it better\ny\n"), &out)
	p, ok := a.ApprovePlan(context.Background(), "task", testPlan())
	if !ok {
		t.Fatalf("not approved:\n%s", out.String())
	}
	if len(p.Subtasks) != 1 || p.Subtasks[0].ID != "b" || p.Subtasks[0].Role != "worker_high" ||
		p.Subtasks[0].Prompt != "do it better" || len(p.Subtasks[0].DependsOn) != 0 {
		t.Fatalf("plan %+v", p.Subtasks)
	}
	a = newTermApprover(strings.NewReader("r 1 nobody\nn\n"), &out)
	if _, ok := a.ApprovePlan(context.Background(), "task", testPlan()); ok {
		t.Fatal("n approved")
	}
	if !strings.Contains(out.String(), "roles:") {
		t.Fatal("bad role not explained")
	}
	// Closed stdin cancels.
	a = newTermApprover(strings.NewReader(""), &out)
	if _, ok := a.ApprovePlan(context.Background(), "task", testPlan()); ok {
		t.Fatal("EOF approved")
	}
}

func TestTermApproverChanges(t *testing.T) {
	cs := orchestrator.ChangeSet{StepID: "b", Title: "change", Files: []orchestrator.FileChange{
		{Path: "a.go", Status: "M", Added: 3, Deleted: 1, Patch: "+x"}, {Path: "b.go", Status: "A", Added: 9},
	}}
	var out bytes.Buffer
	d := newTermApprover(strings.NewReader("v 1\nt 2\ny\n"), &out).ReviewChanges(context.Background(), cs)
	if !reflect.DeepEqual(d.Apply, []string{"a.go"}) || d.Feedback != "" {
		t.Fatalf("decision %+v", d)
	}
	if !strings.Contains(out.String(), "+x") {
		t.Fatal("diff not shown")
	}
	d = newTermApprover(strings.NewReader("f use a map instead\n"), &out).ReviewChanges(context.Background(), cs)
	if d.Feedback != "use a map instead" {
		t.Fatalf("feedback %+v", d)
	}
	d = newTermApprover(strings.NewReader("n\n"), &out).ReviewChanges(context.Background(), cs)
	if len(d.Apply) != 0 || d.Feedback != "" {
		t.Fatalf("reject %+v", d)
	}
}

// Parallel steps reviewing at once get one answer each, in order.
func TestTermApproverConcurrentReviews(t *testing.T) {
	cs := orchestrator.ChangeSet{StepID: "x", Files: []orchestrator.FileChange{{Path: "a.go", Status: "M"}}}
	a := newTermApprover(strings.NewReader("y\nn\n"), io.Discard)
	got := make(chan orchestrator.ChangeDecision, 2)
	for i := 0; i < 2; i++ {
		go func() { got <- a.ReviewChanges(context.Background(), cs) }()
	}
	applied := 0
	for i := 0; i < 2; i++ {
		select {
		case d := <-got:
			applied += len(d.Apply)
		case <-time.After(3 * time.Second):
			t.Fatal("a concurrent review hung")
		}
	}
	if applied != 1 {
		t.Fatalf("want one accepted and one rejected review, applied %d", applied)
	}
}

// The end of a normal rw run or rw resume cancels the run's context too; it
// must not print the Ctrl+C notice (found by rw selftest).
func TestWatchInterruptQuietOnClose(t *testing.T) {
	for _, closing := range []bool{true, false} {
		ctx, stop := context.WithCancel(context.Background())
		var flag atomic.Bool
		flag.Store(closing)
		var buf bytes.Buffer
		done := make(chan struct{})
		go func() { watchInterrupt(ctx, stop, &flag, &buf); close(done) }()
		stop()
		<-done
		if printed := strings.Contains(buf.String(), "cancelling"); printed == closing {
			t.Errorf("closing=%v: printed %q", closing, buf.String())
		}
	}
}
