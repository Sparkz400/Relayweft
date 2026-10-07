package orchestrator

import (
	"strings"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/repodocs"
)

// The repo's own conventions (context.repo_docs): CONTRIBUTING, the PR
// template, CODEOWNERS, CI commands and AGENTS.md / CLAUDE.md, summarized
// without a model call (internal/repodocs). The planner and the final
// reviewer get them as fenced, untrusted repo data; step prompts do not
// (the CLIs read AGENTS.md / CLAUDE.md themselves), and the CI commands
// are never added to the verify commands.

// repoDocs is the summary for a repo root ("" when off or none).
func repoDocs(cfg *config.Config, root string) string {
	if !cfg.Context.RepoDocs || root == "" {
		return ""
	}
	return repodocs.Summary(root, cfg.Context.RepoDocsMaxKB)
}

// docsContext is the fenced conventions block for the planner and the
// final reviewer: every repo's, named, in a multi-repo task.
func (t *task) docsContext() string { return t.docsContextMax(0) }

// docsContextMax is docsContext with the conventions clipped to max bytes
// before they are fenced (0 = no limit), so the fence always closes.
func (t *task) docsContextMax(max int) string {
	fence := func(s string) string {
		if max > 0 && len(s) > max {
			s = s[:max] + "\n... (conventions shortened)"
		}
		return repodocs.Fence(s)
	}
	if len(t.repos) == 0 {
		return fence(t.repoDocs)
	}
	var b strings.Builder
	for _, r := range t.allRepos() {
		if r.repoDocs == "" {
			continue
		}
		name := r.repoName
		if name == "" {
			name = PrimaryRepo
		}
		b.WriteString("## repo " + name + "\n" + r.repoDocs + "\n")
	}
	return fence(strings.TrimRight(b.String(), "\n"))
}
