package router

import (
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
)

// withProviders is the default config with the named presets enabled.
func withProviders(t *testing.T, names ...string) *config.Config {
	t.Helper()
	cfg := config.Default()
	for _, n := range names {
		pc, ok := cfg.Providers[n]
		if !ok {
			t.Fatalf("no preset %s", n)
		}
		pc.Disabled = false
		cfg.Providers[n] = pc
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func routerFor(cfg *config.Config, st state) *Router {
	return &Router{Cfg: func() *config.Config { return cfg }, State: st}
}

func TestDefaultConfigRoutesAsBefore(t *testing.T) {
	// The new presets start disabled: nothing changes for a codex+claude user.
	cfg := config.Default()
	if got := strings.Join(cfg.Enabled(), ","); got != "codex,claude" {
		t.Fatalf("enabled = %s", got)
	}
	r := routerFor(cfg, state{limited: map[string]bool{event.Codex: true, event.Claude: true}})
	if d := r.Route(Step{Kind: KindEdit, Prompt: "fix it"}); d.Provider != event.Codex || d.Fallback {
		t.Fatalf("both limited: %+v", d)
	}
}

func TestLimitFallbackWalksTheOrder(t *testing.T) {
	cfg := withProviders(t, event.Gemini)
	r := routerFor(cfg, state{limited: map[string]bool{event.Codex: true, event.Claude: true}})
	d := r.Route(Step{Kind: KindEdit, Prompt: "fix the bug"})
	if d.Provider != event.Gemini || d.Model != "pro" || !d.Fallback || d.From != event.Codex || d.Rule != RuleLimit {
		t.Fatalf("got %+v", d)
	}
	if !strings.HasPrefix(d.Reason, "codex at limit -> gemini") {
		t.Errorf("reason = %q", d.Reason)
	}
	// Only codex limited: claude is next in the default order.
	r = routerFor(cfg, state{limited: map[string]bool{event.Codex: true}})
	if d := r.Route(Step{Kind: KindEdit, Prompt: "fix it"}); d.Provider != event.Claude {
		t.Fatalf("got %+v", d)
	}
	// routing.provider_order puts gemini before claude.
	cfg.Routing.ProviderOrder = []string{event.Codex, event.Gemini, event.Claude}
	if d := r.Route(Step{Kind: KindEdit, Prompt: "fix it"}); d.Provider != event.Gemini {
		t.Fatalf("provider_order ignored: %+v", d)
	}
	// Every enabled provider limited: no fallback.
	r = routerFor(cfg, state{limited: map[string]bool{event.Codex: true, event.Claude: true, event.Gemini: true}})
	if d := r.Route(Step{Kind: KindEdit, Prompt: "fix it"}); d.Fallback {
		t.Fatalf("all limited but fell back: %+v", d)
	}
}

func TestOnlyPreferredProviderIsNeverAStandIn(t *testing.T) {
	cfg := withProviders(t, "ollama")
	limited := state{limited: map[string]bool{event.Codex: true, event.Claude: true}}
	if d := routerFor(cfg, limited).Route(Step{Kind: KindEdit, Prompt: "fix it"}); d.Provider == "ollama" {
		t.Fatalf("local model took a limit fallback: %+v", d)
	}
	// prefer: other with claude off keeps the reviewer on the planner's
	// provider rather than the local model.
	pc := cfg.Providers[event.Claude]
	pc.Disabled = true
	cfg.Providers[event.Claude] = pc
	r := routerFor(cfg, state{})
	if d := r.Route(Step{Kind: KindReview, MainProvider: event.Codex}); d.Provider != event.Codex {
		t.Fatalf("reviewer = %+v", d)
	}
	// auto never picks it, even unused.
	rc := cfg.Roles[event.RoleWorker]
	rc.Prefer = config.PreferAuto
	cfg.Roles[event.RoleWorker] = rc
	r = routerFor(cfg, state{share: map[string]float64{event.Codex: 1}})
	if d := r.Route(Step{Kind: KindEdit, Prompt: "fix it"}); d.Provider != event.Codex {
		t.Fatalf("auto = %+v", d)
	}
	// A role that names it gets it.
	rc.Prefer = "ollama"
	cfg.Roles[event.RoleWorker] = rc
	if d := r.Route(Step{Kind: KindEdit, Prompt: "fix it"}); d.Provider != "ollama" || d.Model != "qwen3.6:35b-a3b-coding" {
		t.Fatalf("named = %+v", d)
	}
}

func TestPreferOtherAndAutoAmongMore(t *testing.T) {
	cfg := withProviders(t, event.Gemini, "deepseek")
	r := routerFor(cfg, state{share: map[string]float64{event.Codex: 0.5, event.Claude: 0.3, event.Gemini: 0.15, "deepseek": 0.05}})
	// other: the first provider after the planner's, in order.
	if d := r.Route(Step{Kind: KindReview, MainProvider: event.Claude}); d.Provider != event.Codex {
		t.Errorf("other of claude = %+v", d)
	}
	if d := r.Route(Step{Kind: KindReview, MainProvider: event.Gemini}); d.Provider != event.Codex {
		t.Errorf("other of gemini = %+v", d)
	}
	rc := cfg.Roles[event.RoleWorker]
	rc.Prefer = config.PreferAuto
	cfg.Roles[event.RoleWorker] = rc
	if d := r.Route(Step{Kind: KindEdit, Prompt: "fix it"}); d.Provider != "deepseek" || d.Model != "deepseek-v4-pro" {
		t.Errorf("auto = %+v", d)
	}
}

func TestQuotaPreemptSkipsLimitedProviders(t *testing.T) {
	cfg := withProviders(t, event.Gemini)
	r := routerFor(cfg, state{util: map[string]float64{event.Codex: 0.95}, limited: map[string]bool{event.Claude: true}})
	d := r.Route(Step{Kind: KindEdit, Prompt: "fix it"})
	if d.Provider != event.Gemini || d.Rule != RuleQuota || d.From != event.Codex {
		t.Fatalf("got %+v", d)
	}
}

func TestPreferAnyConfiguredProvider(t *testing.T) {
	cfg := withProviders(t, event.Qwen)
	rc := cfg.Roles[event.RoleExplorer]
	rc.Prefer = event.Qwen
	cfg.Roles[event.RoleExplorer] = rc
	d := routerFor(cfg, state{}).Route(Step{Kind: KindExplore, Prompt: "where is the parser"})
	if d.Provider != event.Qwen || d.Model != "qwen3.6:35b-a3b-coding" || d.Fallback {
		t.Fatalf("got %+v", d)
	}
	// Turned off again: the role falls to the first provider that may stand in.
	pc := cfg.Providers[event.Qwen]
	pc.Disabled = true
	cfg.Providers[event.Qwen] = pc
	if d := routerFor(cfg, state{}).Route(Step{Kind: KindExplore, Prompt: "where is the parser"}); d.Provider != event.Codex {
		t.Fatalf("disabled preferred provider: %+v", d)
	}
}
