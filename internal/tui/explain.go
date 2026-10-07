package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/report"
)

// explainCmd is rw explain in the TUI: why a task ran as one agent or
// several, each run's rule and reason, estimate against use and what
// escalated. id "" is the last finished task in dir. It reads the session
// logs off the UI thread and returns the lines for the log.
func explainCmd(dir, id, sessionDir string) tea.Cmd {
	return func() tea.Msg {
		var st *orchestrator.TaskState
		var err error
		if id == "" {
			st, err = report.LastEnded(dir)
		} else {
			st, err = orchestrator.LoadTask(id)
		}
		if err != nil {
			return undoMsg{fmt.Sprintf("explain: %v", err)}
		}
		var b strings.Builder
		if err := report.Explain(st, report.Options{SessionDir: sessionDir}).Text(&b); err != nil {
			return undoMsg{fmt.Sprintf("explain: %v", err)}
		}
		lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
		return undoMsg(append(lines, "rw report "+st.ID+" --open shows the same reasons in the HTML report"))
	}
}
