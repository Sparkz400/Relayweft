package tui

import (
	"strings"
	"testing"
)

// FuzzParseFollowUp (ROADMAP 1.8): typed text is either a task or a
// follow-up to a real-looking agent id; it never panics.
func FuzzParseFollowUp(f *testing.F) {
	for _, s := range []string{
		"@edit also handle empty input", "@ add tests", "@last add tests", "@explore\nline two",
		"@edit", "@", "fix @ the parser", "@types/node bump to v22", "@Component rename props",
		"@\t", "@é x", "",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		agent, msg, ok := parseFollowUp(text)
		if !ok {
			if agent != "" || msg != "" {
				t.Fatalf("%q: not a follow-up but returned (%q, %q)", text, agent, msg)
			}
			return
		}
		if !strings.HasPrefix(text, "@") {
			t.Fatalf("%q taken as a follow-up without @", text)
		}
		if agent != "" && (!isAgentID(agent) || agent == "last") {
			t.Fatalf("%q: follow-up to %q, which is not an agent id", text, agent)
		}
		if msg != strings.TrimSpace(msg) {
			t.Fatalf("%q: message not trimmed: %q", text, msg)
		}
	})
}
