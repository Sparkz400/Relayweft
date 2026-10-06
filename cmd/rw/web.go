package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/limits"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/proc"
	"github.com/sparkz400/relayweft/internal/runner"
	"github.com/sparkz400/relayweft/internal/sessionlog"
	"github.com/sparkz400/relayweft/internal/web"
)

// appIdleExit is how long `rw app` keeps serving after its window closed.
const appIdleExit = 30 * time.Second

// cmdWeb implements `rw web`: the browser UI on 127.0.0.1.
func cmdWeb(args []string) error { return runWeb("rw web", args, false) }

// cmdApp implements `rw app`: the same UI in a chromeless app window
// (Edge or Chrome --app), exiting when the window has been closed.
func cmdApp(args []string) error { return runWeb("rw app", args, true) }

// webServer is a running web UI with its orchestrator.
type webServer struct {
	srv    *web.Server
	events chan event.Event
	log    *sessionlog.Writer
	errc   chan error
}

// startWeb builds the orchestrator and the server and starts serving on
// 127.0.0.1:port (0 = a free port).
func startWeb(c *common, port int, demo bool, speed float64) (*webServer, error) {
	store, dir, err := c.setup()
	if err != nil {
		return nil, err
	}
	_ = proc.Guard()
	cfg := store.Get()
	if !demo {
		prunePoolsInBackground(cfg)
	}
	w := &webServer{events: make(chan event.Event, 4096), errc: make(chan error, 1)}
	runners := runner.New
	mode := "routed"
	if demo {
		mode = "demo"
		fake := runner.NewFakeSet(speed)
		runners = func(*config.Config) runner.Set { return fake }
	} else if w.log, err = sessionlog.Open(cfg.SessionDir(), dir); err != nil {
		fmt.Fprintln(os.Stderr, "warning: session log disabled:", err)
		w.log = nil
	}
	// Always a real approver: a typed nil in the interface would make every
	// task wait forever.
	ap := web.NewApprover()
	orc := orchestrator.New(orchestrator.Options{
		Dir: dir, Store: store, Runners: runners, Tracker: limits.NewTracker(), Log: w.log,
		Events: w.events, ForceProvider: c.provider, NoGit: demo, Mode: mode, Approver: ap,
		Repos: c.workspace,
	})
	opt := web.Options{Orc: orc, Events: w.events, Approver: ap, Dir: dir, Demo: demo, Version: version, SessionLog: w.log.Path(),
		Warn: func(msg string) { fmt.Fprintln(os.Stderr, "\n"+msg) }, AllowSleep: c.allowSleep}
	if demo {
		opt.DemoTask = demoTask
	}
	w.srv, err = web.New(opt)
	if err != nil {
		w.log.Close()
		return nil, err
	}
	ln, err := w.srv.Listen(port)
	if err != nil {
		w.srv.Shutdown()
		w.log.Close()
		return nil, fmt.Errorf("listen on 127.0.0.1:%d: %w", port, err)
	}
	go func() { w.errc <- w.srv.Serve(ln) }()
	return w, nil
}

// stop shuts the server and any running task down.
func (w *webServer) stop() {
	w.srv.Shutdown()
	w.log.Close()
}

// printNewLinks prints a fresh link for every line read (Enter), until
// the input ends.
func printNewLinks(in io.Reader, out io.Writer, newLink func() string) {
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		fmt.Fprintf(out, "open: %s\n", newLink())
	}
}

func runWeb(name string, args []string, app bool) error {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags]\n\n", name)
		fs.PrintDefaults()
	}
	var c common
	c.register(fs)
	port := fs.Int("port", 0, "port on 127.0.0.1 (0 = a free port)")
	noOpen := fs.Bool("no-open", false, "do not open a browser; just print the URL")
	demo := fs.Bool("demo", false, "demo mode with fake agents")
	speed := fs.Float64("speed", 1, "demo speed multiplier")
	client := false
	if !app {
		fs.BoolVar(&client, "client", false, "editor client mode: no browser; print one JSON hello line (address, bootstrap) on stdout, stop when stdin closes")
	}
	parseFlags(fs, args)
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (type tasks in the page)", fs.Arg(0))
	}
	if !*demo && !client {
		if err := firstRun(c.configPath, c.dir, false); err != nil {
			return err
		}
	}
	w, err := startWeb(&c, *port, *demo, *speed)
	if err != nil {
		return err
	}
	defer w.stop()
	if client {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return serveClient(ctx, w, os.Stdin, os.Stdout)
	}
	url := w.srv.NewLink()
	fmt.Printf("Relayweft %s on http://%s\n", map[bool]string{true: "app", false: "web UI"}[app], w.srv.Addr())
	fmt.Printf("open: %s\n", url)
	fmt.Println("(a private link: it works once, within 2 minutes. Press Enter here for a new one, e.g. for another tab; Ctrl+C stops rw)")
	go printNewLinks(os.Stdin, os.Stdout, w.srv.NewLink)
	if !*noOpen {
		if app {
			how, err := web.OpenAppWindow(url)
			if err != nil {
				fmt.Fprintln(os.Stderr, "could not open a window:", err, "- open the link above")
			} else {
				fmt.Println("opened in", how)
			}
		} else if err := web.OpenBrowser(url); err != nil {
			fmt.Fprintln(os.Stderr, "could not open a browser:", err, "- open the link above")
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if app {
		// Edge and Chrome hand an --app window to an already running
		// browser process, so the window's process cannot be waited on:
		// exit when no page has been connected for a while instead.
		go func() {
			w.srv.WaitIdle(ctx, appIdleExit)
			cancel()
		}()
	}
	select {
	case <-ctx.Done():
	case err := <-w.errc:
		if err != nil {
			return err
		}
	}
	fmt.Println("stopping: cancelling the running task, if any...")
	return nil
}
