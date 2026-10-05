package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sparkz400/relayweft/internal/forge"
	"github.com/sparkz400/relayweft/internal/notify"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/proc"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// Team mode (rw run --issues label:<name> --pr --team): several machines
// pull from the same issue label, and each issue runs on one of them.
//
//   - The issue tracker is the queue; the machines share nothing else (no
//     folder, no server). Right before an issue runs, rw claims it: one
//     comment with a hidden marker (queueMarker) naming this machine
//     (sessionlog.MachineID), a random claim id and the end of its lease.
//     Then it reads the comments again: the earliest live claim wins.
//     When two machines claim at once, the later one stands back (its
//     comment says so) and moves on to the next issue.
//   - Only claims by trusted authors count (forge CommentTrusted: owner,
//     members, collaborators, developers), and the token's owner's, so a
//     stranger's comment cannot hold or free an issue. Comments carrying a
//     marker are never part of a task's text.
//   - The lease (--lease, 30m) is renewed while the task runs by editing
//     the claim. A machine that died stops renewing, and its issue is free
//     again when the lease ends.
//   - When the task ends, the claim becomes the result: done (the pull
//     request's link), failed (why), or released (Ctrl+C, a budget stop,
//     a pull request that could not be opened for reasons outside the
//     task). A failed issue is not taken again: --retry-failed takes it
//     anyway, and so does deleting the failed comment.
//   - As without --team, issues an open pull request already closes are
//     skipped and each task starts on a clean working tree. --limit caps
//     the issues one pass runs; --every repeats the pass (keeping the PC
//     awake) until Ctrl+C.

// queueDefaultLease is how long a claim holds without being renewed.
const queueDefaultLease = 30 * time.Minute

// Claim states.
const (
	claimHeld     = "claimed"
	claimDone     = "done"
	claimFailed   = "failed"
	claimReleased = "released"
	claimLost     = "lost"
)

// reQueueMarker matches the marker in a claim comment.
// sy (Switchyard, v0.2.0 and older) wrote "switchyard:queue": a claim of a
// team member that still runs sy counts too.
var reQueueMarker = regexp.MustCompile(`<!-- (?:relayweft|switchyard):queue id=([0-9a-f]{16}) machine=([0-9A-Za-z_-]{1,64}) state=([a-z]+) until=([0-9TZ:-]{20}) -->`)

// queueClaim is one claim comment.
type queueClaim struct {
	comment int64 // the forge's comment id
	id      string
	machine string
	state   string
	until   time.Time
	author  string
}

func (c queueClaim) live(now time.Time) bool { return c.state == claimHeld && now.Before(c.until) }

// queueMarker is the hidden part of a claim comment. until is rounded up
// to the second, so writing it never shortens a lease.
func queueMarker(id, machine, state string, until time.Time) string {
	until = until.Add(time.Second - 1).Truncate(time.Second)
	return fmt.Sprintf("<!-- relayweft:queue id=%s machine=%s state=%s until=%s -->", id, machine, state, until.UTC().Format(time.RFC3339))
}

// parseClaim reads a comment's claim marker.
func parseClaim(c forge.Comment) (queueClaim, bool) {
	m := reQueueMarker.FindStringSubmatch(c.Body)
	if m == nil {
		return queueClaim{}, false
	}
	until, err := time.Parse(time.RFC3339, m[4])
	if err != nil {
		return queueClaim{}, false
	}
	return queueClaim{comment: c.ID, id: m[1], machine: m[2], state: m[3], until: until, author: c.Author}, true
}

// isQueueComment reports whether a comment is a claim (of any machine).
func isQueueComment(body string) bool {
	return strings.Contains(body, "<!-- relayweft:queue ") || strings.Contains(body, "<!-- switchyard:queue ")
}

// queueView is what an issue's comments say about it.
type queueView struct {
	holder *queueClaim // the earliest live claim, if any
	last   *queueClaim // the latest claim that was not lost
}

// evalQueue reads the claims among an issue's comments (oldest first);
// trusted says whether a comment's author counts.
func evalQueue(cs []forge.Comment, trusted func(forge.Comment) bool, now time.Time) queueView {
	var v queueView
	for _, c := range cs {
		cl, ok := parseClaim(c)
		if !ok || !trusted(c) {
			continue
		}
		if v.holder == nil && cl.live(now) {
			h := cl
			v.holder = &h
		}
		if cl.state != claimLost {
			l := cl
			v.last = &l
		}
	}
	return v
}

// teamQueue claims issues for this machine.
type teamQueue struct {
	repo        forge.Repo
	client      forge.Client
	machine     string
	viewer      string // the token's owner
	label       string
	lease       time.Duration
	retryFailed bool
	now         func() time.Time
	// settle is how long to wait for the forge to list a new comment.
	settle time.Duration
}

func newTeamQueue(f *issueFlags) (*teamQueue, error) {
	c := f.client(f.origin)
	if !c.HasToken() {
		return nil, fmt.Errorf("--team claims issues with comments, so it needs a token: %s", noTokenText(f.origin.Kind))
	}
	viewer, err := c.Viewer()
	if err != nil {
		return nil, fmt.Errorf("--team: who is the token's owner? %w", err)
	}
	machine, err := sessionlog.MachineID()
	if err != nil {
		return nil, fmt.Errorf("--team needs a machine id: %w", err)
	}
	return &teamQueue{repo: f.origin, client: c, machine: machine, viewer: viewer, label: f.label, lease: f.lease,
		retryFailed: f.retryFailed, now: time.Now, settle: 2 * time.Second}, nil
}

// trusted reports whether a comment's claim counts.
func (q *teamQueue) trusted(c forge.Comment) bool {
	if strings.EqualFold(c.Author, q.viewer) {
		return true
	}
	ok, err := q.client.CommentTrusted(q.repo, c)
	return err == nil && ok
}

// claim is a claim this machine holds.
type claim struct {
	q       *teamQueue
	n       int
	id      string
	comment int64
	stop    chan struct{}
	done    sync.WaitGroup
	mu      sync.Mutex // serialises edits of the comment
	ended   bool
}

// errLost means another machine holds the issue.
var errLost = errors.New("another machine holds this issue")

// take claims issue n, or says why not (skip: someone else has it, or it
// failed before).
func (q *teamQueue) take(n int) (cl *claim, skip string, err error) {
	cs, err := q.client.Comments(q.repo, n)
	if err != nil {
		return nil, "", fmt.Errorf("read the comments of #%d: %w", n, err)
	}
	now := q.now()
	v := evalQueue(cs, q.trusted, now)
	if h := v.holder; h != nil {
		return nil, fmt.Sprintf("machine %s holds it until %s", h.machine, h.until.Local().Format("15:04")), nil
	}
	if l := v.last; l != nil && l.state == claimFailed && !q.retryFailed {
		return nil, fmt.Sprintf("it failed on machine %s (--retry-failed takes it anyway)", l.machine), nil
	}

	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, "", err
	}
	cl = &claim{q: q, n: n, id: hex.EncodeToString(b[:]), stop: make(chan struct{})}
	if err := q.client.CommentIssue(q.repo, n, cl.body(claimHeld, now.Add(q.lease), "")); err != nil {
		return nil, "", fmt.Errorf("claim #%d: %w", n, err)
	}
	// Read the comments again: the earliest live claim wins. A forge may
	// list a new comment a moment late, so wait before reading (a claim
	// posted just before this one shows up too), and look up to 3 times.
	for try := 0; try < 3 && cl.comment == 0; try++ {
		time.Sleep(q.settle)
		if cs, err = q.client.Comments(q.repo, n); err != nil {
			return nil, "", fmt.Errorf("read the comments of #%d: %w", n, err)
		}
		for _, c := range cs {
			if mc, ok := parseClaim(c); ok && mc.id == cl.id && strings.EqualFold(c.Author, q.viewer) {
				cl.comment = c.ID
			}
		}
	}
	if cl.comment == 0 {
		return nil, "", fmt.Errorf("claim #%d: the claim comment does not show up on %s", n, q.repo.ForgeName())
	}
	v = evalQueue(cs, q.trusted, q.now())
	if v.holder == nil || v.holder.id != cl.id {
		who := "another machine"
		if v.holder != nil {
			who = "machine " + v.holder.machine
		}
		cl.end(claimLost, who+" claimed it first.")
		return nil, who + " claimed it first", nil
	}
	cl.done.Add(1)
	go cl.renew()
	return cl, "", nil
}

// body is the claim comment's text in a state.
func (c *claim) body(state string, until time.Time, note string) string {
	var text string
	switch state {
	case claimHeld:
		text = fmt.Sprintf("Relayweft (machine `%s`) is working on this issue. Other machines pulling `%s` with `rw run --issues --team` leave it alone while this claim is renewed.", c.q.machine, c.q.label)
	case claimDone:
		text = fmt.Sprintf("Relayweft (machine `%s`) finished this issue.", c.q.machine)
	case claimFailed:
		text = fmt.Sprintf("Relayweft (machine `%s`) could not finish this issue; the team queue will not take it again (`--retry-failed` does, and so does deleting this comment).", c.q.machine)
	case claimReleased:
		text = fmt.Sprintf("Relayweft (machine `%s`) stopped working on this issue; it is back in the queue.", c.q.machine)
	case claimLost:
		text = fmt.Sprintf("Relayweft (machine `%s`) stood back:", c.q.machine)
	}
	if note != "" {
		text += " " + note
	}
	return text + "\n\n" + queueMarker(c.id, c.q.machine, state, until) + "\n" + rwMark
}

// renew extends the lease until end.
func (c *claim) renew() {
	defer c.done.Done()
	t := time.NewTicker(c.q.lease / 3)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
		}
		c.mu.Lock()
		if !c.ended {
			if err := c.q.client.EditComment(c.q.repo, c.n, c.comment, c.body(claimHeld, c.q.now().Add(c.q.lease), "")); err != nil {
				fmt.Printf("could not renew the claim on #%d: %v\n", c.n, err)
			}
		}
		c.mu.Unlock()
	}
}

// end stops renewing and turns the claim into its result.
func (c *claim) end(state, note string) {
	c.mu.Lock()
	if c.ended {
		c.mu.Unlock()
		return
	}
	c.ended = true
	c.mu.Unlock()
	close(c.stop)
	c.done.Wait()
	if c.comment == 0 {
		return
	}
	if err := c.q.client.EditComment(c.q.repo, c.n, c.comment, c.body(state, c.q.now(), note)); err != nil {
		fmt.Printf("could not mark the claim on #%d %s: %v\n", c.n, state, err)
	}
}

// claimOutcome is what a finished task means for its claim.
func claimOutcome(res orchestrator.TaskResult, prURL string, interrupted bool) (state, note string) {
	switch {
	case prURL != "":
		return claimDone, "Pull request: " + prURL
	case interrupted:
		return claimReleased, "(interrupted)"
	case strings.HasPrefix(res.Summary, "stopped by budget"):
		return claimReleased, "(" + res.Summary + ")"
	case !res.OK:
		return claimFailed, "The task did not finish ok: " + oneLine(res.Summary, 200)
	}
	return claimFailed, "No pull request could be opened for its changes."
}

// teamRun runs one issue's task.
type teamRun func(task string) orchestrator.TaskResult

// runTeam is rw run --issues --team: passes over the label until Ctrl+C
// (with --every) or once. It returns the tasks run and failed.
func runTeam(h *headless, f *issueFlags, allowSleep bool, run teamRun) (ran, failed int, err error) {
	q, err := newTeamQueue(f)
	if err != nil {
		return 0, 0, err
	}
	fmt.Printf("team queue: %s label %q as machine %s\n", f.origin, f.label, q.machine)
	if f.every > 0 && !allowSleep {
		release := proc.KeepAwake()
		defer release()
	}
	for {
		r, fl, stop, err := teamPass(h, f, q, run)
		ran, failed = ran+r, failed+fl
		if err != nil {
			if f.every == 0 || stop {
				return ran, failed, err
			}
			fmt.Println("team queue:", err) // the next pass may get through
		}
		if f.every == 0 || stop || h.ctx.Err() != nil {
			return ran, failed, nil
		}
		next := time.Now().Add(f.every)
		fmt.Printf("%s next pass at %s - Ctrl+C stops\n", time.Now().Format("15:04"), next.Format("15:04"))
		select {
		case <-h.ctx.Done():
			return ran, failed, nil
		case <-time.After(time.Until(next)):
		}
	}
}

// teamPass claims and runs up to --limit issues. stop ends the run (the
// working tree is not clean, or the budget is spent).
func teamPass(h *headless, f *issueFlags, q *teamQueue, run teamRun) (ran, failed int, stop bool, err error) {
	if err := cleanTree(f.dir); err != nil {
		return 0, 0, true, err
	}
	open, err := q.client.OpenIssues(f.origin, f.label, 0)
	if err != nil {
		return 0, 0, false, err
	}
	pulls, err := q.client.OpenPulls(f.origin)
	if err != nil {
		return 0, 0, false, err
	}
	taken := forge.ClosedBy(pulls)
	for _, is := range open {
		if ran == f.limit || h.ctx.Err() != nil {
			break
		}
		if taken[is.Number] {
			continue
		}
		cl, skip, err := q.take(is.Number)
		if err != nil {
			fmt.Printf("skipping #%d: %v\n", is.Number, err)
			continue
		}
		if cl == nil {
			fmt.Printf("skipping #%d: %s\n", is.Number, skip)
			continue
		}
		it, err := f.fetchOne(q.client, f.origin, is.Number)
		if err != nil {
			cl.end(claimReleased, "(could not read the issue)")
			fmt.Printf("skipping #%d: %v\n", is.Number, err)
			continue
		}
		f.items = append(f.items, it)
		i := len(f.items) - 1
		ran++
		fmt.Printf("\n=== issue #%d (claimed by this machine): %s\n", is.Number, oneLine(it.issue.Title, 100))
		res := run(it.task)
		if !res.OK {
			failed++
		}
		end := false
		if h.ctx.Err() == nil {
			end = f.afterTask(i, res)
		}
		url := ""
		if f.lastPR != nil {
			url = f.lastPR.URL
		}
		state, note := claimOutcome(res, url, h.ctx.Err() != nil)
		cl.end(state, note)
		if state == claimReleased && strings.HasPrefix(res.Summary, "stopped by budget") {
			h.webhook(notify.EventFailed, "Relayweft: team queue stopped", res.Summary)
			return ran, failed, true, nil
		}
		if end {
			return ran, failed, true, nil
		}
	}
	if ran == 0 {
		fmt.Printf("nothing to take: no open issue labelled %q in %s is free\n", f.label, f.origin)
	}
	return ran, failed, false, nil
}

// runTeamCmd is the end of rw run for --team: the queue, then the totals.
func runTeamCmd(h *headless, f *issueFlags, allowSleep bool, run teamRun) error {
	ran, failed, err := runTeam(h, f, allowSleep, run)
	if h.log != nil {
		fmt.Println("session log:", h.log.Path())
	}
	if ran > 0 {
		fmt.Printf("%d of %d issue(s) succeeded on this machine\n", ran-failed, ran)
		if h.ctx.Err() == nil && ran > 1 {
			title, ev := "Relayweft: team queue done", notify.EventDone
			if failed > 0 {
				title, ev = "Relayweft: team queue tasks failed", notify.EventFailed
			}
			h.webhook(ev, title, fmt.Sprintf("%d of %d issue(s) succeeded", ran-failed, ran))
		}
	}
	if err != nil {
		return err
	}
	if f.noPR > 0 {
		fmt.Printf("%d issue(s) finished without the pull request --pr asked for\n", f.noPR)
	}
	if failed > 0 || f.noPR > 0 {
		return errTaskFailed
	}
	return nil
}
