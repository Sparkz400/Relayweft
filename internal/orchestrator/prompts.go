package orchestrator

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/sparkz400/switchyard/internal/router"
	"github.com/sparkz400/switchyard/internal/runner"
)

// Plan is what the planner returns.
type Plan struct {
	Summary  string    `json:"summary"`
	Subtasks []Subtask `json:"subtasks"`
}

// Subtask is one unit of planned work.
type Subtask struct {
	ID        string      `json:"id"`
	Title     string      `json:"title"`
	Kind      router.Kind `json:"kind"`
	Prompt    string      `json:"prompt"`
	Files     []string    `json:"files"`
	DependsOn []string    `json:"depends_on"`
	// Role pins the subtask to a role (set in plan approval); "" = router.
	Role string `json:"role,omitempty"`
}

// Verdict is what the reviewer returns.
type Verdict struct {
	Approve bool     `json:"approve"`
	Advice  string   `json:"advice"`
	Issues  []string `json:"issues"`
}

const maxSubtasks = 8

var reFence = regexp.MustCompile("(?s)```(?:json)?\\s*\\n(.*?)\\n?```")

// extractJSON finds the JSON object in a model reply: the last fenced block
// that parses, else the outermost {...}.
func extractJSON(s string, v any) error {
	blocks := reFence.FindAllStringSubmatch(s, -1)
	for i := len(blocks) - 1; i >= 0; i-- {
		if json.Unmarshal([]byte(strings.TrimSpace(blocks[i][1])), v) == nil {
			return nil
		}
	}
	a, b := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if a >= 0 && b > a {
		if err := json.Unmarshal([]byte(s[a:b+1]), v); err == nil {
			return nil
		} else {
			return fmt.Errorf("reply is not valid JSON: %w", err)
		}
	}
	return fmt.Errorf("no JSON object in reply")
}

// ParsePlan reads and normalizes a planner reply.
func ParsePlan(reply string) (Plan, error) {
	var p Plan
	if err := extractJSON(reply, &p); err != nil {
		return p, err
	}
	return NormalizePlan(p)
}

// NormalizePlan checks and repairs a plan (also after a person edited it):
// unique, safe ids; known kinds; dependencies only on existing subtasks;
// no cycles.
func NormalizePlan(p Plan) (Plan, error) {
	if len(p.Subtasks) == 0 {
		return p, fmt.Errorf("plan has no subtasks")
	}
	if len(p.Subtasks) > maxSubtasks {
		p.Subtasks = p.Subtasks[:maxSubtasks]
	}
	// The fixed agent ids are reserved so a subtask never collides with them.
	seen := map[string]bool{"main": true, "reviewer": true, "judge": true}
	for i := range p.Subtasks {
		st := &p.Subtasks[i]
		st.ID = slug(st.ID)
		for n := i + 1; st.ID == "" || seen[st.ID]; n++ {
			st.ID = fmt.Sprintf("t%d", n)
		}
		seen[st.ID] = true
		switch strings.ToLower(string(st.Kind)) {
		case "explore", "explorer", "search", "read":
			st.Kind = router.KindExplore
		case "research", "researcher", "docs":
			st.Kind = router.KindResearch
		default:
			st.Kind = router.KindEdit
		}
		if st.Title == "" {
			st.Title = firstWords(st.Prompt, 6)
		}
		if st.Prompt == "" {
			st.Prompt = st.Title
		}
	}
	// Drop dependencies on unknown ids and on itself.
	for i := range p.Subtasks {
		st := &p.Subtasks[i]
		var deps []string
		for _, d := range st.DependsOn {
			d = slug(d)
			if seen[d] && d != st.ID {
				deps = append(deps, d)
			}
		}
		st.DependsOn = deps
	}
	if hasCycle(p.Subtasks) {
		// Fall back to running in the planner's order.
		for i := range p.Subtasks {
			p.Subtasks[i].DependsOn = nil
			if i > 0 {
				p.Subtasks[i].DependsOn = []string{p.Subtasks[i-1].ID}
			}
		}
	}
	return p, nil
}

func hasCycle(sts []Subtask) bool {
	deps := map[string][]string{}
	for _, s := range sts {
		deps[s.ID] = s.DependsOn
	}
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(string) bool
	visit = func(id string) bool {
		switch state[id] {
		case 1:
			return true
		case 2:
			return false
		}
		state[id] = 1
		for _, d := range deps[id] {
			if visit(d) {
				return true
			}
		}
		state[id] = 2
		return false
	}
	for _, s := range sts {
		if visit(s.ID) {
			return true
		}
	}
	return false
}

// ParseVerdict reads a reviewer reply. An unreadable reply approves with the
// raw text as advice, so a chatty reviewer never blocks the task.
func ParseVerdict(reply string) Verdict {
	var v Verdict
	if err := extractJSON(reply, &v); err != nil {
		return Verdict{Approve: true, Advice: strings.TrimSpace(reply)}
	}
	return v
}

var reSlug = regexp.MustCompile(`[^a-z0-9_-]+`)

func slug(s string) string {
	return strings.Trim(reSlug.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-"), "-")
}

func firstWords(s string, n int) string {
	w := strings.Fields(s)
	if len(w) > n {
		w = w[:n]
	}
	return strings.Join(w, " ")
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + " ..."
}

func planPrompt(task, advice string, previous *Plan) string {
	var b strings.Builder
	b.WriteString(runner.MarkerPlan + " You are the planner of Switchyard, a team of coding agents.\n")
	b.WriteString("Investigate the repository as needed, but DO NOT modify any files.\n\n")
	b.WriteString("TASK:\n" + task + "\n\n")
	if previous != nil {
		pj, _ := json.MarshalIndent(previous, "", "  ")
		b.WriteString("YOUR PREVIOUS PLAN:\n" + string(pj) + "\n\n")
	}
	if advice != "" {
		b.WriteString("REVIEWER FEEDBACK TO ADDRESS:\n" + advice + "\n\n")
	}
	b.WriteString(`Split the task into 1-6 subtasks. Kinds:
  explore  - read-only: find code, answer "where is X / how does Y work"
  research - read-only: read docs, summarize APIs or conventions
  edit     - changes files (and may run tests)
Rules:
  - Small tasks: a single edit subtask. Do not split for the sake of it.
  - Edit subtasks that run in parallel must touch disjoint files; list the files each will touch.
  - Use depends_on when a subtask needs another's result (its output is passed along).
  - Each prompt must be self-contained: the agent sees only its prompt and dependency results.
Reply with ONLY this JSON in a json code block:
{"summary": "one sentence", "subtasks": [{"id": "short-id", "title": "3-6 words", "kind": "explore|research|edit", "prompt": "...", "files": ["path"], "depends_on": ["id"]}]}
`)
	return b.String()
}

func planReviewPrompt(task string, p Plan) string {
	pj, _ := json.MarshalIndent(p, "", "  ")
	return runner.MarkerPlanReview + ` You are the reviewer at a checkpoint. Do NOT modify files; you may read the repository.
Review this plan before it is executed. Check: does it solve the task, are edit subtasks independent
(disjoint files) where they run in parallel, is anything risky or missing?

TASK:
` + task + `

PLAN:
` + string(pj) + `

Reply with ONLY this JSON in a json code block:
{"approve": true|false, "advice": "concise, actionable advice", "issues": ["..."]}
Approve unless there is a real problem; put minor suggestions in advice.
`
}

func errorReviewPrompt(task string, st Subtask, errText string, attempts int) string {
	return fmt.Sprintf(`%s You are the reviewer at a checkpoint. Do NOT modify files; you may read the repository.
A subtask failed %d times with the same error. Diagnose the root cause and tell the next agent exactly what to do.

OVERALL TASK:
%s

SUBTASK %q:
%s

ERROR:
%s

Reply with ONLY this JSON in a json code block:
{"approve": false, "advice": "root cause and the concrete fix", "issues": ["..."]}
`, runner.MarkerErrorReview, attempts, task, st.Title, st.Prompt, clip(errText, 4000))
}

func finalReviewPrompt(task string, p Plan, results map[string]stepResult, stat, diff string, mergeNotes []string, verifyReport string) string {
	var b strings.Builder
	b.WriteString(runner.MarkerFinalReview + " You are the reviewer at the final checkpoint. Do NOT modify files; you may read the repository and run read-only checks.\n")
	b.WriteString("Decide whether the task is done correctly.\n\nTASK:\n" + task + "\n\nPLAN SUMMARY:\n" + p.Summary + "\n\nSUBTASK RESULTS:\n")
	for _, st := range p.Subtasks {
		r := results[st.ID]
		status := "ok"
		if !r.ok {
			status = "FAILED: " + clip(r.err, 300)
		}
		fmt.Fprintf(&b, "- %s (%s, %s): %s\n  %s\n", st.ID, st.Kind, r.route, status, clip(r.final, 800))
	}
	if len(mergeNotes) > 0 {
		b.WriteString("\nMERGE NOTES:\n- " + strings.Join(mergeNotes, "\n- ") + "\n")
	}
	if verifyReport != "" {
		b.WriteString("\nTHE REPO'S CHECKS (run by Switchyard just now):\n" + clip(verifyReport, 6000) + "\n")
	}
	if stat != "" {
		b.WriteString("\nDIFF STAT:\n" + stat + "\n\nDIFF:\n" + diff + "\n")
	} else {
		b.WriteString("\n(No git diff available; inspect the files directly.)\n")
	}
	b.WriteString(`
Reply with ONLY this JSON in a json code block:
{"approve": true|false, "advice": "what must change (empty if approved)", "issues": ["..."]}
Only reject for real defects: bugs, missing requirements, broken builds or tests.
`)
	return b.String()
}

func stepPrompt(task string, st Subtask, depResults []string, prevErr, advice string, readOnly bool) string {
	var b strings.Builder
	b.WriteString(runner.MarkerStep + " You are one agent in Switchyard, a team of coding agents.\n")
	b.WriteString("OVERALL TASK (for context; do only your part):\n" + task + "\n\n")
	fmt.Fprintf(&b, "YOUR SUBTASK %q (%s):\n%s\n", st.ID, st.Title, st.Prompt)
	if len(st.Files) > 0 {
		b.WriteString("\nFiles you are expected to touch: " + strings.Join(st.Files, ", ") + "\n")
	}
	for _, d := range depResults {
		b.WriteString("\nRESULT FROM A PREVIOUS SUBTASK:\n" + d + "\n")
	}
	if prevErr != "" {
		b.WriteString("\nTHE PREVIOUS ATTEMPT FAILED WITH:\n" + clip(prevErr, 2000) + "\n")
	}
	if advice != "" {
		b.WriteString("\nREVIEWER ADVICE (follow it):\n" + advice + "\n")
	}
	if readOnly {
		b.WriteString("\nThis is a read-only subtask: DO NOT modify any files.\n")
	} else {
		b.WriteString("\nStay within your subtask; other agents may be editing other files in parallel.\n")
	}
	b.WriteString("When finished, reply with a short summary of what you found or changed.\n")
	return b.String()
}

// lfsNote is appended to step prompts that run in a worktree of a Git LFS repo.
const lfsNote = "\nNOTE: Git LFS files (binary assets such as textures, models, audio) appear here as small text pointer files (\"version https://git-lfs.github.com/spec/v1 ...\"). This is expected: do not edit, \"fix\" or delete them, and do not run builds that need those assets.\n"

func fixPrompt(task string, v Verdict) string {
	return runner.MarkerFix + ` You are a worker in Switchyard. The reviewer checked the finished work and asked for changes.

TASK:
` + task + `

REVIEWER ADVICE:
` + v.Advice + `

ISSUES:
- ` + strings.Join(v.Issues, "\n- ") + `

Make the requested changes now, run the relevant tests if you can, then reply with a short summary.
`
}
