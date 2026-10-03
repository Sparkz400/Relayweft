package tui

import (
	"os"
	"runtime"

	"github.com/charmbracelet/lipgloss"
	"github.com/sparkz400/switchyard/internal/event"
)

// Glyphs are the characters used for status and decoration; the ASCII set
// is used on legacy Windows conhost, which mangles box drawing and braille.
type Glyphs struct {
	Spinner                       []string
	OK, Fail, Queued, Killed      string
	Pulse, Dot, Role, Arrow, Back string
	BarFull, BarEmpty             string
	H, V, TeeDown, TeeUp, Cross   string
	CornerTL, CornerTR            string
	CornerBL, CornerBR            string
	Border                        lipgloss.Border
	Logo                          string
}

var unicodeGlyphs = Glyphs{
	Spinner: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
	OK:      "✓", Fail: "✗", Queued: "○", Killed: "⊘",
	Pulse: "●", Dot: "•", Role: "◆", Arrow: "→", Back: "↩",
	BarFull: "█", BarEmpty: "░",
	H: "─", V: "│", TeeDown: "┬", TeeUp: "┴", Cross: "┼",
	CornerTL: "┌", CornerTR: "┐", CornerBL: "└", CornerBR: "┘",
	Border: lipgloss.RoundedBorder(),
	Logo:   "⇄",
}

var asciiGlyphs = Glyphs{
	Spinner: []string{"|", "/", "-", "\\"},
	OK:      "+", Fail: "x", Queued: "o", Killed: "/",
	Pulse: "*", Dot: "*", Role: "#", Arrow: "->", Back: "<-",
	BarFull: "#", BarEmpty: ".",
	H: "-", V: "|", TeeDown: "+", TeeUp: "+", Cross: "+",
	CornerTL: "+", CornerTR: "+", CornerBL: "+", CornerBR: "+",
	Border: lipgloss.NormalBorder(),
	Logo:   "<>",
}

func init() {
	asciiGlyphs.Border = lipgloss.Border{Top: "-", Bottom: "-", Left: "|", Right: "|", TopLeft: "+", TopRight: "+", BottomLeft: "+", BottomRight: "+"}
}

// Theme is the color palette plus glyphs.
type Theme struct {
	G         Glyphs
	ASCII     bool
	Codex     lipgloss.Color
	Claude    lipgloss.Color
	Router    lipgloss.Color
	Reviewer  lipgloss.Color
	Main      lipgloss.Color
	Muted     lipgloss.Color
	Faint     lipgloss.Color
	Text      lipgloss.Color
	OKColor   lipgloss.Color
	FailColor lipgloss.Color
	Warn      lipgloss.Color
	Focus     lipgloss.Color
	RoleColor map[string]lipgloss.Color
}

// NewTheme picks unicode or ASCII. mode is the config value: auto|unicode|ascii.
func NewTheme(mode string) Theme {
	ascii := mode == "ascii" || (mode != "unicode" && legacyConsole())
	t := Theme{
		G: unicodeGlyphs, ASCII: ascii,
		Codex:     lipgloss.Color("#10A37F"),
		Claude:    lipgloss.Color("#D97757"),
		Router:    lipgloss.Color("#4FC1FF"),
		Reviewer:  lipgloss.Color("#B48EFF"),
		Main:      lipgloss.Color("#F2C94C"),
		Muted:     lipgloss.Color("#8A8F98"),
		Faint:     lipgloss.Color("#4A4F57"),
		Text:      lipgloss.Color("#E6E6E6"),
		OKColor:   lipgloss.Color("#3DDC84"),
		FailColor: lipgloss.Color("#FF5C5C"),
		Warn:      lipgloss.Color("#FFB347"),
		Focus:     lipgloss.Color("#FFFFFF"),
	}
	if ascii {
		t.G = asciiGlyphs
	}
	t.RoleColor = map[string]lipgloss.Color{
		event.RolePlanner:    t.Main,
		event.RoleWorker:     lipgloss.Color("#5AA9E6"),
		event.RoleWorkerHigh: lipgloss.Color("#7F8CFF"),
		event.RoleExplorer:   lipgloss.Color("#56D6C9"),
		event.RoleResearcher: lipgloss.Color("#9BD35A"),
		event.RoleReviewer:   t.Reviewer,
		event.RoleJudge:      t.Router,
		"explore":            lipgloss.Color("#56D6C9"),
		"research":           lipgloss.Color("#9BD35A"),
		"edit":               lipgloss.Color("#5AA9E6"),
		"fix":                lipgloss.Color("#5AA9E6"),
	}
	return t
}

// legacyConsole reports whether we run in the old Windows console host,
// which lacks reliable Unicode and true color. Windows Terminal sets
// WT_SESSION; VS Code, ConEmu, WezTerm and others set TERM_PROGRAM/ConEmuANSI.
func legacyConsole() bool {
	if runtime.GOOS != "windows" {
		return false
	}
	for _, k := range []string{"WT_SESSION", "TERM_PROGRAM", "ConEmuANSI", "WEZTERM_PANE", "ALACRITTY_LOG", "TERM"} {
		if os.Getenv(k) != "" {
			return false
		}
	}
	return true
}

// ProviderColor returns the provider's color.
func (t Theme) ProviderColor(p string) lipgloss.Color {
	switch p {
	case event.Codex:
		return t.Codex
	case event.Claude:
		return t.Claude
	}
	return t.Muted
}

// Role returns the role's color.
func (t Theme) Role(r string) lipgloss.Color {
	if c, ok := t.RoleColor[r]; ok {
		return c
	}
	return t.Muted
}

func (t Theme) fg(c lipgloss.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }
func (t Theme) bold(c lipgloss.Color) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(c).Bold(true)
}

func (t Theme) box(c lipgloss.Color, w int) lipgloss.Style {
	return lipgloss.NewStyle().Border(t.G.Border).BorderForeground(c).Width(w-2).Padding(0, 1)
}
