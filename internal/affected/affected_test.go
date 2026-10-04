package affected

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/sparkz400/switchyard/internal/proc"
)

// tree writes files (slash paths) under a new temp folder.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func sel(t *testing.T, dir, cmd string, files ...string) Plan {
	t.Helper()
	return Select(context.Background(), cmd, "", Input{Root: dir, Dir: dir, Files: files})
}

// allowedBy reports whether an agent with these allowed prefixes may run
// cmd (the rule Claude's "X" and "X *" entries implement).
func allowedBy(cmd string, prefixes []string) bool {
	for _, p := range prefixes {
		if cmd == p || strings.HasPrefix(cmd, p+" ") {
			return true
		}
	}
	return false
}

// checkAllowed fails when a narrowed command is not in the allowlist an
// agent gets for cmd: sy and the agents must be able to run the same.
func checkAllowed(t *testing.T, dir, cmd, template string, p Plan) {
	t.Helper()
	pre, _ := Allowed(dir, cmd, template)
	for _, r := range p.Run {
		if !allowedBy(r, append([]string{cmd}, pre...)) {
			t.Errorf("narrowed %q is not allowed by %q", r, append([]string{cmd}, pre...))
		}
	}
}

func TestSelectBasics(t *testing.T) {
	dir := t.TempDir()
	if p := Select(context.Background(), "go test ./...", Off, Input{Root: dir, Dir: dir, Files: []string{"a.go"}}); !p.Full {
		t.Errorf("off: %+v", p)
	}
	if p := sel(t, dir, "go test ./..."); p.Full || len(p.Run) != 0 {
		t.Errorf("nothing changed: %+v", p)
	}
	for _, cmd := range []string{"make test", "go test ./... && go vet ./...", "go test ./... | tee log", "go test $(go list ./...)", "go test ./a/... ./b/..."} {
		if p := sel(t, dir, cmd, "a.go"); !p.Full {
			t.Errorf("%q must run in full: %+v", cmd, p)
		}
	}
	// Outside the check folder: docs do not count, code does.
	sub := filepath.Join(dir, "web")
	os.MkdirAll(sub, 0o755)
	if p := Select(context.Background(), "npx jest", "", Input{Root: dir, Dir: sub, Files: []string{"api/main.go"}}); !p.Full {
		t.Errorf("code outside the folder: %+v", p)
	}
	if p := Select(context.Background(), "npx jest", "", Input{Root: dir, Dir: sub, Files: []string{"README.md"}}); p.Full || len(p.Run) != 0 {
		t.Errorf("docs outside the folder: %+v", p)
	}
	for _, bad := range []string{"../x.go", "/etc/passwd"} {
		if p := sel(t, dir, "go test ./...", bad); !p.Full {
			t.Errorf("%q: %+v", bad, p)
		}
	}
}

// File names come from agents: one that could break out of its quotes
// in sh or cmd.exe, or pass itself off as a flag, is never put into a
// command; the full command runs instead.
func TestUnsafeNamesRunFull(t *testing.T) {
	for _, name := range []string{`a$(touch pwned).js`, "a`id`.js", `a"b.js`, "a'b.js", `%PATH%.js`, `a!x!.js`, `a^b.js`, "a&b.js", "a|b.js",
		"a;b.js", "a\nb.js", `a\b.js`, "a*b.js", "a>b.js"} {
		if safeArg("./" + name) {
			t.Errorf("%q passes as safe", name)
		}
	}
	if safeArg("-exec=evil") || safeArg("") {
		t.Error("a leading - or an empty name passes")
	}
	dir := tree(t, map[string]string{"package.json": `{"scripts":{"test":"jest"}}`, "src/a$(id).js": "x", "src/-rf.js": "x"})
	if p := sel(t, dir, "npm test", "src/a$(id).js"); !p.Full || len(p.Run) != 0 {
		t.Errorf("$(...): %+v", p)
	}
	// "-rf" never starts an argument: files are passed as ./path.
	if p := sel(t, dir, "npm test", "src/-rf.js"); p.Full || len(p.Run) != 1 || !strings.HasSuffix(p.Run[0], " ./src/-rf.js") {
		t.Errorf("leading dash: %+v", p)
	}
}

func TestQuote(t *testing.T) {
	if quote("./a/b_c-1.js") != "./a/b_c-1.js" {
		t.Error("plain names stay bare")
	}
	want := "'./my dir/a.js'"
	if runtime.GOOS == "windows" {
		want = `"./my dir/a.js"`
	}
	if got := quote("./my dir/a.js"); got != want {
		t.Errorf("quote = %s, want %s", got, want)
	}
}

// The quoting must hold in the real shell sy runs checks in (cmd.exe on
// Windows, sh elsewhere): each name arrives as one argument, unchanged.
func TestQuotedArgsSurviveTheShell(t *testing.T) {
	if os.Getenv("SY_AFFECTED_ECHO") == "1" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	args := []string{"./my dir/a b.js", "./ünï/çødé.py", "./x=1,y@2+3:z.go", "./plain/x_test.go"}
	q, bad := quoteAll(args)
	if bad != "" {
		t.Fatalf("%q is not safe", bad)
	}
	exeQ := exe
	if strings.ContainsAny(exe, " ") {
		exeQ = `"` + exe + `"`
		if runtime.GOOS != "windows" {
			exeQ = "'" + exe + "'"
		}
	}
	line := exeQ + " -test.run=TestEchoArgs -- " + q
	cmd := proc.Shell(context.Background(), line)
	cmd.Env = append(cmd.Env, "SY_AFFECTED_ECHO=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var got []string
	for _, l := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(l, "ARG:") {
			got = append(got, strings.TrimPrefix(l, "ARG:"))
		}
	}
	if !slices.Equal(got, args) {
		t.Errorf("args through %s:\n got %q\nwant %q", line, got, args)
	}
}

// TestEchoArgs is the helper process of TestQuotedArgsSurviveTheShell.
func TestEchoArgs(t *testing.T) {
	if os.Getenv("SY_AFFECTED_ECHO") != "1" {
		return
	}
	for i, a := range os.Args {
		if a == "--" {
			for _, x := range os.Args[i+1:] {
				os.Stdout.WriteString("ARG:" + x + "\n")
			}
			break
		}
	}
}

func TestTemplate(t *testing.T) {
	dir := tree(t, map[string]string{"lib/a.rb": "x", "spec/a_spec.rb": "x", "docs/x.md": "x"})
	in := Input{Root: dir, Dir: dir, Files: []string{"lib/a.rb", "spec/a_spec.rb", "docs/x.md"}}
	p := Select(context.Background(), "bundle exec rspec", "bundle exec rspec {files}", in)
	if p.Full || len(p.Run) != 1 || p.Run[0] != "bundle exec rspec ./docs/x.md ./lib/a.rb ./spec/a_spec.rb" {
		t.Errorf("files: %+v", p)
	}
	checkAllowed(t, dir, "bundle exec rspec", "bundle exec rspec {files}", p)
	p = Select(context.Background(), "make test", "make test DIRS={packages}", in)
	if p.Full || len(p.Run) != 1 || p.Run[0] != "make test DIRS=./lib ./spec" {
		t.Errorf("packages: %+v", p)
	}
	checkAllowed(t, dir, "make test", "make test DIRS={packages}", p)
	// A template that would start with a file name allows nothing extra.
	if pre, _ := Allowed(dir, "x", "{files}"); len(pre) != 0 {
		t.Errorf("template without a command allowed %q", pre)
	}
	if p := Select(context.Background(), "make test", "make test", in); !p.Full {
		t.Errorf("template without placeholders: %+v", p)
	}
	// Nothing for the placeholder: nothing to run.
	p = Select(context.Background(), "make test", "make test T={test_files}", Input{Root: dir, Dir: dir, Files: []string{"lib/a.rb"}})
	if p.Full || len(p.Run) != 0 {
		t.Errorf("no test files: %+v", p)
	}
}

func TestOwnerAndClosure(t *testing.T) {
	if owner("a/b/c.go", []string{".", "a", "a/b", "ab"}) != "a/b" || owner("x.go", []string{"a"}) != "" {
		t.Error("owner")
	}
	got := reverseClosure([]string{"a"}, map[string][]string{"b": {"a"}, "c": {"b"}, "d": {"x"}})
	if !got["a"] || !got["b"] || !got["c"] || got["d"] {
		t.Errorf("closure %v", got)
	}
}
