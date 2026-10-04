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

// picker is the model picker: every role x every provider, editable live.
type picker struct {
	// col: 0 prefer, then a model and an effort column per provider in
	// routing order (1 first model, 2 first effort, 3 second model...).
	row, col int
	first    int // first provider shown (two fit side by side)
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

// pickerShown is how many providers fit side by side.
const pickerShown = 2

// pickerCols is the number of columns for the configured providers.
func pickerCols(cfg *config.Config) int { return 1 + 2*len(cfg.ProviderNames()) }

// colProvider is the provider of a model or effort column.
func colProvider(cfg *config.Config, col int) string {
	names := cfg.ProviderNames()
	i := (col - 1) / 2
	if col < 1 || i >= len(names) {
		return ""
	}
	return names[i]
}

// isModelCol reports whether col is a model column (else an effort one).
func isModelCol(col int) bool { return col >= 1 && (col-1)%2 == 0 }

// scroll keeps the selected column's provider visible.
func (p *picker) scroll(cfg *config.Config) {
	if p.col > 0 {
		i := (p.col - 1) / 2
		if i < p.first {
			p.first = i
		}
		if i >= p.first+pickerShown {
			p.first = i - pickerShown + 1
		}
	}
	p.first = max(0, min(p.first, len(cfg.ProviderNames())-pickerShown))
}

func (m *Model) openPicker() {
	in := textinput.New()
	in.Prompt = "model id: "
	in.CharLimit = 120
	m.picker = &picker{input: in}
	m.focus = focusTree
	m.input.Blur()
}

func (p *picker) openOptions(m *Model) {
	cfg := m.store.Get()
	role := event.Roles[p.row]
	rc := cfg.Roles[role]
	p.options = nil
	cur := ""
	prov := colProvider(cfg, p.col)
	switch {
	case p.col == 0:
		cur = rc.Prefer
		for _, name := range cfg.ProviderNames() {
			note := "always " + cfg.ProviderLabel(name) + " (another provider only when it is at its limit)"
			if cfg.Providers[name].Disabled {
				note = "disabled in the config"
			} else if rc.For(name).Model == "" {
				note = "no model set for this role"
			}
			p.options = append(p.options, option{label: name, value: name, note: note})
		}
		p.options = append(p.options,
			option{label: config.PreferOther, value: config.PreferOther, note: "another provider than the planner's (good for review)"},
			option{label: config.PreferAuto, value: config.PreferAuto, note: "whichever provider has used fewer tokens this session"})
	case isModelCol(p.col):
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
	default:
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
	prov := colProvider(cfg, p.col)
	switch {
	case p.col == 0:
		err = m.store.SetPrefer(role, value)
		what = "prefer " + value
	case isModelCol(p.col):
		r := rc.For(prov)
		r.Model = value
		err = m.store.SetRoute(role, prov, r)
		what = prov + " model " + orNone(value)
	default:
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
		n := pickerCols(m.store.Get())
		p.col = (p.col - 1 + n) % n
		p.scroll(m.store.Get())
	case "right", "l", "tab":
		p.col = (p.col + 1) % pickerCols(m.store.Get())
		p.scroll(m.store.Get())
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
	names := cfg.ProviderNames()
	p.scroll(cfg)
	shown := names[p.first:min(len(names), p.first+pickerShown)]
	head := fmt.Sprintf("%-12s %-8s ", "ROLE", "PREFER")
	for _, prov := range shown {
		head += fit(strings.ToUpper(prov)+" MODEL", 20) + " " + fmt.Sprintf("%-8s ", "EFFORT")
	}
	lines = append(lines, th.bold(th.Muted).Render(fit(head+"NOW USES", iw)))
	mainProv := ""
	if n := m.nodes["main"]; n != nil {
		mainProv = n.provider
	}
	for i, role := range event.Roles {
		rc := cfg.Roles[role]
		cells := []string{rc.Prefer}
		cols := []int{0}
		for j, prov := range shown {
			r := rc.For(prov)
			cells = append(cells, orNone(r.Model), orDefault(r.Effort))
			cols = append(cols, 1+2*(p.first+j), 2+2*(p.first+j))
		}
		row := th.fg(th.Role(role)).Render(fit(th.G.Role+" "+role, 12)) + " "
		for c, v := range cells {
			st := lipgloss.NewStyle().Foreground(th.Text)
			width := 8
			if c > 0 {
				st = st.Foreground(th.ProviderColor(colProvider(cfg, cols[c])))
				if isModelCol(cols[c]) {
					width = 20
				}
			}
			cell := fit(v, width)
			if i == p.row && cols[c] == p.col {
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
		col := "prefer"
		if prov := colProvider(cfg, p.col); prov != "" {
			col = prov + " effort"
			if isModelCol(p.col) {
				col = prov + " model"
			}
		}
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
		if len(names) > pickerShown {
			lines = append(lines, th.fg(th.Faint).Render(fmt.Sprintf("providers %d-%d of %d (%s) · move past the edge for the others",
				p.first+1, p.first+len(shown), len(names), strings.Join(names, ", "))))
		}
		lines = append(lines, th.fg(th.Faint).Render("prefer: a provider = that one · other = not the planner's · auto = least used"))
		lines = append(lines, th.fg(th.Faint).Render("At a usage limit the role's route on the next provider (routing.provider_order) is used automatically."))
		lines = append(lines, th.fg(th.Faint).Render("Tip: `sy models --refresh` reads the current Codex catalog from `codex debug models`."))
	}
	box := th.box(th.Main, w).Render(clipLines(strings.Join(lines, "\n"), H-2))
	return indent(box, max(0, (W-w)/2))
}
