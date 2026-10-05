package tui

import (
	"strings"

	"github.com/sparkz400/relayweft/internal/orchestrator"
)

// projectLabel is the header's project name: the folder, plus the extra
// repos of a multi-repo workspace ("api + web").
func (m *Model) projectLabel(base string) string {
	if m.opt.Orc == nil {
		return base
	}
	repos := m.opt.Orc.Repos()
	if len(repos) == 0 {
		return base
	}
	names := make([]string, len(repos))
	for i, r := range repos {
		names[i] = r.Name
	}
	return base + " + " + strings.Join(names, ", ")
}

// planRepo is where a planned subtask works in a multi-repo plan ("" for a
// single-repo plan).
func planRepo(p orchestrator.Plan, st orchestrator.Subtask) string {
	if len(p.Repos) == 0 {
		return ""
	}
	if st.Repo == "" {
		return p.Repos[0]
	}
	return st.Repo
}

// cycleRepo moves a subtask to the next repo of a multi-repo plan.
func cycleRepo(p orchestrator.Plan, cur string) string {
	if len(p.Repos) == 0 {
		return ""
	}
	if cur == "" {
		cur = p.Repos[0]
	}
	for i, r := range p.Repos {
		if r == cur {
			next := p.Repos[(i+1)%len(p.Repos)]
			if next == p.Repos[0] {
				return ""
			}
			return next
		}
	}
	return ""
}
