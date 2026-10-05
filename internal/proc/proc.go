// Package proc starts CLI subprocesses so that killing an agent kills its
// whole process tree (npm shims spawn node, which spawns tools), on Windows
// and Unix alike.
package proc

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sparkz400/relayweft/internal/diag"
)

// Resolve finds a command on PATH. On Windows exec.LookPath honours PATHEXT,
// so "codex" resolves to the npm shim codex.cmd.
func Resolve(name string) (string, error) { return exec.LookPath(name) }

// FixPath repairs PATH entries with stray double quotes on Windows, such as
// `C:\Program Files\PowerShell\7"`. Go's filepath.SplitList treats a quote as
// the start of a quoted section, so one stray quote swallows every entry after
// it and git, codex and claude all go "missing" although the shell finds them.
// It updates rw's own environment (children inherit the fixed PATH) and
// returns the broken entries so `rw doctor` can report them.
func FixPath() []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	fixed, bad := cleanPathList(os.Getenv("PATH"))
	if len(bad) > 0 {
		os.Setenv("PATH", fixed)
	}
	return bad
}

// cleanPathList strips quotes from malformed entries of a Windows PATH list.
// Well-formed quoted entries ("C:\a;b", the only reason to quote) are kept.
func cleanPathList(s string) (string, []string) {
	segs := strings.Split(s, ";")
	out := make([]string, 0, len(segs))
	var bad []string
	for i := 0; i < len(segs); i++ {
		seg := segs[i]
		if !strings.Contains(seg, `"`) {
			out = append(out, seg)
			continue
		}
		if strings.HasPrefix(seg, `"`) {
			j, joined := i, seg
			for strings.Count(joined, `"`) < 2 && j+1 < len(segs) {
				j++
				joined += ";" + segs[j]
			}
			if len(joined) >= 2 && strings.Count(joined, `"`) == 2 && strings.HasSuffix(joined, `"`) {
				out = append(out, joined)
				i = j
				continue
			}
		}
		bad = append(bad, seg)
		out = append(out, strings.ReplaceAll(seg, `"`, ""))
	}
	return strings.Join(out, ";"), bad
}

// TryLock takes an exclusive lock on the file at path without waiting. The
// lock is held until unlock is called or the process exits, so a crashed rw
// never leaves a stale lock behind. Two TryLock calls on the same path fail
// even within one process.
//
// A lock file may be deleted while it is held (rw clean does that). A lock
// taken on a file that is no longer at path is worthless, so TryLock checks
// that the locked file is still the one at path and retries otherwise.
func TryLock(path string) (unlock func(), ok bool) {
	for try := 0; try < 3; try++ {
		f, err := openLock(path)
		if err != nil {
			return nil, false
		}
		if !tryLock(f) {
			f.Close()
			return nil, false
		}
		if stillAt(f, path) {
			return func() { f.Close() }, true
		}
		f.Close() // deleted (and maybe recreated) under us: try the new file
	}
	return nil, false
}

// Locked reports whether someone holds TryLock's exclusive lock on path. It
// probes with a shared lock: probes never block each other, so two rw
// looking at the same lock at once both see it free. (A probe that took
// the exclusive lock and released it made a concurrent probe see it held:
// a stopped task looked like one a rw was running.) A real TryLock that
// meets a probe fails for that moment; callers that must get the lock
// retry.
func Locked(path string) bool {
	f, err := openLock(path)
	if err != nil {
		return true // cannot tell: assume it is held
	}
	defer f.Close()
	return !tryLockShared(f)
}

// stillAt reports whether the open file f is the file currently at path.
func stillAt(f *os.File, path string) bool {
	a, err := f.Stat()
	if err != nil {
		return false
	}
	b, err := os.Stat(path)
	return err == nil && os.SameFile(a, b)
}

// lowPriority runs agents and git below normal CPU priority (set from
// orchestrator.low_priority). Their children inherit it, so a busy agent
// tree slows itself down instead of the rest of the machine.
var lowPriority atomic.Bool

func init() { lowPriority.Store(true) }

// SetLowPriority turns low-priority children on or off.
func SetLowPriority(on bool) { lowPriority.Store(on) }

// Prepare configures cmd so cancelling its context kills the whole tree,
// and so the tree dies with rw (see Guard). Call it after cmd's path and
// arguments are set, and do not set ExtraFiles after it.
func Prepare(cmd *exec.Cmd) {
	prepare(cmd)
	cmd.WaitDelay = 5 * time.Second
}

// Background configures a short helper command (git): no console window and,
// with LowPriority, below-normal priority. Call Started after Start.
func Background(cmd *exec.Cmd) { background(cmd) }

// Started applies settings that need a running process (Unix nice). Errors
// are ignored: priority is best effort.
func Started(cmd *exec.Cmd) {
	if lowPriority.Load() && cmd.Process != nil {
		lower(cmd)
	}
	noteStart(cmd)
}

// Guard makes sure child processes die with this process, even when it is
// killed hard or crashes. On Windows rw joins a job object with
// kill-on-close. Elsewhere the commands Prepare sets up from then on run
// in a wrapper that kills their process group when rw's end of a pipe
// closes (proc_unix.go).
func Guard() error {
	err := guard()
	if err != nil {
		diag.Logf("guard: agents may outlive rw: %v", err)
	}
	return err
}

// Breakaway configures cmd to start outside Guard's job, so it outlives rw:
// for the user's own programs rw merely launches (a browser), never for
// agents. On Windows, Start then fails if an outer job (one rw itself was
// started in) forbids breakaway: start a fresh command without it then.
// Elsewhere it starts cmd in its own session, out of reach of the
// terminal's Ctrl+C and hangup that end rw.
func Breakaway(cmd *exec.Cmd) { breakaway(cmd) }
