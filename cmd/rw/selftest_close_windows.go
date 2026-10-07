//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Closing the window mid-task (rw selftest --close): rw runs a task in a
// console window of its own, in the old console (conhost) and in Windows
// Terminal, both as `rw run` and as the TUI, and the window is closed the
// way a user does it while the last step's agent works. Windows then sends
// CTRL_CLOSE_EVENT to every program attached to that console and ends them
// after about 5 seconds. Then: nothing of the task may be left running, the
// task must be interrupted (not cancelled), and rw resume, rw undo and redo
// must work.
//
// The window runs `rw __selftest-console <spec>` (cmdSelftestConsole): it
// starts the rw command from the spec in its console, with the test
// profile's environment (Windows Terminal starts tabs with its own), writes
// its console's window handle and the pids, and types the task into the
// TUI.

var (
	kernel32              = windows.NewLazySystemDLL("kernel32.dll")
	user32                = windows.NewLazySystemDLL("user32.dll")
	procGetConsoleWindow  = kernel32.NewProc("GetConsoleWindow")
	procWriteConsoleInput = kernel32.NewProc("WriteConsoleInputW")
	procPostMessage       = user32.NewProc("PostMessageW")
	procGetWindowText     = user32.NewProc("GetWindowTextW")
)

const wmClose = 0x0010

// consoleSpec is what the console window runs.
type consoleSpec struct {
	Bin  string   `json:"bin"`
	Args []string `json:"args"`
	Dir  string   `json:"dir"`
	Env  []string `json:"env"`
	Type string   `json:"type,omitempty"` // typed into the program, then Enter
	Out  string   `json:"out"`            // folder for console.json and type.err
}

// consoleInfo is what the console window reports back.
type consoleInfo struct {
	Launcher int    `json:"launcher"`
	Child    int    `json:"child"`
	HWND     uint64 `json:"hwnd"`
	Class    string `json:"class"`
	Err      string `json:"err,omitempty"`
}

// cmdSelftestConsole runs in the test's console window (see above).
func cmdSelftestConsole(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: rw "+selftestConsoleCmd+" <spec.json>")
		os.Exit(2)
	}
	// That it started at all: the test says so when nothing else came.
	_ = os.WriteFile(args[0]+".started", []byte(strconv.Itoa(os.Getpid())), 0o644)
	var spec consoleSpec
	b, err := os.ReadFile(args[0])
	if err == nil {
		err = json.Unmarshal(b, &spec)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "rw selftest console:", err)
		time.Sleep(time.Minute) // keep the window open to be read
		os.Exit(1)
	}
	hwnd, _, _ := procGetConsoleWindow.Call()
	info := consoleInfo{Launcher: os.Getpid(), HWND: uint64(hwnd), Class: windowClass(windows.HWND(hwnd))}
	fmt.Printf("rw selftest: this window is closed by the test while an agent works (pid %d)\n", os.Getpid())
	cmd := exec.Command(spec.Bin, spec.Args...)
	cmd.Dir, cmd.Env = spec.Dir, spec.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		info.Err = err.Error()
	} else {
		info.Child = cmd.Process.Pid
	}
	if err := writeJSONAtomic(filepath.Join(spec.Out, "console.json"), info); err != nil {
		fmt.Fprintln(os.Stderr, "rw selftest console:", err)
	}
	if info.Err != "" {
		fmt.Fprintln(os.Stderr, "rw selftest console:", info.Err)
		time.Sleep(time.Minute)
		os.Exit(1)
	}
	if spec.Type != "" {
		go func() {
			if err := typeIntoConsole(spec.Type); err != nil {
				_ = os.WriteFile(filepath.Join(spec.Out, "type.err"), []byte(err.Error()), 0o644) // the test reports it
			}
		}()
	}
	_ = cmd.Wait() // the test reads rw's exit code itself
}

func writeJSONAtomic(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// inputRecord is a KEY_EVENT INPUT_RECORD.
type inputRecord struct {
	eventType uint16
	_         uint16
	keyDown   int32
	repeat    uint16
	vk        uint16
	scan      uint16
	char      uint16
	ctrl      uint32
}

// typeIntoConsole types text into this console's input, waits until the
// program has read it, then presses Enter: the TUI takes keys that come
// too fast after each other for a paste, whose Enter is a new line.
func typeIntoConsole(text string) error {
	name, _ := windows.UTF16PtrFromString("CONIN$")
	in, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return fmt.Errorf("open the console input: %w", err)
	}
	defer windows.CloseHandle(in)
	write := func(recs []inputRecord) error {
		var n uint32
		r, _, err := procWriteConsoleInput.Call(uintptr(in), uintptr(unsafe.Pointer(&recs[0])), uintptr(len(recs)), uintptr(unsafe.Pointer(&n)))
		if r == 0 {
			return fmt.Errorf("WriteConsoleInput: %w", err)
		}
		return nil
	}
	read := func(what string) error {
		for end := time.Now().Add(90 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			var n uint32
			if err := windows.GetNumberOfConsoleInputEvents(in, &n); err != nil {
				return err
			}
			if n == 0 {
				return nil
			}
		}
		return fmt.Errorf("the TUI did not read %s within 90s", what)
	}
	var recs []inputRecord
	for _, c := range text {
		if c > 0xFFFF {
			return fmt.Errorf("cannot type %q", c)
		}
		recs = append(recs, inputRecord{eventType: 1, keyDown: 1, repeat: 1, char: uint16(c)}, inputRecord{eventType: 1, repeat: 1, char: uint16(c)})
	}
	if err := write(recs); err != nil {
		return err
	}
	if err := read("the typed task"); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	const vkReturn = 0x0D
	if err := write([]inputRecord{{eventType: 1, keyDown: 1, repeat: 1, vk: vkReturn, scan: 0x1C, char: '\r'}, {eventType: 1, repeat: 1, vk: vkReturn, scan: 0x1C, char: '\r'}}); err != nil {
		return err
	}
	return read("Enter")
}

func windowClass(h windows.HWND) string {
	if h == 0 {
		return ""
	}
	buf := make([]uint16, 256)
	n, err := windows.GetClassName(h, &buf[0], int32(len(buf)))
	if err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

func windowText(h windows.HWND) string {
	buf := make([]uint16, 512)
	n, _, _ := procGetWindowText.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf[:n])
}

// topWindows lists the top-level windows of class class.
func topWindows(class string) []windows.HWND {
	var out []windows.HWND
	cb := windows.NewCallback(func(h windows.HWND, _ uintptr) uintptr {
		if windowClass(h) == class {
			out = append(out, h)
		}
		return 1
	})
	_ = windows.EnumWindows(cb, nil) // an empty list says it
	return out
}

// consoleTerm is a terminal the window is closed in.
type consoleTerm struct {
	name string
	// start opens a window that runs `rw __selftest-console spec`.
	start func(t *selftest, spec, title, dir string) (*os.Process, error)
	// window finds the window to close and says how it was identified.
	window func(info consoleInfo, host *os.Process, title string) (windows.HWND, string, error)
	// missing says why the terminal cannot be tested here ("" = it can).
	missing func() string
}

// errSkip marks a reason to skip, not a failure: the terminal or its
// window could not be identified reliably on this machine.
type errSkip struct{ why string }

func (e errSkip) Error() string { return e.why }

var conhostTerm = consoleTerm{
	name: "the old console (conhost)",
	missing: func() string {
		if _, err := os.Stat(conhostPath()); err != nil {
			return "conhost.exe is not there: " + err.Error()
		}
		return ""
	},
	start: func(t *selftest, spec, _, dir string) (*os.Process, error) {
		// conhost.exe with a command line hosts it itself, in the classic
		// console window: a plain new console would go to the default
		// terminal app, which on Windows 11 is often Windows Terminal.
		return startWithoutStdHandles(conhostPath(), []string{t.bin, selftestConsoleCmd, spec}, dir, t.childEnv())
	},
	window: func(info consoleInfo, host *os.Process, _ string) (windows.HWND, string, error) {
		h := windows.HWND(info.HWND)
		if info.Class != "ConsoleWindowClass" {
			return 0, "", errSkip{fmt.Sprintf("conhost.exe did not host the console itself (its window is a %q, not a ConsoleWindowClass): the default terminal app took it", info.Class)}
		}
		// The window's command must be the conhost.exe's own client.
		// (GetWindowThreadProcessId is no help: for a console window it
		// names a program attached to the console, not conhost.)
		if parent := parentPID(info.Launcher); parent != host.Pid {
			return 0, "", fmt.Errorf("the window's command (pid %d) runs below pid %d, not below the conhost.exe the test started (pid %d)", info.Launcher, parent, host.Pid)
		}
		if !windows.IsWindow(h) {
			return 0, "", fmt.Errorf("the console window 0x%x that GetConsoleWindow reported is gone", info.HWND)
		}
		return h, fmt.Sprintf("WM_CLOSE to the console window 0x%x of the conhost.exe the test started (pid %d), as its X button does", info.HWND, host.Pid), nil
	},
}

// parentPID is pid's parent's pid (0 when pid is not running).
func parentPID(pid int) int {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if int(pe.ProcessID) == pid {
			return int(pe.ParentProcessID)
		}
	}
	return 0
}

// startWithoutStdHandles starts a program as Explorer does, without
// standard handles. os/exec always passes some (NUL for none), and
// conhost.exe given standard handles takes them for a pseudo console's
// pipes: it opens no window and its client does not run.
func startWithoutStdHandles(path string, args []string, dir string, env []string) (*os.Process, error) {
	line := syscall.EscapeArg(path)
	for _, a := range args {
		line += " " + syscall.EscapeArg(a)
	}
	cmdLine, err := windows.UTF16PtrFromString(line)
	if err != nil {
		return nil, err
	}
	app, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	wd, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return nil, err
	}
	var block []uint16
	for _, kv := range env {
		block = append(block, windows.StringToUTF16(kv)...) // with its NUL
	}
	block = append(block, 0)
	si := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(app, cmdLine, nil, nil, false, windows.CREATE_UNICODE_ENVIRONMENT, &block[0], wd, &si, &pi); err != nil {
		return nil, fmt.Errorf("start %s: %w", path, err)
	}
	defer windows.CloseHandle(pi.Thread)
	// Held until FindProcess has its own handle: the pid cannot be reused
	// in between.
	defer windows.CloseHandle(pi.Process)
	return os.FindProcess(int(pi.ProcessId))
}

func conhostPath() string {
	return filepath.Join(os.Getenv("SystemRoot"), "System32", "conhost.exe")
}

const wtWindowClass = "CASCADIA_HOSTING_WINDOW_CLASS"

var wtTerm = consoleTerm{
	name: "Windows Terminal",
	missing: func() string {
		if _, err := exec.LookPath("wt.exe"); err != nil {
			return "Windows Terminal is not installed (no wt.exe on PATH)"
		}
		return ""
	},
	start: func(t *selftest, spec, title, dir string) (*os.Process, error) {
		wt, err := exec.LookPath("wt.exe")
		if err != nil {
			return nil, err
		}
		// A new window, whose tab keeps the unique title: the window is
		// found by it. Windows Terminal runs every window in one process,
		// and only this window may be closed.
		cmd := exec.Command(wt, "-w", "new", "new-tab", "--title", title, "--suppressApplicationTitle", "-d", dir, t.bin, selftestConsoleCmd, spec)
		cmd.Dir = dir
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return cmd.Process, nil
	},
	window: func(info consoleInfo, _ *os.Process, title string) (windows.HWND, string, error) {
		var found []windows.HWND
		for end := time.Now().Add(15 * time.Second); ; time.Sleep(200 * time.Millisecond) {
			found = found[:0]
			for _, h := range topWindows(wtWindowClass) {
				if windowText(h) == title {
					found = append(found, h)
				}
			}
			if len(found) > 0 || time.Now().After(end) {
				break
			}
		}
		switch len(found) {
		case 0:
			return 0, "", errSkip{fmt.Sprintf("no Windows Terminal window has the title %q (the tab runs, but its window could not be identified)", title)}
		case 1:
		default:
			return 0, "", errSkip{fmt.Sprintf("%d Windows Terminal windows have the title %q: cannot tell which to close", len(found), title)}
		}
		return found[0], fmt.Sprintf("WM_CLOSE to the new Windows Terminal window 0x%x, found by its unique title %q, as its X button does", uintptr(found[0]), title), nil
	},
}

// closeScenarios runs the task in each terminal, as rw run and as the TUI,
// and closes the window while the last step's agent works.
func (t *selftest) closeScenarios() {
	n := 0
	for _, term := range []*consoleTerm{&conhostTerm, &wtTerm} {
		for _, tui := range []bool{false, true} {
			n++
			what := "rw run"
			if tui {
				what = "the TUI (rw)"
			}
			t.section(fmt.Sprintf("Window closed mid-task: %s in %s", what, term.name))
			if why := term.missing(); why != "" {
				t.check(markSkip, "close", "%s", why)
				t.closeSkipped(term.name)
				continue
			}
			proj := filepath.Join(t.work, "projects", fmt.Sprintf("close %d ä", n))
			if err := t.makeRepo(proj, 50); err != nil {
				t.check(markFail, "repo", "%v", err)
				t.closeSkipped(term.name)
				continue
			}
			before := t.counts
			skipped := false
			t.scenario(proj, filepath.Join(t.work, fmt.Sprintf("agent close %d", n)), t.closeMidRun(term, tui, &skipped))
			if skipped || t.counts[markFail] > before[markFail] {
				t.closeSkipped(term.name)
			}
		}
	}
}

func (t *selftest) closeSkipped(term string) {
	for _, s := range t.closeLeft {
		if s == term {
			return
		}
	}
	t.closeLeft = append(t.closeLeft, term)
}

// treeProc is a process of the task's tree, held open so that its pid
// cannot be reused while the test watches it.
type treeProc struct {
	pid   int
	name  string
	h     windows.Handle
	ended time.Time
}

// processTree opens the processes below root (root included), walking a
// snapshot of the process table: a child must be younger than its parent,
// since Windows reuses the pids of dead parents.
func processTree(root int) []*treeProc {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	children := map[int][]int{}
	names := map[int]string{}
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		pid, parent := int(pe.ProcessID), int(pe.ParentProcessID)
		names[pid] = windows.UTF16ToString(pe.ExeFile[:])
		if pid != parent {
			children[parent] = append(children[parent], pid)
		}
	}
	var out []*treeProc
	var walk func(pid int, parentStart int64)
	walk = func(pid int, parentStart int64) {
		h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE, false, uint32(pid))
		if err != nil {
			return
		}
		var c, e, k, u windows.Filetime
		if windows.GetProcessTimes(h, &c, &e, &k, &u) != nil || c.Nanoseconds() < parentStart {
			windows.CloseHandle(h) // not readable, or a reused pid
			return
		}
		out = append(out, &treeProc{pid: pid, name: names[pid], h: h})
		for _, ch := range children[pid] {
			walk(ch, c.Nanoseconds())
		}
	}
	walk(root, 0)
	return out
}

func (p *treeProc) done() bool {
	if p.ended.IsZero() {
		if ev, _ := windows.WaitForSingleObject(p.h, 0); ev == windows.WAIT_OBJECT_0 {
			p.ended = time.Now()
		}
	}
	return !p.ended.IsZero()
}

func (p *treeProc) exitCode() uint32 {
	var code uint32
	_ = windows.GetExitCodeProcess(p.h, &code) // 0 when unreadable
	return code
}

// waitTree waits until every process has ended, for at most d.
func waitTree(tree []*treeProc, d time.Duration) bool {
	for end := time.Now().Add(d); ; time.Sleep(50 * time.Millisecond) {
		all := true
		for _, p := range tree {
			all = p.done() && all
		}
		if all {
			return true
		}
		if time.Now().After(end) {
			return false
		}
	}
}

// endTree ends what is left of the tree (only processes the test started
// in its window) and closes the handles. Exit code 0 lets Windows Terminal
// close the tab of a process that ended.
func endTree(tree []*treeProc) {
	for _, p := range tree {
		if !p.done() {
			_ = windows.TerminateProcess(p.h, 0)
		}
	}
	closeHandles(tree)
}

func closeHandles(tree []*treeProc) {
	for _, p := range tree {
		if p.h != 0 {
			windows.CloseHandle(p.h)
			p.h = 0
		}
	}
}

func treeNames(tree []*treeProc) string {
	count := map[string]int{}
	for _, p := range tree {
		count[p.name]++
	}
	var names []string
	for n, c := range count {
		if c > 1 {
			n = fmt.Sprintf("%s x%d", n, c)
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// closeMidRun is an interruptFunc that runs the task in term's window and
// closes the window while the last step's agent works. *skipped is set
// when the terminal could not be tested reliably.
func (t *selftest) closeMidRun(term *consoleTerm, tui bool, skipped *bool) interruptFunc {
	return func(proj, stateDir string, env, runArgs []string) (int, bool) {
		args := runArgs
		if tui {
			args = []string{"--config", t.cfg, "--provider", "claude"}
		}
		spec := consoleSpec{Bin: t.bin, Args: args, Dir: proj, Env: t.childEnv(append(env, envSelftestHang+"=1")...), Out: stateDir}
		if tui {
			spec.Type = selftestTask
		}
		specPath := filepath.Join(stateDir, "console spec.json")
		if err := writeJSONAtomic(specPath, spec); err != nil {
			t.check(markFail, "window", "%v", err)
			return 0, false
		}
		title := fmt.Sprintf("rw selftest close %d-%d", os.Getpid(), time.Now().UnixNano())
		began := time.Now()
		host, err := term.start(t, specPath, title, proj)
		if err != nil {
			t.check(markFail, "window", "could not open a window in %s: %v", term.name, err)
			return 0, false
		}
		hostDone := make(chan struct{})
		var hostEnd *os.ProcessState
		go func() {
			hostEnd, _ = host.Wait() // conhost: when the window is gone; wt.exe: at once
			close(hostDone)
		}()

		// The window's command reports its console and pids.
		var info consoleInfo
		infoPath := filepath.Join(stateDir, "console.json")
		for end := time.Now().Add(60 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			if b, err := os.ReadFile(infoPath); err == nil && json.Unmarshal(b, &info) == nil {
				break
			}
			if time.Now().After(end) {
				if _, err := os.Stat(specPath + ".started"); err == nil {
					t.check(markFail, "window", "the window's command started but did not start rw within 60s (%s %s)", term.name, hostState(&hostEnd, hostDone))
					return 0, false
				}
				if term == &wtTerm {
					*skipped = true
					t.check(markSkip, "window", "Windows Terminal did not run the test's command within 60s (wt.exe %s); close the window titled %q if one opened", hostState(&hostEnd, hostDone), title)
				} else {
					t.check(markFail, "window", "%s did not run the test's command within 60s (%s)", term.name, hostState(&hostEnd, hostDone))
				}
				return 0, false
			}
		}
		if info.Err != "" {
			t.check(markFail, "window", "the window could not start rw: %s", info.Err)
			return 0, false
		}
		what := "rw run"
		if tui {
			what = "the TUI"
		}
		launcher := processTree(info.Launcher)
		defer func() { endTree(launcher) }() // ends what is left after a failure: only the window's command and what it started
		var rw *treeProc
		for _, p := range launcher {
			if p.pid == info.Child {
				rw = p
			}
		}
		if rw == nil {
			t.check(markFail, "window", "%s (pid %d) ended at once; its output was in the window, its debug log is in the test profile", what, info.Child)
			return 0, false
		}

		ended := func() (string, bool) {
			if b, err := os.ReadFile(filepath.Join(stateDir, "type.err")); err == nil {
				return "typing the task into the TUI failed: " + string(b), true
			}
			if rw.done() {
				return fmt.Sprintf("%s ended with exit code 0x%X", what, rw.exitCode()), true
			}
			return "", false
		}
		output := func() string {
			return "(" + what + " wrote to its window; the test profile's debug log has what it did)"
		}
		agent, saved, ok := t.waitAgentWorking(stateDir, began, ended, func() {}, output)
		if !ok {
			return 0, false
		}

		// The task's processes now: the window's command, rw, the agent's
		// .cmd shim, the agent and its child, and their hidden consoles.
		closeHandles(launcher)
		tree := processTree(info.Launcher)
		launcher = tree
		rw = nil
		for _, p := range tree {
			if p.pid == info.Child {
				rw = p
			}
		}
		if rw == nil {
			t.check(markFail, "window", "%s (pid %d) ended while its agent worked", what, info.Child)
			return 0, false
		}
		h, how, err := term.window(info, host, title)
		var skip errSkip
		switch {
		case errors.As(err, &skip):
			*skipped = true
			t.check(markSkip, "window", "%s", skip.why)
			return 0, false
		case err != nil:
			t.check(markFail, "window", "%v", err)
			return 0, false
		}
		r, _, err := procPostMessage.Call(uintptr(h), wmClose, 0, 0)
		if r == 0 {
			t.check(markFail, "window", "PostMessage(WM_CLOSE) to 0x%x failed: %v", uintptr(h), err)
			return 0, false
		}
		closed := time.Now()
		if !waitTree([]*treeProc{rw}, 15*time.Second) {
			t.check(markFail, "close", "%s (pid %d) still runs 15s after its window was closed (%s)", what, rw.pid, how)
			return 0, false
		}
		t.check(markOK, "close", "%s; %s (pid %d) ended %s later with exit code 0x%X, after saving the agent's session (waited %s for that)",
			how, what, rw.pid, rw.ended.Sub(closed).Round(10*time.Millisecond), rw.exitCode(), saved.Round(time.Millisecond))

		// Windows ends what the console's programs left within about 5s.
		if waitTree(tree, 15*time.Second) {
			last := closed
			for _, p := range tree {
				if p.ended.After(last) {
					last = p.ended
				}
			}
			t.check(markOK, "orphans", "all %d processes of the window (%s) ended within %s of the close: nothing is left running",
				len(tree), treeNames(tree), last.Sub(closed).Round(10*time.Millisecond))
		} else {
			var left []string
			for _, p := range tree {
				if !p.done() {
					left = append(left, fmt.Sprintf("%s (pid %d)", p.name, p.pid))
				}
			}
			t.check(markFail, "orphans", "15s after the window was closed these still run: %s (the test ends them now)", strings.Join(left, ", "))
			return 0, false
		}
		select {
		case <-hostDone:
		case <-time.After(10 * time.Second):
			if term == &conhostTerm {
				t.check(markWarn, "window", "conhost.exe (pid %d) still runs 10s after its window was closed", host.Pid)
			}
		}
		if windows.IsWindow(h) && term == &wtTerm {
			t.check(markWarn, "window", "the Windows Terminal window 0x%x is still open 10s after WM_CLOSE", uintptr(h))
		}
		return agent, true
	}
}

func hostState(end **os.ProcessState, done <-chan struct{}) string {
	select {
	case <-done:
		if st := *end; st != nil {
			return fmt.Sprintf("it exited with code %d", st.ExitCode())
		}
		return "it exited"
	default:
		return "it still runs"
	}
}
