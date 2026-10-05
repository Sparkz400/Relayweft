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
func (t *task) docsContext() string {
	if len(t.repos) == 0 {
		return repodocs.Fence(t.repoDocs)
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
	return repodocs.Fence(strings.TrimRight(b.String(), "\n"))
}
