package orchestrator

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Acceptance says how far a finished task is known to be done, as three
// separate claims that earlier summaries ran together:
//
//   - Agents: the agents finished their subtasks (they exited ok; nothing
//     more).
//   - Checks: the repo's configured checks (verify.commands) passed when
//     rw ran them.
//   - Requirements: evidence for each acceptance criterion, from the
//     reviewer or tests written first. Command-level check results do not
//     prove that an individual cited test ran and passed.
//
// On the realistic bench, agents finished and checks passed while a named
// part of the task was missing, so a pass at one level never stands in
// for the next.
type Acceptance struct {
	Agents       Level `json:"agents"`
	Checks       Level `json:"checks"`
	Requirements Level `json:"requirements"`
	// Explicit is set when the task text gave its acceptance criteria
	// (ParseCriteria); otherwise the reviewer listed them from the task.
	Explicit bool        `json:"explicit,omitempty"`
	Criteria []Criterion `json:"criteria,omitempty"`
}

// Level is one claim of an Acceptance.
type Level struct {
	Status string `json:"status"` // LevelPass, LevelFail, LevelPartial or LevelUnchecked
	Detail string `json:"detail,omitempty"`
}

// Level statuses.
const (
	LevelPass      = "pass"
	LevelFail      = "fail"
	LevelPartial   = "partial"   // requirements: some verified, none unmet
	LevelUnchecked = "unchecked" // nothing ran that could tell
)

// Criterion is one requirement and what supports it.
type Criterion struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Status   string `json:"status"`             // a Crit* value
	Evidence string `json:"evidence,omitempty"` // the code that meets it, as the reviewer cites it
	Test     string `json:"test,omitempty"`     // the test that covers it: "file: name"
	Note     string `json:"note,omitempty"`     // why it is not verified
}

// Criterion statuses.
const (
	// CritVerified is retained for saved reports. New reports require
	// individual test execution evidence, which checks do not yet record.
	CritVerified = "verified"
	// CritEvidence: supporting code or a test was cited, but no test
	// was confirmed to have run and passed.
	CritEvidence = "evidence"
	// CritUnmet: the reviewer found it missing or wrong.
	CritUnmet = "unmet"
	// CritUnchecked: no reviewer judged it.
	CritUnchecked = "unchecked"
)

// ReqVerdict is the final reviewer's judgment of one requirement.
type ReqVerdict struct {
	ID          string `json:"id"`
	Requirement string `json:"requirement"`
	Met         bool   `json:"met"`
	Evidence    string `json:"evidence"`
	TestFile    string `json:"test_file"`
	TestName    string `json:"test_name"`
}

// reCriteriaHead matches the heading of a task's acceptance criteria:
// "Acceptance criteria:", "## Done when", "**Requirements:**".
var reCriteriaHead = regexp.MustCompile(`(?i)^\s*(?:#{1,6}\s*)?(?:\*\*|__)?\s*(acceptance criteria|acceptance|done when|definition of done|requirements)\s*:?\s*(?:\*\*|__)?\s*:?\s*$`)

// reBullet matches a list item: "- x", "* x", "1. x", "- [ ] x".
var reBullet = regexp.MustCompile(`^\s*(?:[-*+]|\d{1,3}[.)])\s+(?:\[[ xX]\]\s+)?(.*\S)\s*$`)

// maxCriteria caps the criteria rw tracks; more are folded into the last.
const maxCriteria = 30

// ParseCriteria returns the acceptance criteria the task text lists: the
// items of a list under an "Acceptance criteria:" heading (also "Done
// when", "Definition of done", "Requirements"). Indented lines continue the
// item above; the list ends at the next heading or other text. A task
// without such a list has none (the reviewer then lists the task's
// requirements itself).
func ParseCriteria(task string) []string {
	var out []string
	in := false
	for _, line := range strings.Split(strings.ReplaceAll(task, "\r\n", "\n"), "\n") {
		if reCriteriaHead.MatchString(line) {
			in = true
			continue
		}
		if !in {
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if m := reBullet.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
			continue
		}
		if len(out) > 0 && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			out[len(out)-1] += " " + strings.TrimSpace(line)
			continue
		}
		in = false // other text ends the list
	}
	if len(out) > maxCriteria {
		out[maxCriteria-1] = strings.Join(out[maxCriteria-1:], "; ")
		out = out[:maxCriteria]
	}
	return out
}

// WithCriteria appends acceptance criteria to a task text (rw run
// --accept), in the form ParseCriteria reads.
func WithCriteria(task string, criteria []string) string {
	var items []string
	for _, c := range criteria {
		if c = strings.Join(strings.Fields(c), " "); c != "" {
			items = append(items, "- "+c)
		}
	}
	if len(items) == 0 {
		return task
	}
	return strings.TrimRight(task, "\n") + "\n\nAcceptance criteria:\n" + strings.Join(items, "\n")
}

func critID(i int) string { return fmt.Sprintf("R%d", i+1) }

// criteriaBlock is the final review prompt's part about the requirements:
// the task's explicit criteria, or the instruction to list them.
func criteriaBlock(criteria []string) string {
	var b strings.Builder
	if len(criteria) > 0 {
		b.WriteString("\nACCEPTANCE CRITERIA (given with the task; judge every one, by its id):\n")
		for i, c := range criteria {
			fmt.Fprintf(&b, "%s. %s\n", critID(i), c)
		}
	}
	return b.String()
}

// reqsJSONHint is the "requirements" part of the final review's reply.
const reqsJSONHint = `"requirements": [{"id": "R1", "requirement": "...", "met": true|false, "evidence": "file:line - the code that meets it", "test_file": "path/to/the_test file (empty if none)", "test_name": "the test function or case name (empty if none)"}]`

// applyRequirements makes a verdict agree with its requirements: one the
// reviewer found unmet rejects the work, however it voted.
func applyRequirements(v *Verdict) {
	for _, r := range v.Requirements {
		if r.Met {
			continue
		}
		v.Approve = false
		issue := "missing: " + strings.TrimSpace(firstNonEmpty(r.Requirement, r.ID))
		if !containsFold(v.Issues, issue) {
			v.Issues = append(v.Issues, issue)
		}
	}
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), s) {
			return true
		}
	}
	return false
}

// acceptanceInput is what the end of a task knows for its Acceptance.
type acceptanceInput struct {
	done, steps int    // subtasks that finished ok, of all
	allOK       bool   // every subtask and fix round finished
	edits       bool   // the plan changes files
	verifying   bool   // checks are configured
	checksRan   bool   // the checks ran in the last round
	verified    bool   // and passed
	checksWhy   string // why they did not run ("" = the default reason)
	checkCmds   []string
	// The final review of the last round: reviewed is false when none
	// ran or it failed to answer; why says why none ran.
	reviewed bool
	verdict  Verdict
	why      string
	criteria []string // explicit criteria (ParseCriteria)
	// firstTests are the acceptance tests written first (testsfirst.go):
	// without a review, they supply evidence for derived requirements,
	// but command-level check results cannot certify each named test.
	firstTests []AcceptanceTest
	// testFound reports whether a test exists ("" = found); nil skips
	// the lookup (tests).
	testFound func(file, name string) string
}

// buildAcceptance assembles a finished task's Acceptance; nil for a
// read-only task without criteria (a question has nothing to accept).
func buildAcceptance(in acceptanceInput) *Acceptance {
	if !in.edits && len(in.criteria) == 0 {
		return nil
	}
	a := &Acceptance{Explicit: len(in.criteria) > 0}
	a.Agents = Level{Status: LevelPass, Detail: fmt.Sprintf("%d/%d subtasks finished", in.done, in.steps)}
	if !in.allOK {
		a.Agents = Level{Status: LevelFail, Detail: fmt.Sprintf("%d/%d subtasks finished", in.done, in.steps)}
		if in.done == in.steps {
			a.Agents.Detail += "; a fix round failed"
		}
	}
	switch {
	case !in.edits:
		a.Checks = Level{Status: LevelUnchecked, Detail: "a read-only task: no checks ran"}
	case !in.verifying:
		a.Checks = Level{Status: LevelUnchecked, Detail: "no checks configured (verify.commands)"}
	case !in.checksRan:
		a.Checks = Level{Status: LevelUnchecked, Detail: firstNonEmpty(in.checksWhy, "the checks did not run")}
	case in.verified:
		a.Checks = Level{Status: LevelPass, Detail: "passed: " + strings.Join(in.checkCmds, ", ")}
	default:
		a.Checks = Level{Status: LevelFail, Detail: "failing: " + strings.Join(in.checkCmds, ", ")}
	}
	checksPass := a.Checks.Status == LevelPass
	// Check results are command-level only. Finding a test in a file cannot
	// tell us whether that command selected it, skipped it, or ran it at all.
	testNote := func(file, name string) string {
		if strings.TrimSpace(file) == "" || strings.TrimSpace(name) == "" {
			return "no test named"
		}
		if in.testFound != nil {
			if note := in.testFound(file, name); note != "" {
				return note
			}
		}
		if !checksPass && in.verifying {
			return "the checks did not pass"
		}
		if !checksPass {
			return "no checks configured: rw did not run the test"
		}
		return "individual test execution was not confirmed"
	}

	// The criteria: the explicit ones by id, else the reviewer's own list.
	byID := map[string]ReqVerdict{}
	for _, r := range in.verdict.Requirements {
		byID[strings.ToUpper(strings.TrimSpace(r.ID))] = r
	}
	var reqs []ReqVerdict
	if a.Explicit {
		for i, c := range in.criteria {
			r, ok := byID[critID(i)]
			if !ok {
				r = ReqVerdict{ID: critID(i)}
			}
			r.Requirement = c
			if !ok {
				r.ID = "" // not judged
			}
			reqs = append(reqs, r)
		}
	} else if in.reviewed {
		reqs = in.verdict.Requirements
		if len(reqs) > maxCriteria {
			reqs = reqs[:maxCriteria]
		}
	}
	for i, r := range reqs {
		c := Criterion{ID: critID(i), Text: strings.TrimSpace(r.Requirement), Evidence: clip(r.Evidence, 300)}
		if c.Text == "" {
			c.Text = "(the reviewer named no requirement)"
		}
		if r.TestFile != "" || r.TestName != "" {
			c.Test = strings.Trim(strings.TrimSpace(r.TestFile)+": "+strings.TrimSpace(r.TestName), ": ")
		}
		switch {
		case !in.reviewed:
			c.Status, c.Note = CritUnchecked, in.why
			c.Evidence, c.Test = "", ""
		case a.Explicit && r.ID == "":
			c.Status, c.Note = CritUnchecked, "the reviewer did not judge it"
		case !r.Met:
			c.Status, c.Note = CritUnmet, "the reviewer found it missing or wrong"
		case strings.TrimSpace(r.Evidence) == "" && c.Test == "":
			c.Status, c.Note = CritUnchecked, "the reviewer gave no evidence"
		case strings.TrimSpace(r.TestFile) == "" || strings.TrimSpace(r.TestName) == "":
			c.Status, c.Note = CritEvidence, "no test named"
		default:
			c.Status = CritEvidence
			c.Note = testNote(r.TestFile, r.TestName)
		}
		a.Criteria = append(a.Criteria, c)
	}
	if !in.reviewed && !a.Explicit {
		for i, ft := range in.firstTests {
			c := Criterion{ID: critID(i), Text: strings.TrimSpace(ft.Requirement), Test: strings.Trim(ft.File+": "+ft.Name, ": "), Status: CritEvidence,
				Evidence: "its acceptance test, written before the code", Note: testNote(ft.File, ft.Name)}
			if !checksPass {
				c.Status = CritUnchecked
			}
			a.Criteria = append(a.Criteria, c)
		}
	}

	count := map[string]int{}
	for _, c := range a.Criteria {
		count[c.Status]++
	}
	n := len(a.Criteria)
	switch {
	case n == 0 && !in.reviewed:
		a.Requirements = Level{Status: LevelUnchecked, Detail: in.why}
	case n == 0:
		a.Requirements = Level{Status: LevelUnchecked, Detail: "the reviewer listed no requirements"}
	case count[CritUnmet] > 0:
		a.Requirements = Level{Status: LevelFail, Detail: fmt.Sprintf("%d of %d unmet", count[CritUnmet], n)}
	case count[CritVerified] == n:
		a.Requirements = Level{Status: LevelPass, Detail: fmt.Sprintf("%d/%d verified", n, n)}
	case count[CritVerified] > 0:
		a.Requirements = Level{Status: LevelPartial, Detail: fmt.Sprintf("%d/%d verified", count[CritVerified], n)}
	default:
		a.Requirements = Level{Status: LevelUnchecked, Detail: fmt.Sprintf("0/%d verified", n)}
	}
	if a.Requirements.Status != LevelPass && a.Requirements.Status != LevelFail && n > 0 {
		var why []string
		if k := count[CritEvidence]; k > 0 {
			why = append(why, fmt.Sprintf("%d with evidence but no passing test", k))
		}
		if k := count[CritUnchecked]; k > 0 {
			why = append(why, fmt.Sprintf("%d not judged", k))
		}
		if len(why) > 0 {
			a.Requirements.Detail += " (" + strings.Join(why, ", ") + ")"
		}
	}
	return a
}

// SummaryPart is the task summary's part about the requirements ("" when
// there are none to show).
func (a *Acceptance) SummaryPart() string {
	if a == nil || len(a.Criteria) == 0 {
		return ""
	}
	return "requirements " + a.Requirements.Detail
}

// Lines renders the Acceptance for a terminal or a plain-text comment.
func (a *Acceptance) Lines() []string {
	if a == nil {
		return nil
	}
	out := []string{
		"agent finished:        " + a.Agents.mark() + " " + a.Agents.Detail,
		"checks passed:         " + a.Checks.mark() + " " + a.Checks.Detail,
		"requirements verified: " + a.Requirements.mark() + " " + a.Requirements.Detail,
	}
	for _, c := range a.Criteria {
		out = append(out, "  "+c.Mark()+" "+c.ID+" "+c.Text)
		if s := c.Support(); s != "" {
			out = append(out, "       "+s)
		}
	}
	return out
}

// Mark is a criterion's one-character status.
func (c Criterion) Mark() string {
	switch c.Status {
	case CritVerified:
		return "✓"
	case CritUnmet:
		return "✗"
	case CritEvidence:
		return "~"
	}
	return "?"
}

// Support is what backs a criterion: its test, its evidence, and why it is
// not verified.
func (c Criterion) Support() string {
	var parts []string
	if c.Test != "" {
		parts = append(parts, "test: "+c.Test)
	}
	if c.Evidence != "" {
		parts = append(parts, "evidence: "+c.Evidence)
	}
	if c.Note != "" {
		parts = append(parts, "("+c.Note+")")
	}
	return strings.Join(parts, "; ")
}

func (l Level) mark() string {
	switch l.Status {
	case LevelPass:
		return "✓"
	case LevelFail:
		return "✗"
	case LevelPartial:
		return "~"
	}
	return "?"
}

// testFoundIn returns a lookup of a reviewer-named test in the task's repos:
// the file must be inside one of them and contain the test's name.
func (t *task) testFoundIn() func(file, name string) string {
	return func(file, name string) string {
		file = strings.TrimSpace(strings.Trim(file, "`"))
		name = strings.TrimSpace(strings.Trim(name, "`"))
		// The reviewer may cite "file:line" or "repo: file".
		if i := strings.LastIndex(file, ":"); i > 1 && isDigits(file[i+1:]) {
			file = file[:i]
		}
		dirs := []string{}
		for _, r := range t.allRepos() {
			if rest, ok := strings.CutPrefix(file, r.repoName+": "); ok && r.repoName != "" {
				file, dirs = rest, []string{r.dir}
				break
			}
			dirs = append(dirs, r.dir)
		}
		if filepath.IsAbs(file) || file == "" {
			return "test file " + file + " is not a repo path"
		}
		rel := filepath.Clean(filepath.FromSlash(file))
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "test file " + file + " is outside the repo"
		}
		for _, d := range dirs {
			if d == "" {
				continue
			}
			b, err := readCapped(filepath.Join(d, rel), 4<<20)
			if err != nil {
				continue
			}
			if strings.Contains(string(b), testNameStem(name)) {
				return ""
			}
			return fmt.Sprintf("test %s not found in %s", name, file)
		}
		return "test file " + file + " not found"
	}
}

// testNameStem is the part of a test name the file must contain: the name
// up to a subtest ("TestX/case" -> "TestX") or a "Class.method" path's
// last element.
func testNameStem(name string) string {
	if i := strings.Index(name, "/"); i > 0 {
		name = name[:i]
	}
	if i := strings.LastIndex(name, "::"); i >= 0 {
		name = name[i+2:]
	}
	return name
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func readCapped(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.IsDir() {
		return nil, fmt.Errorf("%s is not a file", path)
	}
	return io.ReadAll(io.LimitReader(f, n))
}
