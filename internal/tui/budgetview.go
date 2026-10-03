package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// budgetOverlay asks whether a task may go on past a budget limit:
// y/enter continues (until the task ends), n/esc stops the task.
type budgetOverlay struct{ r *approvalReq }

func (b *budgetOverlay) req() *approvalReq { return b.r }

func (b *budgetOverlay) keys() string {
	return "y continue (until this task ends) · n / esc stop the task"
}

func (b *budgetOverlay) update(m *Model, k tea.KeyMsg) tea.Cmd {
	switch strings.ToLower(k.String()) {
	case "y", "enter":
		m.flashNotice("budget: going on until this task ends")
		m.answer(approvalReply{ok: true})
	case "n", "esc":
		m.flashNotice("budget: stopping the task")
		m.answer(approvalReply{ok: false})
	}
	return nil
}

func (b *budgetOverlay) view(m *Model, W, H int) string {
	th := m.th
	r := b.r.budget
	w := min(W, 90)
	iw := w - 4
	lines := []string{
		fit(th.bold(th.Warn).Render("BUDGET REACHED"), iw),
		"",
		fit(th.fg(th.Text).Render(r.String()), iw),
		fit(th.fg(th.Muted).Render("task: ")+th.fg(th.Text).Render(oneLine(r.Task, 0)), iw),
	}
	if r.Next != "" {
		lines = append(lines, fit(th.fg(th.Muted).Render("next: "+r.Next), iw))
	}
	lines = append(lines, "",
		fit(th.bold(th.Main).Render("Continue? y/n")+th.fg(th.Muted).Render("  (y lets this task run past the limit until it ends; n stops it cleanly)"), iw),
		fit(th.fg(th.Faint).Render("to change it for good: "+r.RaiseHint()), iw))
	h := min(H-2, len(lines))
	box := th.box(th.Warn, w).Height(max(1, h)).Render(clipLines(strings.Join(lines, "\n"), max(1, H-2)))
	return indent(box, max(0, (W-w)/2))
}
