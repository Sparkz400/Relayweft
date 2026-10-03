package tui

import (
	"fmt"
	"strings"

	bkey "github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/router"
)

// planKinds are the subtask kinds a person can pick (NormalizePlan turns
// anything else into edit).
var planKinds = []router.Kind{router.KindExplore, router.KindResearch, router.KindEdit}

// planRoles are the roles a subtask can be pinned to; "" lets the router
// decide.
var planRoles = []string{"", event.RolePlanner, event.RoleWorker, event.RoleWorkerHigh, event.RoleExplorer, event.RoleResearcher}

// planOverlay lets the person edit, delete, reorder and re-route the
// planned subtasks before any agent runs.
type planOverlay struct {
	r       *approvalReq
	plan    orchestrator.Plan
	sel     int
	editing bool
	ta      textarea.Model
	err     string
	// The dependency picker for the selected step (x): the other steps
	// with a checkbox each.
	deps   bool
	depSel int
	depOn  []bool // parallel to depIDs
	depIDs []string
}

func newPlanOverlay(r *approvalReq, ascii bool) *planOverlay {
	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.Prompt = "  "
	if !ascii {
		ta.Prompt = "│ "
	}
	ta.KeyMap.InsertNewline = bkey.NewBinding(bkey.WithKeys("enter", "ctrl+j"))
	return &planOverlay{r: r, plan: clonePlan(*r.plan), ta: ta}
}

func (p *planOverlay) req() *approvalReq { return p.r }

func (p *planOverlay) keys() string {
	if p.editing {
		return "editing the prompt · ctrl+s keep · esc discard"
	}
	if p.deps {
		return "dependencies: ↑↓ step · space toggle · enter done · esc cancel"
	}
	return "↑↓ select · e edit · x deps · d delete · k kind · r role · J/K move · enter run · esc cancel task"
}

func cycleKind(k router.Kind) router.Kind {
	for i, x := range planKinds {
		if x == k {
			return planKinds[(i+1)%len(planKinds)]
		}
	}
	return planKinds[0]
}

func cycleRole(r string) string {
	for i, x := range planRoles {
		if x == r {
			return planRoles[(i+1)%len(planRoles)]
		}
	}
	return planRoles[0]
}

func (p *planOverlay) update(m *Model, k tea.KeyMsg) tea.Cmd {
	if p.editing {
		switch k.String() {
		case "ctrl+s":
			if p.sel < len(p.plan.Subtasks) {
				p.plan.Subtasks[p.sel].Prompt = strings.TrimSpace(p.ta.Value())
			}
			p.editing = false
			p.ta.Blur()
		case "esc":
			p.editing = false
			p.ta.Blur()
		default:
			var cmd tea.Cmd
			p.ta, cmd = p.ta.Update(k)
			return cmd
		}
		return nil
	}
	if p.deps {
		p.updateDeps(k)
		return nil
	}
	sts := p.plan.Subtasks
	p.err = ""
	switch k.String() {
	case "x":
		if len(sts) < 2 {
			p.err = "a step can only wait for another step: this plan has one"
			return nil
		}
		p.openDeps()
	case "up":
		if p.sel > 0 {
			p.sel--
		}
	case "down":
		if p.sel < len(sts)-1 {
			p.sel++
		}
	case "d", "delete":
		if len(sts) == 0 {
			return nil
		}
		gone := sts[p.sel].ID
		p.plan.Subtasks = append(sts[:p.sel:p.sel], sts[p.sel+1:]...)
		for i := range p.plan.Subtasks {
			st := &p.plan.Subtasks[i]
			deps := st.DependsOn[:0:0]
			for _, d := range st.DependsOn {
				if d != gone {
					deps = append(deps, d)
				}
			}
			st.DependsOn = deps
		}
		p.sel = min(p.sel, max(0, len(p.plan.Subtasks)-1))
	case "e":
		if len(sts) == 0 {
			return nil
		}
		p.editing = true
		p.ta.SetValue(sts[p.sel].Prompt)
		return p.ta.Focus()
	case "k":
		if len(sts) > 0 {
			sts[p.sel].Kind = cycleKind(sts[p.sel].Kind)
		}
	case "r":
		if len(sts) > 0 {
			sts[p.sel].Role = cycleRole(sts[p.sel].Role)
		}
	case "J", "shift+down":
		if p.sel < len(sts)-1 {
			sts[p.sel], sts[p.sel+1] = sts[p.sel+1], sts[p.sel]
			p.sel++
		}
	case "K", "shift+up":
		if p.sel > 0 {
			sts[p.sel], sts[p.sel-1] = sts[p.sel-1], sts[p.sel]
			p.sel--
		}
	case "enter":
		np, err := orchestrator.NormalizePlan(clonePlan(p.plan))
		if err != nil {
			p.err = "cannot run this plan: " + err.Error() + " (esc cancels the task)"
			return nil
		}
		m.flashNotice(fmt.Sprintf("plan approved: %d subtasks", len(np.Subtasks)))
		m.answer(approvalReply{plan: np, ok: true})
	case "esc":
		m.flashNotice("plan not approved: cancelling the task")
		m.answer(approvalReply{plan: p.plan, ok: false})
	}
	return nil
}

// openDeps opens the dependency picker for the selected step.
func (p *planOverlay) openDeps() {
	st := p.plan.Subtasks[p.sel]
	on := map[string]bool{}
	for _, d := range st.DependsOn {
		on[d] = true
	}
	p.depIDs, p.depOn, p.depSel = nil, nil, 0
	for _, o := range p.plan.Subtasks {
		if o.ID == st.ID {
			continue
		}
		p.depIDs = append(p.depIDs, o.ID)
		p.depOn = append(p.depOn, on[o.ID])
	}
	p.deps = true
}

func (p *planOverlay) updateDeps(k tea.KeyMsg) {
	switch k.String() {
	case "up":
		if p.depSel > 0 {
			p.depSel--
		}
	case "down":
		if p.depSel < len(p.depIDs)-1 {
			p.depSel++
		}
	case " ", "space":
		if p.depSel < len(p.depOn) {
			p.depOn[p.depSel] = !p.depOn[p.depSel]
		}
		p.err = ""
	case "enter":
		var deps []string
		for i, id := range p.depIDs {
			if p.depOn[i] {
				deps = append(deps, id)
			}
		}
		st := &p.plan.Subtasks[p.sel]
		if cyc := depCycle(p.plan.Subtasks, st.ID, deps); cyc != nil {
			p.err = "that makes a cycle: " + strings.Join(cyc, " after ") + " (untick a step, or esc)"
			return
		}
		st.DependsOn = deps
		p.deps, p.err = false, ""
	case "esc":
		p.deps, p.err = false, ""
	}
}

// depCycle reports the cycle that giving step id the dependencies deps
// would create (as "a after b after a"), or nil.
func depCycle(sts []orchestrator.Subtask, id string, deps []string) []string {
	graph := map[string][]string{}
	for _, s := range sts {
		graph[s.ID] = s.DependsOn
	}
	graph[id] = deps
	// A cycle through id: a path from one of its deps back to id.
	var path []string
	seen := map[string]bool{}
	var walk func(n string) bool
	walk = func(n string) bool {
		path = append(path, n)
		if n == id {
			return true
		}
		if !seen[n] {
			seen[n] = true
			for _, d := range graph[n] {
				if walk(d) {
					return true
				}
			}
		}
		path = path[:len(path)-1]
		return false
	}
	for _, d := range deps {
		path = []string{id}
		if walk(d) {
			return path
		}
	}
	return nil
}

func (p *planOverlay) view(m *Model, W, H int) string {
	th := m.th
	w := min(W, 118)
	iw := w - 4
	var lines []string
	n := len(p.plan.Subtasks)
	lines = append(lines, fit(th.bold(th.Main).Render("APPROVE THE PLAN")+th.fg(th.Muted).Render(fmt.Sprintf(" · %d subtasks · nothing has run yet", n)), iw))
	lines = append(lines, fit(th.fg(th.Muted).Render("task: ")+th.fg(th.Text).Render(oneLine(p.r.task, 0)), iw))
	if p.plan.Summary != "" {
		lines = append(lines, fit(th.fg(th.Muted).Render("plan: "+oneLine(p.plan.Summary, 0)), iw))
	}
	lines = append(lines, "")
	head := fmt.Sprintf("  %-12s %-9s %-12s %s", "ID", "KIND", "ROLE", "TITLE")
	lines = append(lines, th.bold(th.Muted).Render(fit(head, iw)))
	if n == 0 {
		lines = append(lines, th.fg(th.Warn).Render("  (no subtasks left: esc cancels the task)"))
	}
	for i, st := range p.plan.Subtasks {
		cursor := "  "
		if i == p.sel {
			cursor = th.fg(th.Main).Render(th.G.Arrow + " ")
			if th.ASCII {
				cursor = th.fg(th.Main).Render("> ")
			}
		}
		role := st.Role
		rs := th.fg(th.Role(role))
		if role == "" {
			role = "auto"
			rs = th.fg(th.Faint)
		}
		title := th.fg(th.Text).Render(oneLine(st.Title, 0))
		if i == p.sel {
			title = th.bold(th.Focus).Render(oneLine(st.Title, 0))
		}
		if len(st.DependsOn) > 0 {
			title += th.fg(th.Muted).Render(" · after " + strings.Join(st.DependsOn, ", "))
		}
		row := cursor + th.fg(th.Text).Render(fit(st.ID, 12)) + " " + th.fg(th.Role(string(st.Kind))).Render(fit(string(st.Kind), 9)) + " " + rs.Render(fit(role, 12)) + " " + title
		lines = append(lines, fit(row, iw))
	}
	lines = append(lines, "")
	if p.err != "" {
		lines = append(lines, fit(th.fg(th.FailColor).Render(p.err), iw))
	}
	room := max(2, H-2-len(lines))
	if p.sel < n {
		st := p.plan.Subtasks[p.sel]
		if p.deps {
			lines = append(lines, th.bold(th.Router).Render(st.ID+" runs after")+th.fg(th.Muted).Render(" · space toggle · enter done · esc cancel"))
			first := max(0, min(p.depSel-(room-2), len(p.depIDs)-(room-1)))
			for i, id := range p.depIDs {
				if i < first || i >= first+max(1, room-1) {
					continue
				}
				cur := "  "
				if i == p.depSel {
					cur = th.fg(th.Main).Render("> ")
				}
				box := "[ ]"
				if p.depOn[i] {
					box = th.fg(th.OKColor).Render("[x]")
				}
				title := ""
				for _, o := range p.plan.Subtasks {
					if o.ID == id {
						title = oneLine(o.Title, 0)
					}
				}
				lines = append(lines, fit(cur+box+" "+th.fg(th.Text).Render(fit(id, 12))+" "+th.fg(th.Muted).Render(title), iw))
			}
		} else if p.editing {
			lines = append(lines, th.bold(th.Router).Render("prompt of "+st.ID)+th.fg(th.Muted).Render(" · ctrl+s keep · esc discard"))
			p.ta.SetWidth(iw)
			p.ta.SetHeight(max(1, room-1))
			lines = append(lines, p.ta.View())
		} else {
			lines = append(lines, th.fg(th.Router).Render("prompt of "+st.ID)+th.fg(th.Muted).Render(" · e to edit"))
			wrapped := strings.Split(lipgloss.NewStyle().Width(iw).Render(strings.TrimSpace(st.Prompt)), "\n")
			if len(wrapped) > room-1 {
				wrapped = append(wrapped[:max(0, room-2)], ellipsis)
			}
			for _, l := range wrapped {
				lines = append(lines, th.fg(th.Muted).Render(fit(l, iw)))
			}
		}
	}
	box := th.box(th.Main, w).Height(max(1, H-2)).Render(clipLines(strings.Join(lines, "\n"), H-2))
	return indent(box, max(0, (W-w)/2))
}
