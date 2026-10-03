package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/limits"
	"github.com/sparkz400/switchyard/internal/notify"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/proc"
	"github.com/sparkz400/switchyard/internal/runner"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// headless runs tasks without the TUI and prints their events.
type headless struct {
	orc    *orchestrator.Orchestrator
	cfg    *config.Config
	log    *sessionlog.Writer
	events chan event.Event
	flush  chan chan struct{}
	done   chan struct{}
	ctx    context.Context
	stop   context.CancelFunc
}

func startHeadless(c *common, quiet bool, ap orchestrator.Approver) (*headless, error) {
	store, dir, err := c.setup()
	if err != nil {
		return nil, err
	}
	_ = proc.Guard()
	cfg := store.Get()
	prunePoolsInBackground(cfg)
	log, err := sessionlog.Open(cfg.SessionDir(), dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: session log disabled:", err)
		log = nil
	}
	h := &headless{cfg: cfg, log: log, events: make(chan event.Event, 4096), flush: make(chan chan struct{}), done: make(chan struct{})}
	h.orc = orchestrator.New(orchestrator.Options{
		Dir: dir, Store: store, Runners: runner.New, Tracker: limits.NewTracker(), Log: log,
		Events: h.events, ForceProvider: c.provider, Approver: ap,
		Repos: c.workspace,
	})
	h.ctx, h.stop = signal.NotifyContext(context.Background(), os.Interrupt)
	go func() {
		<-h.ctx.Done()
		h.stop() // a second Ctrl+C now kills sy immediately
		fmt.Fprintln(os.Stderr, "\ncancelling: stopping all agents... (Ctrl+C again to force quit)")
	}()
	go func() {
		defer close(h.done)
		for {
			select {
			case e, ok := <-h.events:
				if !ok {
					return
				}
				h.print(e, quiet)
			case reply := <-h.flush:
				// Everything a finished task emitted is already buffered.
				for len(h.events) > 0 {
					h.print(<-h.events, quiet)
				}
				close(reply)
			}
		}
	}()
	return h, nil
}

func (h *headless) print(e event.Event, quiet bool) {
	printEvent(e, quiet)
	if e.Kind == event.ProviderState && !e.Until.IsZero() && h.cfg.Notify.Enabled {
		go notify.Send("Switchyard: "+e.Provider+" limit", e.Text)
	}
}

// drain waits until every event emitted so far is printed.
func (h *headless) drain() {
	reply := make(chan struct{})
	h.flush <- reply
	<-reply
}

func (h *headless) close() {
	close(h.events)
	<-h.done
	h.stop()
	h.log.Close()
}

// report prints a task's result and sends the desktop notification.
func (h *headless) report(res orchestrator.TaskResult) {
	h.drain()
	status := "OK"
	if !res.OK {
		status = "FAILED"
	}
	fmt.Printf("\n%s in %s · %s\n", status, res.Duration.Round(time.Second), res.Summary)
	fmt.Printf("cost: %s\n", res.Cost.Summary())
	if res.UndoKey != "" {
		fmt.Printf("undo: sy undo %s   (preview first; your later edits are kept)\n", res.UndoKey)
	}
	if n := h.cfg.Notify; n.Enabled && res.Duration >= n.MinTask.D() && h.ctx.Err() == nil {
		title := "Switchyard: done"
		if !res.OK {
			title = "Switchyard: failed"
		}
		if notify.Send(title, oneLine(res.Summary, 200)) != nil {
			fmt.Fprint(os.Stderr, notify.Bell())
		}
	}
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("sy run", flag.ExitOnError)
	fs.Usage = usage
	var c common
	c.register(fs)
	single := fs.String("single", "", "provider:model[:effort] - one agent, no planning or review (baseline)")
	quiet := fs.Bool("quiet", false, "only print routing, results and errors")
	file := fs.String("file", "", "run the tasks in this file one after another, unattended (one per line, or blocks separated by a line with ---)")
	approve := fs.Bool("approve", false, "ask on the terminal before a plan runs (and per change when orchestrator.review_changes is on)")
	fs.Parse(args)
	var tasks []string
	if *file != "" {
		data, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		if tasks = parseTaskFile(string(data)); len(tasks) == 0 {
			return fmt.Errorf("%s has no tasks", *file)
		}
		if fs.NArg() > 0 {
			return errors.New("give either --file or a task, not both")
		}
	} else if task := strings.TrimSpace(strings.Join(fs.Args(), " ")); task != "" {
		tasks = []string{task}
	} else {
		return errors.New(`usage: sy run [flags] "task"   or   sy run --file tasks.txt`)
	}
	var single0 struct {
		prov  string
		route config.Route
	}
	if *single != "" {
		prov, route, err := config.ParseRouteSpec(*single)
		if err != nil {
			return err
		}
		single0.prov, single0.route = prov, route
	}
	var ap orchestrator.Approver
	if *approve && *file == "" {
		ap = newTermApprover(os.Stdin, os.Stdout)
	}
	h, err := startHeadless(&c, *quiet, ap)
	if err != nil {
		return err
	}
	defer h.close()
	failed := 0
	for i, task := range tasks {
		if h.ctx.Err() != nil {
			break
		}
		if len(tasks) > 1 {
			fmt.Printf("\n=== task %d/%d: %s\n", i+1, len(tasks), oneLine(task, 100))
		}
		var res orchestrator.TaskResult
		if single0.prov != "" {
			res = h.orc.RunSingle(h.ctx, task, single0.prov, single0.route)
		} else {
			res = h.orc.RunWith(h.ctx, task, orchestrator.TaskOptions{Unattended: *file != ""})
		}
		h.report(res)
		if !res.OK {
			failed++
		}
	}
	if h.log != nil {
		fmt.Println("session log:", h.log.Path())
	}
	if len(tasks) > 1 {
		fmt.Printf("%d of %d task(s) succeeded\n", len(tasks)-failed, len(tasks))
	}
	if failed > 0 {
		return errTaskFailed
	}
	return nil
}

// parseTaskFile splits a task file: blocks separated by a line "---", or,
// without any separator, one task per non-empty line. Lines starting with #
// are comments.
func parseTaskFile(s string) []string {
	var lines []string
	sep := false
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		if strings.TrimSpace(l) == "---" {
			sep = true
		}
		lines = append(lines, l)
	}
	var out []string
	if !sep {
		for _, l := range lines {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
		return out
	}
	var cur []string
	add := func() {
		if t := strings.TrimSpace(strings.Join(cur, "\n")); t != "" {
			out = append(out, t)
		}
		cur = nil
	}
	for _, l := range lines {
		if strings.TrimSpace(l) == "---" {
			add()
			continue
		}
		cur = append(cur, l)
	}
	add()
	return out
}

func cmdHistory(args []string) error {
	fs := flag.NewFlagSet("sy history", flag.ExitOnError)
	all := fs.Bool("all", false, "tasks from every directory")
	n := fs.Int("n", 20, "how many")
	dirFlag := fs.String("dir", "", "project directory (default current directory)")
	fs.Parse(args)
	dir := ""
	if !*all {
		d, err := absDir(*dirFlag)
		if err != nil {
			return err
		}
		dir = d
	}
	hist := orchestrator.History(dir, *n)
	if len(hist) == 0 {
		fmt.Println("no tasks yet")
		return nil
	}
	printHistory(os.Stdout, hist, *all)
	return nil
}

func printHistory(w io.Writer, hist []orchestrator.TaskState, withDir bool) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	head := "ID\tWHEN\tSTATUS\tSTEPS\tTASK\tCOST"
	if withDir {
		head += "\tDIR"
	}
	fmt.Fprintln(tw, head)
	for _, s := range hist {
		status := s.Status
		if s.Interrupted() {
			status = "interrupted"
		}
		steps := "-"
		if s.Plan != nil {
			ok := 0
			for _, r := range s.Results {
				if r.OK {
					ok++
				}
			}
			steps = fmt.Sprintf("%d/%d", ok, len(s.Plan.Subtasks))
		}
		line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s", s.ID, s.Created.Format("01-02 15:04"), status, steps, oneLine(s.Task, 50), s.CostLine)
		if withDir {
			line += "\t" + s.Dir
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()
	for _, s := range hist {
		if s.Interrupted() {
			fmt.Printf("\n%s was interrupted: `sy resume %s` continues it.\n", s.ID, s.ID)
			break
		}
	}
}

func cmdResume(args []string) error {
	fs := flag.NewFlagSet("sy resume", flag.ExitOnError)
	fs.Usage = usage
	var c common
	c.register(fs)
	quiet := fs.Bool("quiet", false, "only print routing, results and errors")
	approve := fs.Bool("approve", false, "ask per change when orchestrator.review_changes is on")
	force := fs.Bool("force", false, "resume a task that finished or failed (re-runs its unfinished steps)")
	fs.Parse(args)
	var st *orchestrator.TaskState
	if id := fs.Arg(0); id != "" {
		s, err := orchestrator.LoadTask(id)
		if err != nil {
			return err
		}
		st = s
	} else {
		dir, err := absDir(c.dir)
		if err != nil {
			return err
		}
		if st = orchestrator.LastInterrupted(dir); st == nil {
			return errors.New("no interrupted task in this directory (see `sy history`)")
		}
	}
	if st.Status == "running" && !st.Interrupted() {
		return fmt.Errorf("%s is still running in another sy", st.ID)
	}
	if !st.Interrupted() && !*force {
		return fmt.Errorf("%s is %s, not interrupted (use --force to run its unfinished steps again)", st.ID, st.Status)
	}
	if c.dir == "" {
		c.dir = st.Dir
	} else if d, _ := absDir(c.dir); d != st.Dir {
		fmt.Printf("note: the task ran in %s; resuming in %s\n", st.Dir, d)
	}
	fmt.Printf("resuming %s: %s\n", st.ID, oneLine(st.Task, 100))
	var ap orchestrator.Approver
	if *approve {
		ap = newTermApprover(os.Stdin, os.Stdout)
	}
	h, err := startHeadless(&c, *quiet, ap)
	if err != nil {
		return err
	}
	defer h.close()
	res := h.orc.RunWith(h.ctx, st.Task, orchestrator.TaskOptions{Resume: st, Force: *force})
	h.report(res)
	if !res.OK {
		return errTaskFailed
	}
	return nil
}

func absDir(d string) (string, error) {
	if d == "" {
		var err error
		if d, err = os.Getwd(); err != nil {
			return "", err
		}
	}
	return filepath.Abs(d)
}

// termApprover asks on the terminal. Parallel steps may ask at the same
// time: one dialog runs at a time, and a single goroutine reads stdin, so
// an answer always reaches the question on screen.
type termApprover struct {
	mu    sync.Mutex // one dialog at a time
	lines chan string
	out   io.Writer
}

func newTermApprover(in io.Reader, out io.Writer) *termApprover {
	a := &termApprover{lines: make(chan string), out: out}
	go func() {
		defer close(a.lines)
		r := bufio.NewReader(in)
		for {
			s, err := r.ReadString('\n')
			if s != "" || err == nil {
				a.lines <- s
			}
			if err != nil {
				return
			}
		}
	}()
	return a
}

// ask reads one answer line; ok=false when stdin is closed or ctx ended.
func (a *termApprover) ask(ctx context.Context, prompt string) (string, bool) {
	fmt.Fprint(a.out, prompt)
	select {
	case <-ctx.Done():
		return "", false
	case s, open := <-a.lines:
		if !open {
			return "", false
		}
		return strings.TrimSpace(s), true
	}
}

func (a *termApprover) printPlan(p orchestrator.Plan) {
	fmt.Fprintf(a.out, "\nPlan: %s\n", p.Summary)
	for i, st := range p.Subtasks {
		role := st.Role
		if role == "" {
			role = "auto"
		}
		deps := ""
		if len(st.DependsOn) > 0 {
			deps = " after " + strings.Join(st.DependsOn, ",")
		}
		if len(p.Repos) > 0 { // multi-repo task: where the step works
			repo := st.Repo
			if repo == "" {
				repo = p.Repos[0]
			}
			deps += " in " + repo
		}
		fmt.Fprintf(a.out, "  %d. [%s, %s] %s%s\n", i+1, st.Kind, role, st.Title, deps)
	}
}

func (a *termApprover) ApprovePlan(ctx context.Context, task string, p orchestrator.Plan) (orchestrator.Plan, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for {
		a.printPlan(p)
		q := "Run it? [y]es, [n]o, d N (drop step), r N role (set role; auto = router), p N text (new prompt), s N (show prompt): "
		if len(p.Repos) > 0 {
			q = strings.Replace(q, "s N (show prompt)", "s N (show prompt), o N repo (move to repo)", 1)
		}
		ans, ok := a.ask(ctx, q)
		if !ok {
			return p, false
		}
		cmd, rest, _ := strings.Cut(ans, " ")
		switch strings.ToLower(cmd) {
		case "", "y", "yes":
			np, err := orchestrator.NormalizePlan(p)
			if err != nil {
				fmt.Fprintln(a.out, "plan is not valid:", err)
				continue
			}
			return np, true
		case "n", "no", "q":
			return p, false
		}
		numS, arg, _ := strings.Cut(strings.TrimSpace(rest), " ")
		n, err := strconv.Atoi(numS)
		if err != nil || n < 1 || n > len(p.Subtasks) {
			fmt.Fprintln(a.out, "which step? give its number")
			continue
		}
		i := n - 1
		switch strings.ToLower(cmd) {
		case "d":
			id := p.Subtasks[i].ID
			p.Subtasks = append(p.Subtasks[:i:i], p.Subtasks[i+1:]...)
			for j := range p.Subtasks {
				p.Subtasks[j].DependsOn = without(p.Subtasks[j].DependsOn, id)
			}
		case "r":
			arg = strings.TrimSpace(arg)
			if arg == "auto" {
				arg = ""
			} else if !validRole(arg) {
				fmt.Fprintln(a.out, "roles:", strings.Join(event.Roles, ", "), "or auto")
				continue
			}
			p.Subtasks[i].Role = arg
		case "p":
			if strings.TrimSpace(arg) == "" {
				fmt.Fprintln(a.out, "give the new prompt after the number")
				continue
			}
			p.Subtasks[i].Prompt = strings.TrimSpace(arg)
		case "s":
			fmt.Fprintf(a.out, "\n%s\n", p.Subtasks[i].Prompt)
		case "o":
			arg = strings.TrimSpace(arg)
			if !slices.Contains(p.Repos, arg) {
				fmt.Fprintln(a.out, "repos:", strings.Join(p.Repos, ", "))
				continue
			}
			p.Subtasks[i].Repo = arg
		default:
			fmt.Fprintln(a.out, "unknown answer")
		}
	}
}

func validRole(r string) bool {
	for _, x := range event.Roles {
		if x == r && x != event.RoleJudge {
			return true
		}
	}
	return false
}

func without(xs []string, x string) []string {
	var out []string
	for _, v := range xs {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}

func (a *termApprover) ReviewChanges(ctx context.Context, cs orchestrator.ChangeSet) orchestrator.ChangeDecision {
	a.mu.Lock()
	defer a.mu.Unlock()
	keep := make([]bool, len(cs.Files))
	for i := range keep {
		keep[i] = true
	}
	for {
		round := ""
		if cs.Round > 1 {
			round = fmt.Sprintf(" (round %d)", cs.Round)
		}
		fmt.Fprintf(a.out, "\nChanges from %s: %s%s\n", cs.StepID, cs.Title, round)
		if cs.Summary != "" {
			fmt.Fprintf(a.out, "  %s\n", oneLine(cs.Summary, 200))
		}
		for i, f := range cs.Files {
			mark := "x"
			if !keep[i] {
				mark = " "
			}
			stat := fmt.Sprintf("+%d -%d", f.Added, f.Deleted)
			if f.Binary {
				stat = "binary"
			}
			fmt.Fprintf(a.out, "  [%s] %d. %s %s  %s\n", mark, i+1, f.Status, f.Path, stat)
		}
		ans, ok := a.ask(ctx, "Apply? [y]es (checked files), [n]o (reject all), t N (toggle), v N (view diff), f text (send back with feedback): ")
		if !ok {
			return orchestrator.ChangeDecision{}
		}
		cmd, rest, _ := strings.Cut(ans, " ")
		rest = strings.TrimSpace(rest)
		switch strings.ToLower(cmd) {
		case "", "y", "yes":
			var paths []string
			for i, f := range cs.Files {
				if keep[i] {
					paths = append(paths, f.Path)
				}
			}
			return orchestrator.ChangeDecision{Apply: paths}
		case "n", "no":
			return orchestrator.ChangeDecision{}
		case "f":
			if rest == "" {
				fmt.Fprintln(a.out, "write the feedback after f")
				continue
			}
			return orchestrator.ChangeDecision{Feedback: rest}
		case "t", "v":
			n, err := strconv.Atoi(rest)
			if err != nil || n < 1 || n > len(cs.Files) {
				fmt.Fprintln(a.out, "which file? give its number")
				continue
			}
			if strings.ToLower(cmd) == "t" {
				keep[n-1] = !keep[n-1]
			} else {
				fmt.Fprintln(a.out, cs.Files[n-1].Patch)
			}
		default:
			fmt.Fprintln(a.out, "unknown answer")
		}
	}
}

var _ orchestrator.Approver = (*termApprover)(nil)
