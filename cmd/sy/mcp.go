package main

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/proc"
)

// doctorMCP lists the MCP servers the agents get here (user config plus a
// trusted .switchyard.yaml in dir) and checks their commands. It prints
// names, commands and URL hosts only: never env values or headers.
func doctorMCP(w io.Writer, cfg *config.Config, dir string, ok func(bool) string, warn string) (problems int) {
	c := cfg.Clone()
	info, err := config.ApplyRepo(c, dir)
	if err != nil {
		c = cfg // the repo file is broken; sy run reports that itself
	}
	for _, k := range info.Ignored {
		if k == "mcp" {
			fmt.Fprintf(w, "%s mcp         %s sets MCP servers; they apply after you review them: sy trust\n", warn, info.Path)
		}
	}
	m := c.MCP
	if len(m.Servers) == 0 {
		return 0
	}
	fmt.Fprintf(w, "%s mcp         %d server(s) for %s\n", stMuted.Render("info"), len(m.Servers), strings.Join(m.RoleList(), ", "))
	for _, name := range m.Names() {
		s, missing := m.Servers[name].Expanded(os.LookupEnv)
		var desc string
		mark := ok(true)
		if s.URL != "" {
			desc = s.Transport() + " " + urlHost(s.URL)
		} else if bin, err := proc.Resolve(s.Command); err != nil {
			problems++
			mark = ok(false)
			desc = fmt.Sprintf("%q not found on PATH", s.Command)
		} else {
			desc = "stdio " + bin
		}
		if len(s.Providers) > 0 {
			desc += " (" + strings.Join(s.Providers, ", ") + " only)"
		}
		if len(missing) > 0 {
			if mark == ok(true) {
				mark = warn
			}
			desc += "; unset: ${" + strings.Join(missing, "}, ${") + "}"
		}
		fmt.Fprintf(w, "%s   %-9s %s\n", mark, name, desc)
	}
	return problems
}

// urlHost keeps scheme and host: a path or query may carry a token.
func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(invalid url)"
	}
	return u.Scheme + "://" + u.Host
}
