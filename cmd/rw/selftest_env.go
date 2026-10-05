package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// oneDriveRoots returns the OneDrive folders of this user (personal and
// work or school), from the variables the OneDrive client sets.
func oneDriveRoots() []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	var roots []string
	seen := map[string]bool{}
	for _, k := range []string{"OneDrive", "OneDriveConsumer", "OneDriveCommercial"} {
		v := strings.TrimSpace(os.Getenv(k))
		if v == "" {
			continue
		}
		v = filepath.Clean(v)
		if st, err := os.Stat(v); err != nil || !st.IsDir() || seen[strings.ToLower(v)] {
			continue
		}
		seen[strings.ToLower(v)] = true
		roots = append(roots, v)
	}
	return roots
}

// inOneDrive reports the OneDrive folder that contains path, if any.
func inOneDrive(path string) (string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	for _, root := range oneDriveRoots() {
		if within(abs, root) {
			return root, true
		}
		if r, err := filepath.EvalSymlinks(root); err == nil && within(abs, r) {
			return root, true
		}
	}
	return "", false
}

func within(path, root string) bool {
	p, r := strings.ToLower(filepath.Clean(path)), strings.ToLower(filepath.Clean(root))
	return p == r || strings.HasPrefix(p, strings.TrimRight(r, `\/`)+string(filepath.Separator))
}

const oneDriveAdvice = "OneDrive syncs every file the agents change and can hold files open while git or rw writes them " +
	"(\"access denied\", \"being used by another process\"); keep repos outside OneDrive, or pause syncing while tasks run"

// environment reports what on this machine tends to break Windows tools.
func (t *selftest) environment() {
	if runtime.GOOS != "windows" {
		t.check(markSkip, "onedrive", "Windows only")
		t.check(markSkip, "defender", "Windows only")
		return
	}
	roots := oneDriveRoots()
	if len(roots) == 0 {
		t.check(markOK, "onedrive", "no OneDrive folder on this account")
	} else {
		wd, _ := os.Getwd()
		cfgDir, _ := os.UserConfigDir()
		cacheDir, _ := os.UserCacheDir()
		self, _ := os.Executable()
		found := false
		for _, c := range []struct{ what, path string }{
			{"the current folder", wd},
			{"rw's config, task states and logs", filepath.Join(cfgDir, "relayweft")},
			{"rw's worktree pool", filepath.Join(cacheDir, "relayweft")},
			{"rw itself", self},
		} {
			if c.path == "" {
				continue
			}
			if root, in := inOneDrive(c.path); in {
				found = true
				t.check(markWarn, "onedrive", "%s is inside OneDrive (%s): %s", c.what, root, oneDriveAdvice)
			}
		}
		if !found {
			t.check(markOK, "onedrive", "OneDrive at %s; this folder and rw's own folders are outside it", strings.Join(roots, ", "))
		}
	}
	t.defender()

	term := "Windows Terminal"
	if os.Getenv("WT_SESSION") == "" {
		term = "not Windows Terminal (the old console, or another terminal)"
	}
	t.check(markInfo, "terminal", "%s", term)
	if home, err := os.UserHomeDir(); err == nil && !strings.ContainsAny(home, " ()") {
		t.check(markInfo, "user name", "your profile %s has no space; the test profile covers that case", home)
	}
}
