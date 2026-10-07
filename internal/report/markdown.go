package report

import (
	"fmt"
	"io"
	"strings"

	"github.com/sparkz400/relayweft/internal/event"
)

// Markdown writes the report as Markdown (GitHub flavour). Untrusted text
// goes into fenced blocks whose fence is longer than any backtick run in
// it, or is escaped for inline use, so it cannot inject markup or HTML.
func (d *Data) Markdown(w io.Writer) error {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	p("# Relayweft task report: %s\n\n", md(d.ID))
	p("**Status:** %s", md(d.Status))
	if d.Mode != "" {
		p(" · **Mode:** %s", md(d.Mode))
	}
	p(" · **Started:** %s · **Took:** %s", d.Created.Format("2006-01-02 15:04:05"), humanDur(d.Duration))
	if d.Dir != "" {
		p(" · **Dir:** %s", md(d.Dir))
	}
	p("\n\n")
	for _, n := range d.Notes {
		p("> %s\n\n", md(n))
	}
	p("## Task\n\n%s\n", fence(d.Task, "text"))
	if d.Summary != "" {
		p("## Result\n\n%s\n", fence(d.Summary, "text"))
	}
	if a := d.Acceptance; a != nil {
		p("## Acceptance\n\n| Level | Status | Detail |\n|---|---|---|\n")
		p("| Agent finished | %s | %s |\n", md(a.Agents.Status), md(a.Agents.Detail))
		p("| Configured checks passed | %s | %s |\n", md(a.Checks.Status), md(a.Checks.Detail))
		p("| Requirements verified | %s | %s |\n\n", md(a.Requirements.Status), md(a.Requirements.Detail))
		if len(a.Criteria) > 0 {
			p("| Requirement | Status | Test | Evidence |\n|---|---|---|---|\n")
			for _, c := range a.Criteria {
				status := c.Status
				if c.Note != "" {
					status += " (" + c.Note + ")"
				}
				p("| %s %s | %s | %s | %s |\n", md(c.ID), md(c.Text), md(status), md(c.Test), md(c.Evidence))
			}
			p("\n")
		}
	}
	if len(d.Steps) > 0 {
		p("## Plan\n\n")
		if d.PlanSummary != "" {
			p("%s\n\n", md(d.PlanSummary))
		}
		p("| Step | Title | Kind | Role | Route | Depends on | Result |\n|---|---|---|---|---|---|---|\n")
		for _, s := range d.Steps {
			p("| %s | %s | %s | %s | %s | %s | %s |\n", md(s.ID), md(s.Title), md(s.Kind), md(s.Role), md(s.Route),
				md(strings.Join(s.DependsOn, ", ")), md(s.Result))
		}
		p("\n")
		bestOf := false
		for _, s := range d.Steps {
			if s.BestOf != "" {
				p("- %s: %s\n", md(s.ID), md(s.BestOf))
				bestOf = true
			}
		}
		if bestOf {
			p("\n")
		}
		for _, s := range d.Steps {
			if s.Final != "" || s.Err != "" {
				p("<details><summary>%s: final answer</summary>\n\n%s\n%s\n</details>\n\n", htmlText(s.ID), fence(s.Final, "text"), fence(s.Err, "text"))
			}
		}
	}
	if e := d.Why; e != nil {
		p("## Why it ran this way\n\n**%s**", md(e.Headline()))
		if e.Shape != "" {
			p(" · %s: %s", md(e.Shape), md(e.ShapeWhy))
		}
		p("\n\n")
		if len(e.Runs) > 0 {
			p("- %d agent run(s): %s\n", len(e.Runs), md(e.Agents()))
			p("- fresh tokens: est %s (%s–%s), actual %s %s\n", fmtTok(int64(e.EstTokens.Mid)), fmtTok(int64(e.EstTokens.Low)), fmtTok(int64(e.EstTokens.High)),
				fmtTok(e.Tokens.Total()), md(e.TokenDelta()))
			if e.HasUSD() {
				p("- $ (API-equivalent): est $%.2f, actual $%.2f %s\n", e.EstUSD.Mid, e.Tokens.CostUSD, md(e.USDDelta()))
			}
		}
		for _, c := range e.Reviews {
			p("- final review %s: %s\n", md(c.Outcome), md(c.Why))
		}
		for _, c := range e.ReqTests {
			p("- independent tests %s (round %d): %s\n", md(c.Outcome), c.Round, md(c.Why))
		}
		for _, pw := range e.Providers {
			p("- %s: %d run(s), %s fresh tokens · %s\n", md(pw.Provider), pw.Runs, fmtTok(pw.Tokens), md(strings.Join(pw.Rules, ", ")))
		}
		if len(e.Escalations) == 0 {
			p("- escalations: none, every run kept its first route\n")
		}
		for _, x := range e.Escalations {
			if where := x.Where(); where != "" {
				p("- escalation at %s: %s — %s\n", md(where), md(x.What), md(x.Cause))
			} else {
				p("- escalation: %s — %s\n", md(x.What), md(x.Cause))
			}
		}
		p("\n")
	}
	if len(d.Routes) > 0 {
		p("## Routing decisions\n\n| Agent| Step | Role | Route | Rule | Reason | Confidence | Outcome | Tokens | Time |\n|---|---|---|---|---|---|---|---|---|---|\n")
		for _, r := range d.Routes {
			out := "not run"
			switch {
			case r.LimitHit:
				out = "limit"
			case r.Ran && r.OK:
				out = "ok"
			case r.Ran:
				out = "failed"
			}
			rule := r.Rule
			if r.Judged {
				rule += " (judged)"
			}
			if r.Fallback {
				rule += " (fallback)"
			}
			conf := ""
			if r.Confidence > 0 {
				conf = fmt.Sprintf("%.2f", r.Confidence)
			}
			tok, took := "", ""
			if r.Ran {
				tok, took = fmtTok(r.Tokens.Total()), humanDur(r.Duration)
			}
			step := r.Step
			if r.Attempt > 1 {
				step += fmt.Sprintf(" #%d", r.Attempt)
			}
			p("| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", md(r.Agent), md(step), md(r.Role), md(r.Label()), md(rule), md(r.Reason), conf, out, tok, took)
		}
		p("\n")
	}
	if len(d.Reviews) > 0 {
		p("## Reviews\n\n")
		for _, r := range d.Reviews {
			v := "approved"
			if !r.Approve {
				v = "changes requested"
			}
			p("- **%s** (%s): %s\n", md(r.Checkpoint), md(r.Provider+":"+r.Model), v)
			if r.Advice != "" {
				p("\n%s\n", indent(fence(r.Advice, "text"), "  "))
			}
		}
		p("\n")
	}
	if len(d.Checks) > 0 {
		p("## Checks\n\n| Command | Kind | Tests | Result | Time |\n|---|---|---|---|---|\n")
		for _, c := range d.Checks {
			res := "pass"
			if !c.OK {
				res = "**fail**"
			}
			p("| %s | %s | %s | %s | %s |\n", mdCode(c.Command), c.Kind, md(c.Tests()), res, humanDur(c.Duration))
		}
		p("\n")
	}
	if len(d.Limits) > 0 {
		p("## Limits\n\n")
		for _, l := range d.Limits {
			p("- %s %s: %s\n", md(l.Provider+":"+l.Model), md(l.Agent), md(l.Text))
		}
		p("\n")
	}
	p("## Cost\n\n")
	if d.HasCost {
		p("| Provider | Fresh tokens | Input | Cached | Output | Limit before | Limit after |\n|---|---|---|---|---|---|---|\n")
		for _, pr := range d.Providers() {
			u := d.Cost.PerProvider[pr]
			q := func(m map[string]float64) string {
				if v, ok := m[pr]; ok {
					return fmt.Sprintf("%.0f%%", v*100)
				}
				return "-"
			}
			p("| %s | %s | %s | %s | %s | %s | %s |\n", pr, fmtTok(u.Total()), fmtTok(u.Input), fmtTok(u.Cached), fmtTok(u.Output), q(d.Cost.QuotaBefore), q(d.Cost.QuotaAfter))
		}
		if d.Cost.CostUSD > 0 {
			p("\n≈ $%.2f API-equivalent for the Claude part (not billed on a subscription).\n", d.Cost.CostUSD)
		}
		p("\n")
	} else if d.CostLine != "" {
		p("%s\n\n", md(d.CostLine))
	} else {
		p("no cost recorded\n\n")
	}
	if df := d.Diff; df != nil {
		p("## Changes\n\n%d file(s), +%d -%d (`%s` → `%s`)\n\n", len(df.Files), df.Add, df.Del, df.Before, df.After)
		if df.Undone {
			p("> This task was undone since; the diff shows what it had changed.\n\n")
		}
		if df.Truncated {
			p("> Large diff: some lines are not shown. Full diff: `git diff %s %s`\n\n", df.Before, df.After)
		}
		for _, f := range df.Files {
			p("<details><summary>%s %s (+%d -%d)</summary>\n\n", htmlText(f.Status), htmlText(f.Path), f.Add, f.Del)
			switch {
			case f.Binary:
				p("binary file\n\n")
			case len(f.Lines) > 0:
				var body strings.Builder
				for _, l := range f.Lines {
					body.WriteString(l.Text())
					body.WriteByte('\n')
				}
				p("%s", fence(strings.TrimSuffix(body.String(), "\n"), "diff"))
				if f.Truncated {
					p("\nmore lines not shown\n")
				}
			default:
				p("content not shown (size limit)\n")
			}
			p("\n</details>\n\n")
		}
	}
	if d.UndoCmd != "" {
		p("## Undo\n\n%s\n", fence(d.UndoCmd, "sh"))
	}
	p("---\nGenerated by Relayweft %s on %s. Contains the task text, agent answers and the repo diff; no environment or credentials.\n",
		md(d.Version), d.Generated.Format("2006-01-02 15:04:05"))
	_, err := io.WriteString(w, b.String())
	return err
}

func fmtTok(n int64) string { return event.HumanTokens(n) }

// fence wraps s in a code fence longer than any backtick run inside it.
func fence(s, lang string) string {
	if s == "" {
		return ""
	}
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", max(3, longest+1))
	return f + lang + "\n" + s + "\n" + f + "\n"
}

func indent(s, pre string) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = pre + l
	}
	return strings.Join(lines, "\n") + "\n"
}

var mdEsc = strings.NewReplacer(
	"\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)",
	"#", "\\#", "!", "\\!", "|", "\\|", "~", "\\~", "<", "&lt;", ">", "&gt;", "&", "&amp;",
	"\r\n", " ", "\n", " ", "\r", " ",
)

// md escapes text for inline Markdown (one line, table-safe, no HTML).
func md(s string) string { return mdEsc.Replace(s) }

// mdCode is inline code safe in a table cell.
func mdCode(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	n := 1
	for strings.Contains(s, strings.Repeat("`", n)) {
		n++
	}
	f := strings.Repeat("`", n)
	return f + " " + s + " " + f
}

var htmlEsc = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;", "\n", " ", "\r", " ")

// htmlText escapes text used inside raw HTML in the Markdown (<summary>).
func htmlText(s string) string { return htmlEsc.Replace(s) }
