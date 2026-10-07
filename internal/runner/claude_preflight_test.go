package runner

import (
	"slices"
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
)

func TestClaudePreflightRequiresToolResult(t *testing.T) {
	for _, mode := range []string{"claim", "call-only", "error", "success", "denied"} {
		p := &claudeParser{}
		if mode != "claim" {
			p.Line([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tool-1","name":"PowerShell","input":{"command":"go test ./pkg"}}]}}`))
		}
		if mode == "success" || mode == "denied" {
			p.Line([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","content":"ok"}]}}`))
		}
		if mode == "error" {
			p.Line([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","is_error":true,"content":"permission required"}]}}`))
		}
		if mode == "denied" {
			p.Line([]byte(`{"type":"system","subtype":"permission_denied","tool_name":"PowerShell","tool_use_id":"tool-1"}`))
		}
		p.Line([]byte(`{"type":"result","subtype":"success","result":"I ran go test ./pkg"}`))
		var r Result
		p.Finish(&r)
		if slices.Contains(r.Commands, "go test ./pkg") != (mode == "success") {
			t.Fatalf("%s: unproven execution accepted: %+v", mode, r)
		}
		if r.PermissionDenied != (mode == "denied") {
			t.Fatalf("%s: lost denial", mode)
		}
	}
}

func TestClaudeCheckOnlyToolsAndWindowsRules(t *testing.T) {
	cfg := config.ProviderCfg{WriteAllowedTools: []string{"Edit", "Bash(*)"}, WritePermissionMode: "acceptEdits"}
	args := ClaudeArgs(cfg, Spec{CheckOnly: true, AllowedCommands: []string{"go test ./pkg", "gofmt -l pkg"}})
	text := strings.Join(args, " ")
	for _, want := range []string{"--permission-mode dontAsk", "--tools Bash,PowerShell", "PowerShell(go test ./pkg)", "Bash(gofmt -l pkg)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s: %s", want, text)
		}
	}
	if strings.Contains(text, "Edit") || strings.Contains(text, "Bash(*)") {
		t.Fatal("preflight inherited broad write tools", text)
	}
}
