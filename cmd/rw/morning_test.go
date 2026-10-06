package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPrintMorningSchedule(t *testing.T) {
	clock, _ := time.Parse("15:04", "07:30")
	var b bytes.Buffer
	printMorningSchedule(&b, "windows", `C:\Program Files\rw\rw.exe`, clock)
	if s := b.String(); !strings.Contains(s, `schtasks /create /tn "Relayweft morning" /sc daily /st 07:30 /tr "\"C:\Program Files\rw\rw.exe\" morning --send"`) ||
		!strings.Contains(s, "notify.morning") {
		t.Fatalf("windows:\n%s", s)
	}
	b.Reset()
	printMorningSchedule(&b, "linux", "/usr/local/bin/rw", clock)
	if s := b.String(); !strings.Contains(s, "30 7 * * * /usr/local/bin/rw morning --send") {
		t.Fatalf("linux:\n%s", s)
	}
	b.Reset()
	printMorningSchedule(&b, "windows", `C:\100%\rw.exe`, clock)
	if s := b.String(); strings.Contains(s, "schtasks /create") {
		t.Fatalf("a %% path got a command:\n%s", s)
	}
}

func TestCmdMorning(t *testing.T) {
	isolate(t)
	chdir(t, gitInit(t))
	out, err := captureStdout(t, func() error { return cmdMorning([]string{"--json", "--since", "2d"}) })
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Tasks []any `json:"tasks"`
		Since string
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.Tasks == nil {
		t.Fatalf("json: %v %s", err, out)
	}
	out, err = captureStdout(t, func() error { return cmdMorning(nil) })
	if err != nil || !strings.Contains(out, "nothing ran unattended since") || !strings.Contains(out, "rw morning --all") {
		t.Fatalf("text: %v %s", err, out)
	}
	out, err = captureStdout(t, func() error { return cmdMorning([]string{"--send"}) })
	if err != nil || !strings.Contains(out, "nothing sent") {
		t.Fatalf("send: %v %s", err, out)
	}
	if err := cmdMorning([]string{"--since", "yesterday-ish"}); err == nil {
		t.Fatal("a bad --since was accepted")
	}
}

func TestPrintNewLinksPhone(t *testing.T) {
	var out bytes.Buffer
	printNewLinks(strings.NewReader("p\n\n"), &out, func() string { return "L" }, func() string { return "http://100.64.0.1:9/#b=ab" })
	s := out.String()
	if !strings.Contains(s, "Scan with your phone") || !strings.Contains(s, "▀") || !strings.Contains(s, "open: L") {
		t.Fatalf("output:\n%s", s)
	}
}
