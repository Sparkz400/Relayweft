package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sparkz400/switchyard/internal/orchestrator"
)

// reviewOverlay shows one agent's changes file by file before they land:
// the person picks which files to apply, rejects all, or sends the agent
// back with feedback.
type reviewOverlay struct {
	r        *approvalReq
	cs       orchestrator.ChangeSet
	sel      int
	include  []bool
	scroll   int // diff lines scrolled
	diffH    int // diff lines on screen at the last render (for paging)
	feedback bool
	fi       textinput.Model
	confirm  bool // enter with nothing selected: ask once more
}

func newReviewOverlay(r *approvalReq) *reviewOverlay {
	fi := textinput.New()
	fi.Prompt = "feedback: "
	fi.Placeholder = "what should the agent change?"
	fi.CharLimit = 2000
	inc := make([]bool, len(r.changes.Files))
	for i := range inc {
		inc[i] = true
	}
	return &reviewOverlay{r: r, cs: *r.changes, include: inc, fi: fi}
}

func (v *reviewOverlay) req() *approvalReq { return v.r }

func (v *reviewOverlay) keys() string {
	if v.feedback {
		return "type what to change · enter send to the agent · esc back"
	}
	return "↑↓ file · space toggle · a/n all/none · j/k pgdn scroll · enter apply · f feedback · esc reject"
}

// selected lists the paths to apply.
func (v *reviewOverlay) selected() []string {
	var out []string
	for i, f := range v.cs.Files {
		if v.include[i] {
			out = append(out, f.Path)
		}
	}
	return out
}

func (v *reviewOverlay) update(m *Model, k tea.KeyMsg) tea.Cmd {
	if v.feedback {
		switch k.Type {
		case tea.KeyEnter:
			text := strings.TrimSpace(v.fi.Value())
			if text == "" {
				return nil
			}
			m.flashNotice(v.cs.StepID + ": sent back to the agent with your feedback")
			m.answer(approvalReply{decision: orchestrator.ChangeDecision{Feedback: text}})
			return nil
		case tea.KeyEsc:
			v.feedback = false
			v.fi.Blur()
			return nil
		}
		var cmd tea.Cmd
		v.fi, cmd = v.fi.Update(k)
		return cmd
	}
	s := k.String()
	if s != "enter" {
		v.confirm = false
	}
	page := max(1, v.diffH-1)
	switch s {
	case "up":
		if v.sel > 0 {
			v.sel--
			v.scroll = 0
		}
	case "down", "tab":
		if v.sel < len(v.cs.Files)-1 {
			v.sel++
			v.scroll = 0
		}
	case " ", "space":
		if len(v.include) > 0 {
			v.include[v.sel] = !v.include[v.sel]
		}
	case "a":
		for i := range v.include {
			v.include[i] = true
		}
	case "n":
		for i := range v.include {
			v.include[i] = false
		}
	case "j":
		v.scroll++
	case "k":
		v.scroll = max(0, v.scroll-1)
	case "pgdown", "ctrl+d":
		v.scroll += page
	case "pgup", "ctrl+u":
		v.scroll = max(0, v.scroll-page)
	case "f":
		v.feedback = true
		v.fi.SetValue("")
		return v.fi.Focus()
	case "enter":
		apply := v.selected()
		if len(apply) == 0 && !v.confirm {
			v.confirm = true
			return nil
		}
		if len(apply) == 0 {
			m.flashNotice(v.cs.StepID + ": all changes rejected (kept on a branch)")
		} else {
			m.flashNotice(fmt.Sprintf("%s: applying %d of %d file(s)", v.cs.StepID, len(apply), len(v.cs.Files)))
		}
		m.answer(approvalReply{decision: orchestrator.ChangeDecision{Apply: apply}})
	case "esc":
		m.flashNotice(v.cs.StepID + ": all changes rejected (kept on a branch)")
		m.answer(approvalReply{decision: orchestrator.ChangeDecision{}})
	}
	return nil
}

// wheel scrolls the diff.
func (v *reviewOverlay) wheel(d int) { v.scroll = max(0, v.scroll+d) }

func (v *reviewOverlay) view(m *Model, W, H int) string {
	th := m.th
	w := W
	iw := w - 4
	var lines []string
	title := th.bold(th.Main).Render("REVIEW CHANGES") + th.fg(th.Muted).Render(" · "+v.cs.StepID+" · "+oneLine(v.cs.Title, 0))
	if v.cs.Round > 1 {
		title += th.fg(th.Warn).Render(fmt.Sprintf(" · round %d (after your feedback)", v.cs.Round))
	}
	if n := len(m.approvals); n > 1 {
		title += th.fg(th.Muted).Render(fmt.Sprintf(" · %d more waiting", n-1))
	}
	lines = append(lines, fit(title, iw))
	if v.cs.Summary != "" {
		lines = append(lines, fit(th.fg(th.Muted).Render("agent: "+oneLine(v.cs.Summary, 0)), iw))
	}
	// File list: at most a third of the room, scrolled to the selection.
	nf := len(v.cs.Files)
	listH := max(1, min(nf, (H-2)/3))
	start := 0
	if v.sel >= listH {
		start = v.sel - listH + 1
	}
	added, deleted := 0, 0
	for _, f := range v.cs.Files {
		added += f.Added
		deleted += f.Deleted
	}
	lines = append(lines, fit(th.fg(th.Muted).Render(fmt.Sprintf("%d of %d file(s) selected · ", len(v.selected()), nf))+
		th.fg(th.OKColor).Render(fmt.Sprintf("+%d", added))+" "+th.fg(th.FailColor).Render(fmt.Sprintf("-%d", deleted)), iw))
	for i := start; i < nf && i < start+listH; i++ {
		f := v.cs.Files[i]
		cursor := "  "
		if i == v.sel {
			cursor = th.fg(th.Main).Render("> ")
			if !th.ASCII {
				cursor = th.fg(th.Main).Render(th.G.Arrow + " ")
			}
		}
		box := "[ ]"
		if v.include[i] {
			box = th.fg(th.OKColor).Render("[x]")
		}
		sc := th.Main
		switch f.Status {
		case "A":
			sc = th.OKColor
		case "D":
			sc = th.FailColor
		}
		counts := th.fg(th.OKColor).Render(fmt.Sprintf("+%d", f.Added)) + " " + th.fg(th.FailColor).Render(fmt.Sprintf("-%d", f.Deleted))
		if f.Binary {
			counts = th.fg(th.Muted).Render("binary")
		}
		ps := th.fg(th.Text)
		if i == v.sel {
			ps = th.bold(th.Focus)
		}
		lines = append(lines, fit(cursor+box+" "+th.bold(sc).Render(f.Status)+" "+ps.Render(f.Path)+"  "+counts, iw))
	}
	foot := ""
	switch {
	case v.feedback:
		foot = v.fi.View()
	case v.confirm:
		foot = th.fg(th.Warn).Render("nothing selected: enter again rejects all changes (kept on a branch) · a selects all")
	}
	// The diff of the selected file.
	diffH := H - 2 - len(lines) - 1
	if foot != "" {
		diffH--
	}
	v.diffH = diffH
	if diffH > 0 && nf > 0 {
		f := v.cs.Files[v.sel]
		var dl []string
		switch {
		case f.Binary:
			dl = []string{"(binary file, no diff)"}
		case strings.TrimSpace(f.Patch) == "":
			dl = []string{"(no diff text)"}
		default:
			dl = strings.Split(strings.TrimRight(f.Patch, "\n"), "\n")
		}
		v.scroll = min(v.scroll, max(0, len(dl)-diffH))
		end := min(len(dl), v.scroll+diffH)
		pos := ""
		if len(dl) > diffH {
			pos = fmt.Sprintf(" · lines %d-%d of %d", v.scroll+1, end, len(dl))
		}
		lines = append(lines, fit(th.fg(th.Router).Render(th.G.H+th.G.H+" "+f.Path)+th.fg(th.Faint).Render(pos), iw))
		for _, l := range dl[v.scroll:end] {
			lines = append(lines, fit(m.diffLine(l), iw))
		}
	}
	if foot != "" {
		lines = append(lines, fit(foot, iw))
	}
	return th.box(th.Main, w).Height(max(1, H-2)).Render(clipLines(strings.Join(lines, "\n"), H-2))
}

// diffLine colors one unified-diff line.
func (m *Model) diffLine(l string) string {
	th := m.th
	l = strings.ReplaceAll(strings.TrimRight(l, "\r"), "\t", "    ")
	switch {
	case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"), strings.HasPrefix(l, "diff "), strings.HasPrefix(l, "index "):
		return th.fg(th.Faint).Render(l)
	case strings.HasPrefix(l, "@@"):
		return th.fg(th.Router).Render(l)
	case strings.HasPrefix(l, "+"):
		return th.fg(th.OKColor).Render(l)
	case strings.HasPrefix(l, "-"):
		return th.fg(th.FailColor).Render(l)
	}
	return th.fg(th.Text).Render(l)
}
