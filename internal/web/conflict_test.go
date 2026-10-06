package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/orchestrator"
)

// A conflict question reaches the page (and the editor clients, which use
// the same API) with its text and hint, is answered once, and a conflict
// resolution under review carries what conflicted.
func TestConflictQuestionInBrowser(t *testing.T) {
	env := newEnv(t, nil)
	q := orchestrator.ConflictQuestion{Task: "t", StepID: "b", Title: "say b", With: "your uncommitted edits", Files: []string{"shared.txt"}, Yours: true}
	for _, yes := range []bool{true, false} {
		got := make(chan bool, 1)
		go func() { got <- env.srv.ap.ApproveResolve(context.Background(), q) }()
		var id string
		waitFor(t, "the conflict question", func() bool {
			for _, a := range env.state().Approvals {
				if a.Type == "conflict" && a.Conflict != nil && a.Conflict.Text == "b conflicts with your uncommitted edits in shared.txt" &&
					a.Conflict.Yours && strings.Contains(a.Conflict.Hint, "never in your folder") {
					id = a.ID
				}
			}
			return id != ""
		})
		if res, _ := env.do("POST", "/api/approvals/"+id+"/budget", map[string]any{"ok": true}, nil); res.StatusCode/100 == 2 {
			t.Error("a conflict question was answered as a budget question")
		}
		var out map[string]string
		env.call("POST", "/api/approvals/"+id+"/conflict", map[string]bool{"ok": yes}, &out)
		if want := map[bool]string{true: "an agent resolves", false: "kept on a branch"}[yes]; !strings.Contains(out["message"], want) {
			t.Errorf("answer = %v", out)
		}
		if <-got != yes {
			t.Errorf("answered %v, got the other", yes)
		}
		if res, _ := env.do("POST", "/api/approvals/"+id+"/conflict", map[string]bool{"ok": true}, nil); res.StatusCode != http.StatusGone {
			t.Errorf("second answer: %d", res.StatusCode)
		}
	}

	cs := demoChangeSet()
	cs.Conflict = "b conflicts with step a (say a) in shared.txt"
	done := make(chan orchestrator.ChangeDecision, 1)
	go func() { done <- env.srv.ap.ReviewChanges(context.Background(), cs) }()
	var id string
	waitFor(t, "the review", func() bool {
		for _, a := range env.state().Approvals {
			if a.Type == "changes" && a.Changes.Conflict == cs.Conflict {
				id = a.ID
			}
		}
		return id != ""
	})
	env.call("POST", "/api/approvals/"+id+"/changes", map[string]any{"apply": []string{}}, nil)
	<-done
}
