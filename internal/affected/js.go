package affected

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// jsCmd is a jest or vitest command, run directly or through a package
// manager's test script.
type jsCmd struct {
	kind   string   // "jest" or "vitest" ("" = not one)
	pm     string   // npm, pnpm, yarn ("" = run directly)
	runner []string // how to start the runner directly (npx vitest, pnpm exec vitest, ...)
	flags  []string // the runner's own flags from the command or the script
}

var jestValues = map[string]bool{"-c": true, "--config": true, "-t": true, "--testNamePattern": true, "-w": true, "--maxWorkers": true,
	"--reporters": true, "--testTimeout": true, "--selectProjects": true, "--shard": true, "--coverageReporters": true}

// jsShape recognizes jest and vitest commands: `npx jest`, `jest`,
// `npx vitest run`, `vitest run`, and `npm|pnpm|yarn [run] test` whose
// package.json test script is one of those (flags only).
func jsShape(f []string, dir string) jsCmd {
	pre := func(p ...string) bool {
		return len(f) >= len(p) && strings.Join(f[:len(p)], " ") == strings.Join(p, " ")
	}
	direct := func(f []string) jsCmd {
		for _, p := range [][]string{{"npx", "jest"}, {"jest"}, {"yarn", "jest"}, {"pnpm", "jest"}, {"pnpm", "exec", "jest"}} {
			if len(f) >= len(p) && strings.Join(f[:len(p)], " ") == strings.Join(p, " ") && onlyFlags(f[len(p):], jestValues) {
				return jsCmd{kind: "jest", runner: p, flags: f[len(p):]}
			}
		}
		for _, p := range [][]string{{"npx", "vitest"}, {"vitest"}, {"yarn", "vitest"}, {"pnpm", "vitest"}, {"pnpm", "exec", "vitest"}} {
			if len(f) >= len(p) && strings.Join(f[:len(p)], " ") == strings.Join(p, " ") {
				rest := f[len(p):]
				if len(rest) > 0 && rest[0] == "run" {
					rest = rest[1:]
				} else if !containsStr(rest, "--run") {
					continue // watch mode: never as a check
				}
				if onlyFlags(rest, jestValues) {
					return jsCmd{kind: "vitest", runner: p, flags: rest}
				}
			}
		}
		return jsCmd{}
	}
	if c := direct(f); c.kind != "" {
		return c
	}
	for _, p := range [][]string{{"npm", "test"}, {"npm", "run", "test"}, {"pnpm", "test"}, {"pnpm", "run", "test"}, {"yarn", "test"}, {"yarn", "run", "test"}} {
		if !pre(p...) || len(f) != len(p) {
			continue
		}
		script := testScript(dir)
		c := direct(strings.Fields(script))
		if c.kind == "" || hasShellSyntax(script) {
			return jsCmd{}
		}
		c.pm = p[0]
		// The script's runner, started through the package manager.
		bin := c.runner[len(c.runner)-1]
		switch c.pm {
		case "npm":
			c.runner = []string{"npx", "--no", bin} // never download a runner the project does not have
		case "pnpm":
			c.runner = []string{"pnpm", "exec", bin}
		case "yarn":
			c.runner = []string{"yarn", bin}
		}
		return c
	}
	return jsCmd{}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func testScript(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return ""
	}
	return strings.TrimSpace(pkg.Scripts["test"])
}

// jsNarrow is the narrowed command without its files: jest gets
// --findRelatedTests (through the script, so its setup is kept), vitest
// runs `vitest related` (a subcommand, which a script cannot take).
func jsNarrow(cmd string, c jsCmd) string {
	if c.kind == "jest" {
		if c.pm == "npm" {
			// npm passes arguments on to the script only after "--"; pnpm
			// and yarn pass them all.
			return cmd + " -- --findRelatedTests --passWithNoTests"
		}
		return cmd + " --findRelatedTests --passWithNoTests"
	}
	return strings.Join(append(append(append([]string(nil), c.runner...), "related", "--run", "--passWithNoTests"), c.flags...), " ")
}

func jsAllowed(f []string, dir string) ([]string, string) {
	c := jsShape(f, dir)
	n := jsNarrow(strings.Join(f, " "), c)
	if c.kind == "jest" {
		return nil, n + " <files>" // starts with the command itself
	}
	return []string{n}, n + " <files>"
}

var jsCode = map[string]bool{".js": true, ".jsx": true, ".ts": true, ".tsx": true, ".mjs": true, ".cjs": true, ".mts": true, ".cts": true}

// selectJS narrows a jest or vitest run to the tests related to the
// changed source files. Dependencies, configs, snapshots, mocks, deleted
// and non-code files make it full.
func selectJS(cmd string, f []string, c *change) Plan {
	js := jsShape(f, c.dir)
	var files []string
	for _, file := range c.files {
		base := strings.ToLower(path.Base(file))
		ext := path.Ext(base)
		parts := strings.Split(file, "/")
		switch {
		case containsStr(parts, "node_modules"):
			return full(cmd, file+" is a dependency")
		case base == "package.json" || base == "package-lock.json" || base == "yarn.lock" || base == "pnpm-lock.yaml" || base == "bun.lock" || base == "bun.lockb" ||
			strings.HasPrefix(base, "tsconfig") || strings.HasPrefix(base, ".babelrc") || strings.HasPrefix(base, ".env") || base == ".swcrc" ||
			strings.Contains(base, ".config."):
			return full(cmd, file+" changed (dependencies or configuration)")
		case containsStr(parts, "__snapshots__") || containsStr(parts, "__mocks__") || containsStr(parts, "__fixtures__"):
			return full(cmd, file+" is a snapshot, mock or fixture")
		case jsCode[ext] || ext == ".json":
			if !c.exists(file) {
				return full(cmd, file+" was deleted, so the tests that used it cannot be found")
			}
			files = append(files, dotSlash(file))
		case isDoc(file):
		default:
			return full(cmd, file+" is not JavaScript or TypeScript, so rw cannot tell which tests use it")
		}
	}
	if len(files) == 0 {
		return Plan{Command: cmd, Why: "no test is affected by the " + c.changedWhy()}
	}
	q, bad := quoteAll(sortedSet(files))
	if bad != "" {
		return full(cmd, unsafeWhy(bad))
	}
	return Plan{Command: cmd, Run: []string{jsNarrow(cmd, js) + " " + q}, Why: js.kind + " tests related to the " + c.changedWhy()}
}
