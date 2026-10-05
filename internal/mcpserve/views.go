package mcpserve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/report"
	"github.com/sparkz400/relayweft/internal/web"
)

// Status is what task_status returns.
type Status struct {
	TaskID  string      `json:"task_id"`
	Kind    string      `json:"kind"`
	Task    string      `json:"task"`
	Status  string      `json:"status"` // starting, running, waiting, done, failed, cancelled, interrupted
	Phase   string      `json:"phase,omitempty"`
	Elapsed string      `json:"elapsed,omitempty"`
	Waiting []Waiting   `json:"waiting,omitempty"`
	Plan    *PlanView   `json:"plan,omitempty"`
	Agents  []agentView `json:"agents,omitempty"`
	Cost    *CostView   `json:"cost,omitempty"`
	Limits  []LimitView `json:"limits,omitempty"`
	Recent  []string    `json:"recent,omitempty"`
	Summary string      `json:"summary,omitempty"`
	Next    string      `json:"next"`
}

// Waiting is a question the task waits on.
type Waiting struct {
	RequestID string       `json:"request_id"`
	Type      string       `json:"type"` // plan or changes
	Plan      *PlanView    `json:"plan,omitempty"`
	Estimate  string       `json:"estimate,omitempty"`
	Changes   *ChangesView `json:"changes,omitempty"`
	Answer    string       `json:"answer"`
}

// PlanView is a plan with each step's progress.
type PlanView struct {
	Summary string     `json:"summary,omitempty"`
	Steps   []StepView `json:"steps"`
}

// StepView is one planned step.
type StepView struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Kind      string   `json:"kind"`
	Role      string   `json:"role,omitempty"`
	Repo      string   `json:"repo,omitempty"`
	DependsOn []string `json:"depends_on,omitempty"`
	Files     []string `json:"files,omitempty"`
	Prompt    string   `json:"prompt,omitempty"` // in a plan waiting for approval
	Status    string   `json:"status,omitempty"` // pending, running, done, failed
	Result    string   `json:"result,omitempty"`
}

// ChangesView is one agent's changes waiting for review.
type ChangesView struct {
	Step      string     `json:"step"`
	Title     string     `json:"title"`
	Summary   string     `json:"summary,omitempty"`
	Round     int        `json:"round"`
	Files     []FileView `json:"files"`
	Truncated bool       `json:"truncated,omitempty"` // patches were shortened
}

// FileView is one changed file.
type FileView struct {
	Path    string `json:"path"`
	Status  string `json:"status"` // A, M, D
	Added   int    `json:"added,omitempty"`
	Deleted int    `json:"deleted,omitempty"`
	Binary  bool   `json:"binary,omitempty"`
	Patch   string `json:"patch,omitempty"`
}

// CostView is what a task used.
type CostView struct {
	FreshTokens int64   `json:"fresh_tokens"`
	USD         float64 `json:"usd_api_equivalent,omitempty"`
	Line        string  `json:"line,omitempty"`
	BudgetUSD   float64 `json:"budget_usd,omitempty"`
	BudgetTok   int64   `json:"budget_tokens,omitempty"`
}

// LimitView is a provider at or near its usage limit.
type LimitView struct {
	Provider     string `json:"provider"`
	LimitedUntil string `json:"limited_until,omitempty"`
	Used         string `json:"used,omitempty"` // share of the subscription window, as the CLI reports it
}

// Patch limits for task_status: enough to judge a change, small enough
// for the calling agent's context.
const (
	maxFilePatch  = 6 << 10
	maxTotalPatch = 40 << 10
)

// reTaskID matches the task ids rw writes (no path separators).
var reTaskID = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,200}$`)

func validID(id string) bool { return reTaskID.MatchString(id) && !strings.Contains(id, "..") }

// loadHere reads a task state of this folder. Tasks of other folders are
// not found: the server works only where it was started.
func (e *Engine) loadHere(id string) (*orchestrator.TaskState, error) {
	if !validID(id) {
		return nil, fmt.Errorf("invalid task id %q", oneLine(id, 60))
	}
	st, err := orchestrator.LoadTask(id)
	if err != nil {
		return nil, fmt.Errorf("no task %q here (list_tasks lists them)", id)
	}
	if !sameDir(st.Dir, e.opt.Dir) {
		return nil, fmt.Errorf("no task %q here (list_tasks lists them)", id)
	}
	return st, nil
}

func sameDir(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// current returns the id of the newest job, for calls that leave it out.
func (e *Engine) current(id string) (string, error) {
	if id != "" {
		return id, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cur != nil && e.cur.id != "" {
		return e.cur.id, nil
	}
	for i := len(e.jobs) - 1; i >= 0; i-- {
		if e.jobs[i].id != "" {
			return e.jobs[i].id, nil
		}
	}
	return "", errors.New("no task yet in this session: give task_id (list_tasks lists them)")
}

// maxWait bounds task_status's wait: MCP clients time tool calls out
// (Codex after 60s by default).
const maxWait = 50 * time.Second

// Status describes a task. wait > 0 first waits (at most maxWait) until
// the task ends or needs an answer; progress, when set, is called every
// few seconds meanwhile.
func (e *Engine) Status(ctx context.Context, id string, wait time.Duration, progress func(msg string)) (Status, error) {
	id, err := e.current(id)
	if err != nil {
		return Status{}, err
	}
	if j := e.find(id); j != nil && wait > 0 {
		e.wait(ctx, j, min(wait, maxWait), progress)
	}
	if j := e.find(id); j != nil {
		return e.jobStatus(j), nil
	}
	st, err := e.loadHere(id)
	if err != nil {
		return Status{}, err
	}
	return e.stateStatus(st), nil
}

// wait returns when j ends, when it waits for an answer (or another one),
// when d has passed or when ctx ends.
func (e *Engine) wait(ctx context.Context, j *job, d time.Duration, progress func(string)) {
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	start := e.pendingKey(j)
	lastProgress := time.Now()
	for {
		select {
		case <-j.done:
			return
		case <-deadline.C:
			return
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if k := e.pendingKey(j); k != start && k != "" {
			return
		}
		if progress != nil && time.Since(lastProgress) >= 5*time.Second {
			lastProgress = time.Now()
			e.mu.Lock()
			msg := j.status
			if j.phase != "" {
				msg += ": " + j.phase
			}
			e.mu.Unlock()
			progress(msg)
		}
	}
}

// pendingKey names the questions j waits on ("" = none).
func (e *Engine) pendingKey(j *job) string {
	if !e.isCurrent(j) {
		return ""
	}
	var ids []string
	for _, r := range e.ap.Pending() {
		ids = append(ids, r.ID)
	}
	return strings.Join(ids, ",")
}

func (e *Engine) isCurrent(j *job) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cur == j
}

func (e *Engine) jobStatus(j *job) Status {
	e.mu.Lock()
	s := Status{TaskID: j.id, Kind: j.kind, Task: oneLine(j.prompt, 300), Status: j.status, Phase: j.phase,
		Recent: append([]string(nil), j.recent...), Summary: j.summary}
	end := j.ended
	if end.IsZero() {
		end = time.Now()
	}
	s.Elapsed = end.Sub(j.started).Round(time.Second).String()
	running := j.ended.IsZero()
	for _, id := range j.order {
		if a := j.agents[id]; a != nil && (a.State == "running" || a.State == "queued") && running {
			s.Agents = append(s.Agents, *a)
		}
	}
	cost := j.cost
	current := e.cur == j
	e.mu.Unlock()

	if j.kind == KindTask || j.kind == KindResume {
		if st, err := orchestrator.LoadTask(j.id); err == nil {
			s.Plan = planView(st)
		}
	}
	if current {
		for _, r := range e.ap.Pending() {
			s.Waiting = append(s.Waiting, waitingView(r))
		}
		if len(s.Waiting) > 0 {
			s.Status = "waiting"
		}
		b := e.orc.BudgetStatus()
		s.Cost = &CostView{FreshTokens: b.TaskTokens, USD: round2(b.TaskUSD), BudgetUSD: b.Limits.TaskUSD, BudgetTok: b.Limits.TaskTokens}
	} else if running {
		s.Cost = &CostView{}
	} else {
		s.Cost = costView(cost)
	}
	s.Limits = e.limits()
	s.Next = next(s)
	return s
}

// stateStatus describes a task of the history that this process did not
// start (an earlier rw mcp, rw run, rw web).
func (e *Engine) stateStatus(st *orchestrator.TaskState) Status {
	s := Status{TaskID: st.ID, Kind: KindTask, Task: oneLine(st.Task, 300), Status: st.Status, Phase: st.Phase,
		Plan: planView(st), Summary: st.Summary}
	if st.Status == "running" {
		if st.Interrupted() {
			s.Status = "interrupted"
		} else {
			s.Status = "running elsewhere"
		}
	}
	if st.CostLine != "" {
		s.Cost = &CostView{Line: st.CostLine}
	}
	s.Limits = e.limits()
	s.Next = next(s)
	return s
}

// next tells the calling agent what to do now.
func next(s Status) string {
	switch s.Status {
	case "starting", "running":
		n := "still working: call task_status again with wait_seconds (up to 50) instead of polling quickly"
		if lim := limitedNames(s.Limits); lim != "" {
			n += "; " + lim + " is at its usage limit, so the task may wait or switch providers"
		}
		return n
	case "waiting":
		return "the task waits for you: answer each item in waiting (approve_plan or edit_plan for a plan, apply or reject for changes)"
	case "done":
		return "finished: task_result has the summary, the diff stat, the checks and the report path; undo reverts it"
	case "failed":
		return "failed: task_result has the details; resume_task runs the steps that did not succeed"
	case "cancelled":
		return "cancelled: resume_task continues it (finished steps are kept)"
	case "interrupted":
		return "interrupted (its rw stopped): resume_task continues it"
	case "running elsewhere":
		return "this task runs in another rw process; its status here updates after each step"
	}
	return ""
}

func limitedNames(ls []LimitView) string {
	var out []string
	for _, l := range ls {
		if l.LimitedUntil != "" {
			out = append(out, l.Provider)
		}
	}
	return strings.Join(out, " and ")
}

// limits lists the providers at their limit or with a reported share of
// their subscription window.
func (e *Engine) limits() []LimitView {
	cfg := e.store.Get()
	tr := e.orc.Tracker()
	var out []LimitView
	for _, p := range cfg.ProviderNames() {
		if cfg.Providers[p].Disabled {
			continue
		}
		st := tr.Snapshot(p)
		var l LimitView
		if st.Limited(time.Now()) {
			l.LimitedUntil = st.LimitedUntil.Format(time.RFC3339)
		}
		if u, ok := tr.Utilization(p); ok && st.Quota != nil {
			l.Used = fmt.Sprintf("%.0f%%", u*100)
			if st.Quota.Window != "" {
				l.Used += " of the " + st.Quota.Window + " window"
			}
		}
		if l.LimitedUntil != "" || l.Used != "" {
			l.Provider = p
			out = append(out, l)
		}
	}
	return out
}

func planView(st *orchestrator.TaskState) *PlanView {
	if st == nil || st.Plan == nil {
		return nil
	}
	v := &PlanView{Summary: oneLine(st.Plan.Summary, 400), Steps: []StepView{}}
	for _, t := range st.Plan.Subtasks {
		sv := stepView(t, false)
		sv.Status = "pending"
		if r, ok := st.Results[t.ID]; ok {
			sv.Status = map[bool]string{true: "done", false: "failed"}[r.OK]
			sv.Result = oneLine(r.Final, 300)
			if !r.OK && r.Err != "" {
				sv.Result = oneLine(r.Err, 300)
			}
		} else if _, ok := st.Running[t.ID]; ok && st.Status == "running" {
			sv.Status = "running"
		}
		v.Steps = append(v.Steps, sv)
	}
	return v
}

func stepView(t orchestrator.Subtask, prompt bool) StepView {
	sv := StepView{ID: t.ID, Title: t.Title, Kind: string(t.Kind), Role: t.Role, Repo: t.Repo, DependsOn: t.DependsOn, Files: t.Files}
	if prompt {
		sv.Prompt = t.Prompt
	}
	return sv
}

func waitingView(r *web.Request) Waiting {
	w := Waiting{RequestID: r.ID, Type: r.Type}
	switch r.Type {
	case "plan":
		w.Answer = "approve_plan (approve or reject it as is) or edit_plan (run your edited plan)"
		if r.Plan != nil {
			pv := &PlanView{Summary: r.Plan.Summary, Steps: []StepView{}}
			for _, t := range r.Plan.Subtasks {
				pv.Steps = append(pv.Steps, stepView(t, true))
			}
			w.Plan = pv
		}
		if r.Estimate != nil {
			w.Estimate = oneLine(r.Estimate.TotalLine(), 200)
		}
	case "changes":
		w.Answer = "apply (all files, or the ones you name) or reject (with feedback: the agent tries again; without: nothing lands, the work is kept on a branch)"
		if c := r.Changes; c != nil {
			cv := &ChangesView{Step: c.StepID, Title: c.Title, Summary: oneLine(c.Summary, 600), Round: c.Round, Files: []FileView{}}
			total := 0
			for _, f := range c.Files {
				fv := FileView{Path: f.Path, Status: f.Status, Added: f.Added, Deleted: f.Deleted, Binary: f.Binary}
				if room := min(maxFilePatch, maxTotalPatch-total); room > 0 {
					fv.Patch = clipBytes(f.Patch, room)
					cv.Truncated = cv.Truncated || len(f.Patch) > room
					total += len(fv.Patch)
				} else if f.Patch != "" {
					cv.Truncated = true
				}
				cv.Files = append(cv.Files, fv)
			}
			w.Changes = cv
		}
	default:
		w.Answer = "not answerable here"
	}
	return w
}

func costView(c event.TaskCost) *CostView {
	v := &CostView{USD: round2(c.CostUSD), Line: c.Summary()}
	for _, u := range c.PerProvider {
		v.FreshTokens += u.Total()
	}
	return v
}

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

// --- list --------------------------------------------------------------------

// ListRow is one task in list_tasks.
type ListRow struct {
	TaskID  string `json:"task_id"`
	Kind    string `json:"kind"`
	Status  string `json:"status"`
	Task    string `json:"task"`
	Created string `json:"created"`
	Summary string `json:"summary,omitempty"`
}

// List returns this session's tasks, then this folder's history, newest
// first.
func (e *Engine) List(n int) []ListRow {
	if n <= 0 || n > 50 {
		n = 10
	}
	rows := []ListRow{}
	seen := map[string]bool{}
	e.mu.Lock()
	for i := len(e.jobs) - 1; i >= 0 && len(rows) < n; i-- {
		j := e.jobs[i]
		if j.id == "" {
			continue
		}
		seen[j.id] = true
		rows = append(rows, ListRow{TaskID: j.id, Kind: j.kind, Status: j.status, Task: oneLine(j.prompt, 120),
			Created: j.started.Format(time.RFC3339), Summary: oneLine(j.summary, 160)})
	}
	e.mu.Unlock()
	for _, st := range orchestrator.History(e.opt.Dir, n) {
		if len(rows) >= n {
			break
		}
		if seen[st.ID] {
			continue
		}
		status := st.Status
		if status == "running" {
			status = "running elsewhere"
			if st.Interrupted() {
				status = "interrupted"
			}
		}
		rows = append(rows, ListRow{TaskID: st.ID, Kind: KindTask, Status: status, Task: oneLine(st.Task, 120),
			Created: st.Created.Format(time.RFC3339), Summary: oneLine(st.Summary, 160)})
	}
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].Created > rows[b].Created })
	return rows
}

// --- result ------------------------------------------------------------------

// Result is what task_result returns.
type Result struct {
	TaskID   string      `json:"task_id"`
	Kind     string      `json:"kind"`
	Task     string      `json:"task"`
	Status   string      `json:"status"`
	Summary  string      `json:"summary"`
	Answer   string      `json:"answer,omitempty"` // a read-only task's answer
	Steps    []StepView  `json:"steps,omitempty"`
	Diff     *DiffView   `json:"diff,omitempty"`
	Checks   []CheckView `json:"checks,omitempty"`
	Cost     *CostView   `json:"cost,omitempty"`
	UndoKey  string      `json:"undo_key,omitempty"`
	Report   string      `json:"report,omitempty"` // a Markdown report of the task
	FollowUp []AgentRef  `json:"follow_up_agents,omitempty"`
	Next     string      `json:"next"`
}

// DiffView is the diff stat of a task.
type DiffView struct {
	Files   []FileView `json:"files"`
	Added   int        `json:"added"`
	Deleted int        `json:"deleted"`
	Undone  bool       `json:"undone,omitempty"`
	More    int        `json:"more_files,omitempty"`
}

// CheckView is one verify check (or hook) the task ran.
type CheckView struct {
	Kind    string `json:"kind"` // verify or hook
	Command string `json:"command"`
	OK      bool   `json:"ok"`
	Tests   string `json:"tests,omitempty"`
}

// AgentRef is an agent that takes a follow-up.
type AgentRef struct {
	Agent string `json:"agent"`
	Role  string `json:"role,omitempty"`
	Step  string `json:"step,omitempty"`
}

const (
	maxAnswer    = 30 << 10
	maxDiffFiles = 100
)

// Result describes a finished task.
func (e *Engine) Result(id string) (Result, error) {
	id, err := e.current(id)
	if err != nil {
		return Result{}, err
	}
	var st *orchestrator.TaskState
	r := Result{TaskID: id}
	if j := e.find(id); j != nil {
		e.mu.Lock()
		ended := !j.ended.IsZero()
		r.Kind, r.Task, r.Status, r.Summary, r.UndoKey = j.kind, oneLine(j.prompt, 300), j.status, j.summary, j.undoKey
		answer, cost := j.answer, j.cost
		e.mu.Unlock()
		if !ended {
			return Result{}, fmt.Errorf("task %s is still %s: task_status follows it", id, r.Status)
		}
		r.Answer = clipBytes(answer, maxAnswer)
		r.Cost = costView(cost)
		if r.Kind == KindTask || r.Kind == KindResume {
			st, _ = orchestrator.LoadTask(id)
		}
	} else {
		if st, err = e.loadHere(id); err != nil {
			return Result{}, err
		}
		if st.Status == "running" {
			return Result{}, fmt.Errorf("task %s has not finished (status: running): resume_task continues it if it was interrupted", id)
		}
		r.Kind, r.Task, r.Status, r.Summary, r.UndoKey = KindTask, oneLine(st.Task, 300), st.Status, st.Summary, st.UndoKey
		r.Cost = &CostView{Line: st.CostLine}
	}
	if st != nil {
		data := report.Build(st, report.Options{SessionDir: e.opt.SessionDir, Version: e.opt.Version})
		for _, s := range data.Steps {
			r.Steps = append(r.Steps, StepView{ID: s.ID, Title: s.Title, Kind: s.Kind, Role: s.Role, Status: s.Result,
				Result: oneLine(firstNonEmpty(s.Err, s.Final), 600)})
		}
		if d := data.Diff; d != nil {
			r.Diff = &DiffView{Files: []FileView{}, Added: d.Add, Deleted: d.Del, Undone: d.Undone}
			for i, f := range d.Files {
				if i == maxDiffFiles {
					r.Diff.More = len(d.Files) - i
					break
				}
				r.Diff.Files = append(r.Diff.Files, FileView{Path: f.Path, Status: f.Status, Added: f.Add, Deleted: f.Del, Binary: f.Binary})
			}
		}
		for _, c := range data.Checks {
			r.Checks = append(r.Checks, CheckView{Kind: c.Kind, Command: c.Command, OK: c.OK, Tests: c.Tests()})
		}
		if r.UndoKey == "" {
			r.UndoKey = st.UndoKey
		}
		if p, err := e.writeReport(data); err == nil {
			r.Report = p
		}
	} else if r.Kind == KindFollowUp && r.UndoKey != "" {
		if plan, err := orchestrator.PreviewUndo(e.opt.Dir, r.UndoKey, false); err == nil {
			r.Diff = &DiffView{Files: []FileView{}}
			for _, c := range plan.Changes {
				status, path, _ := strings.Cut(c, " ")
				r.Diff.Files = append(r.Diff.Files, FileView{Path: path, Status: status})
			}
		}
	}
	for _, s := range e.orc.Sessions() {
		if len(r.FollowUp) == 5 {
			break
		}
		r.FollowUp = append(r.FollowUp, AgentRef{Agent: s.AgentID, Role: s.Role, Step: oneLine(s.Title, 80)})
	}
	switch {
	case r.Kind == KindReadOnly:
		r.Next = "the answer is in answer"
	case r.UndoKey != "" && r.Diff != nil && len(r.Diff.Files) > 0:
		r.Next = "the changes are in the working tree; undo reverts them, follow_up asks an agent for more"
	default:
		r.Next = "follow_up asks an agent for more"
	}
	return r, nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// writeReport writes the task's Markdown report (like rw report --md).
func (e *Engine) writeReport(d *report.Data) (string, error) {
	if e.opt.ReportDir == "" {
		return "", errors.New("no report folder")
	}
	var b strings.Builder
	if err := d.Markdown(&b); err != nil {
		return "", err
	}
	if err := os.MkdirAll(e.opt.ReportDir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(e.opt.ReportDir, unsafeName.ReplaceAllString(d.ID, "_")+".md")
	return p, os.WriteFile(p, []byte(b.String()), 0o644)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "\n[... shortened]"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
