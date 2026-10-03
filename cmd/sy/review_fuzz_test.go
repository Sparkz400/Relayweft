package main

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzParseFindings (ROADMAP 1.8): any reviewer reply parses without
// panicking into findings sy can post: bounded, with a body, a known
// severity, a clean path and a sane line; and placing them inline only
// ever uses lines the diff shows.
func FuzzParseFindings(f *testing.F) {
	f.Add("```json\n{\"summary\":\"s\",\"findings\":[{\"file\":\"a.txt\",\"line\":2,\"severity\":\"high\",\"body\":\"x\"}]}\n```", reviewDiff)
	f.Add(`{"findings":[{"path":"..\\b.go","line":"L9","level":"nit","message":"m"},{"line":1e99},{"body":"@x closes #1"}]}`, "")
	f.Add(`{"summary": 5, "findings": [null, [], "x", {"file": "a.txt", "line": -3, "body": "\u0000"}]}`, "@@ -1 +1 @@\n+a\n")
	f.Add("no json", "diff --git a/x b/x\n+++ b/x\n@@ -0,0 +1,3 @@\n+a\n+b\n")
	f.Add("{", "+++ \"b/\\377\"\n@@ -1 +99999999999999999999 @@\n+x\n")
	f.Add("", "")
	f.Fuzz(func(t *testing.T, reply, diff string) {
		rv := parseFindings(reply)
		if len(rv.Findings) > maxFindings {
			t.Fatalf("%d findings", len(rv.Findings))
		}
		if utf8.RuneCountInString(rv.Summary) > maxReviewSumm+3 {
			t.Fatalf("summary of %d runes", utf8.RuneCountInString(rv.Summary))
		}
		for _, fd := range rv.Findings {
			if strings.TrimSpace(fd.Body) == "" || utf8.RuneCountInString(fd.Body) > maxFindingBody+3 {
				t.Fatalf("bad body %q", fd.Body)
			}
			if !slices.Contains(severities, fd.Severity) {
				t.Fatalf("severity %q", fd.Severity)
			}
			if fd.Line < 0 || strings.ContainsAny(fd.File, "\\\n\r") || strings.HasPrefix(fd.File, "/") {
				t.Fatalf("bad place %q:%d", fd.File, fd.Line)
			}
		}
		lines := diffLines(diff)
		inline, rest := placeFindings(rv, lines)
		if len(inline)+len(rest) != len(rv.Findings) {
			t.Fatalf("%d inline + %d rest != %d findings", len(inline), len(rest), len(rv.Findings))
		}
		for _, c := range inline {
			if !lines[c.Path][c.Line] || c.Line < 1 {
				t.Fatalf("inline comment on %s:%d, which the diff does not show", c.Path, c.Line)
			}
			if reMention.MatchString(c.Body) || reCloseRef.MatchString(c.Body) {
				t.Fatalf("live reference in %q", c.Body)
			}
		}
		body := reviewBody(rv, rest, len(inline), "codex")
		if !strings.HasSuffix(body, syMark+"\n") || reMention.MatchString(body) || reCloseRef.MatchString(body) {
			t.Fatalf("body not defused or unmarked:\n%s", body)
		}
	})
}
