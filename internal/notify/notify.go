// Package notify shows desktop notifications ("task finished") without
// extra dependencies, by shelling out to what each OS already ships.
package notify

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// ErrUnsupported means this machine has no way to show a notification
// (for example Linux without notify-send). Callers can fall back to Bell.
var ErrUnsupported = errors.New("notify: desktop notifications not supported here")

// timeout bounds a notification: PowerShell can take seconds to start on a
// cold machine, and a notification must never stall the caller for long.
const timeout = 10 * time.Second

// powerShellAppID is the AppUserModelID Windows registers for PowerShell.
// Toasts need a registered AppID; borrowing PowerShell's avoids having to
// install a Start-menu shortcut for rw.
const powerShellAppID = `{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe`

// Bell returns the terminal bell, the fallback that works everywhere.
func Bell() string { return "\a" }

// Send shows a desktop notification. It is best effort: it returns an
// error if the notification could not be shown and never blocks longer
// than about 10 seconds.
func Send(title, body string) error {
	name, args, err := command(runtime.GOOS, title, body, exec.LookPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd)
	// WaitDelay stops a grandchild that inherited our pipes from holding
	// CombinedOutput open after the timeout kill.
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("notify: %s timed out", name)
		}
		return fmt.Errorf("notify: %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// command picks the program and arguments for goos. lookPath is injected so
// the choice can be tested without the tools installed.
func command(goos, title, body string, lookPath func(string) (string, error)) (string, []string, error) {
	switch goos {
	case "windows":
		return "powershell.exe", []string{"-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", windowsScript(title, body)}, nil
	case "darwin":
		return "osascript", []string{"-e", appleScript(title, body)}, nil
	case "linux", "freebsd", "openbsd", "netbsd":
		p, err := lookPath("notify-send")
		if err != nil {
			return "", nil, ErrUnsupported
		}
		// "--" so a title starting with "-" is not read as an option.
		return p, []string{"--app-name=Relayweft", "--", argText(title), markupText(argText(body))}, nil
	}
	return "", nil, ErrUnsupported
}

// windowsScript builds a one-line PowerShell script that shows a WinRT
// toast. Title and body are XML-escaped with every non-ASCII, control and
// quote character written as a numeric character reference, so the
// script is plain ASCII with no quotes or newlines from the caller: it
// survives the Windows command line and console code pages intact and
// cannot end the PowerShell string early. psQuote is defence in depth.
func windowsScript(title, body string) string {
	xml := "<toast><visual><binding template='ToastGeneric'><text>" + xmlText(title) +
		"</text><text>" + xmlText(body) + "</text></binding></visual></toast>"
	return strings.Join([]string{
		"$ErrorActionPreference='Stop'",
		"[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType=WindowsRuntime] > $null",
		"[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType=WindowsRuntime] > $null",
		"$x = New-Object Windows.Data.Xml.Dom.XmlDocument",
		"$x.LoadXml(" + psQuote(xml) + ")",
		"$t = New-Object Windows.UI.Notifications.ToastNotification $x",
		"[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier(" + psQuote(powerShellAppID) + ").Show($t)",
	}, "; ")
}

// xmlText escapes s for XML character data and writes everything outside
// printable ASCII (plus quotes and PowerShell's special characters) as
// &#N;. Invalid UTF-8 becomes U+FFFD; control characters XML 1.0 forbids
// are dropped.
func xmlText(s string) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(s, "�") {
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r == '\n' || r == '\t':
			fmt.Fprintf(&b, "&#%d;", r)
		case r < 0x20 || r == 0x7f || r == 0xFFFE || r == 0xFFFF:
			// not allowed in XML 1.0; drop
		case r == '"' || r == '\'' || r == '`' || r == '$' || r > 0x7e:
			fmt.Fprintf(&b, "&#%d;", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// psQuote returns s as a PowerShell single-quoted string. PowerShell also
// treats the typographic quotes U+2018..U+201B as single quotes, so they
// are doubled too.
func psQuote(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\'', '‘', '’', '‚', '‛':
			b.WriteRune(r)
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}

// argText makes s safe as a program argument: valid UTF-8 without control
// characters other than newline and tab. A NUL byte in an argument makes
// exec fail outright ("invalid argument"), so the notification was lost.
func argText(s string) string {
	return strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
}

// markupText escapes s for a notification body: notification servers
// (GNOME Shell, KDE, dunst, mako) read the body as markup, so "Vec<T>"
// lost its "<T>" and "&amp;" showed as "&" (seen with dunst). The title is
// never markup.
func markupText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// appleScript builds a `display notification` statement. It is passed as
// an exec argument, not through a shell, so only AppleScript string
// escaping is needed.
func appleScript(title, body string) string {
	return "display notification " + asQuote(body) + " with title " + asQuote(title)
}

// asQuote returns s as an AppleScript string literal.
func asQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range strings.ToValidUTF8(s, "�") {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '"':
			b.WriteString(`\"`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			// drop other control characters
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
