package notify

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestRealDesktopToast shows real toasts on this machine and, on Windows,
// reads them back from the notification history to prove they were
// delivered (a toast with an unregistered AppUserModelID is dropped
// without any error). It is opt-in: SY_REAL_DESKTOP=1 go test -run RealDesktop
func TestRealDesktopToast(t *testing.T) {
	if os.Getenv("SY_REAL_DESKTOP") != "1" {
		t.Skip("set SY_REAL_DESKTOP=1 to show real desktop notifications")
	}
	marker := fmt.Sprintf("sytest%d", time.Now().UnixNano())
	cases := []struct{ name, title, body string }{
		{"plain", "Switchyard: done", "plain body " + marker},
		{"quotes", `He said "hi" & it's 'ok' ` + "`$x` $(calc) “curly” ‘single’", "quotes " + marker},
		{"xml", "<b>bold</b> & <!-- c --> ]]>", "xml " + marker + " <toast/> &amp;"},
		{"unicode", "Grüße ä ö ü ß 日本語 🚀", "unicode " + marker + " Ελληνικά"},
		{"newlines", "line1\nline2", "a\nb\r\nc\td " + marker},
		{"long", strings.Repeat("T", 300), strings.Repeat("long body ", 200) + marker},
		{"control", "bell\a nul\x00 esc\x1b", "ctl " + marker + "\x01\x7f"},
	}
	for _, c := range cases {
		start := time.Now()
		if err := Send(c.title, c.body); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		t.Logf("%s: sent in %v", c.name, time.Since(start).Round(time.Millisecond))
	}
	if runtime.GOOS != "windows" {
		return
	}
	script := "[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType=WindowsRuntime] > $null; " +
		"[Windows.UI.Notifications.ToastNotificationManager]::History.GetHistory(" + psQuote(powerShellAppID) + ") | " +
		"ForEach-Object { $_.Content.GetXml() }"
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		t.Fatalf("read toast history: %v: %s", err, out)
	}
	got := strings.Count(string(out), marker)
	t.Logf("history holds %d of %d toasts with marker %s", got, len(cases), marker)
	if got != len(cases) {
		t.Errorf("only %d of %d toasts reached the notification history (notifications for Windows PowerShell disabled?)\n%s", got, len(cases), out)
	}
}
