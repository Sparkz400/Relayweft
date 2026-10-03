package tui

import (
	"strings"
	"time"

	bkey "github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Paste handling.
//
// Unix terminals send pastes as one bracketed-paste message (KeyMsg.Paste),
// which keeps newlines. On Windows, Bubble Tea reads console input records
// and a paste arrives as individual keystrokes, so the first newline would
// look like Enter and submit the first line. Keystrokes from a paste arrive
// microseconds apart while a human needs tens of milliseconds between keys,
// so:
//   - an Enter that follows another key within pasteGap is a newline;
//   - any other Enter only submits after submitDelay with no further input;
//     if more keys arrive first, it was the start of a paste and becomes a
//     newline too.
//
// Nothing ever submits a paste: you always press Enter yourself.
const (
	pasteGap    = 12 * time.Millisecond
	submitDelay = 40 * time.Millisecond
	maxPromptH  = 8
)

type promptBox struct {
	ta        textarea.Model
	now       func() time.Time
	lastKey   time.Time
	pending   bool // an Enter is waiting to see if a paste follows
	pendingID int
}

// submitCheckMsg fires submitDelay after an Enter.
type submitCheckMsg int

func newPrompt(ascii bool) promptBox {
	ta := textarea.New()
	ta.Placeholder = "describe a task and press enter  ·  paste multi-line text freely  ·  alt+enter new line  ·  /help"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.EndOfBufferCharacter = ' '
	first, rest := "› ", "  "
	if ascii {
		first = "> "
	}
	ta.SetPromptFunc(2, func(i int) string {
		if i == 0 {
			return first
		}
		return rest
	})
	focused, blurred := textarea.DefaultStyles()
	focused.CursorLine = lipgloss.NewStyle()
	focused.Base = lipgloss.NewStyle()
	blurred.Base = lipgloss.NewStyle()
	ta.FocusedStyle, ta.BlurredStyle = focused, blurred
	// Enter is handled by promptBox; these insert a newline explicitly.
	ta.KeyMap.InsertNewline = bkey.NewBinding(bkey.WithKeys("alt+enter", "ctrl+j"))
	ta.SetHeight(1)
	ta.Focus()
	return promptBox{ta: ta, now: time.Now}
}

func (p *promptBox) Value() string  { return p.ta.Value() }
func (p *promptBox) Focus() tea.Cmd { return p.ta.Focus() }
func (p *promptBox) Blur()          { p.ta.Blur() }
func (p *promptBox) Focused() bool  { return p.ta.Focused() }
func (p *promptBox) SetWidth(w int) { p.ta.SetWidth(w) }
func (p *promptBox) View() string   { return p.ta.View() }
func (p *promptBox) CursorEnd()     { p.ta.CursorEnd() }
func (p *promptBox) InsertString(s string) {
	p.ta.InsertString(s)
	p.fitHeight()
}

func (p *promptBox) SetValue(s string) {
	p.ta.SetValue(s)
	p.pending = false
	p.fitHeight()
}

func (p *promptBox) fitHeight() {
	p.ta.SetHeight(max(1, min(maxPromptH, p.ta.LineCount())))
}

// key handles a key while the prompt has focus. submit is true when the
// user really pressed Enter (decided later, via submitCheckMsg). It returns
// handled=false for keys the caller should treat itself (tab/esc for focus),
// unless they arrive inside a paste.
func (p *promptBox) key(k tea.KeyMsg) (cmd tea.Cmd, handled bool) {
	now := p.now()
	burst := !p.lastKey.IsZero() && now.Sub(p.lastKey) < pasteGap
	p.lastKey = now

	if k.Paste { // bracketed paste: newlines included, never submits
		var c tea.Cmd
		p.ta, c = p.ta.Update(k)
		p.fitHeight()
		return c, true
	}
	if p.pending {
		// Another key right after an Enter: that Enter was part of a paste.
		p.pending = false
		p.ta.InsertString("\n")
		burst = true
	}
	switch k.Type {
	case tea.KeyEnter:
		if burst || k.Alt { // alt+enter: explicit new line
			p.ta.InsertString("\n")
			p.fitHeight()
			return nil, true
		}
		p.pending = true
		p.pendingID++
		id := p.pendingID
		return tea.Tick(submitDelay, func(time.Time) tea.Msg { return submitCheckMsg(id) }), true
	case tea.KeyTab:
		if burst {
			p.ta.InsertString("    ")
			return nil, true
		}
		return nil, false
	case tea.KeyEsc:
		if burst {
			return nil, true // stray escape inside a paste
		}
		return nil, false
	}
	var c tea.Cmd
	p.ta, c = p.ta.Update(k)
	p.fitHeight()
	return c, true
}

// inBurst reports whether a key arriving now belongs to a fast burst (a
// paste), without recording it.
func (p *promptBox) inBurst() bool {
	return !p.lastKey.IsZero() && p.now().Sub(p.lastKey) < pasteGap
}

// confirm reports whether a submitCheckMsg means "submit now" and returns
// the text, clearing the box.
func (p *promptBox) confirm(id submitCheckMsg) (string, bool) {
	if !p.pending || int(id) != p.pendingID {
		return "", false
	}
	p.pending = false
	text := strings.TrimSpace(p.ta.Value())
	if text == "" {
		return "", false
	}
	p.SetValue("")
	return text, true
}

// update passes non-key messages (cursor blink) to the textarea.
func (p *promptBox) update(msg tea.Msg) tea.Cmd {
	var c tea.Cmd
	p.ta, c = p.ta.Update(msg)
	return c
}
