package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// cli renders the CLI reference from the rw binary: `rw --help`, then
// `rw <command> -h` for every command the usage names.
func (g *gen) cli() string {
	if g.o.rw == "" {
		if g.o.requireCLI {
			g.errorf("the CLI reference needs an rw binary: -rw or $RW_BIN")
		}
		return "> [!NOTE]\n> This page is generated from `rw --help` when the site is built with an rw binary (`website/build.sh` builds one). Run `rw --help` and `rw <command> -h` to see it.\n"
	}
	home, err := os.MkdirTemp("", "rw-docs-cli-")
	if err != nil {
		g.errorf("cli: %v", err)
		return ""
	}
	defer os.RemoveAll(home)
	run := func(args ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, g.o.rw, args...)
		cmd.Dir = home
		// A throwaway profile: rw must not read or migrate anyone's config.
		cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home, "APPDATA="+home, "LOCALAPPDATA="+home,
			"XDG_CONFIG_HOME="+home, "XDG_CACHE_HOME="+home, "XDG_DATA_HOME="+home, "XDG_STATE_HOME="+home,
			"RW_NO_SETUP=1", "NO_COLOR=1")
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		_ = cmd.Run() // -h exits 0 or 2; the text is what counts
		if ctx.Err() != nil {
			g.errorf("cli: rw %s did not finish in 30s", strings.Join(args, " "))
		}
		return strings.TrimRight(strings.ReplaceAll(out.String(), "\r\n", "\n"), "\n")
	}
	usage := run("--help")
	if !strings.Contains(usage, "Usage:") {
		g.errorf("cli: rw --help printed no usage:\n%s", usage)
		return ""
	}
	var b strings.Builder
	b.WriteString("## Usage\n\n" + codeBlock(usage, "text") + "\n")
	firstLine, _, _ := strings.Cut(usage, "\n")
	for _, c := range commands(usage) {
		help := run(c, "-h")
		fmt.Fprintf(&b, "\n## rw %s\n\n", c)
		if strings.HasPrefix(help, firstLine) {
			b.WriteString("`rw " + c + " -h` prints the usage above.\n")
			continue
		}
		b.WriteString(codeBlock(help, "text") + "\n")
	}
	return b.String()
}

var reCommand = regexp.MustCompile(`(?m)^  rw ([a-z][a-z-]*)\b`)

// commands lists the subcommands the usage text names, in its order.
func commands(usage string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range reCommand.FindAllStringSubmatch(usage, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}
