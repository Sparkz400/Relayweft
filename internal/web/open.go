package web

import (
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"

	"github.com/sparkz400/relayweft/internal/proc"
)

// OpenBrowser opens url in the default browser.
func OpenBrowser(url string) error {
	name, args := browserCommand(runtime.GOOS, url)
	return startDetached(name, args)
}

// browserCommand is the command that opens url in the default browser.
func browserCommand(goos, url string) (string, []string) {
	switch goos {
	case "windows":
		// rundll32 takes the URL as one argument; `cmd /c start` would
		// need & and ^ escaped.
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		return "open", []string{url}
	}
	return "xdg-open", []string{url}
}

// OpenAppWindow opens url as an app window (no tabs or address bar) in
// Edge or Chrome, falling back to the default browser. It returns what was
// used, for the console.
func OpenAppWindow(url string) (string, error) {
	name, args, label, ok := appCommand(runtime.GOOS, url, os.Getenv, pathExists, exec.LookPath)
	if ok {
		if err := startDetached(name, args); err == nil {
			return label + " app window", nil
		}
	}
	if err := OpenBrowser(url); err != nil {
		return "", err
	}
	return "default browser (no Edge or Chrome found for an app window)", nil
}

// pathExists reports whether a file or directory (a macOS .app bundle)
// exists.
func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// appCommand finds a Chromium-based browser that supports --app. exists
// and lookPath are injected for tests.
func appCommand(goos, url string, getenv func(string) string, exists func(string) bool, lookPath func(string) (string, error)) (name string, args []string, label string, ok bool) {
	appArgs := []string{"--app=" + url, "--new-window"}
	switch goos {
	case "windows":
		type cand struct{ env, rel, label string }
		edge := filepath.Join("Microsoft", "Edge", "Application", "msedge.exe")
		chrome := filepath.Join("Google", "Chrome", "Application", "chrome.exe")
		cands := []cand{
			{"ProgramFiles(x86)", edge, "Edge"}, {"ProgramFiles", edge, "Edge"}, {"LOCALAPPDATA", edge, "Edge"},
		}
		for _, c := range cands {
			if base := getenv(c.env); base != "" {
				if p := filepath.Join(base, c.rel); exists(p) {
					return p, appArgs, c.label, true
				}
			}
		}
		if p, err := lookPath("msedge"); err == nil {
			return p, appArgs, "Edge", true
		}
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"} {
			if base := getenv(env); base != "" {
				if p := filepath.Join(base, chrome); exists(p) {
					return p, appArgs, "Chrome", true
				}
			}
		}
		if p, err := lookPath("chrome"); err == nil {
			return p, appArgs, "Chrome", true
		}
	case "darwin":
		home := getenv("HOME")
		// macOS paths: always forward slashes (path, not filepath, so the
		// result does not depend on the OS this is built or tested on).
		for _, app := range []string{"Google Chrome", "Microsoft Edge", "Chromium", "Brave Browser"} {
			for _, dir := range []string{"/Applications", path.Join(home, "Applications")} {
				if exists(path.Join(dir, app+".app")) {
					return "open", append([]string{"-na", app, "--args"}, appArgs...), app, true
				}
			}
		}
	default:
		for _, bin := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge", "microsoft-edge-stable", "brave-browser"} {
			if p, err := lookPath(bin); err == nil {
				return p, appArgs, bin, true
			}
		}
	}
	return "", nil, "", false
}

// startDetached starts a process without waiting for it (it is reaped in
// the background). It starts outside rw's kill-on-close job: the browser
// it opens is the user's, and must not be killed when rw exits.
func startDetached(name string, args []string) error {
	_, err := startDetachedCmd(name, args)
	return err
}

func startDetachedCmd(name string, args []string) (*exec.Cmd, error) {
	if name == "" {
		return nil, errors.New("no command")
	}
	cmd := exec.Command(name, args...)
	proc.Breakaway(cmd)
	if err := cmd.Start(); err != nil {
		// A job rw was started in may forbid breakaway.
		cmd = exec.Command(name, args...)
		if err := cmd.Start(); err != nil {
			return nil, err
		}
	}
	go cmd.Wait()
	return cmd, nil
}
