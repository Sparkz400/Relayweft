package mcpserve

import (
	"context"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sparkz400/relayweft/internal/diag"
)

// Instructions tell the calling agent how to use the tools (MCP clients
// show them to the model).
const Instructions = `Relayweft (rw) runs a multi-step coding task with its own planner, worker and reviewer agents (Claude Code, Codex and other CLIs) in this repository, and lands their changes in the working tree.

Workflow: run_task returns a task_id at once; tasks take minutes. Follow it with task_status and wait_seconds (up to 50: it returns early when the task ends or needs an answer). When status is "waiting", answer each item: approve_plan or edit_plan for a plan, apply or reject for changes. When it is done, task_result has the summary, the diff stat, the checks and a report path; undo reverts the task's changes. One task runs at a time.

rw works only in the folder it was started in. Its agents may use the same subscription as you (the same Claude account): while a task runs, avoid heavy work of your own, and if task_status reports a provider at its usage limit, tell the user instead of retrying.`

// NewServer returns the MCP server for an engine.
func NewServer(e *Engine, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "relayweft", Title: "Relayweft", Version: version},
		&mcp.ServerOptions{Instructions: Instructions})
	addTools(s, e)
	return s
}

func ro(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: boolPtr(false)}
}

func act(title string, destructive bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: boolPtr(destructive), OpenWorldHint: boolPtr(false)}
}

func boolPtr(b bool) *bool { return &b }

// RunTaskIn are run_task's arguments.
type RunTaskIn struct {
	Prompt        string  `json:"prompt" jsonschema:"the task, as you would type it for a person: what to change and how to check it"`
	ApprovePlan   *bool   `json:"approve_plan,omitempty" jsonschema:"true: wait for approve_plan or edit_plan before any agent runs (default: the user's rw config)"`
	ReviewChanges *bool   `json:"review_changes,omitempty" jsonschema:"true: wait for apply or reject before each agent's changes land (default: the user's rw config)"`
	ReadOnly      bool    `json:"read_only,omitempty" jsonschema:"true: one read-only agent answers the prompt (explain, find, review); nothing is changed"`
	BudgetUSD     float64 `json:"budget_usd,omitempty" jsonschema:"stop the task at this API-equivalent cost in USD; can only lower the user's budget"`
	BudgetTokens  int64   `json:"budget_tokens,omitempty" jsonschema:"stop the task at this many fresh tokens; can only lower the user's budget"`
	Workflow      string  `json:"workflow,omitempty" jsonschema:"run the prompt as the task of this saved workflow (rw workflow lists them): its checks, budget caps and approvals apply, and approve_plan or review_changes false cannot drop its approvals"`
}

// TaskIn names a task.
type TaskIn struct {
	TaskID string `json:"task_id" jsonschema:"the id run_task or list_tasks returned"`
}

// StatusIn are task_status's arguments.
type StatusIn struct {
	TaskID      string `json:"task_id,omitempty" jsonschema:"the task (default: the newest task of this session)"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"wait up to this many seconds (at most 50) for the task to end or need an answer"`
}

// ResultIn are task_result's arguments.
type ResultIn struct {
	TaskID string `json:"task_id,omitempty" jsonschema:"the task (default: the newest task of this session)"`
}

// ListIn are list_tasks' arguments.
type ListIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"how many tasks (default 10, at most 50)"`
}

// FollowUpIn are follow_up's arguments.
type FollowUpIn struct {
	Message string `json:"message" jsonschema:"what to tell the agent"`
	Agent   string `json:"agent,omitempty" jsonschema:"the agent id (task_status agents, task_result follow_up_agents); empty: the newest finished agent"`
}

// ApprovePlanIn are approve_plan's arguments.
type ApprovePlanIn struct {
	TaskID    string `json:"task_id"`
	RequestID string `json:"request_id,omitempty" jsonschema:"the waiting item's request_id (needed only when several wait)"`
	Approve   *bool  `json:"approve,omitempty" jsonschema:"false rejects the plan and cancels the task (default true)"`
}

// EditPlanIn are edit_plan's arguments.
type EditPlanIn struct {
	TaskID    string    `json:"task_id"`
	RequestID string    `json:"request_id,omitempty" jsonschema:"the waiting item's request_id (needed only when several wait)"`
	Plan      PlanInput `json:"plan" jsonschema:"the whole plan to run instead (start from the waiting plan's steps)"`
}

// ApplyIn are apply's arguments.
type ApplyIn struct {
	TaskID    string   `json:"task_id"`
	RequestID string   `json:"request_id,omitempty" jsonschema:"the waiting item's request_id (needed only when several wait)"`
	Files     []string `json:"files,omitempty" jsonschema:"apply only these changed files (default: all); the rest is kept on a branch"`
}

// RejectIn are reject's arguments.
type RejectIn struct {
	TaskID    string `json:"task_id"`
	RequestID string `json:"request_id,omitempty" jsonschema:"the waiting item's request_id (needed only when several wait)"`
	Feedback  string `json:"feedback,omitempty" jsonschema:"what to fix: the agent tries again and its changes are shown again. Empty: reject them all"`
}

// UndoIn are undo's arguments.
type UndoIn struct {
	TaskID   string `json:"task_id"`
	Redo     bool   `json:"redo,omitempty" jsonschema:"true: put an undone task's changes back"`
	AllFiles bool   `json:"all_files,omitempty" jsonschema:"true: also revert files no agent reported changing (by default they are left alone: they may be your own edits)"`
}

// Message is a tool's plain answer.
type Message struct {
	TaskID  string `json:"task_id,omitempty"`
	Message string `json:"message"`
}

func addTools(s *mcp.Server, e *Engine) {
	mcp.AddTool(s, &mcp.Tool{Name: "run_task", Annotations: act("Run a task", false),
		Description: "Start a multi-step coding task in this repository (planner, workers, reviewer). Returns task_id at once; follow it with task_status. One task at a time."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in RunTaskIn) (*mcp.CallToolResult, any, error) {
			id, err := e.Run(ctx, RunOptions(in))
			if err != nil {
				return nil, nil, err
			}
			return nil, Message{TaskID: id, Message: "started: follow it with task_status (wait_seconds up to 50)"}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "task_status", Annotations: ro("Task status"),
		Description: "A task's status: phase, plan with each step's progress, running agents, cost so far, provider limits, recent activity, and what it waits for (status \"waiting\"). wait_seconds waits for the end or a question."},
		func(ctx context.Context, req *mcp.CallToolRequest, in StatusIn) (*mcp.CallToolResult, any, error) {
			var progress func(string)
			if tok := req.Params.GetProgressToken(); tok != nil && req.Session != nil {
				n := 0.0
				progress = func(msg string) {
					n++
					if err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: tok, Progress: n, Message: msg}); err != nil {
						diag.Logf("mcp: progress note not sent: %v", err) // the status itself still comes
					}
				}
			}
			wait := time.Duration(max(0, in.WaitSeconds)) * time.Second
			st, err := e.Status(ctx, in.TaskID, wait, progress)
			if err != nil {
				return nil, nil, err
			}
			return nil, st, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "task_result", Annotations: ro("Task result"),
		Description: "A finished task's result: summary (or a read-only task's answer), steps, diff stat, checks, cost, undo key, a Markdown report path, and agents that take a follow_up."},
		func(_ context.Context, _ *mcp.CallToolRequest, in ResultIn) (*mcp.CallToolResult, any, error) {
			r, err := e.Result(in.TaskID)
			if err != nil {
				return nil, nil, err
			}
			return nil, r, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "list_tasks", Annotations: ro("List tasks"),
		Description: "This session's tasks and this repository's task history (also tasks of an earlier rw mcp or rw run), newest first."},
		func(_ context.Context, _ *mcp.CallToolRequest, in ListIn) (*mcp.CallToolResult, any, error) {
			return nil, map[string]any{"tasks": e.List(in.Limit)}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "cancel_task", Annotations: act("Cancel a task", true),
		Description: "Stop the running task and its agents. Finished steps stay in the working tree; resume_task continues it."},
		func(_ context.Context, _ *mcp.CallToolRequest, in TaskIn) (*mcp.CallToolResult, any, error) {
			return message(in.TaskID)(e.Cancel(in.TaskID))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "resume_task", Annotations: act("Resume a task", false),
		Description: "Continue an interrupted, failed or cancelled task of this repository: finished steps are skipped, an interrupted agent continues its own session."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in TaskIn) (*mcp.CallToolResult, any, error) {
			id, err := e.Resume(ctx, in.TaskID)
			if err != nil {
				return nil, nil, err
			}
			return nil, Message{TaskID: id, Message: "resuming: follow it with task_status"}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "follow_up", Annotations: act("Follow up with an agent", false),
		Description: "Send a message to one of rw's agents. A running agent gets it when its current turn ends; a finished one continues its own session as a new task (returns task_id)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in FollowUpIn) (*mcp.CallToolResult, any, error) {
			id, told, err := e.FollowUp(ctx, in.Agent, in.Message)
			if err != nil {
				return nil, nil, err
			}
			if told != "" {
				return nil, Message{Message: told}, nil
			}
			return nil, Message{TaskID: id, Message: "follow-up started: follow it with task_status"}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "approve_plan", Annotations: act("Approve the plan", false),
		Description: "Answer a waiting plan: approve it as it is (default), or reject it (approve: false), which cancels the task."},
		func(_ context.Context, _ *mcp.CallToolRequest, in ApprovePlanIn) (*mcp.CallToolResult, any, error) {
			return message(in.TaskID)(e.ApprovePlan(in.TaskID, in.RequestID, pick(in.Approve, true)))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "edit_plan", Annotations: act("Edit and approve the plan", false),
		Description: "Answer a waiting plan with an edited one, which then runs: change, add, remove or reorder steps (start from the steps task_status shows)."},
		func(_ context.Context, _ *mcp.CallToolRequest, in EditPlanIn) (*mcp.CallToolResult, any, error) {
			return message(in.TaskID)(e.EditPlan(in.TaskID, in.RequestID, in.Plan))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "apply", Annotations: act("Apply changes", false),
		Description: "Answer a waiting change review: land the agent's changes in the working tree (all files, or the ones named)."},
		func(_ context.Context, _ *mcp.CallToolRequest, in ApplyIn) (*mcp.CallToolResult, any, error) {
			return message(in.TaskID)(e.Apply(in.TaskID, in.RequestID, in.Files))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "reject", Annotations: act("Reject changes", false),
		Description: "Answer a waiting change review: send the changes back with feedback (the agent tries again), or reject them all (nothing lands; the work is kept on a branch)."},
		func(_ context.Context, _ *mcp.CallToolRequest, in RejectIn) (*mcp.CallToolResult, any, error) {
			return message(in.TaskID)(e.Reject(in.TaskID, in.RequestID, in.Feedback))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "undo", Annotations: act("Undo a task", true),
		Description: "Revert a finished task's changes in the working tree (redo: true puts them back). Edits made since are kept by a 3-way merge; if one conflicts, nothing changes."},
		func(_ context.Context, _ *mcp.CallToolRequest, in UndoIn) (*mcp.CallToolResult, any, error) {
			return message(in.TaskID)(e.Undo(in.TaskID, in.Redo, in.AllFiles))
		})
}

// message turns a (text, error) answer into a tool result.
func message(id string) func(string, error) (*mcp.CallToolResult, any, error) {
	return func(msg string, err error) (*mcp.CallToolResult, any, error) {
		if err != nil {
			return nil, nil, err
		}
		return nil, Message{TaskID: id, Message: msg}, nil
	}
}

// NewNestedServer is the server of an rw mcp that runs under one of rw's
// own agents: it offers no tools, and every tool call fails with why.
func NewNestedServer(why, version string) *mcp.Server {
	refusal := "rw mcp refuses here: " + why + ". A task's agent must not start rw tasks of its own (that would recurse). Do the work yourself."
	s := mcp.NewServer(&mcp.Implementation{Name: "relayweft", Title: "Relayweft", Version: version},
		&mcp.ServerOptions{Instructions: refusal})
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/call" {
				res := &mcp.CallToolResult{}
				res.SetError(errors.New(refusal))
				return res, nil
			}
			return next(ctx, method, req)
		}
	})
	return s
}
