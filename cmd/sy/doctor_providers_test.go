package main

import (
	"testing"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
)

// Review finding: sy review must not hand the second opinion to a local
// only_preferred model or a provider without a reviewer model.
func TestReviewerFor(t *testing.T) {
	cfg := config.Default()
	if got := reviewerFor(cfg, event.Codex); got != event.Claude {
		t.Errorf("codex wrote it: %q", got)
	}
	if got := reviewerFor(cfg, event.Claude); got != event.Codex {
		t.Errorf("claude wrote it: %q", got)
	}
	for _, p := range []string{"ollama", event.Gemini} {
		pc := cfg.Providers[p]
		pc.Disabled = false
		cfg.Providers[p] = pc
	}
	cfg.Routing.ProviderOrder = []string{"ollama", event.Gemini, event.Codex, event.Claude}
	rc := cfg.Roles[event.RoleReviewer]
	rc.Extra[event.Gemini] = config.Route{}
	cfg.Roles[event.RoleReviewer] = rc
	if got := reviewerFor(cfg, event.Codex); got != event.Claude {
		t.Errorf("skips ollama (only_preferred) and gemini (no reviewer model): %q", got)
	}
	if got := reviewerFor(cfg, "bard"); got != "" {
		t.Errorf("unknown writer: %q", got)
	}
}
