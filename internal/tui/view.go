package tui

import (
	"fmt"
	"math"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/sparkz400/switchyard/internal/diag"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/schedule"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// ellipsis is "…" or "..." for the ASCII theme (set by New).
var ellipsis = "…"

// fit truncates (ANSI-aware) and pads s to exactly w cells.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(s, w, ellipsis)
	if pad := w - lipgloss.Width(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

func clipLines(s string, h int) string {
	if h <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}

func padLines(s string, h int) string {
	lines := strings.Split(s, "\n")
	for len(lines) < h {
		lines = append(lines, "")
	}
	return strings.Join(lines[:h], "\n")
}

func dur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func (m *Model) statusGlyph(s nodeStatus) string {
	g := m.th.G
	switch s {
	case stRunning:
		return m.th.fg(m.th.Main).Render(g.Spinner[m.frame%len(g.Spinner)])
	case stOK:
		return m.th.fg(m.th.OKColor).Render(g.OK)
	case stFailed:
		return m.th.fg(m.th.FailColor).Render(g.Fail)
	case stKilled:
		return m.th.fg(m.th.Warn).Render(g.Killed)
	}
	return m.th.fg(m.th.Faint).Render(g.Queued)
}

func statusWord(s nodeStatus) string {
	return [...]string{"queued", "running", "done", "failed", "killed"}[s]
}

func (m *Model) bar(v float64, w int, c lipgloss.Color) string {
	if w <= 0 {
		return ""
	}
	v = math.Max(0, math.Min(1, v))
	full := int(math.Round(v * float64(w)))
	return m.th.fg(c).Render(strings.Repeat(m.th.G.BarFull, full)) + m.th.fg(m.th.Faint).Render(strings.Repeat(m.th.G.BarEmpty, w-full))
}

func (m *Model) route(provider, model string) string {
	if provider == "" {
		return m.th.fg(m.th.Faint).Render("routing…")
	}
	return m.th.fg(m.th.ProviderColor(provider)).Render(provider + ":" + model)
}

// View implements tea.Model.
func (m *Model) View() string {
	defer func() {
		if r := recover(); r != nil {
			diag.Crash("tui view", r, debug.Stack())
			panic(r)
		}
	}()
	return m.view()
}

func (m *Model) view() string {
	if m.width == 0 {
		return "starting switchyard…"
	}
	W, H := m.width, m.height
	header := m.viewHeader(W)
	legend := m.viewLegend(W)
	prompt := m.viewPrompt(W) + "\n" + m.viewKeys(W)
	notice := ""
	if m.notice != "" {
		notice = fit(" "+m.th.fg(m.th.Warn).Render(m.notice), W)
	}
	fixed := lipgloss.Height(header) + lipgloss.Height(legend) + lipgloss.Height(prompt)
	if notice != "" {
		fixed++
	}
	rest := max(4, H-fixed)
	logH := max(6, rest/3)
	panel := m.picker != nil || m.overlay != nil
	if m.overlay != nil {
		logH = min(logH, rest/5) // approvals get the room
		if logH < 4 {
			logH = 0
		}
	}
	if need := m.bodyNeed(W); !panel && rest-logH < need {
		logH = max(6, rest-need) // give the tree room before the log
	}
	if m.fullLog && m.overlay == nil {
		logH = rest
	}
	bodyH := rest - logH

	parts := []string{header, legend}
	if bodyH > 0 {
		var body string
		if panel {
			body = m.viewPanel(W, bodyH)
		} else {
			body = m.viewBody(W, bodyH)
		}
		parts = append(parts, padLines(clipLines(body, bodyH), bodyH))
	} else if panel {
		logH = 0
		parts = append(parts, padLines(clipLines(m.viewPanel(W, rest), rest), rest))
	}
	if logH > 0 {
		parts = append(parts, m.viewLog(W, logH))
	}
	if notice != "" {
		parts = append(parts, notice)
	}
	parts = append(parts, prompt)
	return strings.Join(parts, "\n")
}

// viewPanel is what replaces the agent tree: an approval, else the picker.
func (m *Model) viewPanel(W, H int) string {
	if m.overlay != nil {
		return m.overlay.view(m, W, H)
	}
	return m.picker.view(m, W, H)
}

func (m *Model) viewHeader(W int) string {
	th := m.th
	left := th.bold(th.Main).Render(" "+th.G.Logo+" SWITCHYARD") + th.fg(th.Muted).Render(" · "+m.projectLabel(filepath.Base(m.opt.Dir)))
	phase := m.phase
	if m.running {
		phase += " · " + dur(time.Since(m.taskStart))
	}
	left += th.fg(th.Muted).Render(" · ") + th.fg(th.Router).Render(phase)
	if n := len(m.queue); n > 0 {
		left += th.fg(th.Warn).Render(fmt.Sprintf(" · queued (%d)", n))
		if next := m.nextScheduled(); !next.IsZero() {
			left += th.fg(th.Warn).Render(" · next " + schedule.Clock(next, time.Now()))
		}
	}
	if b := m.budgetLine(); b != "" {
		left += th.fg(th.Muted).Render(" · ") + b
	}
	if m.orc.Paused() {
		left += " " + lipgloss.NewStyle().Background(th.Warn).Foreground(lipgloss.Color("#000000")).Bold(true).Render(" PAUSED ")
	}
	if m.opt.Demo {
		left += " " + lipgloss.NewStyle().Background(th.Reviewer).Foreground(lipgloss.Color("#000000")).Render(" DEMO ")
	}
	var right []string
	now := time.Now()
	for _, p := range event.Providers {
		pv := m.provs[p]
		c := th.ProviderColor(p)
		name := th.bold(c).Render(th.G.Dot + " " + p)
		var info string
		switch {
		case now.Before(pv.until):
			info = th.bold(th.FailColor).Render("LIMIT") + th.fg(th.Muted).Render(" until "+pv.until.Format("15:04"))
		case pv.quota != nil:
			info = m.bar(pv.bar, 8, c) + th.fg(th.Muted).Render(fmt.Sprintf(" %2.0f%% %s", pv.quota.Utilization*100, shortWindow(pv.quota.Window)))
		default:
			info = m.bar(pv.bar, 8, c) + th.fg(th.Muted).Render(" "+sessionlog.Human(pv.tokens.Total())+" tok")
		}
		right = append(right, name+" "+info)
	}
	r := strings.Join(right, th.fg(th.Faint).Render("  "+th.G.V+"  ")) + " "
	gap := W - lipgloss.Width(left) - lipgloss.Width(r)
	if gap < 1 {
		// Narrow: provider state without bars.
		var short []string
		for _, p := range event.Providers {
			pv := m.provs[p]
			info := sessionlog.Human(pv.tokens.Total())
			if pv.quota != nil {
				info = fmt.Sprintf("%.0f%%", pv.quota.Utilization*100)
			}
			if now.Before(pv.until) {
				info = th.bold(th.FailColor).Render("LIMIT")
			}
			short = append(short, th.fg(th.ProviderColor(p)).Render(p)+" "+info)
		}
		r = strings.Join(short, " ") + " "
		gap = W - lipgloss.Width(left) - lipgloss.Width(r)
		if gap < 1 {
			return fit(left, W)
		}
	}
	return left + strings.Repeat(" ", gap) + r
}

// budgetLine is the budget status for the header, e.g. "$0.42/$2 today"
// ("" when no budget is set). It turns yellow at warn_at and red at the
// limit.
func (m *Model) budgetLine() string {
	if !m.store.Budget().Any() {
		return ""
	}
	st := m.orc.BudgetStatus()
	b := st.Limits
	th := m.th
	var parts []string
	worst := 0.0
	add := func(used, max float64, s string) {
		parts = append(parts, s)
		worst = math.Max(worst, used/max)
	}
	if b.DayUSD > 0 {
		add(st.DayUSD, b.DayUSD, fmt.Sprintf("$%.2f/$%s today", st.DayUSD, money(b.DayUSD)))
	} else if b.DayTokens > 0 {
		add(float64(st.DayTokens), float64(b.DayTokens), fmt.Sprintf("%s/%s tok today", sessionlog.Human(st.DayTokens), sessionlog.Human(b.DayTokens)))
	}
	if st.Running && b.TaskUSD > 0 {
		add(st.TaskUSD, b.TaskUSD, fmt.Sprintf("task $%.2f/$%s", st.TaskUSD, money(b.TaskUSD)))
	} else if st.Running && b.TaskTokens > 0 {
		add(float64(st.TaskTokens), float64(b.TaskTokens), fmt.Sprintf("task %s/%s tok", sessionlog.Human(st.TaskTokens), sessionlog.Human(b.TaskTokens)))
	}
	if len(parts) == 0 {
		return ""
	}
	c := th.Muted
	switch {
	case worst >= 1:
		c = th.FailColor
	case b.WarnAt > 0 && worst >= b.WarnAt:
		c = th.Warn
	}
	return th.fg(c).Render(strings.Join(parts, " · "))
}

// money formats a limit: "2" for whole dollars, else "2.50".
func money(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

func shortWindow(w string) string {
	switch w {
	case "five_hour":
		return "5h"
	case "seven_day":
		return "7d"
	case "seven_day_opus":
		return "7d-opus"
	}
	return w
}

func (m *Model) viewLegend(W int) string {
	th := m.th
	var chips []string
	chips = append(chips, th.fg(th.Codex).Render("■ codex"), th.fg(th.Claude).Render("■ claude"))
	if th.ASCII {
		chips[0], chips[1] = th.fg(th.Codex).Render("# codex"), th.fg(th.Claude).Render("# claude")
	}
	for _, r := range []string{event.RolePlanner, event.RoleWorker, event.RoleWorkerHigh, event.RoleExplorer, event.RoleResearcher, event.RoleReviewer} {
		chips = append(chips, th.fg(th.Role(r)).Render(th.G.Role+" "+r))
	}
	return fit(" "+strings.Join(chips, " "), W)
}

func (m *Model) viewKeys(W int) string {
	var keys string
	switch {
	case m.overlay != nil:
		keys = m.overlay.keys()
	case m.picker != nil:
		keys = "↑↓ role · ←→ column · enter change · s save · esc close"
	case m.focus == focusPrompt:
		keys = "enter run · alt+enter new line · tab/esc agents · ctrl+o models · ctrl+x cancel · /help · ctrl+c quit"
	default:
		keys = "tab select agent · k k kill · p pause · x cancel task · l full log · m models · enter prompt · q quit"
	}
	return fit(" "+m.th.fg(m.th.Muted).Render(keys), W)
}

func (m *Model) viewPrompt(W int) string {
	th := m.th
	c := th.Faint
	if m.focus == focusPrompt && m.picker == nil && m.overlay == nil {
		c = th.Main
	}
	inner := m.input.View()
	switch {
	case m.overlay != nil:
		inner = th.fg(th.Warn).Render("waiting for your answer above · ctrl+x cancels the task")
	case m.running && m.focus != focusPrompt:
		inner = th.fg(th.Muted).Render("task running · enter to type (a new task is queued) · x or ctrl+x to cancel")
	}
	lines := strings.Split(inner, "\n")
	for i := range lines {
		lines[i] = fit(lines[i], W-2)
	}
	return lipgloss.NewStyle().Border(th.G.Border).BorderForeground(c).Width(W - 2).Render(strings.Join(lines, "\n"))
}

// bodyNeed is the smallest height that shows the whole tree (compact boxes,
// one router line).
func (m *Model) bodyNeed(W int) int {
	cw, extra := W, 1 // narrow layouts add a reviewer line
	if W >= 96 {
		cw, extra = W-min(36, W/4)-1, 0
	}
	n := len(m.order)
	if n == 0 {
		return extra + 4 + 1 + 4 + 1 + 3
	}
	bw := max(22, min(34, (cw-(n-1))/n))
	perRow := max(1, (cw+1)/(bw+1))
	rows := (n + perRow - 1) / perRow
	return extra + 4 + 1 + 4 + 1 + 5*rows + 1 + 3
}

func (m *Model) viewBody(W, H int) string {
	revW := 0
	if W >= 96 {
		revW = min(36, W/4)
	}
	cw := W - revW
	if revW > 0 {
		cw--
	}
	if revW == 0 {
		return m.reviewerLine(W) + "\n" + m.viewCenter(cw, H-1)
	}
	center := m.viewCenter(cw, H)
	rev := m.viewReviewer(revW, H)
	return lipgloss.JoinHorizontal(lipgloss.Top, rev, " ", center)
}

// reviewerLine is the one-line reviewer summary used on narrow terminals.
func (m *Model) reviewerLine(W int) string {
	th := m.th
	r := m.reviewer
	s := th.bold(th.Reviewer).Render(" "+th.G.Role+" REVIEWER ") + m.statusGlyph(r.status) + " " + m.route(r.provider, r.model)
	for _, cp := range r.checkpoints {
		g := th.fg(th.OKColor).Render(th.G.OK)
		if !cp.ok {
			g = th.fg(th.Warn).Render(th.G.Fail)
		}
		s += " " + g + th.fg(th.Muted).Render(cp.name)
	}
	if n := len(r.checkpoints); n > 0 {
		text := r.checkpoints[n-1].text
		if i := strings.Index(text, ":"); i >= 0 {
			text = text[i+1:]
		}
		s += th.fg(th.Muted).Render(" · " + oneLine(text, 0))
	}
	return fit(s, W)
}

func (m *Model) viewReviewer(w, h int) string {
	th := m.th
	r := m.reviewer
	c := th.Reviewer
	if r.flash > 0.4 && m.frame%4 < 2 {
		c = th.Focus
	}
	if m.selectedAgent() == orchestrator.AgentReviewer {
		c = th.Focus
	}
	iw := w - 4
	var lines []string
	lines = append(lines, th.bold(th.Reviewer).Render(th.G.Role+" REVIEWER")+th.fg(th.Muted).Render(" · checkpoints"))
	lines = append(lines, m.statusGlyph(r.status)+" "+fit(m.route(r.provider, r.model), iw-2))
	lines = append(lines, th.fg(th.Muted).Render(fmt.Sprintf("calls %d · %s tok", r.calls, sessionlog.Human(r.tokens.Total()))))
	lines = append(lines, "")
	if len(r.checkpoints) == 0 {
		lines = append(lines, th.fg(th.Faint).Render("silent until a checkpoint:"), th.fg(th.Faint).Render(" before plan · repeated error"), th.fg(th.Faint).Render(" · before done"))
	}
	for _, cp := range r.checkpoints {
		g := th.fg(th.OKColor).Render(th.G.OK)
		if !cp.ok {
			g = th.fg(th.Warn).Render(th.G.Fail)
		}
		lines = append(lines, g+" "+fit(cp.name, iw-10)+th.fg(th.Muted).Render(" "+cp.at.Format("15:04")))
	}
	if n := len(r.checkpoints); n > 0 {
		lines = append(lines, "", th.fg(th.Reviewer).Render("last advice"))
		text := r.checkpoints[n-1].text
		if i := strings.Index(text, ":"); i >= 0 {
			text = strings.TrimSpace(text[i+1:])
		}
		wrapped := lipgloss.NewStyle().Width(iw).Render(text)
		lines = append(lines, strings.Split(wrapped, "\n")...)
	} else if r.status == stRunning && r.last != "" {
		lines = append(lines, th.fg(th.Muted).Render(fit(r.last, iw)))
	}
	content := clipLines(strings.Join(lines, "\n"), h-2)
	return th.box(c, w).Height(h - 2).Render(content)
}

func (m *Model) viewCenter(cw, H int) string {
	th := m.th
	n := len(m.order)
	bw, perRow, rows := 0, 0, 0
	if n > 0 {
		bw = max(22, min(34, (cw-(n-1))/n))
		perRow = max(1, (cw+1)/(bw+1))
		rows = (n + perRow - 1) / perRow
	}
	// Heights: main 4, conn 1, router 3+k (border + title), branch 1,
	// agents 7/row, join 1, back 3.
	boxH := 7
	fixed := 4 + 1 + 3 + 1 + boxH*rows + 1 + 3
	compact := false
	if n > 0 && fixed+1 > H {
		// Not enough room: three-line agent boxes.
		compact, boxH = true, 5
		fixed = 4 + 1 + 3 + 1 + boxH*rows + 1 + 3
	}
	if n == 0 {
		fixed = 4 + 1 + 3 + 1 + 3
	}
	k := max(1, min(maxDecisions, H-fixed))
	if len(m.decisions) < k {
		k = max(1, len(m.decisions))
	}

	var out []string
	mid := cw / 2
	out = append(out, m.viewMain(cw))
	out = append(out, m.vConnector(cw, mid, false))
	out = append(out, m.viewRouter(cw, k))
	if n == 0 {
		out = append(out, th.fg(th.Faint).Render(fit(strings.Repeat(" ", max(0, mid-12))+"agents appear here once planned", cw)))
		out = append(out, m.viewBack(cw))
		return strings.Join(out, "\n")
	}
	// Agent rows.
	for row := 0; row < rows; row++ {
		ids := m.order[row*perRow : min(n, (row+1)*perRow)]
		rowW := len(ids)*bw + (len(ids) - 1)
		offset := max(0, (cw-rowW)/2)
		centers := make([]int, len(ids))
		for i := range ids {
			centers[i] = offset + i*(bw+1) + bw/2
		}
		if row == 0 {
			out = append(out, m.branchLine(cw, mid, ids, centers, false))
		}
		boxes := make([]string, len(ids))
		for i, id := range ids {
			boxes[i] = m.viewAgent(id, bw, compact)
		}
		rowStr := lipgloss.JoinHorizontal(lipgloss.Top, interleave(boxes, " ")...)
		out = append(out, indent(rowStr, offset))
		if row == rows-1 {
			out = append(out, m.branchLine(cw, mid, ids, centers, true))
		}
	}
	out = append(out, m.viewBack(cw))
	return strings.Join(out, "\n")
}

func interleave(xs []string, sep string) []string {
	var out []string
	for i, x := range xs {
		if i > 0 {
			out = append(out, sep)
		}
		out = append(out, x)
	}
	return out
}

func indent(s string, n int) string {
	if n <= 0 {
		return s
	}
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}

// vConnector draws the single-line link between main and router, lighting
// up while a dispatch pulse is leaving.
func (m *Model) vConnector(cw, mid int, _ bool) string {
	g := m.th.fg(m.th.Faint).Render(m.th.G.V)
	for _, p := range m.pulses {
		if !p.join && p.t < 0.5 {
			g = m.th.bold(m.th.Router).Render(m.th.G.Pulse)
			break
		}
	}
	return strings.Repeat(" ", max(0, mid)) + g
}

// branchLine draws ┌──┴──┐ (dispatch) or └──┬──┘ (join) with pulses.
func (m *Model) branchLine(cw, mid int, ids []string, centers []int, join bool) string {
	g := m.th.G
	cells := make([]string, cw)
	styles := make([]lipgloss.Style, cw)
	base := m.th.fg(m.th.Faint)
	for i := range cells {
		cells[i] = " "
		styles[i] = base
	}
	lo, hi := mid, mid
	for _, c := range centers {
		lo, hi = min(lo, c), max(hi, c)
	}
	for x := lo; x <= hi && x < cw; x++ {
		cells[x] = g.H
	}
	isCenter := map[int]bool{}
	for _, c := range centers {
		isCenter[c] = true
		if c >= cw {
			continue
		}
		switch {
		case join && c == lo && c != hi:
			cells[c] = g.CornerBL
		case join && c == hi && c != lo:
			cells[c] = g.CornerBR
		case !join && c == lo && c != hi:
			cells[c] = g.CornerTL
		case !join && c == hi && c != lo:
			cells[c] = g.CornerTR
		case join:
			cells[c] = g.TeeUp
		default:
			cells[c] = g.TeeDown
		}
	}
	if mid < cw {
		switch {
		case isCenter[mid]:
			cells[mid] = g.Cross
		case join:
			cells[mid] = g.TeeDown
		default:
			cells[mid] = g.TeeUp
		}
		if lo == hi {
			cells[mid] = g.V
		}
	}
	idx := map[string]int{}
	for i, id := range ids {
		idx[id] = i
	}
	for _, p := range m.pulses {
		if p.join != join {
			continue
		}
		i, ok := idx[p.target]
		if !ok {
			continue
		}
		from, to := mid, centers[i]
		if join {
			from, to = centers[i], mid
		}
		x := from + int(math.Round(float64(to-from)*easeInOut(p.t)))
		if x >= 0 && x < cw {
			cells[x] = g.Pulse
			c := m.th.Router
			if n := m.nodes[p.target]; n != nil && n.provider != "" {
				c = m.th.ProviderColor(n.provider)
			}
			styles[x] = m.th.bold(c)
		}
	}
	var b strings.Builder
	for i := range cells {
		b.WriteString(styles[i].Render(cells[i]))
	}
	return b.String()
}

func easeInOut(t float64) float64 {
	if t < 0.5 {
		return 2 * t * t
	}
	return 1 - math.Pow(-2*t+2, 2)/2
}

func (m *Model) viewMain(cw int) string {
	th := m.th
	n := m.nodes[orchestrator.AgentMain]
	w := min(cw, max(44, cw*2/3))
	c := th.Main
	if m.selectedAgent() == orchestrator.AgentMain || n.flash > 0.5 {
		c = th.Focus
	}
	iw := w - 4
	title := th.bold(th.Main).Render(th.G.Role+" MAIN") + th.fg(th.Muted).Render(" · planner  ") + m.statusGlyph(n.status) + " " + m.route(n.provider, n.model)
	task := m.taskText
	if task == "" {
		task = "waiting for a task"
	}
	sub := th.fg(th.Text).Render(oneLine(task, 0))
	if n.status == stRunning && n.last != "" {
		sub = th.fg(th.Muted).Render(oneLine(n.last, 0))
	}
	body := fit(title, iw) + "\n" + fit(sub, iw)
	return indent(th.box(c, w).Render(body), max(0, (cw-w)/2))
}

func (m *Model) viewRouter(cw, k int) string {
	th := m.th
	iw := cw - 4
	title := th.bold(th.Router).Render("ROUTER") + th.fg(th.Muted).Render(" · rules first · judge for unclear steps")
	lines := []string{fit(title, iw)}
	ds := m.decisions
	if len(ds) > k {
		ds = ds[len(ds)-k:]
	}
	if len(ds) == 0 {
		lines = append(lines, th.fg(th.Faint).Render("no decisions yet"))
	}
	barW := 10
	if iw < 70 {
		barW = 6
	}
	for _, d := range ds {
		dd := d.d
		agent := fit(th.fg(th.Text).Render(d.agent), 10)
		role := fit(th.fg(th.Role(dd.Role)).Render(dd.Role), 11)
		route := fit(th.fg(th.ProviderColor(dd.Provider)).Render(dd.Label()), 26)
		bar := m.bar(d.bar, barW, th.Router) + th.fg(th.Muted).Render(fmt.Sprintf(" %.2f ", dd.Confidence))
		rule := dd.Rule
		rs := th.fg(th.Muted)
		if dd.Fallback {
			rs = th.fg(th.Warn)
			rule = "⚠ " + rule
			if th.ASCII {
				rule = "! " + dd.Rule
			}
		}
		if dd.Judged {
			rule += " (judge)"
		}
		lines = append(lines, fit(agent+" "+role+" "+route+" "+bar+rs.Render(rule+" · "+dd.Reason), iw))
	}
	return th.box(th.Router, cw).Render(strings.Join(lines, "\n"))
}

func (m *Model) viewAgent(id string, w int, compact bool) string {
	th := m.th
	n := m.nodes[id]
	iw := w - 4
	role := n.role
	if role == "" {
		role = n.kind
	}
	c := th.ProviderColor(n.provider)
	if n.provider == "" {
		c = th.Faint
	}
	focused := m.selectedAgent() == id
	if focused || (n.flash > 0.5 && m.frame%4 < 2) {
		c = th.Focus
	}
	title := th.bold(th.Role(role)).Render(th.G.Role+" "+role) + th.fg(th.Muted).Render(" · "+id)
	el := ""
	if !n.started.IsZero() {
		end := n.ended
		if end.IsZero() {
			end = time.Now()
		}
		el = " " + dur(end.Sub(n.started))
	}
	att := ""
	if n.attempts > 1 {
		att = th.fg(th.Warn).Render(fmt.Sprintf(" try %d", n.attempts))
	}
	status := m.statusGlyph(n.status) + " " + th.fg(th.Text).Render(statusWord(n.status)) + th.fg(th.Muted).Render(el) + att
	last := th.fg(th.Muted).Render(oneLine(n.last, 0))
	if n.last == "" {
		last = th.fg(th.Faint).Render(oneLine(n.title, 0))
	}
	foot := th.fg(th.Muted).Render(fmt.Sprintf("%s tok · %d files", sessionlog.Human(n.tokens.Total()), len(n.files)))
	if n.merge != "" {
		mc, mg := th.OKColor, th.G.OK
		if !n.mergeOK {
			mc, mg = th.FailColor, th.G.Fail
		}
		foot += " " + th.fg(mc).Render(mg+"merge")
	}
	body := strings.Join([]string{fit(title, iw), fit(status, iw), fit(m.route(n.provider, n.model), iw), fit(last, iw), fit(foot, iw)}, "\n")
	if compact {
		body = strings.Join([]string{fit(title, iw), fit(status+" "+m.route(n.provider, n.model), iw), fit(last, iw)}, "\n")
	}
	st := th.box(c, w)
	if focused {
		st = st.BorderStyle(lipgloss.ThickBorder())
		if th.ASCII {
			st = st.BorderStyle(asciiGlyphs.Border)
		}
	}
	return st.Render(body)
}

func (m *Model) viewBack(cw int) string {
	th := m.th
	w := min(cw, max(44, cw*2/3))
	iw := w - 4
	merged, failed := 0, 0
	for _, id := range m.order {
		if n := m.nodes[id]; n.merge != "" {
			if n.mergeOK {
				merged++
			} else {
				failed++
			}
		}
	}
	text := th.bold(th.Main).Render(th.G.Back+" BACK TO MAIN") + th.fg(th.Muted).Render(" · merge + review + verify  ")
	switch {
	case m.result != "" && !m.running:
		g := th.fg(th.OKColor).Render(th.G.OK + " ")
		if !m.resultOK {
			g = th.fg(th.FailColor).Render(th.G.Fail + " ")
		}
		text += g + th.fg(th.Text).Render(m.result)
		if m.cost != "" {
			text += th.fg(th.Muted).Render(" · " + m.cost)
		}
	case m.phase == "review" || m.phase == "fix":
		text += th.fg(th.Reviewer).Render(m.phase + "…")
	default:
		if merged+failed > 0 {
			text += th.fg(th.Muted).Render(fmt.Sprintf("%d merged", merged))
			if failed > 0 {
				text += th.fg(th.FailColor).Render(fmt.Sprintf(" · %d conflict", failed))
			}
		}
	}
	c := th.Faint
	if m.phase == "review" || m.phase == "fix" || m.phase == "done" {
		c = th.Main
	}
	return indent(th.box(c, w).Render(fit(text, iw)), max(0, (cw-w)/2))
}

func (m *Model) viewLog(W, h int) string {
	th := m.th
	sel := m.selectedAgent()
	var lines []logLine
	title := "SESSION LOG"
	if sel != "" {
		title = "LOG · " + sel
		if n := m.nodes[sel]; n != nil {
			lines = n.lines
		}
		if sel == orchestrator.AgentReviewer {
			for _, l := range m.logs {
				if l.agent == orchestrator.AgentReviewer {
					lines = append(lines, l)
				}
			}
		}
	} else {
		lines = m.logs
	}
	inner := h - 2
	if inner < 1 {
		return ""
	}
	maxScroll := max(0, len(lines)-(inner-1))
	if m.logScroll > maxScroll {
		m.logScroll = maxScroll
	}
	end := len(lines) - m.logScroll
	start := max(0, end-(inner-1))
	iw := W - 4
	scroll := ""
	if m.logScroll > 0 {
		scroll = fmt.Sprintf("  (scrolled %d · pgdn)", m.logScroll)
	}
	out := []string{fit(th.bold(th.Muted).Render(title)+th.fg(th.Faint).Render(scroll+"  · l: full log · pgup/pgdn"), iw)}
	for _, l := range lines[start:end] {
		out = append(out, fit(m.fmtLog(l), iw))
	}
	return th.box(th.Faint, W).Height(inner).Render(strings.Join(out, "\n"))
}

func (m *Model) fmtLog(l logLine) string {
	th := m.th
	ts := th.fg(th.Faint).Render(l.ts.Format("15:04:05"))
	tag := ""
	if l.agent != "" {
		c := th.ProviderColor(l.prov)
		if l.prov == "" {
			c = th.Muted
		}
		tag = th.fg(c).Render(fit(l.agent, 9))
	} else {
		tag = th.fg(th.Router).Render(fit("sy", 9))
	}
	kind := ""
	tc := th.Text
	switch l.kind {
	case event.ToolCall:
		kind = th.fg(th.Router).Render("tool ")
	case event.FileEdit:
		kind = th.fg(th.Main).Render("edit ")
	case event.Thinking:
		tc = th.Muted
	case event.Error:
		tc = th.FailColor
	case event.LimitHit:
		tc = th.Warn
		kind = th.bold(th.Warn).Render("LIMIT ")
	case event.Route:
		tc = th.Router
	case event.Checkpoint:
		tc = th.Reviewer
	case event.Phase:
		tc = th.Main
	}
	return ts + " " + tag + " " + kind + th.fg(tc).Render(oneLine(l.text, 0))
}
