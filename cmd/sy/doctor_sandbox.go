package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/proc"
	"github.com/sparkz400/switchyard/internal/sandbox"
)

// doctorSandbox checks the container sandbox (docs/sandbox.md) when the
// config here (user config plus .switchyard.yaml in dir) turns it on: the
// runtime, Linux containers, the image, each sandboxed CLI in the image,
// its sign-in variables (names only) and credential files, and containers
// an ended sy left behind.
func doctorSandbox(w io.Writer, cfg *config.Config, dir string, ok func(bool) string, warn string) (problems int) {
	c := cfg.Clone()
	info, err := config.ApplyRepo(c, dir)
	if err != nil {
		c = cfg // the repo file is broken; sy run reports that itself
	}
	for _, k := range info.Ignored {
		if k == "sandbox" {
			fmt.Fprintf(w, "%s sandbox     %s would make the sandbox weaker; that applies after you review it: sy trust\n", warn, info.Path)
		}
	}
	boxed := c.SandboxedProviders()
	if len(boxed) == 0 && !c.Sandbox.On() {
		fmt.Fprintf(w, "%s sandbox     off: agents run on this machine with your permissions (docs/sandbox.md)\n", stMuted.Render("info"))
		return 0
	}
	// One check per runtime and image.
	type rtImage struct{ rt, image string }
	images := map[rtImage][]string{}
	if c.Sandbox.On() {
		images[rtImage{c.Sandbox.Mode, c.Sandbox.ImageName()}] = nil
	}
	names := make([]string, 0, len(boxed))
	for p := range boxed {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		s := boxed[p]
		k := rtImage{s.Mode, s.ImageName()}
		images[k] = append(images[k], p)
	}
	keys := make([]rtImage, 0, len(images))
	for k := range images {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].rt+keys[i].image < keys[j].rt+keys[j].image })
	for _, k := range keys {
		bin, why := sandbox.Usable(k.rt)
		if bin == "" {
			problems++
			fmt.Fprintf(w, "%s sandbox     %s: agents in the sandbox cannot run (install or start it, or set sandbox.mode: off)\n", ok(false), why)
			continue
		}
		if gone, err := sandbox.Sweep(k.rt, bin, false); err == nil && len(gone) > 0 {
			fmt.Fprintf(w, "%s sandbox     %d container(s) of an sy that ended are still there; the next sandboxed run removes them (%s rm -f %s)\n",
				warn, len(gone), k.rt, strings.Join(gone, " "))
		}
		if out, err := runtimeOut(bin, "image", "inspect", "--format", "{{.Id}}", k.image); err != nil {
			problems++
			fmt.Fprintf(w, "%s sandbox     image %q not found (%s): build it with `%s build -t %s packaging/sandbox` (docs/sandbox.md)\n",
				ok(false), k.image, oneLine(out, 120), k.rt, config.DefaultSandboxImage)
			continue
		}
		fmt.Fprintf(w, "%s sandbox     %s, image %s\n", ok(true), k.rt, k.image)
		for _, p := range images[k] {
			problems += doctorSandboxedCLI(w, bin, p, c, boxed[p], ok, warn)
		}
	}
	return problems
}

// doctorSandboxedCLI checks one sandboxed provider: its CLI in the image,
// and that it has something to sign in with.
func doctorSandboxedCLI(w io.Writer, bin, p string, c *config.Config, s config.SandboxCfg, ok func(bool) string, warn string) (problems int) {
	cli := sandbox.CLIName(s, c.Providers[p].Command)
	out, err := runtimeOut(bin, "run", "--rm", "--entrypoint", "sh", s.ImageName(), "-c", `command -v "$0" && "$0" --version`, cli)
	if err != nil {
		problems++
		fmt.Fprintf(w, "%s   %-9s %q is not in the image (%s): install it there, or set providers.%s.sandbox.command\n", ok(false), p, cli, oneLine(out, 100), p)
		return problems
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	desc := oneLine(lines[len(lines)-1], 80)
	if len(s.Roles) > 0 {
		desc += " (roles " + strings.Join(s.Roles, ", ") + ")"
	}
	if s.NetworkOff() {
		desc += "; network off: the CLI cannot reach its model API"
	}
	fmt.Fprintf(w, "%s   %-9s %s in the image  %s\n", ok(true), p, cli, stMuted.Render(desc))
	var set, unset []string
	for _, n := range s.Env {
		if v, found := os.LookupEnv(n); found && v != "" {
			set = append(set, n)
		} else {
			unset = append(unset, n)
		}
	}
	if pe, _ := c.Providers[p].EnvFor(nil); len(pe) > 0 {
		set = append(set, "providers."+p+".env")
	}
	for _, f := range s.Credentials {
		if path, err := expandUserPath(f); err != nil || !fileExists(path) {
			problems++
			fmt.Fprintf(w, "%s   %-9s credential file %s is missing\n", ok(false), p, f)
		} else {
			set = append(set, f)
		}
	}
	switch {
	case len(set) == 0 && len(s.Env) > 0:
		fmt.Fprintf(w, "%s   %-9s none of %s is set and no credential file is mounted: the CLI cannot sign in\n", warn, p, strings.Join(s.Env, ", "))
	case len(set) > 0:
		fmt.Fprintf(w, "%s   %-9s sign-in from %s\n", ok(true), p, strings.Join(set, ", "))
	}
	return problems
}

func runtimeOut(bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	proc.Background(cmd)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func expandUserPath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = home + p[1:]
	}
	return p, nil
}
