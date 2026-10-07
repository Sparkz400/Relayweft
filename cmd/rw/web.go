package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
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
// the input ends; "p" prints a fresh phone link as a QR code (phone nil:
// phone access is off).
func printNewLinks(in io.Reader, out io.Writer, newLink func() string, phone func() string) {
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		if phone != nil && strings.EqualFold(strings.TrimSpace(sc.Text()), "p") {
			printPhoneLink(out, phone())
			continue
		}
		fmt.Fprintf(out, "open: %s\n", newLink())
	}
}

// printPhoneLink prints a phone pairing link as a QR code to scan.
func printPhoneLink(out io.Writer, link string) {
	q, err := web.QRText(link)
	if err != nil {
		fmt.Fprintf(out, "phone: %s\n", link)
		return
	}
	fmt.Fprintf(out, "\nScan with your phone's camera (works once, within 2 minutes; p + Enter for a new one):\n%s%s\n\n", q, link)
}

// startPhone opens the phone listener (rw web --phone) and prints how to
// pair a phone.
func startPhone(w *webServer, addr string, out io.Writer) error {
	var ip net.IP
	if addr != "" {
		if ip = net.ParseIP(addr); ip == nil {
			return fmt.Errorf("--phone-addr %q: want an IP address", addr)
		}
	} else {
		addrs, err := net.InterfaceAddrs()
		if err != nil {
			return err
		}
		if ip, err = web.PickPhoneIP(addrs); err != nil {
			return err
		}
	}
	ln, err := w.srv.ListenPhone(ip)
	if err != nil {
		return fmt.Errorf("phone access: %w", err)
	}
	go func() {
		if err := w.srv.Serve(ln); err != nil {
			fmt.Fprintln(os.Stderr, "phone access stopped:", err)
		}
	}()
	how := "over your local network: plain HTTP, so use it on a network you trust (Tailscale encrypts the way)"
	if web.IsTailscale(ip) {
		how = "over Tailscale (encrypted)"
	}
	fmt.Fprintf(out, "phone: %s %s\n", w.srv.PhoneURL(), how)
	fmt.Fprintln(out, "  A phone can answer approvals, pause and cancel; tasks, plan edits and settings stay on this PC.")
	if runtime.GOOS == "windows" {
		fmt.Fprintln(out, "  If Windows Firewall asks, allow rw on private networks.")
	}
	printPhoneLink(out, w.srv.NewPhoneLink())
	return nil
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
	phone := fs.Bool("phone", false, "also serve the page to your phone on this machine's Tailscale or private network address (approve from your phone; prints a QR code)")
	phoneAddr := fs.String("phone-addr", "", "with --phone: the address to listen on (a Tailscale or private network address; default: Tailscale first, else the LAN)")
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
	mctx, mstop := context.WithCancel(context.Background())
	defer mstop()
	if !*demo {
		startMorning(mctx, w.srv.Config)
	}
	if client {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return serveClient(ctx, w, os.Stdin, os.Stdout)
	}
	url := w.srv.NewLink()
	fmt.Printf("Relayweft %s on http://%s\n", map[bool]string{true: "app", false: "web UI"}[app], w.srv.Addr())
	fmt.Printf("open: %s\n", url)
	fmt.Println("(a private link: it works once, within 2 minutes. Press Enter here for a new one, e.g. for another tab; Ctrl+C stops rw)")
	var phoneLink func() string
	if *phone || *phoneAddr != "" {
		if err := startPhone(w, *phoneAddr, os.Stdout); err != nil {
			return err
		}
		phoneLink = w.srv.NewPhoneLink
	}
	go printNewLinks(os.Stdin, os.Stdout, w.srv.NewLink, phoneLink)
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
