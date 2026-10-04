package orchestrator

import (
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/limits"
)

// Review finding: a limit hit is retried only while a provider that could
// take the role is free; an enabled only_preferred provider (or one
// without a route for the role) does not count.
func TestAllLimitedCountsOnlyProvidersThatCanTakeTheRole(t *testing.T) {
	cfg := config.Default()
	pc := cfg.Providers["ollama"]
	pc.Disabled = false
	cfg.Providers["ollama"] = pc
	tr := limits.NewTracker()
	o := &Orchestrator{opts: Options{Store: config.NewStore(cfg, ""), Tracker: tr}}
	d := event.Decision{Role: event.RoleWorker, Provider: event.Codex}
	until := time.Now().Add(time.Hour)
	tr.MarkLimited(event.Codex, until)
	if o.allLimited(d) {
		t.Fatal("claude is free")
	}
	tr.MarkLimited(event.Claude, until)
	if !o.allLimited(d) {
		t.Fatal("only ollama (only_preferred) is left: nothing can take the worker")
	}
	// A role that prefers ollama by name ran there: it is free.
	if o.allLimited(event.Decision{Role: event.RoleWorker, Provider: "ollama"}) {
		t.Fatal("the provider the role ran on is free")
	}
}

// A provider standing by for a role counts as free for it, so a limit hit
// is retried there (the router sends it to the standby) instead of waiting.
func TestAllLimitedCountsStandby(t *testing.T) {
	cfg := config.Default()
	pc := cfg.Providers[event.Qwen]
	pc.Disabled = false
	cfg.Providers[event.Qwen] = pc
	tr := limits.NewTracker()
	o := &Orchestrator{opts: Options{Store: config.NewStore(cfg, ""), Tracker: tr}}
	until := time.Now().Add(time.Hour)
	tr.MarkLimited(event.Codex, until)
	tr.MarkLimited(event.Claude, until)
	if o.allLimited(event.Decision{Role: event.RoleExplorer, Provider: event.Claude}) {
		t.Fatal("qwen stands by for the explorer")
	}
	if !o.allLimited(event.Decision{Role: event.RoleWorker, Provider: event.Codex}) {
		t.Fatal("qwen does not stand by for the worker")
	}
}
