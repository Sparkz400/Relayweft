package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/report"
	"github.com/sparkz400/relayweft/internal/web"
)

// cmdReport writes one self-contained page (or Markdown) about a task.
func cmdReport(args []string) error {
	fs := flag.NewFlagSet("rw report", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file")
	dirFlag := fs.String("dir", "", "project directory for the default task (default current directory)")
	out := fs.String("out", "", "output file (default <config dir>/relayweft/reports/<task-id>.html)")
	asMD := fs.Bool("md", false, "write Markdown instead of HTML")
	open := fs.Bool("open", false, "open the report when written")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: rw report [task-id] [--out file.html] [--md] [--open]

Writes one shareable page about a task: the task, plan and results, every
routing decision with its rule and reason, reviews, checks, the diff and
the cost. Default: the last task in this directory (rw history lists them).
The page holds the task text, agent answers and the repo diff; no
environment values or credentials.
`)
	}
	// Accept flags after the task id too (rw report <id> --open).
	var id string
	rest := args
	for len(rest) > 0 {
		fs.Parse(rest)
		if fs.NArg() == 0 {
			break
		}
		if id != "" {
			return fmt.Errorf("one task id at a time (got %q and %q)", id, fs.Arg(0))
		}
		id, rest = fs.Arg(0), fs.Args()[1:]
	}
	cfg, _, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	var st *orchestrator.TaskState
	if id != "" {
		if st, err = orchestrator.LoadTask(id); err != nil {
			return err
		}
	} else {
		dir, err := absDir(*dirFlag)
		if err != nil {
			return err
		}
		h := orchestrator.History(dir, 1)
		if len(h) == 0 {
			return errors.New("no task in this directory yet (rw history --all lists every task)")
		}
		st = &h[0]
	}
	data := report.Build(st, report.Options{SessionDir: cfg.SessionDir(), Version: version})
	var buf bytes.Buffer
	if *asMD {
		err = data.Markdown(&buf)
	} else {
		err = data.HTML(&buf)
	}
	if err != nil {
		return err
	}
	path := *out
	if path == "" {
		if path, err = reportPath(st.ID, *asMD); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return err
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	fmt.Println(path)
	if *open {
		if err := web.OpenBrowser(path); err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
	}
	return nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// reportPath is <user config dir>/relayweft/reports/<task-id>.html (.md).
func reportPath(id string, md bool) (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	ext := ".html"
	if md {
		ext = ".md"
	}
	return filepath.Join(d, "relayweft", "reports", unsafeName.ReplaceAllString(id, "_")+ext), nil
}
