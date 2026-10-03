package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
)

// picker is the model picker: every role x both providers, editable live.
type picker struct {
	row, col int // col: 0 prefer, 1 codex model, 2 codex effort, 3 claude model, 4 claude effort
	choosing bool
	options  []option
	optIdx   int
	custom   bool
	input    textinput.Model
}

type option struct {
	label, value, note string
	custom             bool
}

const pickerCols = 5

func (m *Model) openPicker() {
	in := textinput.New()
	in.Prompt = "model id: "
	in.CharLimit = 120
	m.picker = &picker{input: in}
	m.focus = focusTree
	m.input.Blur()
}

func colProvider(col int) string {
	if col == 1 || col == 2 {
		return event.Codex
	}
	return event.Claude
}

func (p *picker) openOptions(m *Model) {
	cfg := m.store.Get()
	role := event.Roles[p.row]
	rc := cfg.Roles[role]
	p.options = nil
	cur := ""
	switch p.col {
	case 0:
		cur = rc.Prefer
		notes := map[string]string{
			config.PreferCodex:  "always Codex (other provider only when Codex is at its limit)",
			config.PreferClaude: "always Claude (other provider only when Claude is at its limit)",
			config.PreferOther:  "opposite of the planner's provider (good for review)",
			config.PreferAuto:   "whichever provider has used fewer tokens this session",
		}
		for _, o := range config.PreferOptions {
			p.options = append(p.options, option{label: o, value: o, note: notes[o]})
		}
	case 1, 3:
		prov := colProvider(p.col)
		cur = rc.For(prov).Model
		found := false
		for _, mi := range cfg.Providers[prov].Models {
			p.options = append(p.options, option{label: mi.ID, value: mi.ID, note: strings.TrimSpace(mi.Label + "  " + mi.Tier)})
			if mi.ID == cur {
				found = true
			}
		}
		if cur != "" && !found {
			p.options = append([]option{{label: cur, value: cur, note: "custom"}}, p.options...)
		}
		p.options = append(p.options,
			option{label: "custom…", custom: true, note: "type any model id the CLI accepts"},
			option{label: "(none)", value: "", note: "never use " + prov + " for this role"})
	case 2, 4:
		prov := colProvider(p.col)
		cur = rc.For(prov).Effort
		p.options = append(p.options, option{label: "(default)", value: "", note: "let the CLI decide"})
		for _, e := range cfg.Providers[prov].Efforts {
			p.options = append(p.options, option{label: e, value: e})
		}
	}
	p.optIdx = 0
	for i, o := range p.options {
		if !o.custom && o.value == cur && (cur != "" || o.label != "(none)") {
			p.optIdx = i
			break
		}
	}
	p.choosing = true
}

func (p *picker) apply(m *Model, value string) {
	role := event.Roles[p.row]
	cfg := m.store.Get()
	rc := cfg.Roles[role]
	var err error
	var what string
	switch p.col {
	case 0:
		err = m.store.SetPrefer(role, value)
		what = "prefer " + value
	case 1, 3:
		prov := colProvider(p.col)
		r := rc.For(prov)
		r.Model = value
		err = m.store.SetRoute(role, prov, r)
		what = prov + " model " + orNone(value)
	case 2, 4:
		prov := colProvider(p.col)
		r := rc.For(prov)
		r.Effort = value
		err = m.store.SetRoute(role, prov, r)
		what = prov + " effort " + orDefault(value)
	}
	if err != nil {
		m.flashNotice("not changed: " + err.Error())
		return
	}
	m.dirty = true
	m.flashNotice(fmt.Sprintf("%s: %s (applies to the next agent; s saves)", role, what))
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func orDefault(s string) string {
	if s == "" {
		return "(default)"
	}
	return s
}

func (p *picker) update(m *Model, k tea.KeyMsg) tea.Cmd {
	if p.custom {
		switch k.Type {
		case tea.KeyEnter:
			v := strings.TrimSpace(p.input.Value())
			if v != "" {
				p.apply(m, v)
			}
			p.custom, p.choosing = false, false
			p.input.Blur()
			return nil
		case tea.KeyEsc:
			p.custom = false
			p.input.Blur()
			return nil
		}
		var cmd tea.Cmd
		p.input, cmd = p.input.Update(k)
		return cmd
	}
	if p.choosing {
		switch k.String() {
		case "up", "k":
			p.optIdx = (p.optIdx - 1 + len(p.options)) % len(p.options)
		case "down", "j", "tab":
			p.optIdx = (p.optIdx + 1) % len(p.options)
		case "enter":
			o := p.options[p.optIdx]
			if o.custom {
				p.custom = true
				p.input.SetValue("")
				p.input.Focus()
				return textinput.Blink
			}
			p.apply(m, o.value)
			p.choosing = false
		case "esc", "left", "h":
			p.choosing = false
		}
		return nil
	}
	switch k.String() {
	case "up", "k":
		p.row = (p.row - 1 + len(event.Roles)) % len(event.Roles)
	case "down", "j":
		p.row = (p.row + 1) % len(event.Roles)
	case "left", "h", "shift+tab":
		p.col = (p.col - 1 + pickerCols) % pickerCols
	case "right", "l", "tab":
		p.col = (p.col + 1) % pickerCols
	case "enter", " ":
		p.openOptions(m)
	case "s":
		if err := m.store.Save(); err != nil {
			m.flashNotice("save failed: " + err.Error())
		} else {
			m.dirty = false
			m.flashNotice("saved to " + m.store.Path())
		}
	case "esc", "q", "m", "ctrl+o":
		m.picker = nil
	}
	return nil
}

func (p *picker) view(m *Model, W, H int) string {
	th := m.th
	cfg := m.store.Get()
	w := min(W, 118)
	iw := w - 4
	var lines []string
	dirty := ""
	if m.dirty {
		dirty = th.fg(th.Warn).Render("  · unsaved (s to save)")
	}
	lines = append(lines, th.bold(th.Main).Render("MODELS")+th.fg(th.Muted).Render(" · any model for any job · changes apply to the next agent")+dirty)
	lines = append(lines, th.fg(th.Faint).Render("config: "+m.store.Path()))
	lines = append(lines, "")
	head := fmt.Sprintf("%-12s %-8s %-20s %-8s %-20s %-8s %s", "ROLE", "PREFER", "CODEX MODEL", "EFFORT", "CLAUDE MODEL", "EFFORT", "NOW USES")
	lines = append(lines, th.bold(th.Muted).Render(fit(head, iw)))
	mainProv := ""
	if n := m.nodes["main"]; n != nil {
		mainProv = n.provider
	}
	for i, role := range event.Roles {
		rc := cfg.Roles[role]
		cells := []string{rc.Prefer, orNone(rc.Codex.Model), orDefault(rc.Codex.Effort), orNone(rc.Claude.Model), orDefault(rc.Claude.Effort)}
		widths := []int{8, 20, 8, 20, 8}
		row := th.fg(th.Role(role)).Render(fit(th.G.Role+" "+role, 12)) + " "
		for c, v := range cells {
			st := lipgloss.NewStyle().Foreground(th.Text)
			if c == 1 || c == 2 {
				st = st.Foreground(th.Codex)
			} else if c == 3 || c == 4 {
				st = st.Foreground(th.Claude)
			}
			cell := fit(v, widths[c])
			if i == p.row && c == p.col {
				st = st.Reverse(true).Bold(true)
			}
			row += st.Render(cell) + " "
		}
		d := m.orc.Router().Preview(role, mainProv)
		now := th.fg(th.ProviderColor(d.Provider)).Render(d.Label())
		if d.Fallback {
			now += th.fg(th.Warn).Render(" (limit fallback)")
		}
		lines = append(lines, fit(row+now, iw))
	}
	lines = append(lines, "")
	if p.choosing {
		role := event.Roles[p.row]
		col := []string{"prefer", "codex model", "codex effort", "claude model", "claude effort"}[p.col]
		lines = append(lines, th.bold(th.Router).Render(fmt.Sprintf("%s · %s", role, col))+th.fg(th.Muted).Render("  ↑↓ choose · enter apply · esc back"))
		maxOpts := max(3, H-len(lines)-4)
		start := 0
		if p.optIdx >= maxOpts {
			start = p.optIdx - maxOpts + 1
		}
		for i := start; i < len(p.options) && i < start+maxOpts; i++ {
			o := p.options[i]
			cursor := "  "
			st := th.fg(th.Text)
			if i == p.optIdx {
				cursor = th.fg(th.Main).Render(th.G.Arrow + " ")
				st = th.bold(th.Main)
			}
			lines = append(lines, fit(cursor+st.Render(fmt.Sprintf("%-22s", o.label))+th.fg(th.Muted).Render(o.note), iw))
		}
		if p.custom {
			lines = append(lines, "", p.input.View())
		}
	} else {
		lines = append(lines, th.fg(th.Muted).Render("↑↓ role · ←→ column · enter change · s save to config · esc close"))
		lines = append(lines, th.fg(th.Faint).Render("prefer: codex|claude = that provider · other = opposite of the planner · auto = least used"))
		lines = append(lines, th.fg(th.Faint).Render("At a usage limit the role's route on the other provider is used automatically."))
		lines = append(lines, th.fg(th.Faint).Render("Tip: `sy models --refresh` reads the current Codex catalog from `codex debug models`."))
	}
	box := th.box(th.Main, w).Render(clipLines(strings.Join(lines, "\n"), H-2))
	return indent(box, max(0, (W-w)/2))
}
