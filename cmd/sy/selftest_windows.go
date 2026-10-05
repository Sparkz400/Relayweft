//go:build windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// writeAgentShim writes <name>.cmd the way npm writes CLI shims: a batch
// file that starts the real program with %*. A name other than claude is
// passed on in SY_SELFTEST_AS.
func writeAgentShim(dir, bin, name string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	rel, err := filepath.Rel(dir, bin)
	if err != nil {
		return "", err
	}
	shim := filepath.Join(dir, name+".cmd")
	body := "@ECHO off\r\n"
	if name != "claude" {
		body += "set \"" + envSelftestAs + "=" + name + "\"\r\n"
	}
	body += "\"%~dp0\\" + rel + "\" " + selftestAgentCmd + " %*\r\n"
	return shim, os.WriteFile(shim, []byte(body), 0o755)
}

type defenderInfo struct {
	RealTime   bool     `json:"rt"`
	Mode       string   `json:"mode"`
	Exclusions []string `json:"ex"`
}

const defenderScript = `$ErrorActionPreference='Stop'; $s=Get-MpComputerStatus; $p=Get-MpPreference; ` +
	`[pscustomobject]@{rt=[bool]$s.RealTimeProtectionEnabled; mode=[string]$s.AMRunningMode; ex=@($p.ExclusionPath | Where-Object {$_})} | ConvertTo-Json -Compress`

// defender reports Microsoft Defender's real-time protection and whether
// sy's folders are excluded. Exclusions are readable only as administrator.
func (t *selftest) defender() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", defenderScript)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.Output()
	var d defenderInfo
	if err == nil {
		err = json.Unmarshal(out, &d)
	}
	if err != nil {
		t.check(markInfo, "defender", "status unknown (%v): Defender may be replaced by another antivirus", firstErrLine(err))
		return
	}
	if !d.RealTime {
		t.check(markWarn, "defender", "real-time protection is off (mode %q): the checks below ran without it; run sy selftest again with it on", d.Mode)
		return
	}
	t.check(markOK, "defender", "real-time protection is on (mode %s): the checks below run with it on", d.Mode)

	cacheDir, _ := os.UserCacheDir()
	pool := filepath.Join(cacheDir, "switchyard")
	hidden := false
	var excl []string
	for _, e := range d.Exclusions {
		if strings.HasPrefix(e, "N/A") {
			hidden = true
			continue
		}
		excl = append(excl, e)
	}
	tip := fmt.Sprintf("optional, to make pool checkouts faster (only for folders you trust), in an administrator PowerShell: Add-MpPreference -ExclusionPath '%s'", pool)
	switch {
	case hidden:
		t.check(markInfo, "exclusions", "only readable as administrator; %s", tip)
	case excludedBy(pool, excl) != "":
		t.check(markInfo, "exclusions", "sy's worktree pool %s is excluded (%s)", pool, excludedBy(pool, excl))
	default:
		t.check(markInfo, "exclusions", "sy's worktree pool %s is scanned; %s", pool, tip)
	}
}

func excludedBy(path string, exclusions []string) string {
	for _, e := range exclusions {
		if within(path, os.ExpandEnv(strings.ReplaceAll(e, "%", "$"))) {
			return e
		}
	}
	return ""
}

func firstErrLine(err error) string {
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		return oneLine(string(ee.Stderr), 160)
	}
	return oneLine(err.Error(), 160)
}
