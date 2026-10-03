package notify

import (
	"encoding/xml"
	"errors"
	"strings"
	"testing"
)

var tricky = []string{
	"plain",
	`it's "quoted"`,
	"line one\nline two\ttab",
	"<b>&amp; tags</b> & more > less",
	"Grüße – 日本語 ✓ 🚀",
	"smart ‘quotes’ ‚ ‛ “double”",
	"'); Remove-Item C:\\ -Recurse; ('",
	"$env:USERPROFILE `whoami` $(calc)",
	`back\slash "and" end\`,
	"bell\a and nul\x00 dropped",
	"-starts with dash",
}

// unPS parses the first PowerShell single-quoted literal starting at s[i]
// and returns its value and the index after it, using PowerShell's rule
// that any two adjacent single-quote characters stand for one.
func unPS(t *testing.T, s string, i int) (string, int) {
	t.Helper()
	isQ := func(r rune) bool { return strings.ContainsRune("'\u2018\u2019\u201A\u201B", r) }
	rs := []rune(s[i:])
	if len(rs) == 0 || !isQ(rs[0]) {
		t.Fatalf("no literal at %d: %q", i, s[i:])
	}
	var b strings.Builder
	for j := 1; j < len(rs); j++ {
		if isQ(rs[j]) {
			if j+1 < len(rs) && isQ(rs[j+1]) {
				b.WriteRune(rs[j+1])
				j++
				continue
			}
			return b.String(), i + len(string(rs[:j+1]))
		}
		b.WriteRune(rs[j])
	}
	t.Fatalf("unterminated literal in %q", s)
	return "", 0
}

// clean is what a string should look like after the round trip: control
// characters other than tab and newline are dropped.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func TestWindowsScriptRoundTrip(t *testing.T) {
	for _, title := range tricky {
		body := title + " (body)"
		script := windowsScript(title, body)
		for _, r := range script {
			if r > 0x7e || r < 0x20 || r == '"' {
				t.Fatalf("script has %q; want plain ASCII without double quotes:\n%s", r, script)
			}
		}
		const call = "$x.LoadXml("
		i := strings.Index(script, call)
		if i < 0 {
			t.Fatalf("no LoadXml in %s", script)
		}
		lit, end := unPS(t, script, i+len(call))
		if !strings.HasPrefix(script[end:], "); $t = New-Object") {
			t.Fatalf("PowerShell literal ended early for %q: rest %q", title, script[end:])
		}
		var toast struct {
			Binding struct {
				Template string   `xml:"template,attr"`
				Texts    []string `xml:"text"`
			} `xml:"visual>binding"`
		}
		if err := xml.Unmarshal([]byte(lit), &toast); err != nil {
			t.Fatalf("bad toast XML for %q: %v\n%s", title, err, lit)
		}
		b := toast.Binding
		if b.Template != "ToastGeneric" || len(b.Texts) != 2 {
			t.Fatalf("toast = %+v", toast)
		}
		if b.Texts[0] != clean(title) || b.Texts[1] != clean(body) {
			t.Errorf("round trip:\n got %q / %q\nwant %q / %q", b.Texts[0], b.Texts[1], clean(title), clean(body))
		}
	}
	s := windowsScript("t", "b")
	for _, want := range []string{
		"[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType=WindowsRuntime]",
		"CreateToastNotifier('" + powerShellAppID + "')",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q:\n%s", want, s)
		}
	}
}

func TestPSQuote(t *testing.T) {
	for _, s := range tricky {
		q := psQuote(s)
		got, end := unPS(t, q, 0)
		if got != s || end != len(q) {
			t.Errorf("psQuote(%q) = %s, parses back to %q", s, q, got)
		}
	}
}

// unAS reverses asQuote.
func unAS(t *testing.T, s string) (string, string) {
	t.Helper()
	if !strings.HasPrefix(s, `"`) {
		t.Fatalf("no literal: %q", s)
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
			b.WriteByte(map[byte]byte{'n': '\n', 'r': '\r', 't': '\t', '"': '"', '\\': '\\'}[s[i]])
		case '"':
			return b.String(), s[i+1:]
		default:
			b.WriteByte(s[i])
		}
	}
	t.Fatalf("unterminated: %q", s)
	return "", ""
}

func TestAppleScriptRoundTrip(t *testing.T) {
	for _, title := range tricky {
		body := "body: " + title
		s := appleScript(title, body)
		if strings.ContainsAny(s, "\n\r") {
			t.Errorf("raw newline in %q", s)
		}
		const pre = "display notification "
		if !strings.HasPrefix(s, pre) {
			t.Fatalf("got %q", s)
		}
		gotBody, rest := unAS(t, s[len(pre):])
		const mid = " with title "
		if !strings.HasPrefix(rest, mid) {
			t.Fatalf("literal ended early for %q: rest %q", title, rest)
		}
		gotTitle, rest := unAS(t, rest[len(mid):])
		if rest != "" {
			t.Fatalf("trailing %q", rest)
		}
		if gotTitle != clean(title) || gotBody != clean(body) {
			t.Errorf("round trip: got %q / %q", gotTitle, gotBody)
		}
	}
}

func TestCommand(t *testing.T) {
	found := func(string) (string, error) { return "/usr/bin/notify-send", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }

	name, args, err := command("windows", "t", "b", missing)
	if err != nil || name != "powershell.exe" || strings.Join(args[:5], " ") != "-NoProfile -NonInteractive -WindowStyle Hidden -Command" {
		t.Errorf("windows: %s %q %v", name, args, err)
	}
	name, args, err = command("darwin", "t", "b", missing)
	if err != nil || name != "osascript" || args[0] != "-e" {
		t.Errorf("darwin: %s %q %v", name, args, err)
	}
	name, args, err = command("linux", "-t", "b", found)
	if err != nil || name != "/usr/bin/notify-send" || strings.Join(args, "|") != "--app-name=Switchyard|--|-t|b" {
		t.Errorf("linux: %s %q %v", name, args, err)
	}
	if _, _, err := command("linux", "t", "b", missing); !errors.Is(err, ErrUnsupported) {
		t.Errorf("linux without notify-send: %v", err)
	}
	if _, _, err := command("plan9", "t", "b", found); !errors.Is(err, ErrUnsupported) {
		t.Errorf("plan9: %v", err)
	}
}

func TestBell(t *testing.T) {
	if Bell() != "\a" {
		t.Fatal("Bell")
	}
}
