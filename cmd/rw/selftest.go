package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/diag"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/proc"
	"github.com/sparkz400/relayweft/internal/runner"
	"gopkg.in/yaml.v3"
)

// rw selftest is the automated part of the Windows test pass (ROADMAP 1.6).
// It builds a throwaway world in one work folder:
//
//   - a user profile whose path has spaces, parentheses and non-ASCII
//     letters (the children's APPDATA, LOCALAPPDATA and TEMP point there, so
//     task state, logs and the worktree pool live there too);
//   - a copy of rw in that profile, and a "claude" CLI next to it, which is
//     the copy running as a scripted agent (on Windows an npm-style .cmd
//     shim, exactly how the real Claude Code is launched);
//   - a git repo with many files (and a Git LFS file when git-lfs is
//     installed) in a folder with spaces.
//
// It then runs `rw run` as a real child process, kills it hard while an
// agent is working (as closing the window does), and checks that the agent
// died with it and the finished steps survived; then `rw history`,
// `rw resume`, `rw undo` and `rw undo --redo`. On Windows it also reports
// OneDrive and Microsoft Defender, and with --onedrive runs a task inside
// the OneDrive folder.

const (
	selftestAgentCmd = "__selftest-agent" // hidden: rw acting as the scripted agent CLI
	envSelftestDir   = "RW_SELFTEST_DIR"  // where the agent writes its pid and call log
	envSelftestHang  = "RW_SELFTEST_HANG" // "1": the combine step hangs until killed
	envSelftestAs    = "RW_SELFTEST_AS"   // the CLI the agent stands in for (default claude)
)

// The scripted task. The agent recognises its steps by these texts, so the
// task itself must not contain them.
const selftestTask = "Relayweft self-test task: create two small text files in parallel, then combine both of them into a third file, and keep everything else in the repository unchanged"

var (
	stFileA = filepath.ToSlash(filepath.Join("notes ä", "a file.txt"))
	stFileB = "b.txt"
	stFileC = "c.txt"
	reWrite = regexp.MustCompile(`write <<([^>]+)>> containing <<([^>]+)>>`)
)

const stCombine = "combine <<a>> and <<b>> into <<c>>"

type stMark int

const (
	markOK stMark = iota
	markWarn
	markFail
	markInfo
	markSkip
)

type selftest struct {
	out     io.Writer
	work    string // everything lives below this folder
	profile string // the fake user profile
	bin     string // the copy of rw the children run
	shim    string // the fake claude CLI
	cfg     string // config file for the children
	logs    string // child output, one file per command
	lfs     bool
	blobSum string // sha256 of the LFS file
	counts  [5]int
}

func cmdSelftest(args []string) error {
	fs := flag.NewFlagSet("rw selftest", flag.ExitOnError)
	files := fs.Int("files", 2000, "tracked files in the test repo")
	keep := fs.Bool("keep", false, "keep the work folder (it is always kept when a check fails)")
	oneDrive := fs.Bool("onedrive", false, "also run a task in a test repo inside your OneDrive folder (removed afterwards; OneDrive may keep a copy in its recycle bin)")
	in := fs.String("in", "", "create the work folder in this folder (default: the temp folder)")
	sandboxMode := fs.String("sandbox", "auto", "auto: also run tasks in a container sandbox when docker or podman is there; off; only: just that")
	firstOnly := fs.Bool("first-run", false, "only the guided first run (rw setup) from fresh profiles, timed")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: rw selftest [--files 2000] [--onedrive] [--sandbox auto|off|only] [--keep] [--in <folder>]

Runs the automated part of the Windows test pass in a throwaway folder,
with a scripted agent instead of Codex or Claude (no quota is used):
a user profile and project path with spaces and non-ASCII letters, a
.cmd-shim CLI, many files and Git LFS, a task killed mid-run (as closing
the window does), then rw resume, rw undo and rw undo --redo. It also
reports OneDrive and Microsoft Defender. With docker or podman it also
runs tasks with the agent in a container sandbox (a scripted agent in a
small test image). Your own repos and config are not touched. What is
left to check by hand is printed at the end.
`)
	}
	parseFlags(fs, args)
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *sandboxMode != "auto" && *sandboxMode != "off" && *sandboxMode != "only" {
		return fmt.Errorf("--sandbox must be auto, off or only")
	}
	if *files < 1 {
		*files = 1
	}
	// Ctrl+C or a crash of this rw takes the test's rw and agents along.
	_ = proc.Guard()

	base := *in
	if base == "" {
		base = os.TempDir()
	}
	base, err := filepath.Abs(base)
	if err != nil {
		return err
	}
	t := &selftest{out: os.Stdout, work: filepath.Join(base, "rw selftest "+time.Now().Format("0102-150405"))}
	t.profile = filepath.Join(t.work, "Users", "Test User (äö)")
	t.logs = filepath.Join(t.work, "logs")
	if err := os.MkdirAll(t.logs, 0o755); err != nil {
		return err
	}
	fmt.Fprintf(t.out, "rw selftest %s - the automated part of the Windows test pass (ROADMAP 1.6)\n", version)
	fmt.Fprintf(t.out, "work folder: %s\n", t.work)
	diag.Logf("selftest: work folder %s", t.work)

	start := time.Now()
	proj := filepath.Join(t.work, "projects", "my project ä")
	if *firstOnly {
		*files = 1
	}
	ready := t.setup(proj, *files)
	switch {
	case ready && *firstOnly:
		t.section("First run: guided setup from a fresh profile")
		t.firstRun()
	case ready && *sandboxMode == "only":
	case ready:
		t.section("Environment")
		t.environment()
		t.section("Task killed mid-run, then resume and undo")
		t.scenario(proj, filepath.Join(t.work, "agent main"), true)
		if *oneDrive {
			t.section("A task inside OneDrive")
			t.oneDriveRun()
		}
		t.section("First run: guided setup from a fresh profile")
		t.firstRun()
	}
	if ready && !*firstOnly && *sandboxMode != "off" {
		t.section("Agents in a container sandbox")
		t.sandboxScenario()
	}

	if !*firstOnly {
		t.section("Still to do by hand")
		fmt.Fprint(t.out, manualSteps)
	}
	fmt.Fprintf(t.out, "\n%d ok, %d warning(s), %d failed, %d skipped in %s\n",
		t.counts[markOK], t.counts[markWarn], t.counts[markFail], t.counts[markSkip], time.Since(start).Round(time.Second))
	diag.Logf("selftest: %d ok, %d warn, %d failed, %d skipped", t.counts[markOK], t.counts[markWarn], t.counts[markFail], t.counts[markSkip])
	if t.counts[markFail] > 0 || *keep {
		fmt.Fprintf(t.out, "The work folder is kept: %s\n", t.work)
		if t.counts[markFail] > 0 {
			fmt.Fprintln(t.out, "Its logs folder has the output of every rw command, and AppData below the test profile has their")
			fmt.Fprintln(t.out, "debug logs and task states. Zip the folder and send it with `rw bugreport`.")
			return fmt.Errorf("%d check(s) failed", t.counts[markFail])
		}
		return nil
	}
	if err := removeAllRetry(t.work); err != nil {
		fmt.Fprintf(t.out, "note: could not remove the work folder %s: %v\n", t.work, err)
	}
	return nil
}

const manualSteps = `These need you at the keyboard; run each in a git repo of your own with a real task:
  1. Sleep and resume: start a task, put the PC to sleep (Start > Power > Sleep) while an agent works,
     wake it after a minute or more. rw must either carry on or stop with a clear message, never hang;
     if it stopped, ` + "`rw resume`" + ` continues the task.
  2. Closing the window: start a task in Windows Terminal and close the tab while an agent works;
     then do the same in the old console (run rw from conhost.exe, or set Terminal's default terminal
     to "Windows Console Host"). Each time, open a new window: Task Manager shows no codex/claude/node
     left over, ` + "`rw history`" + ` lists the task as interrupted, ` + "`rw resume`" + ` finishes it and ` + "`rw undo`" + ` reverts it.
  After any problem, run ` + "`rw bugreport`" + ` and send the zip.
`

func (t *selftest) section(title string) {
	fmt.Fprintf(t.out, "\n%s\n", lipgloss.NewStyle().Bold(true).Render(title))
}

func (t *selftest) check(m stMark, name, format string, args ...any) {
	t.counts[m]++
	detail := fmt.Sprintf(format, args...)
	labels := [...]string{stOK.Render("ok  "), stRev.Render("warn"), stErr.Render("FAIL"), stMuted.Render("info"), stMuted.Render("skip")}
	words := [...]string{"ok", "warn", "FAIL", "info", "skip"}
	fmt.Fprintf(t.out, "%s %-11s %s\n", labels[m], name, detail)
	diag.Logf("selftest %s %s: %s", words[m], name, detail)
}

// setup creates the profile, the rw copy, the fake CLI, the config and the
// repo. It reports false when the test cannot go on.
func (t *selftest) setup(proj string, files int) bool {
	t.section("Setup")
	roaming, local := filepath.Join(t.profile, "AppData", "Roaming"), filepath.Join(t.profile, "AppData", "Local")
	for _, d := range []string{roaming, filepath.Join(local, "Temp"), filepath.Join(t.profile, ".config"), filepath.Join(t.profile, ".cache")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.check(markFail, "profile", "%v", err)
			return false
		}
	}
	t.check(markOK, "profile", "a user profile with spaces and non-ASCII letters: %s", t.profile)

	// rw where a per-user install puts it; the CLI where npm puts claude.cmd.
	self, err := os.Executable()
	if err != nil {
		t.check(markFail, "binary", "cannot find this rw: %v", err)
		return false
	}
	exe := "rw"
	if runtime.GOOS == "windows" {
		exe = "rw.exe"
	}
	t.bin = filepath.Join(local, "Programs", "rw tools", exe)
	if err := copyExe(self, t.bin); err != nil {
		t.check(markFail, "binary", "copy rw to %s: %v", t.bin, err)
		return false
	}
	began := time.Now()
	out, err := t.rw(filepath.Dir(t.bin), "version-first-start", nil, "version")
	took := time.Since(began)
	if err != nil || !strings.Contains(out, "relayweft") {
		t.check(markFail, "binary", "the copy at %s did not start: %v %s (blocked by antivirus or SmartScreen?)", t.bin, err, oneLine(out, 200))
		return false
	}
	m := markOK
	note := ""
	if took > 3*time.Second {
		m, note = markWarn, " - slow: an antivirus scan of new executables? (agents and git start often)"
	}
	t.check(m, "binary", "a fresh copy in a folder with spaces starts in %s%s", took.Round(10*time.Millisecond), note)

	if t.shim, err = writeAgentShim(filepath.Join(roaming, "npm"), t.bin, "claude"); err != nil {
		t.check(markFail, "agent cli", "%v", err)
		return false
	}
	kind := "a shell script"
	if runtime.GOOS == "windows" {
		kind = "an npm-style .cmd shim"
	}
	t.check(markOK, "agent cli", "scripted claude CLI as %s: %s", kind, t.shim)

	cfg := map[string]any{
		"providers": map[string]any{
			"claude": map[string]any{"command": t.shim},
			"codex":  map[string]any{"disabled": true},
		},
		"notify": map[string]any{"enabled": false},
	}
	data, _ := yaml.Marshal(cfg)
	t.cfg = filepath.Join(roaming, "relayweft", "relayweft.yaml")
	os.MkdirAll(filepath.Dir(t.cfg), 0o755)
	if err := os.WriteFile(t.cfg, data, 0o644); err != nil {
		t.check(markFail, "config", "%v", err)
		return false
	}

	began = time.Now()
	if err := t.makeRepo(proj, files); err != nil {
		t.check(markFail, "repo", "%v", err)
		return false
	}
	what := fmt.Sprintf("%d files", files)
	if t.lfs {
		what += " and a 2 MB Git LFS file"
	}
	t.check(markOK, "repo", "%s committed in %s: %s", what, time.Since(began).Round(100*time.Millisecond), proj)
	if !t.lfs {
		t.check(markSkip, "lfs", "git-lfs is not installed (git lfs version failed)")
	}
	return true
}

func (t *selftest) makeRepo(dir string, files int) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	git := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = t.childEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, oneLine(string(out), 300))
		}
		return nil
	}
	if err := git("init", "-q"); err != nil {
		return err
	}
	for i := 0; i < files; i++ {
		p := filepath.Join(dir, "src", fmt.Sprintf("pkg %02d", i%20), fmt.Sprintf("file%04d.txt", i))
		if i < 20 {
			os.MkdirAll(filepath.Dir(p), 0o755)
		}
		if err := os.WriteFile(p, []byte(fmt.Sprintf("file %d\n", i)), 0o644); err != nil {
			return err
		}
	}
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# rw selftest\n"), 0o644)
	if exec.Command("git", "lfs", "version").Run() == nil {
		if err := git("lfs", "install", "--local"); err == nil {
			if err := git("lfs", "track", "*.bin"); err != nil {
				return err
			}
			blob := make([]byte, 2<<20)
			rand.New(rand.NewSource(1)).Read(blob)
			sum := sha256.Sum256(blob)
			t.blobSum = hex.EncodeToString(sum[:])
			os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
			if err := os.WriteFile(filepath.Join(dir, "assets", "blob.bin"), blob, 0o644); err != nil {
				return err
			}
			t.lfs = true
		}
	}
	if err := git("add", "-A"); err != nil {
		return err
	}
	return git("-c", "user.name=rw selftest", "-c", "user.email=selftest@localhost", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "selftest base")
}

// childEnv is this process's environment with the user folders moved into
// the test profile.
func (t *selftest) childEnv(extra ...string) []string {
	return profileEnv(t.profile, extra...)
}

// profileEnv is this process's environment with the user folders moved
// into profile.
func profileEnv(profile string, extra ...string) []string {
	set := map[string]string{envSelftestDir: "", envSelftestHang: "", envSelftestAs: "", envSetupInteractive: ""}
	if runtime.GOOS == "windows" {
		set["APPDATA"] = filepath.Join(profile, "AppData", "Roaming")
		set["LOCALAPPDATA"] = filepath.Join(profile, "AppData", "Local")
		set["TEMP"] = filepath.Join(profile, "AppData", "Local", "Temp")
		set["TMP"] = set["TEMP"]
	} else {
		set["XDG_CONFIG_HOME"] = filepath.Join(profile, ".config")
		set["XDG_CACHE_HOME"] = filepath.Join(profile, ".cache")
		if runtime.GOOS == "darwin" {
			// os.UserConfigDir and UserCacheDir read only HOME there; git
			// identity comes from the command lines, not ~/.gitconfig.
			set["HOME"] = profile
		}
	}
	for _, kv := range extra {
		k, v, _ := strings.Cut(kv, "=")
		set[k] = v
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := lookupFold(set, k); !ok {
			env = append(env, kv)
		}
	}
	for k, v := range set {
		if v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func lookupFold(m map[string]string, k string) (string, bool) {
	for mk, v := range m {
		if mk == k || (runtime.GOOS == "windows" && strings.EqualFold(mk, k)) {
			return v, true
		}
	}
	return "", false
}

// rw runs the test's rw copy in dir and returns its output, which is also
// saved in the logs folder.
func (t *selftest) rw(dir, logName string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.Command(t.bin, args...)
	cmd.Dir = dir
	cmd.Env = t.childEnv(extraEnv...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := runTimeout(cmd, 5*time.Minute)
	os.WriteFile(filepath.Join(t.logs, logName+".log"), buf.Bytes(), 0o644)
	return buf.String(), err
}

func runTimeout(cmd *exec.Cmd, d time.Duration) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		cmd.Process.Kill()
		<-done
		return fmt.Errorf("no result after %s", d)
	}
}

// scenario runs the scripted task in proj. With kill, rw is killed while
// the last step's agent works and the task is resumed; then it is undone
// and redone.
func (t *selftest) scenario(proj, stateDir string, kill bool) {
	os.MkdirAll(stateDir, 0o755)
	env := []string{envSelftestDir + "=" + stateDir}
	tag := filepath.Base(stateDir)
	runArgs := []string{"run", "--config", t.cfg, "--provider", "claude", selftestTask}
	if !kill {
		out, err := t.rw(proj, tag+" run", env, runArgs...)
		if err != nil {
			t.check(markFail, "run", "rw run failed: %v\n%s", err, tailLines(out, 15))
			return
		}
		t.check(markOK, "run", "rw run finished the task")
	} else if !t.killMidRun(proj, stateDir, env, runArgs) {
		return
	}

	want := t.wantFiles()
	if !t.filesAre(proj, want, "result", "every step's file is in the tree; c.txt combines the other two") {
		return
	}
	if t.lfs {
		if sum, err := fileSum(filepath.Join(proj, "assets", "blob.bin")); err != nil || sum != t.blobSum {
			t.check(markFail, "lfs", "assets/blob.bin changed or is missing (%v): the LFS file must be untouched", err)
		} else {
			t.check(markOK, "lfs", "the LFS file is intact")
		}
	}

	out, err := t.rw(proj, tag+" undo", nil, "undo", "--yes")
	if err != nil {
		t.check(markFail, "undo", "rw undo --yes failed: %v\n%s", err, tailLines(out, 15))
		return
	}
	if st, err := gitStatus(proj); err != nil || st != "" {
		t.check(markFail, "undo", "after rw undo the tree is not clean (%v):\n%s", err, st)
		return
	}
	t.check(markOK, "undo", "rw undo --yes put the tree back (git status is clean)")
	if !kill {
		return
	}
	out, err = t.rw(proj, tag+" redo", nil, "undo", "--redo", "--yes")
	if err != nil {
		t.check(markFail, "redo", "rw undo --redo --yes failed: %v\n%s", err, tailLines(out, 15))
		return
	}
	t.filesAre(proj, want, "redo", "rw undo --redo --yes put the task's files back")
}

// killMidRun starts rw run, kills it hard while the combine agent runs,
// checks what is left, and resumes the task.
func (t *selftest) killMidRun(proj, stateDir string, env, runArgs []string) bool {
	logf, err := os.Create(filepath.Join(t.logs, filepath.Base(stateDir)+" run (killed).log"))
	if err != nil {
		t.check(markFail, "run", "%v", err)
		return false
	}
	defer logf.Close()
	cmd := exec.Command(t.bin, runArgs...)
	cmd.Dir = proj
	cmd.Env = t.childEnv(append(env, envSelftestHang+"=1")...)
	cmd.Stdout, cmd.Stderr = logf, logf
	began := time.Now()
	if err := cmd.Start(); err != nil {
		t.check(markFail, "run", "start rw run: %v", err)
		return false
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// The combine step's agent writes its pid when it starts hanging.
	pidFile := filepath.Join(stateDir, "agent.pid")
	agent := 0
	deadline := time.After(4 * time.Minute)
wait:
	for {
		select {
		case err := <-exited:
			t.check(markFail, "run", "rw run ended before the last step (%v):\n%s", err, tailLines(fileText(logf.Name()), 15))
			return false
		case <-deadline:
			cmd.Process.Kill()
			<-exited
			t.check(markFail, "run", "the last step's agent did not start within 4 minutes:\n%s", tailLines(fileText(logf.Name()), 15))
			return false
		case <-time.After(50 * time.Millisecond):
			if b, err := os.ReadFile(pidFile); err == nil {
				if agent, err = strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
					break wait
				}
			}
		}
	}
	t.check(markOK, "run", "rw run planned the task and finished 3 steps; the last step's agent (pid %d) is working after %s",
		agent, time.Since(began).Round(100*time.Millisecond))

	// The agent prints its session id before it writes its pid, and rw
	// saves the id in the task state as soon as it reads that line. Under
	// load, saving can take longer than the 50ms poll above. A kill before
	// the save is a real case, and rw handles it with a fresh agent. But
	// this check is about continuing the session, so the kill waits for
	// the save: the kill point no longer depends on timing.
	sid := fmt.Sprintf("selftest-%d", agent)
	saved, ok := waitSessionSaved(profileConfigDir(t.profile), sid, 30*time.Second)
	if !ok {
		cmd.Process.Kill()
		<-exited
		t.check(markFail, "run", "the agent reported session %s, but rw had not saved it in the task state after %s:\n%s", sid, saved.Round(time.Millisecond), tailLines(fileText(logf.Name()), 15))
		return false
	}

	// TerminateProcess, as when the console is closed and Windows ends rw.
	if err := cmd.Process.Kill(); err != nil {
		t.check(markFail, "kill", "could not kill rw run: %v", err)
		return false
	}
	<-exited
	t.check(markOK, "kill", "rw run killed hard (pid %d) once it had saved the agent's session (waited %s for that), as closing the window does",
		cmd.Process.Pid, saved.Round(time.Millisecond))

	gone := waitGone(agent, 10*time.Second)
	switch {
	case gone:
		t.check(markOK, "orphans", "the agent died with rw: nothing is left running")
	case runtime.GOOS == "windows":
		t.check(markFail, "orphans", "the agent (pid %d) is still running 10s after rw died: the job object did not kill it", agent)
		killTree(agent)
	default:
		// Unix has no job objects: the next rw that takes the pool slot
		// kills the agent's process group (proc.ReapOrphans).
		t.check(markInfo, "orphans", "the agent (pid %d) outlives rw on %s until the next rw reaps it", agent, runtime.GOOS)
	}

	// The finished steps were merged into the tree before rw died.
	partial := t.wantFiles()
	delete(partial, stFileC)
	if !t.filesAre(proj, partial, "saved work", "the two finished steps' files are in the tree") {
		return false
	}
	if _, err := os.Stat(filepath.Join(proj, stFileC)); err == nil {
		t.check(markFail, "saved work", "c.txt exists although its step was killed")
		return false
	}

	out, err := t.rw(proj, "history", nil, "history")
	if err != nil || !strings.Contains(out, "interrupted") {
		t.check(markFail, "history", "rw history does not list the task as interrupted (%v):\n%s", err, tailLines(out, 10))
		return false
	}
	t.check(markOK, "history", "rw history lists the task as interrupted")

	before := len(readCalls(stateDir))
	began = time.Now()
	out, err = t.rw(proj, "resume", env, "resume", "--config", t.cfg, "--provider", "claude")
	if err != nil {
		t.check(markFail, "resume", "rw resume failed: %v\n%s", err, tailLines(out, 15))
		return false
	}
	var rerun []string
	for _, c := range readCalls(stateDir)[before:] {
		if c == "plan" || c == "write" {
			rerun = append(rerun, c)
		}
	}
	if len(rerun) > 0 {
		t.check(markFail, "resume", "rw resume ran the planner or finished steps again: %s", strings.Join(rerun, ", "))
		return false
	}
	t.check(markOK, "resume", "rw resume finished the task in %s without re-running the planner or finished steps",
		time.Since(began).Round(100*time.Millisecond))
	// The interrupted step continued its agent's own session, in the folder
	// it ran in (a pool worktree), instead of starting over.
	want := fmt.Sprintf("resume:selftest-%d", agent)
	continued, fresh := false, false
	for _, c := range readCalls(stateDir)[before:] {
		continued = continued || c == want
		fresh = fresh || c == "combine"
	}
	wdKilled, wdResumed := fileText(filepath.Join(stateDir, "agent.wd")), fileText(filepath.Join(stateDir, "resume.wd"))
	switch {
	case !continued || fresh:
		t.check(markFail, "resume", "the interrupted step did not continue its agent's session %s (calls: %s); rw resume said:\n%s", strings.TrimPrefix(want, "resume:"),
			strings.Join(readCalls(stateDir)[before:], ", "), resumeReasons(out))
		return false
	case wdKilled == "" || !orchestrator.SamePath(wdKilled, wdResumed):
		t.check(markFail, "resume", "the interrupted step's session was continued in %s, not where it ran (%s)", wdResumed, wdKilled)
		return false
	default:
		t.check(markOK, "resume", "the interrupted step continued its agent's session in the folder it ran in")
	}
	if !gone && runtime.GOOS != "windows" {
		if waitGone(agent, 5*time.Second) {
			t.check(markOK, "orphans", "the next rw reaped the agent rw left behind")
		} else {
			t.check(markWarn, "orphans", "the agent (pid %d) is still running after the resume; killing it", agent)
			killTree(agent)
		}
	}
	return true
}

// wantFiles is what the scripted task leaves in the tree.
func (t *selftest) wantFiles() map[string]string {
	return map[string]string{
		stFileA: "selftest-a\n",
		stFileB: "selftest-b\n",
		stFileC: "selftest-a\nselftest-b\n",
	}
}

func (t *selftest) filesAre(proj string, want map[string]string, name, okText string) bool {
	for rel, w := range want {
		b, err := os.ReadFile(filepath.Join(proj, filepath.FromSlash(rel)))
		if err != nil {
			t.check(markFail, name, "%s is missing: %v", rel, err)
			return false
		}
		// core.autocrlf may turn LF into CRLF on the way through git.
		if got := strings.ReplaceAll(string(b), "\r\n", "\n"); got != w {
			t.check(markFail, name, "%s is %q, want %q", rel, got, w)
			return false
		}
	}
	t.check(markOK, name, "%s", okText)
	return true
}

func gitStatus(dir string) (string, error) {
	cmd := exec.Command("git", proc.GitArgs("status", "--porcelain", "--ignore-submodules=all")...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func fileSum(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// resumeReasons picks the lines of rw resume's output that say why an
// interrupted step was or was not continued.
func resumeReasons(out string) string {
	var keep []string
	for _, l := range strings.Split(out, "\n") {
		for _, k := range []string{"interrupted", "fresh agent", "cannot be used", "could not be continued", "continuing", "starts over"} {
			if strings.Contains(l, k) {
				keep = append(keep, strings.TrimRight(l, "\r"))
				break
			}
		}
	}
	if len(keep) == 0 {
		return tailLines(out, 15)
	}
	return strings.Join(keep, "\n")
}

// profileConfigDir is os.UserConfigDir for a rw started with
// profileEnv(profile).
func profileConfigDir(profile string) string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(profile, "AppData", "Roaming")
	case "darwin":
		return filepath.Join(profile, "Library", "Application Support")
	}
	return filepath.Join(profile, ".config")
}

// waitSessionSaved waits until a task state under cfgDir records a running
// step with session sid, for at most d. It returns how long it waited.
func waitSessionSaved(cfgDir, sid string, d time.Duration) (time.Duration, bool) {
	began := time.Now()
	for {
		files, _ := filepath.Glob(filepath.Join(cfgDir, "relayweft", "tasks", "*.json"))
		for _, f := range files {
			var st struct {
				Running map[string]struct {
					Session string `json:"session"`
				} `json:"running"`
			}
			if b, err := os.ReadFile(f); err == nil && json.Unmarshal(b, &st) == nil {
				for _, r := range st.Running {
					if r.Session == sid {
						return time.Since(began), true
					}
				}
			}
		}
		if time.Since(began) > d {
			return time.Since(began), false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitGone(pid int, d time.Duration) bool {
	for end := time.Now().Add(d); ; {
		if !proc.Alive(pid) {
			return true
		}
		if time.Now().After(end) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func killTree(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		p.Kill()
	}
}

func fileText(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\r\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return "    " + strings.Join(lines, "\n    ")
}

func readCalls(stateDir string) []string {
	return strings.Fields(fileText(filepath.Join(stateDir, "calls.log")))
}

func copyExe(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}

// removeAllRetry removes a folder, retrying while an antivirus scan or the
// search indexer still holds a file in it.
func removeAllRetry(dir string) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return err
}

// oneDriveRun runs the task (without the kill) in a repo inside OneDrive.
func (t *selftest) oneDriveRun() {
	roots := oneDriveRoots()
	if len(roots) == 0 {
		t.check(markSkip, "onedrive", "no OneDrive folder found")
		return
	}
	proj := filepath.Join(roots[0], "rw selftest "+time.Now().Format("0102-150405"))
	defer func() {
		if err := removeAllRetry(proj); err != nil {
			t.check(markWarn, "onedrive", "could not remove %s: %v", proj, err)
		}
	}()
	if err := t.makeRepo(proj, 200); err != nil {
		t.check(markFail, "onedrive", "create a repo in %s: %v", proj, err)
		return
	}
	t.check(markOK, "onedrive", "a repo inside OneDrive: %s", proj)
	t.scenario(proj, filepath.Join(t.work, "agent onedrive"), false)
}

// --- the scripted agent ---------------------------------------------------

// selftestQuickCheck answers the commands rw setup and rw doctor run, as a
// logged-in Claude Code or a logged-out Codex of the tested versions. It
// reports whether args were one of them.
func selftestQuickCheck(as string, args []string) bool {
	if as == "" {
		as = "claude"
	}
	switch strings.Join(args, " ") {
	case "--version":
		fmt.Println(config.Default().Providers[as].TestedVersion)
	case "auth status":
		fmt.Println(`{"loggedIn": true, "authMethod": "claude.ai", "subscriptionType": "selftest"}`)
	case "login status":
		fmt.Fprintln(os.Stderr, "Not logged in")
		os.Exit(1)
	default:
		return false
	}
	return true
}

// cmdSelftestAgent speaks enough of `claude -p --output-format stream-json`
// for the orchestrator: a fixed plan, approving reviews, and steps that
// write files in the agent's working directory.
func cmdSelftestAgent() {
	if selftestQuickCheck(os.Getenv(envSelftestAs), os.Args[2:]) {
		return
	}
	in, _ := io.ReadAll(os.Stdin)
	prompt := string(in)
	dir := os.Getenv(envSelftestDir)
	note := func(what string) {
		if dir == "" {
			return
		}
		if f, err := os.OpenFile(filepath.Join(dir, "calls.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintln(f, what)
			f.Close()
		}
	}
	enc := json.NewEncoder(os.Stdout)
	sid := fmt.Sprintf("selftest-%d", os.Getpid())
	resumed := ""
	for i, a := range os.Args {
		if a == "--resume" && i+1 < len(os.Args) {
			resumed = os.Args[i+1]
			sid = resumed // a resumed session keeps its id
		}
	}
	enc.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": sid, "model": "selftest"})
	result := func(text string) {
		enc.Encode(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text,
			"session_id": sid, "usage": map[string]any{"input_tokens": 100, "output_tokens": 10}})
	}
	wd, _ := os.Getwd()
	write := func(rel, content string) error {
		p := filepath.Join(wd, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
		// Real CLIs report absolute paths.
		enc.Encode(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "name": "Write", "input": map[string]any{"file_path": p}},
		}}})
		return nil
	}
	fail := func(err error) {
		enc.Encode(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": err.Error(), "session_id": sid})
		os.Exit(1)
	}

	combine := func() {
		a, errA := os.ReadFile(filepath.Join(wd, filepath.FromSlash(stFileA)))
		b, errB := os.ReadFile(filepath.Join(wd, filepath.FromSlash(stFileB)))
		if err := errors.Join(errA, errB); err != nil {
			fail(fmt.Errorf("the finished steps' files are not here: %w", err))
		}
		norm := func(b []byte) string { return strings.ReplaceAll(string(b), "\r\n", "\n") }
		if err := write(stFileC, norm(a)+norm(b)); err != nil {
			fail(err)
		}
		result("combined")
	}

	switch {
	case strings.Contains(prompt, runner.MarkerResume):
		// rw resume continues the combine step's session (the only step
		// that is interrupted): record which session and where.
		note("resume:" + resumed)
		if dir != "" {
			os.WriteFile(filepath.Join(dir, "resume.wd"), []byte(wd), 0o644)
		}
		combine()
	case strings.Contains(prompt, runner.MarkerPlanReview), strings.Contains(prompt, runner.MarkerFinalReview),
		strings.Contains(prompt, runner.MarkerErrorReview):
		note("review")
		result(`{"approve": true, "advice": "ok", "issues": []}`)
	case strings.Contains(prompt, runner.MarkerJudge):
		note("judge")
		result("A")
	case strings.Contains(prompt, runner.MarkerPlan):
		note("plan")
		plan := map[string]any{
			"summary": "Look around, write two files in parallel, then combine them.",
			"subtasks": []map[string]any{
				{"id": "look", "title": "look around", "kind": "explore", "prompt": "List the top-level files.", "files": []string{}},
				{"id": "a", "title": "write a", "kind": "edit", "prompt": "write <<" + stFileA + ">> containing <<selftest-a>>", "files": []string{stFileA}},
				{"id": "b", "title": "write b", "kind": "edit", "prompt": "write <<" + stFileB + ">> containing <<selftest-b>>", "files": []string{stFileB}},
				{"id": "c", "title": "combine", "kind": "edit", "prompt": strings.NewReplacer("<<a>>", "<<"+stFileA+">>", "<<b>>", "<<"+stFileB+">>", "<<c>>", "<<"+stFileC+">>").Replace(stCombine),
					"files": []string{stFileC}, "depends_on": []string{"a", "b"}},
			},
		}
		b, _ := json.MarshalIndent(plan, "", "  ")
		result("```json\n" + string(b) + "\n```")
	case strings.Contains(prompt, "combine <<"):
		if os.Getenv(envSelftestHang) == "1" && dir != "" {
			// Wait to be killed with rw; the pid tells the test who to watch.
			note("hang")
			os.WriteFile(filepath.Join(dir, "agent.wd"), []byte(wd), 0o644)
			os.WriteFile(filepath.Join(dir, "agent.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)
			time.Sleep(10 * time.Minute)
			fail(errors.New("selftest agent: was not killed within 10 minutes"))
		}
		note("combine")
		combine()
	default:
		if m := reWrite.FindStringSubmatch(prompt); m != nil {
			note("write")
			if err := write(m[1], m[2]+"\n"); err != nil {
				fail(err)
			}
			result("wrote " + m[1])
			return
		}
		note("explore")
		result("The repository has a README.md, src/ and assets/.")
	}
}
