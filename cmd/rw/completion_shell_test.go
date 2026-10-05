package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/orchestrator"
)

// The completion scripts in the real shells, with rw built into a folder
// with a space in its name. A shell that is not installed is skipped,
// unless RW_TEST_SHELLS (comma-separated, CI) requires it.
//
// Each driver loads `rw completion <shell>` and completes the lines in
// shellCases. bash gets COMP_LINE/COMP_WORDS as readline sets them, zsh
// gets stubs for _describe/_files (no completion widget runs in a
// script), fish and PowerShell complete for real (complete -C,
// TabExpansion2).
var shellCases = []struct {
	line string
	want []string // substrings of the completions, each in at least one
}{
	{"rw re", []string{"review", "resume", "report"}},
	{"rw run --pre", []string{"--prefer"}},
	{"rw run --prefer worker=c", []string{"claude", "codex"}},
	{"rw run --route worker=claude:op", []string{"opus"}},
	{"rw run --provider ", []string{"claude", "gemini"}},
	{"rw resume ", []string{"20261005-1-aaaa-task-1"}},
	{"rw completion p", []string{"powershell"}},
}

const shellBash = `
eval "$(rw completion bash)"
probe() {
    COMP_LINE="$1"
    COMP_POINT=${#1}
    local tmp="${1//=/ = }"
    tmp="${tmp//:/ : }"
    read -r -a COMP_WORDS <<< "$tmp"
    case "$1" in *" ") COMP_WORDS+=("") ;; esac
    COMP_CWORD=$((${#COMP_WORDS[@]} - 1))
    COMPREPLY=()
    _rw_complete
    echo "## $1"
    local c
    for c in "${COMPREPLY[@]}"; do echo "$c"; done
}
`

const shellZsh = `
compdef() { :; }
_files() { echo FILES; }
_describe() { local arr=$4; print -rl -- "${(@P)arr}"; }
eval "$(rw completion zsh)"
probe() {
    words=(${(z)1})
    [[ "$1" == *" " ]] && words+=("")
    CURRENT=${#words}
    echo "## $1"
    _rw
}
`

const shellFish = `
rw completion fish | source
function probe
    echo "## $argv[1]"
    complete -C "$argv[1]"
end
`

const shellPS = `
$ErrorActionPreference = 'Stop'
rw completion powershell | Out-String | Invoke-Expression
function probe($line) {
    "## $line"
    $r = TabExpansion2 -inputScript $line -cursorColumn $line.Length
    $r.CompletionMatches | ForEach-Object { $_.CompletionText }
}
`

func TestCompletionInRealShells(t *testing.T) {
	if testing.Short() {
		t.Skip("builds rw")
	}
	required := map[string]bool{}
	for _, s := range strings.Split(os.Getenv("RW_TEST_SHELLS"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			required[s] = true
		}
	}
	type shell struct {
		name, script, ext string
		args              []string
	}
	shells := []shell{
		{"bash", shellBash, ".sh", []string{"--norc", "--noprofile"}},
		{"zsh", shellZsh, ".zsh", []string{"-f"}},
		{"fish", shellFish, ".fish", []string{"--no-config"}},
		{"pwsh", shellPS, ".ps1", []string{"-NoProfile", "-NonInteractive", "-File"}},
		{"powershell", shellPS, ".ps1", []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File"}},
	}
	type found struct {
		shell
		path string
	}
	var run []found
	for _, s := range shells {
		p := shellPath(s.name)
		if p == "" {
			if required[s.name] {
				t.Errorf("%s is required (RW_TEST_SHELLS) but not installed", s.name)
			}
			continue
		}
		run = append(run, found{s, p})
	}
	if len(run) == 0 {
		t.Skip("no shell to test")
	}

	bin := filepath.Join(t.TempDir(), "bin dir")
	exe := filepath.Join(bin, "rw")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	isolate(t)
	project := t.TempDir()
	chdir(t, project)
	writeTaskState(t, orchestrator.TaskState{ID: "20261005-1-aaaa-task-1", Task: "fix the parser", Dir: project, Status: "running", Created: time.Now()})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, s := range run {
		t.Run(s.name, func(t *testing.T) {
			var script strings.Builder
			script.WriteString(s.script)
			for _, c := range shellCases {
				script.WriteString("probe '" + c.line + "'\n")
			}
			f := filepath.Join(t.TempDir(), "drive"+s.ext)
			if err := os.WriteFile(f, []byte(script.String()), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(s.path, append(s.args, f)...)
			cmd.Dir = project
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			got := map[string][]string{}
			var cur string
			for _, l := range strings.Split(strings.ReplaceAll(string(out), "\r", ""), "\n") {
				if h, ok := strings.CutPrefix(l, "## "); ok {
					cur = h
					continue
				}
				if l != "" {
					got[cur] = append(got[cur], l)
				}
			}
			for _, c := range shellCases {
				for _, w := range c.want {
					if !containsSub(got[c.line], w) {
						t.Errorf("%q: no completion with %q (got %q)", c.line, w, got[c.line])
					}
				}
			}
			t.Logf("output:\n%s", out) // shown with -v, and on failure
		})
	}
}

func containsSub(list []string, s string) bool {
	for _, l := range list {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// shellPath finds a shell; on Windows, bash is Git's (System32\bash.exe
// is WSL's launcher).
func shellPath(name string) string {
	if runtime.GOOS == "windows" && name == "bash" {
		for _, p := range []string{
			filepath.Join(os.Getenv("ProgramFiles"), "Git", "bin", "bash.exe"),
			`C:\Program Files\Git\bin\bash.exe`,
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		return ""
	}
	if runtime.GOOS != "windows" && name == "powershell" {
		return "" // Windows PowerShell 5.1 is Windows only
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}
