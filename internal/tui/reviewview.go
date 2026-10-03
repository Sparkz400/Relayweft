package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sparkz400/switchyard/internal/orchestrator"
)

// reviewOverlay shows one agent's changes file by file before they land:
// the person picks which files (and, in modified files, which hunks) to
// apply, rejects all, or sends the agent back with feedback.
type reviewOverlay struct {
	r        *approvalReq
	cs       orchestrator.ChangeSet
	sel      int
	include  []bool
	hunks    [][]bool // per file: one flag per hunk; nil = the file can't be split
	inDiff   bool     // focus on the hunks of the selected file
	hunk     int      // the hunk under the cursor (inDiff)
	scroll   int      // diff lines scrolled
	diffH    int      // diff lines on screen at the last render (for paging)
	feedback bool
	fi       textinput.Model
	confirm  bool   // enter with nothing selected: ask once more
	note     string // a one-off hint shown above the diff
}

func newReviewOverlay(r *approvalReq) *reviewOverlay {
	fi := textinput.New()
	fi.Prompt = "feedback: "
	fi.Placeholder = "what should the agent change?"
	fi.CharLimit = 2000
	inc := make([]bool, len(r.changes.Files))
	hunks := make([][]bool, len(r.changes.Files))
	for i, f := range r.changes.Files {
		inc[i] = true
		if f.Splittable() {
			_, hs := orchestrator.SplitHunks(f.Patch)
			hunks[i] = make([]bool, len(hs))
			for j := range hunks[i] {
				hunks[i][j] = true
			}
		}
	}
	return &reviewOverlay{r: r, cs: *r.changes, include: inc, hunks: hunks, fi: fi}
}

func (v *reviewOverlay) req() *approvalReq { return v.r }

func (v *reviewOverlay) keys() string {
	switch {
	case v.feedback:
		return "type what to change · enter send to the agent · esc back"
	case v.inDiff:
		return "hunks: ↑↓ hunk · space toggle hunk · tab/esc files · enter apply · j/k scroll · f feedback"
	}
	return "↑↓ file · space toggle · tab hunks · a/n all/none · j/k pgdn scroll · enter apply · f feedback · esc reject"
}

// unsplittable says why a file can only be taken whole ("" if it can be
// split hunk by hunk).
func unsplittable(f orchestrator.FileChange) string {
	switch {
	case f.Splittable():
		return ""
	case f.Binary:
		return "binary file"
	case f.Status == "A":
		return "new file"
	case f.Status == "D":
		return "deleted file"
	case strings.Contains(f.Patch, "\n... (diff truncated) ..."):
		return "diff truncated"
	}
	return "only one hunk"
}

// partial reports whether file i is applied with only some of its hunks.
func (v *reviewOverlay) partial(i int) bool {
	if !v.include[i] || v.hunks[i] == nil {
		return false
	}
	for _, on := range v.hunks[i] {
		if !on {
			return true
		}
	}
	return false
}

// setFile includes or excludes a whole file (all of its hunks).
func (v *reviewOverlay) setFile(i int, on bool) {
	v.include[i] = on
	for j := range v.hunks[i] {
		v.hunks[i][j] = on
	}
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

// decision is the answer for the current selection: every file with at
// least one hunk on in Apply, and the kept hunks of partly selected files
// in Hunks.
func (v *reviewOverlay) decision() orchestrator.ChangeDecision {
	d := orchestrator.ChangeDecision{Apply: v.selected()}
	for i, f := range v.cs.Files {
		if !v.partial(i) {
			continue
		}
		var keep []int
		for j, on := range v.hunks[i] {
			if on {
				keep = append(keep, j)
			}
		}
		if d.Hunks == nil {
			d.Hunks = map[string][]int{}
		}
		d.Hunks[f.Path] = keep
	}
	return d
}

// diffLines is the selected file's diff as rendered, with the index of the
// hunk each line belongs to (-1 for the header).
func (v *reviewOverlay) diffLines() (lines []string, hunkOf []int, starts []int) {
	f := v.cs.Files[v.sel]
	switch {
	case f.Binary:
		return []string{"(binary file, no diff)"}, []int{-1}, nil
	case strings.TrimSpace(f.Patch) == "":
		return []string{"(no diff text)"}, []int{-1}, nil
	}
	lines = strings.Split(strings.TrimRight(f.Patch, "\n"), "\n")
	h := -1
	for n, l := range lines {
		if strings.HasPrefix(l, "@@ ") {
			h++
			starts = append(starts, n)
		}
		hunkOf = append(hunkOf, h)
	}
	return lines, hunkOf, starts
}

// moveHunk moves the hunk cursor and scrolls its header into view.
func (v *reviewOverlay) moveHunk(d int) {
	hs := v.hunks[v.sel]
	if hs == nil {
		return
	}
	v.inDiff = true
	v.hunk = max(0, min(len(hs)-1, v.hunk+d))
	if _, _, st := v.diffLines(); v.hunk < len(st) {
		v.scroll = max(0, st[v.hunk]-1)
	}
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
	v.note = ""
	page := max(1, v.diffH-1)
	nf := len(v.cs.Files)
	if v.inDiff {
		switch s {
		case "up", "[":
			v.moveHunk(-1)
			return nil
		case "down", "]":
			v.moveHunk(1)
			return nil
		case " ", "space":
			if hs := v.hunks[v.sel]; v.hunk < len(hs) {
				hs[v.hunk] = !hs[v.hunk]
				on := false
				for _, x := range hs {
					on = on || x
				}
				v.include[v.sel] = on
			}
			return nil
		case "tab", "esc":
			v.inDiff = false
			return nil
		}
	}
	switch s {
	case "up":
		if v.sel > 0 {
			v.sel--
			v.scroll, v.hunk = 0, 0
		}
	case "down":
		if v.sel < nf-1 {
			v.sel++
			v.scroll, v.hunk = 0, 0
		}
	case "tab", "[", "]":
		if nf == 0 {
			return nil
		}
		if why := unsplittable(v.cs.Files[v.sel]); why != "" {
			v.note = "this file can only be taken whole (" + why + ")"
			return nil
		}
		v.moveHunk(0)
	case " ", "space":
		if nf > 0 {
			v.setFile(v.sel, !v.include[v.sel] || v.partial(v.sel))
		}
	case "a":
		for i := range v.include {
			v.setFile(i, true)
		}
	case "n":
		for i := range v.include {
			v.setFile(i, false)
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
		d := v.decision()
		if len(d.Apply) == 0 && !v.confirm {
			v.confirm = true
			return nil
		}
		if len(d.Apply) == 0 {
			m.flashNotice(v.cs.StepID + ": all changes rejected (kept on a branch)")
		} else {
			msg := fmt.Sprintf("%s: applying %d of %d file(s)", v.cs.StepID, len(d.Apply), nf)
			if len(d.Hunks) > 0 {
				msg += fmt.Sprintf(", %d of them in part", len(d.Hunks))
			}
			m.flashNotice(msg)
		}
		m.answer(approvalReply{decision: d})
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
	partial := 0
	for i := range v.cs.Files {
		if v.partial(i) {
			partial++
		}
	}
	sum := fmt.Sprintf("%d of %d file(s) selected", len(v.selected()), nf)
	if partial > 0 {
		sum += fmt.Sprintf(" (%d in part)", partial)
	}
	lines = append(lines, fit(th.fg(th.Muted).Render(sum+" · ")+
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
		switch {
		case v.partial(i):
			box = th.fg(th.Warn).Render("[~]")
		case v.include[i]:
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
		if hs := v.hunks[i]; hs != nil {
			on := 0
			for _, x := range hs {
				if x {
					on++
				}
			}
			counts += th.fg(th.Muted).Render(fmt.Sprintf(" · %d/%d hunks", on, len(hs)))
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
	case v.note != "":
		foot = th.fg(th.Warn).Render(v.note)
	}
	// The diff of the selected file.
	diffH := H - 2 - len(lines) - 1
	if foot != "" {
		diffH--
	}
	v.diffH = diffH
	if diffH > 0 && nf > 0 {
		f := v.cs.Files[v.sel]
		dl, hunkOf, _ := v.diffLines()
		hs := v.hunks[v.sel]
		v.scroll = min(v.scroll, max(0, len(dl)-diffH))
		end := min(len(dl), v.scroll+diffH)
		pos := ""
		if len(dl) > diffH {
			pos = fmt.Sprintf(" · lines %d-%d of %d", v.scroll+1, end, len(dl))
		}
		mode := ""
		switch {
		case hs != nil && v.inDiff:
			mode = fmt.Sprintf(" · hunk %d of %d · space toggles it", v.hunk+1, len(hs))
		case hs != nil:
			mode = fmt.Sprintf(" · %d hunks · tab to pick hunks", len(hs))
		case f.Status == "M" && !f.Binary:
			mode = " · whole file only (" + unsplittable(f) + ")"
		}
		lines = append(lines, fit(th.fg(th.Router).Render(th.G.H+th.G.H+" "+f.Path)+th.fg(th.Faint).Render(pos+mode), iw))
		bar := th.fg(th.Main).Render(th.G.V + " ")
		if th.ASCII {
			bar = th.fg(th.Main).Render("| ")
		}
		for n := v.scroll; n < end; n++ {
			l := dl[n]
			if hs == nil {
				lines = append(lines, fit(m.diffLine(l), iw))
				continue
			}
			h := hunkOf[n]
			gutter := "  "
			if v.inDiff && h == v.hunk {
				gutter = bar
				if strings.HasPrefix(l, "@@ ") {
					gutter = th.fg(th.Main).Render("> ")
				}
			}
			body := m.diffLine(l)
			switch {
			case h < 0:
				gutter += "    "
			case strings.HasPrefix(l, "@@ "):
				box := "[ ] "
				if hs[h] {
					box = th.fg(th.OKColor).Render("[x]") + " "
				}
				gutter += box
			default:
				gutter += "    "
				if !hs[h] {
					body = th.fg(th.Faint).Render(strings.ReplaceAll(strings.TrimRight(l, "\r"), "\t", "    "))
				}
			}
			lines = append(lines, fit(gutter+body, iw))
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
