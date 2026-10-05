package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// serveClient runs `rw web --client`: the mode for an editor extension
// that started rw and reads its stdout through a pipe.
//
// stdout carries JSON lines only. The first is the hello (address and a
// single-use bootstrap, see web.ClientHello); after that rw answers one
// line per command read from stdin:
//
//	link       {"link": "http://127.0.0.1:P/#b=..."}  a page link (open in browser)
//	bootstrap  {"bootstrap": "..."}                    another bootstrap (re-login)
//
// and {"error": "..."} for anything else. When stdin ends (the editor
// closed the pipe or died), rw stops like on Ctrl+C: the running task is
// cancelled and its agents are stopped. Human-readable messages go to
// stderr.
func serveClient(ctx context.Context, w *webServer, in io.Reader, out io.Writer) error {
	var mu sync.Mutex
	emit := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		_, err = out.Write(append(b, '\n'))
		return err
	}
	if err := emit(w.srv.Hello(os.Getpid())); err != nil {
		return fmt.Errorf("write hello: %w", err)
	}
	eof := make(chan struct{})
	go func() {
		defer close(eof)
		sc := bufio.NewScanner(in)
		for sc.Scan() {
			switch cmd := strings.TrimSpace(sc.Text()); cmd {
			case "":
			case "link":
				_ = emit(map[string]string{"link": w.srv.NewLink()})
			case "bootstrap":
				_ = emit(map[string]string{"bootstrap": w.srv.NewBootstrap()})
			default:
				_ = emit(map[string]string{"error": fmt.Sprintf("unknown command %q (want link or bootstrap)", oneLine(cmd, 40))})
			}
		}
	}()
	select {
	case <-ctx.Done():
	case <-eof:
		fmt.Fprintln(os.Stderr, "rw web --client: stdin closed, stopping")
	case err := <-w.errc:
		return err
	}
	return nil
}
