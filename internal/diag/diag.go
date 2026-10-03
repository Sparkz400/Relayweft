// Package diag is Switchyard's flight recorder: a rotating debug log of every
// command, phase and error, crash logs for panics, and the files that
// `sy bugreport` bundles. It never fails the caller: if the log cannot be
// written, diagnostics are silently kept in memory only.
package diag

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

const (
	debugFile = "sy-debug.log"
	maxSize   = 10 << 20 // rotate to sy-debug.log.1 above this
	ringSize  = 400      // recent lines kept in memory for crash logs
)

var (
	mu      sync.Mutex
	f       *os.File
	dir     string
	written int64
	ring    []string
	// Version is printed in crash logs and bug reports.
	Version = "dev"
)

// DefaultDir is <user config dir>/switchyard/logs (%AppData% on Windows).
func DefaultDir() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "switchyard", "logs")
	}
	return filepath.Join(os.TempDir(), "switchyard-logs")
}

// Init opens the debug log in d (created if needed). Safe to call again.
func Init(d string) error {
	mu.Lock()
	defer mu.Unlock()
	if f != nil {
		f.Close()
		f = nil
	}
	dir = d
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	path := filepath.Join(d, debugFile)
	if st, err := os.Stat(path); err == nil && st.Size() > maxSize {
		os.Rename(path, path+".1")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	f = file
	if st, err := file.Stat(); err == nil {
		written = st.Size()
	}
	return nil
}

// Dir returns the log directory ("" before Init).
func Dir() string {
	mu.Lock()
	defer mu.Unlock()
	return dir
}

// Close flushes and closes the debug log.
func Close() {
	mu.Lock()
	defer mu.Unlock()
	if f != nil {
		f.Close()
		f = nil
	}
}

// Logf appends one timestamped line.
func Logf(format string, args ...any) {
	line := time.Now().Format("2006-01-02 15:04:05.000") + " " + strings.TrimRight(fmt.Sprintf(format, args...), "\n")
	line = strings.ReplaceAll(line, "\n", "\n    ")
	mu.Lock()
	defer mu.Unlock()
	ring = append(ring, line)
	if len(ring) > ringSize {
		ring = ring[len(ring)-ringSize:]
	}
	if f == nil {
		return
	}
	n, _ := f.WriteString(line + "\n")
	written += int64(n)
	if written > maxSize {
		path := filepath.Join(dir, debugFile)
		f.Close()
		os.Rename(path, path+".1")
		if nf, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644); err == nil {
			f, written = nf, 0
		} else {
			f = nil
		}
	}
}

// Recent returns the last lines logged in this process.
func Recent() []string {
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), ring...)
}

// Crash writes crash-<time>.log with the panic, its stack and the recent
// debug lines, and returns its path ("" if it could not be written).
func Crash(where string, value any, stack []byte) string {
	Logf("PANIC in %s: %v", where, value)
	var b strings.Builder
	fmt.Fprintf(&b, "Switchyard %s crashed in %s at %s\n", Version, where, time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "os %s/%s, go %s\n\npanic: %v\n\n%s\n", runtime.GOOS, runtime.GOARCH, runtime.Version(), value, stack)
	b.WriteString("\n--- recent debug log ---\n")
	for _, l := range Recent() {
		b.WriteString(l + "\n")
	}
	d := Dir()
	if d == "" {
		d = DefaultDir()
		os.MkdirAll(d, 0o755)
	}
	path := filepath.Join(d, "crash-"+time.Now().Format("20060102-150405.000")+".log")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return ""
	}
	return path
}

// Recover is deferred at the top of goroutines: a panic is written to a
// crash log and reported through onPanic instead of killing sy (which would
// leave the terminal in raw mode and agents running). onPanic may be nil.
func Recover(where string, onPanic func(crashLog string)) {
	if r := recover(); r != nil {
		path := Crash(where, r, debug.Stack())
		if onPanic != nil {
			onPanic(path)
		}
	}
}
