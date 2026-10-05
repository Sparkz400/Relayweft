package proc

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every git command sy runs on this machine goes through GitGuard (most
// run in a folder an agent wrote to; the rule is simpler to keep for all).
// Exceptions build git's arguments with the guard elsewhere, or set up the
// throwaway repositories of sy selftest and sy bench --starter, which no
// agent has touched yet.
func TestHostGitIsGuarded(t *testing.T) {
	allowed := map[string][]string{
		"internal/orchestrator/git.go": {`exec.Command("git", args...)`},      // args start with GitGuard (git.exec)
		"internal/report/diff.go":      {`append(append([]string(nil), base`}, // base starts with GitGuard
		"cmd/sy/selftest.go":           {`exec.Command("git", args...)`, `exec.Command("git", "lfs", "version")`},
		"cmd/sy/selftest_firstrun.go":  {`exec.Command("git", args...)`},
		"cmd/sy/selftest_sandbox.go":   {`"remote", "add"`, `"rev-parse", "HEAD"`, `append([]string{"-C", proj}, args...)`},
		"cmd/sy/starter.go":            {`exec.Command("git", args...)`},
	}
	re := regexp.MustCompile(`exec\.Command(Context)?\([^"]*"git"`)
	root := filepath.Join("..", "..")
	var bad []string
	for _, top := range []string{"internal", "cmd"} {
		filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
		lines:
			for i, line := range strings.Split(string(data), "\n") {
				if !re.MatchString(line) || strings.Contains(line, "GitArgs(") || strings.Contains(line, "GitGuard") {
					continue
				}
				for _, ok := range allowed[rel] {
					if strings.Contains(line, ok) {
						continue lines
					}
				}
				bad = append(bad, rel+":"+itoa(i+1)+": "+strings.TrimSpace(line))
			}
			return nil
		})
	}
	if len(bad) > 0 {
		t.Errorf("git without proc.GitArgs (core.fsmonitor and submodule recursion from an agent's config):\n%s", strings.Join(bad, "\n"))
	}
}

func itoa(n int) string {
	s := ""
	for {
		s = string(rune('0'+n%10)) + s
		n /= 10
		if n == 0 {
			return s
		}
	}
}
