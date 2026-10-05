package affected

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sparkz400/switchyard/internal/proc"
)

var cargoValues = map[string]bool{"--features": true, "-F": true, "-j": true, "--jobs": true, "--target": true, "--profile": true,
	"--target-dir": true, "--color": true, "--message-format": true, "-Z": true}

// cargoShape matches `cargo test` with flags only (no package selection
// of its own).
func cargoShape(f []string) bool {
	if len(f) < 2 || f[0] != "cargo" || f[1] != "test" {
		return false
	}
	for _, a := range f[2:] {
		switch a {
		case "-p", "--package", "--exclude", "--manifest-path":
			return false
		}
		if strings.HasPrefix(a, "--package=") || strings.HasPrefix(a, "--exclude=") || strings.HasPrefix(a, "--manifest-path=") {
			return false
		}
	}
	args := f[2:]
	for i, a := range args {
		if a == "--" {
			args = args[:i] // the rest goes to the test binaries
			break
		}
	}
	return onlyFlags(args, cargoValues)
}

// cargoArgsEnd is the index of "--" in f, or len(f).
func cargoArgsEnd(f []string) int {
	for i, a := range f {
		if a == "--" {
			return i
		}
	}
	return len(f)
}

// cargoMetadata runs `cargo metadata` in dir (tests swap it).
var cargoMetadata = func(ctx context.Context, dir string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "cargo", "metadata", "--format-version", "1", "--no-deps", "--offline")
	proc.Prepare(cmd)
	cmd.Env = proc.WithoutSecrets(os.Environ())
	cmd.Dir = dir
	return cmd.Output()
}

// selectCargo narrows `cargo test` to the workspace crates of the changed
// files and the crates that depend on them (-p per crate). Cargo.toml,
// Cargo.lock and files outside every crate make it full.
func selectCargo(ctx context.Context, cmd string, f []string, c *change) Plan {
	for _, file := range c.files {
		base := path.Base(file)
		if base == "Cargo.toml" || base == "Cargo.lock" || strings.HasPrefix(file, ".cargo/") || strings.HasPrefix(base, "rust-toolchain") {
			return full(cmd, file+" changed")
		}
	}
	var out []byte
	var err error
	if c.exec != nil {
		out, err = c.exec(ctx, c.dir, []string{"cargo", "metadata", "--format-version", "1", "--no-deps", "--offline"})
	} else {
		out, err = cargoMetadata(ctx, c.dir)
	}
	if err != nil {
		return full(cmd, "cargo metadata failed: "+clipStr(err.Error(), 200))
	}
	var meta struct {
		Packages []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			ManifestPath string `json:"manifest_path"`
			Dependencies []struct {
				Name string `json:"name"`
				Path string `json:"path"`
			} `json:"dependencies"`
		} `json:"packages"`
		// What plain `cargo test` tests in a workspace (cargo 1.71+).
		DefaultMembers *[]string `json:"workspace_default_members"`
	}
	if err := json.Unmarshal(out, &meta); err != nil || len(meta.Packages) == 0 {
		return full(cmd, "could not read cargo metadata")
	}
	byDir := map[string]string{} // crate folder (relative, slash) -> name
	var dirs []string
	relOf := func(p string) (string, bool) { return relDir(c.dir, p) }
	for _, p := range meta.Packages {
		d, ok := relOf(filepath.Dir(p.ManifestPath))
		if !ok {
			return full(cmd, "crate "+p.Name+" is outside the check folder")
		}
		byDir[d] = p.Name
		dirs = append(dirs, d)
	}
	deps := map[string][]string{}
	for _, p := range meta.Packages {
		for _, d := range p.Dependencies {
			if d.Path == "" {
				continue
			}
			if rd, ok := relOf(d.Path); ok && byDir[rd] != "" {
				deps[p.Name] = append(deps[p.Name], byDir[rd])
			}
		}
	}
	var start []string
	for _, file := range c.files {
		d := owner(file, dirs)
		switch {
		case d != "":
			start = append(start, byDir[d])
		case isDoc(file):
		default:
			return full(cmd, file+" is outside every crate")
		}
	}
	if len(start) == 0 {
		return Plan{Command: cmd, Why: "no crate is affected by the " + c.changedWhy()}
	}
	// Narrow within what the command tests: never run tests of a crate
	// that the full command leaves out.
	defaults := map[string]bool{}
	switch {
	case slices.Contains(f, "--workspace") || slices.Contains(f, "--all"):
		for _, p := range meta.Packages {
			defaults[p.Name] = true
		}
	case meta.DefaultMembers != nil:
		for _, p := range meta.Packages {
			if slices.Contains(*meta.DefaultMembers, p.ID) {
				defaults[p.Name] = true
			}
		}
	case byDir["."] != "":
		return full(cmd, "plain cargo test tests only the root package, and this cargo does not list the default members")
	default: // a virtual workspace tests every member
		for _, p := range meta.Packages {
			defaults[p.Name] = true
		}
	}
	set := map[string]bool{}
	for n := range reverseClosure(start, deps) {
		if defaults[n] {
			set[n] = true
		}
	}
	if len(set) == 0 {
		return Plan{Command: cmd, Why: "no crate the command tests is affected by the " + c.changedWhy()}
	}
	if len(set) >= len(defaults) {
		return full(cmd, "every crate is affected by the "+c.changedWhy())
	}
	var args []string
	for _, n := range sortedKeys(set) {
		if !safeArg(n) {
			return full(cmd, unsafeWhy(n))
		}
		args = append(args, "-p", quote(n))
	}
	// Before a "--": what follows it goes to the test binaries.
	i := cargoArgsEnd(f)
	narrowed := strings.Join(append(append(append([]string(nil), f[:i]...), args...), f[i:]...), " ")
	return Plan{Command: cmd, Run: []string{narrowed},
		Why: fmt.Sprintf("%d of %d crates, affected by the %s", len(set), len(defaults), c.changedWhy())}
}
