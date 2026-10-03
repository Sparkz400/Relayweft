// Package proc starts CLI subprocesses so that killing an agent kills its
// whole process tree (npm shims spawn node, which spawns tools), on Windows
// and Unix alike.
package proc

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Resolve finds a command on PATH. On Windows exec.LookPath honours PATHEXT,
// so "codex" resolves to the npm shim codex.cmd.
func Resolve(name string) (string, error) { return exec.LookPath(name) }

// FixPath repairs PATH entries with stray double quotes on Windows, such as
// `C:\Program Files\PowerShell\7"`. Go's filepath.SplitList treats a quote as
// the start of a quoted section, so one stray quote swallows every entry after
// it and git, codex and claude all go "missing" although the shell finds them.
// It updates sy's own environment (children inherit the fixed PATH) and
// returns the broken entries so `sy doctor` can report them.
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
// lock is held until unlock is called or the process exits, so a crashed sy
// never leaves a stale lock behind. Two TryLock calls on the same path fail
// even within one process.
func TryLock(path string) (unlock func(), ok bool) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, false
	}
	if !tryLock(f) {
		f.Close()
		return nil, false
	}
	return func() { f.Close() }, true
}

// Prepare configures cmd so cancelling its context kills the whole tree.
func Prepare(cmd *exec.Cmd) {
	prepare(cmd)
	cmd.WaitDelay = 5 * time.Second
}

// Guard makes sure child processes die with this process (a Windows job
// object with kill-on-close). It is a no-op elsewhere, where agents get
// their own process group and are killed explicitly.
func Guard() error { return guard() }
