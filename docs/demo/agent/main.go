// Command agent is the scripted stand-in for the `claude` and `codex` CLIs
// in the recorded demo (docs/demo). It speaks enough of
// `claude -p --output-format stream-json` and `codex exec --json` for rw:
// a fixed plan, approving reviews, and steps that really edit the sample
// project, so plan approval, worktrees, change review and undo all run for
// real. It uses no network and no quota.
//
//	agent setup <dir>     write the sample project into dir
//	claude ... / codex ...   (the binary copied or linked under those names)
//
// RW_DEMO_AS (claude or codex) overrides the name it was started as, and
// RW_DEMO_SPEED scales every delay (2 = twice as fast, 0 = no delays).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/runner"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "setup" {
		if err := setup(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "setup:", err)
			os.Exit(1)
		}
		return
	}
	as := os.Getenv("RW_DEMO_AS")
	if as == "" {
		as = strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	}
	if as != "codex" {
		as = "claude"
	}
	if quickCheck(as, os.Args[1:]) {
		return
	}
	in, _ := io.ReadAll(os.Stdin)
	a := &agent{as: as, prompt: string(in), out: json.NewEncoder(os.Stdout)}
	a.wd, _ = os.Getwd()
	a.run()
}

// quickCheck answers the version and login checks of rw doctor and rw
// setup as the tested, logged-in CLI.
func quickCheck(as string, args []string) bool {
	switch strings.Join(args, " ") {
	case "--version":
		fmt.Println(config.Default().Providers[as].TestedVersion)
	case "auth status":
		fmt.Println(`{"loggedIn": true, "authMethod": "claude.ai", "subscriptionType": "demo"}`)
	case "login status":
		fmt.Println("Logged in using ChatGPT")
	default:
		return false
	}
	return true
}

type agent struct {
	as     string
	prompt string
	wd     string
	out    *json.Encoder
	n      int // item counter (codex)
}

func speed() float64 {
	if s, err := strconv.ParseFloat(os.Getenv("RW_DEMO_SPEED"), 64); err == nil && s >= 0 {
		return s
	}
	return 1
}

func pause(ms int) {
	if s := speed(); s > 0 {
		time.Sleep(time.Duration(float64(ms)/s) * time.Millisecond)
	}
}

func (a *agent) emit(v map[string]any) { _ = a.out.Encode(v) }

func (a *agent) start(sid string) {
	if a.as == "codex" {
		a.emit(map[string]any{"type": "thread.started", "thread_id": sid})
		a.emit(map[string]any{"type": "turn.started"})
		return
	}
	a.emit(map[string]any{"type": "system", "subtype": "init", "session_id": sid, "model": "demo", "cwd": a.wd})
}

func (a *agent) item(v map[string]any) {
	a.n++
	v["id"] = fmt.Sprintf("item_%d", a.n)
	a.emit(map[string]any{"type": "item.completed", "item": v})
}

func (a *agent) say(text string) {
	if a.as == "codex" {
		a.item(map[string]any{"type": "reasoning", "text": text})
		return
	}
	a.emit(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "text", "text": text}}}})
}

// tool shows one tool call: a Claude tool (name, input) or, for Codex, a
// shell command.
func (a *agent) tool(name string, input map[string]any, cmd string) {
	if a.as == "codex" {
		a.n++
		it := map[string]any{"id": fmt.Sprintf("item_%d", a.n), "type": "command_execution", "command": cmd, "aggregated_output": "", "status": "in_progress"}
		a.emit(map[string]any{"type": "item.started", "item": it})
		zero := 0
		it["exit_code"], it["status"] = &zero, "completed"
		a.emit(map[string]any{"type": "item.completed", "item": it})
		return
	}
	a.emit(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_%d", a.n), "name": name, "input": input}}}})
}

// write replaces a file of the sample project and reports it the way the
// real CLIs do (absolute paths).
func (a *agent) write(rel, content string) {
	p := filepath.Join(a.wd, filepath.FromSlash(rel))
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		a.fail(err)
	}
	if a.as == "codex" {
		a.item(map[string]any{"type": "file_change", "status": "completed",
			"changes": []any{map[string]any{"path": p, "kind": "update"}}})
		return
	}
	a.emit(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_w%d", a.n), "name": "Edit", "input": map[string]any{"file_path": p}}}}})
}

// finish ends the run with the agent's answer and a plausible token count
// (Claude Code also reports its API-equivalent price).
func (a *agent) finish(sid, text string, in, out int, usd float64) {
	if a.as == "codex" {
		a.item(map[string]any{"type": "agent_message", "text": text})
		a.emit(map[string]any{"type": "turn.completed", "usage": map[string]any{
			"input_tokens": in, "cached_input_tokens": in / 2, "output_tokens": out, "reasoning_output_tokens": out / 3}})
		return
	}
	a.emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text, "session_id": sid,
		"total_cost_usd": usd, "usage": map[string]any{"input_tokens": in, "output_tokens": out}})
}

func (a *agent) fail(err error) {
	if a.as == "codex" {
		a.emit(map[string]any{"type": "turn.failed", "error": map[string]any{"message": err.Error()}})
	} else {
		a.emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": err.Error()})
	}
	os.Exit(1)
}

// subtask is the step's own prompt: the text after "YOUR SUBTASK" up to the
// first blank line (rw adds file lists and earlier results after it).
func subtask(prompt string) string {
	_, s, _ := strings.Cut(prompt, "YOUR SUBTASK ")
	s, _, _ = strings.Cut(s, "\n\n")
	return s
}

func verdict(advice string) string {
	b, _ := json.Marshal(map[string]any{"approve": true, "advice": advice, "issues": []string{}})
	return "```json\n" + string(b) + "\n```"
}

func (a *agent) run() {
	sid := fmt.Sprintf("demo-%s-%d", a.as, os.Getpid())
	a.start(sid)
	p := a.prompt
	step := func(id string) bool { return strings.Contains(p, fmt.Sprintf("YOUR SUBTASK %q", id)) }
	switch {
	case strings.Contains(p, runner.MarkerJudge):
		a.finish(sid, "A", 900, 5, 0.001)
	case strings.Contains(p, runner.MarkerPlanReview):
		pause(500)
		a.tool("Read", map[string]any{"file_path": "parse.go"}, "sed -n 1,40p parse.go")
		pause(700)
		a.finish(sid, verdict("Sound plan. The two edits touch different files, so they can run in parallel."), 6200, 140, 0.031)
	case strings.Contains(p, runner.MarkerFinalReview), strings.Contains(p, runner.MarkerErrorReview):
		pause(600)
		a.tool("Read", map[string]any{"file_path": "parse.go"}, "git diff --stat")
		pause(800)
		a.finish(sid, verdict("Trailing empty fields are kept, --strict stops on the first bad line, and go test passes."), 9800, 210, 0.052)
	case strings.Contains(p, runner.MarkerPlan):
		pause(500)
		a.tool("Glob", map[string]any{"pattern": "**/*.go"}, "rg --files")
		pause(500)
		a.tool("Read", map[string]any{"file_path": "parse.go"}, "sed -n 1,60p parse.go")
		pause(600)
		a.finish(sid, "```json\n"+planJSON+"\n```", 7400, 520, 0.044)
	case step("map"):
		pause(400)
		a.tool("Grep", map[string]any{"pattern": "splitFields"}, "rg -n splitFields")
		pause(700)
		a.tool("Read", map[string]any{"file_path": "main.go"}, "sed -n 1,40p main.go")
		pause(600)
		a.finish(sid, "parse.go: splitFields trims the trailing separator, so \"pears,4,\" loses its last field. main.go skips lines that do not parse.", 4100, 180, 0.006)
	case step("fields"):
		a.say("Removing the TrimRight in splitFields and adding a test.")
		pause(900)
		a.tool("Read", map[string]any{"file_path": "parse.go"}, "sed -n 1,50p parse.go")
		pause(900)
		a.write("parse.go", parseAfter)
		pause(700)
		a.write("parse_test.go", parseTestAfter)
		pause(900)
		a.tool("Bash", map[string]any{"command": "go test ./..."}, "go test ./...")
		pause(600)
		a.finish(sid, "splitFields keeps trailing empty fields; added TestSplitFieldsKeepsTrailingEmpty. go test passes.", 18200, 1400, 0.11)
	case step("strict"):
		a.say("Adding --strict to main.go.")
		pause(2600) // finishes after "fields", so the recording's reviews come in a fixed order
		a.tool("Read", map[string]any{"file_path": "main.go"}, "sed -n 1,40p main.go")
		pause(900)
		a.write("main.go", mainAfter)
		summary := "Added --strict: the first malformed line stops the run with exit 1."
		if strings.Contains(strings.ToLower(subtask(p)), "readme") {
			pause(700)
			a.write("README.md", readmeAfter)
			summary += " Documented it in the README."
		}
		pause(800)
		a.tool("Bash", map[string]any{"command": "go test ./..."}, "go test ./...")
		pause(500)
		a.finish(sid, summary, 15600, 1100, 0.09)
	default:
		pause(500)
		a.finish(sid, "Nothing to do for this step.", 1200, 40, 0.004)
	}
}
