package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// conflictOverlay asks whether an agent may resolve a merge conflict:
// y/enter lets it, n/esc keeps the change on a branch.
type conflictOverlay struct{ r *approvalReq }

func (c *conflictOverlay) req() *approvalReq { return c.r }

func (c *conflictOverlay) keys() string {
	return "y let an agent resolve it · n / esc keep it on a branch"
}

func (c *conflictOverlay) update(m *Model, k tea.KeyMsg) tea.Cmd {
	switch strings.ToLower(k.String()) {
	case "y", "enter":
		m.flashNotice(c.r.conflict.StepID + ": an agent resolves the conflict")
		m.answer(approvalReply{ok: true})
	case "n", "esc":
		m.flashNotice(c.r.conflict.StepID + ": the change is kept on a branch")
		m.answer(approvalReply{ok: false})
	}
	return nil
}

func (c *conflictOverlay) view(m *Model, W, H int) string {
	th := m.th
	q := c.r.conflict
	w := min(W, 90)
	iw := w - 4
	lines := []string{
		fit(th.bold(th.Warn).Render("MERGE CONFLICT"), iw),
		"",
		fit(th.fg(th.Text).Render(q.String()), iw),
		fit(th.fg(th.Muted).Render("step: ")+th.fg(th.Text).Render(oneLine(q.Title, 0)), iw),
	}
	for _, f := range q.Files {
		lines = append(lines, fit(th.fg(th.Muted).Render("  "+f), iw))
	}
	lines = append(lines, "",
		fit(th.bold(th.Main).Render("Let an agent resolve it? y/n"), iw))
	for _, l := range wrapWords(q.Hint(), iw) {
		lines = append(lines, fit(th.fg(th.Faint).Render(l), iw))
	}
	h := min(H-2, len(lines))
	box := th.box(th.Warn, w).Height(max(1, h)).Render(clipLines(strings.Join(lines, "\n"), max(1, H-2)))
	return indent(box, max(0, (W-w)/2))
}

// wrapWords breaks text into lines of at most w runes at spaces.
func wrapWords(text string, w int) []string {
	var out []string
	line := ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case len([]rune(line))+1+len([]rune(word)) > w:
			out = append(out, line)
			line = word
		default:
			line += " " + word
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}
