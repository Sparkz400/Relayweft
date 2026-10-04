//go:build windows

package proc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func prepare(cmd *exec.Cmd) {
	attr := &syscall.SysProcAttr{
		// CREATE_NO_WINDOW: agents never share sy's console, so they cannot
		// retitle the tab or change the console mode under the TUI. All
		// stdio is piped, so nothing is lost.
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW | priorityFlag(),
		HideWindow:    true,
	}
	if ext := strings.ToLower(filepath.Ext(cmd.Path)); ext == ".cmd" || ext == ".bat" {
		// npm installs CLIs as .cmd shims. Letting CreateProcess run them
		// implicitly breaks when both the shim path and an argument are
		// quoted (cmd strips the outer quotes; Go issue #15566), e.g. a
		// user name with a space. Run cmd.exe explicitly with /s, which
		// keeps everything between the outer quotes verbatim.
		comspec := os.Getenv("ComSpec")
		if comspec == "" {
			comspec = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
		}
		parts := []string{`"` + cmd.Path + `"`}
		for _, a := range cmd.Args[1:] {
			parts = append(parts, CmdQuote(a))
		}
		attr.CmdLine = syscall.EscapeArg(comspec) + ` /d /s /c "` + strings.Join(parts, " ") + `"`
		cmd.Path = comspec
	}
	cmd.SysProcAttr = attr
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// taskkill /T walks the tree (cmd.exe shim -> node -> tools).
		kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
		if err := kill.Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}

// belowNormal is BELOW_NORMAL_PRIORITY_CLASS. A child of a below-normal
// process inherits the class, so the whole agent tree stays below normal.
const belowNormal = 0x00004000

func priorityFlag() uint32 {
	if lowPriority.Load() {
		return belowNormal
	}
	return 0
}

func background(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW | priorityFlag()}
}

// breakaway starts cmd outside sy's kill-on-close job (guard allows that
// with BREAKAWAY_OK). Without it a browser sy opened would join the job:
// when the browser was not running yet, that process becomes the user's
// browser, and every window they open later was killed when sy exited.
func breakaway(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_BREAKAWAY_FROM_JOB
}

// lower is a no-op: the priority class is set at creation.
func lower(int) {}

var job windows.Handle

// guard puts sy itself into a job object with KILL_ON_JOB_CLOSE. Children
// inherit the job, so when sy exits or crashes Windows kills every agent
// instead of leaving orphans that keep burning quota. BREAKAWAY_OK lets a
// child that asks for it (Breakaway: the browser) leave the job; children
// that do not ask stay in it.
func guard() error {
	if job != 0 {
		return nil
	}
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK,
		},
	}
	if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(h)
		return err
	}
	if err := windows.AssignProcessToJobObject(h, windows.CurrentProcess()); err != nil {
		windows.CloseHandle(h)
		return err
	}
	job = h // intentionally never closed: closing it is what kills the children
	return nil
}

// Shell runs a command line through cmd.exe exactly as typed (/s keeps
// everything between the outer quotes verbatim).
func Shell(ctx context.Context, line string) *exec.Cmd {
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	}
	cmd := exec.CommandContext(ctx, comspec)
	Prepare(cmd)
	cmd.SysProcAttr.CmdLine = syscall.EscapeArg(comspec) + ` /d /s /c "` + line + `"`
	return cmd
}
