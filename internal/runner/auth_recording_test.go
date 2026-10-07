package runner

import (
	"context"
	"testing"

	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// Recorded on Windows, 6 Oct 2026, with codex-cli 0.160.0 and an empty,
// isolated CODEX_HOME. Both WebSocket and HTTPS attempts returned 401. Only
// session and request tracing IDs are redacted; the signed-in profile was untouched.
func TestCodexRealLoggedOutRecording(t *testing.T) {
	pc, detector := providerCfg(t, event.Codex)
	fakeExe(t, &pc, "codex_real_logged_out.jsonl", 1, "")
	var c collector
	r := NewCodex(pc, detector).Run(context.Background(), Spec{AgentID: "auth", Dir: t.TempDir(), ReadOnly: true}, c.emit)
	if r.OK() || r.Err == nil || !sessionlog.UnavailableError(r.Err.Error()) {
		t.Fatalf("logged-out CLI was not reported as unavailable: %+v", r)
	}
	if len(r.Files) != 0 || r.Final != "" || r.Tokens != (event.TokenUsage{}) {
		t.Fatalf("logged-out attempt invented work or usage: %+v", r)
	}
	if countKind(c.evs, event.Quota) != 0 || countKind(c.evs, event.Done) != 1 {
		t.Fatalf("unexpected events: %v", kinds(c.evs))
	}
	for _, e := range c.evs {
		if e.Kind == event.Done && e.OK {
			t.Fatal("authentication failure emitted successful completion")
		}
	}
}
