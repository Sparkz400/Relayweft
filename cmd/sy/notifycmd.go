package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sparkz400/switchyard/internal/notify"
)

// notifyOut is where sy notify prints; tests replace it.
var notifyOut io.Writer = os.Stdout

// postWebhook is notify.Post; tests replace it.
var postWebhook = notify.Post

// cmdNotify lists the notification settings, or sends a test message to
// every webhook (sy notify --test) so a wrong URL shows up now, not after
// an overnight run.
func cmdNotify(args []string) error {
	fs := flag.NewFlagSet("sy notify", flag.ExitOnError)
	var c common
	c.register(fs)
	test := fs.Bool("test", false, "send a test message to every webhook and report each")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: sy notify [--test] [--dir repo]

Shows where sy sends notifications: desktop notifications (notify.enabled)
and the webhooks in notify.webhooks (Slack, Discord, ntfy or plain JSON).
--test sends a test message to every webhook, whatever its events.
`)
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	store, dir, err := c.setup()
	if err != nil {
		return err
	}
	cfg := store.Get()
	out := notifyOut
	desktop := "off"
	if cfg.Notify.Enabled {
		desktop = "on"
	}
	fmt.Fprintf(out, "desktop notifications: %s (tasks of at least %s)\n", desktop, cfg.Notify.MinTask.D())
	hooks := cfg.Notify.Webhooks
	if len(hooks) == 0 {
		fmt.Fprintf(out, "webhooks: none (add notify.webhooks to %s; sy init --print shows an example)\n", store.Path())
		if *test {
			return errors.New("no webhooks to test")
		}
		return nil
	}
	failed := 0
	for i, w := range hooks {
		events := "all events"
		if len(w.Events) > 0 {
			events = strings.Join(w.Events, ", ")
		}
		line := fmt.Sprintf("webhook %d: %s (%s)", i+1, w.Name(), events)
		if !*test {
			fmt.Fprintln(out, line)
			continue
		}
		err := postWebhook(context.Background(), w, notify.Message{
			Event: notify.EventTest, Title: "Switchyard: test",
			Body:   "sy notify --test: notifications from sy reach you here.",
			Source: filepath.Base(dir),
		})
		if err != nil {
			failed++
			fmt.Fprintf(out, "%s: failed: %v\n", line, err)
			continue
		}
		fmt.Fprintf(out, "%s: sent\n", line)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d webhook(s) failed", failed, len(hooks))
	}
	return nil
}
