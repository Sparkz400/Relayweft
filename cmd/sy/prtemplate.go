package main

import (
	"regexp"
	"strings"

	"github.com/sparkz400/switchyard/internal/repodocs"
)

// Pull request templates: when the repo has one (in any place GitHub
// reads it from), sy pr lays out the description by the template's
// headings and fills each from the task:
//
//   - a summary/description heading gets the task (fenced) and the plan's
//     summary; a plan/approach heading the steps table; a testing heading
//     the checks; a notes/result heading the status and cost;
//   - the template's own prose and HTML comments are left out: they are
//     instructions for a person, written by whoever pushed to the repo,
//     and sy does not act on them. Checkbox items stay, unticked, for the
//     reviewer to tick;
//   - whatever found no heading goes under a "Switchyard" heading at the
//     end, so nothing of the default layout is lost;
//   - a template without headings is not used.

// prTemplate is the text of the repo's pull request template ("" if none).
func prTemplate(root string) string {
	_, text := repodocs.PRTemplate(root)
	return text
}

var (
	reHeading  = regexp.MustCompile(`^(#{1,6})[ \t]+(.+?)[ \t#]*$`)
	reFenceTok = regexp.MustCompile("^[ \t]{0,3}(```+|~~~+)")
	reCheckbox = regexp.MustCompile(`^[ \t]*[-*+][ \t]+\[[ xX]\][ \t]+\S`)
	reTicked   = regexp.MustCompile(`\[[xX]\]`)
	reHTMLNote = regexp.MustCompile(`(?s)<!--.*?(-->|$)`)

	// What a heading asks for, tried in this order.
	headingKinds = []struct {
		kind string
		re   *regexp.Regexp
	}{
		{"other", regexp.MustCompile(`\b(type of|checklist|check list|screenshots?|breaking)\b`)},
		{"checks", regexp.MustCompile(`\b(test\w*|verif\w*|checks?|qa|validat\w*)\b`)},
		{"plan", regexp.MustCompile(`\b(plan|implementation|approach|how|steps|design|solution)\b`)},
		{"task", regexp.MustCompile(`\b(summary|description|describe|overview|what|changes?|about|motivation|context|why|purpose|problem|issue|ticket|background)\b`)},
		{"result", regexp.MustCompile(`\b(results?|outcome|status|notes?|cost|additional|comments?|impact)\b`)},
	}
)

// tmplSection is one heading of a template and its checkbox items.
type tmplSection struct {
	heading string // the heading line as written
	title   string
	boxes   []string
}

// parseTemplate splits a template into its headings (outside code
// blocks), keeping only each section's checkbox items.
func parseTemplate(text string) []tmplSection {
	text = reHTMLNote.ReplaceAllString(strings.ReplaceAll(text, "\r\n", "\n"), "")
	var out []tmplSection
	fence := ""
	for _, l := range strings.Split(text, "\n") {
		if m := reFenceTok.FindStringSubmatch(l); m != nil {
			switch {
			case fence == "":
				fence = m[1]
			case strings.HasPrefix(m[1], fence[:1]) && len(m[1]) >= len(fence):
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		if m := reHeading.FindStringSubmatch(l); m != nil {
			title := strings.TrimSpace(m[2])
			out = append(out, tmplSection{heading: m[1] + " " + oneLine(title, 120), title: title})
			continue
		}
		if len(out) > 0 && reCheckbox.MatchString(l) {
			s := &out[len(out)-1]
			s.boxes = append(s.boxes, reTicked.ReplaceAllString(strings.TrimRight(l, " \t"), "[ ]"))
		}
	}
	return out
}

// headingKind classifies a template heading: task, plan, checks, result
// or other.
func headingKind(title string) string {
	t := strings.ToLower(title)
	for _, k := range headingKinds {
		if k.re.MatchString(t) {
			return k.kind
		}
	}
	return "other"
}

// fillPRTemplate lays the description out by the template's headings ("" when
// the template has none: the default layout is used).
func fillPRTemplate(tmpl string, p prParts) string {
	secs := parseTemplate(tmpl)
	if len(secs) == 0 {
		return ""
	}
	pieces := map[string]string{
		"task": p.task,
		"plan": p.plan,
		"checks": func() string {
			if p.checks != "" {
				return p.checks
			}
			return "- Checks: none were run by Switchyard (no verify commands)\n"
		}(),
		"result": p.result,
	}
	if p.summary != "" {
		pieces["task"] += "\n" + p.summary + "\n"
	}
	// A testing heading shows the checks: the result does not repeat them.
	for _, s := range secs {
		if headingKind(s.title) == "checks" {
			if p.checks != "" {
				pieces["result"] = strings.Replace(p.result, p.checks, "", 1)
			}
			break
		}
	}
	used := map[string]bool{}
	var b strings.Builder
	b.WriteString(p.warning)
	for _, s := range secs {
		b.WriteString(s.heading + "\n\n")
		kind := headingKind(s.title)
		body := ""
		if kind != "other" && !used[kind] && pieces[kind] != "" {
			used[kind] = true
			body = pieces[kind]
		}
		if body == "" && len(s.boxes) == 0 {
			body = "_Not filled in by Switchyard._\n"
		}
		b.WriteString(body)
		if len(s.boxes) > 0 {
			if body != "" {
				b.WriteString("\n")
			}
			b.WriteString(strings.Join(s.boxes, "\n") + "\n")
		}
		b.WriteString("\n")
	}
	// What found no heading, so nothing of the default layout is lost.
	var rest strings.Builder
	if !used["task"] {
		rest.WriteString("**Task**\n\n" + pieces["task"] + "\n")
	}
	if !used["plan"] && p.plan != "" {
		rest.WriteString("**Plan**\n\n" + p.plan + "\n")
	}
	if !used["result"] {
		rest.WriteString("**Result**\n\n" + pieces["result"])
	}
	if rest.Len() > 0 {
		b.WriteString("## Switchyard\n\n" + rest.String())
	}
	b.WriteString(p.footer)
	return b.String()
}
