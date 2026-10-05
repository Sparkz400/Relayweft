package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// sySetup writes a user folder the way sy (v0.2.0) left it under base and
// returns its path.
func sySetup(t *testing.T, base string) string {
	t.Helper()
	old := filepath.Join(base, "switchyard")
	writeFile(t, filepath.Join(old, "switchyard.yaml"), "routing:\n  judge: true\n")
	writeFile(t, filepath.Join(old, "switchyard.yaml.bak-20261004-120000"), "# before rw setup\n")
	writeFile(t, filepath.Join(old, "logs", "sy-health.log"), "2026-10-04 18:18:47.823 start pid=1 ver=0.2.0 cmd=health\n")
	writeFile(t, filepath.Join(old, "logs", "sy-debug.log"), "2026-10-04 18:18:47.823 === sy 0.2.0 health\n")
	writeFile(t, filepath.Join(old, "logs", "sy-debug.log.1"), "older\n")
	writeFile(t, filepath.Join(old, "logs", "crash-20261004-165400.657.log"), "Switchyard 0.2.0 crashed in main\n")
	writeFile(t, filepath.Join(old, "tasks", "20261004-174111-e746-task-1.json"), `{"id":"20261004-174111-e746-task-1"}`)
	writeFile(t, filepath.Join(old, "tasks", "20261004-174111-e746-task-1.lock"), "")
	writeFile(t, filepath.Join(old, "tasks", "sessions", "abc.json"), `{}`)
	writeFile(t, filepath.Join(old, "sessions", "2026-10-04.jsonl"), `{"task":"x"}`+"\n")
	writeFile(t, filepath.Join(old, "learned", "0123.yaml"), "roles: {}\n")
	writeFile(t, filepath.Join(old, "trusted.json.tmp"), "half written")
	trust := map[string]string{
		"c:/users/jörg müller/repo/.switchyard.yaml": "h-repo",
		"c:/users/jörg müller/proj/switchyard.yaml":  "local:h-local",
		"//server/share/team repo/.switchyard.yaml":  "h-unc",
		"/home/me/repo/.switchyard.yaml":             "h-unix",
		"d:/other/.relayweft.yaml":                   "h-new",
		"c:/x/notswitchyard.yaml":                    "h-other",
		"c:/y/.switchyard.yaml.bak":                  "h-bak",
		"c:/z/.switchyard.yaml":                      "h-z-old",
		"c:/z/.relayweft.yaml":                       "h-z-new",
	}
	b, _ := json.Marshal(trust)
	writeFile(t, filepath.Join(old, "trusted.json"), string(b))
	past := time.Date(2026, 10, 4, 18, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(old, "logs", "sy-health.log"), past, past); err != nil {
		t.Fatal(err)
	}
	return old
}

// tree lists the files under dir (slash paths) with their content.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = readFile(t, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMigrateSwitchyardCopies(t *testing.T) {
	// A Windows profile path with a space and non-ASCII letters.
	base := filepath.Join(t.TempDir(), "Jörg Müller", "AppData", "Roaming")
	old := sySetup(t, base)
	before := tree(t, old)

	msg, err := migrateSwitchyard(base)
	if err != nil {
		t.Fatal(err)
	}
	to := filepath.Join(base, "relayweft")
	for _, want := range []string{old, to, "unchanged", ".switchyard.yaml", ".relayweft.yaml", "trust of 4"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not say %q", msg, want)
		}
	}
	if strings.Contains(msg, "\n") {
		t.Errorf("message is more than one line: %q", msg)
	}

	got := tree(t, to)
	for name, want := range map[string]string{
		"relayweft.yaml":                         "routing:\n  judge: true\n",
		"relayweft.yaml.bak-20261004-120000":     "# before rw setup\n",
		"logs/rw-health.log":                     before["logs/sy-health.log"],
		"logs/rw-debug.log":                      before["logs/sy-debug.log"],
		"logs/rw-debug.log.1":                    "older\n",
		"logs/crash-20261004-165400.657.log":     before["logs/crash-20261004-165400.657.log"],
		"tasks/20261004-174111-e746-task-1.json": before["tasks/20261004-174111-e746-task-1.json"],
		"tasks/sessions/abc.json":                "{}",
		"sessions/2026-10-04.jsonl":              before["sessions/2026-10-04.jsonl"],
		"learned/0123.yaml":                      "roles: {}\n",
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
	for _, name := range []string{"switchyard.yaml", "logs/sy-health.log", "tasks/20261004-174111-e746-task-1.lock", "trusted.json.tmp"} {
		if _, ok := got[name]; ok {
			t.Errorf("%s was copied", name)
		}
	}
	// The health log keeps its time (rw health reads the streak from it).
	st, err := os.Stat(filepath.Join(to, "logs", "rw-health.log"))
	if err != nil || !st.ModTime().Equal(time.Date(2026, 10, 4, 18, 0, 0, 0, time.UTC)) {
		t.Errorf("rw-health.log time = %v, %v", st.ModTime(), err)
	}

	var trust map[string]string
	if err := json.Unmarshal([]byte(got["trusted.json"]), &trust); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"c:/users/jörg müller/repo/.relayweft.yaml": "h-repo",
		"c:/users/jörg müller/proj/relayweft.yaml":  "local:h-local",
		"//server/share/team repo/.relayweft.yaml":  "h-unc",
		"/home/me/repo/.relayweft.yaml":             "h-unix",
		"d:/other/.relayweft.yaml":                  "h-new",
		"c:/x/notswitchyard.yaml":                   "h-other",
		"c:/y/.switchyard.yaml.bak":                 "h-bak",
		"c:/z/.relayweft.yaml":                      "h-z-new", // rw's own entry wins
	}
	if len(trust) != len(want) {
		t.Errorf("trust = %v", trust)
	}
	for k, v := range want {
		if trust[k] != v {
			t.Errorf("trust[%s] = %q, want %q", k, trust[k], v)
		}
	}

	// sy's folder is exactly as it was.
	after := tree(t, old)
	if len(after) != len(before) {
		t.Errorf("the old folder changed: %v", after)
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("old %s changed", k)
		}
	}
	leftovers, _ := filepath.Glob(filepath.Join(base, "relayweft.migrating-*"))
	if len(leftovers) > 0 {
		t.Errorf("temporary folders left: %v", leftovers)
	}

	// Once is enough: the next start leaves everything alone.
	writeFile(t, filepath.Join(to, "relayweft.yaml"), "edited in rw\n")
	if msg, err := migrateSwitchyard(base); msg != "" || err != nil {
		t.Errorf("second start: %q, %v", msg, err)
	}
	if readFile(t, filepath.Join(to, "relayweft.yaml")) != "edited in rw\n" {
		t.Error("the second start overwrote rw's config")
	}
}

func TestMigrateSwitchyardNeverOverwrites(t *testing.T) {
	base := t.TempDir()
	sySetup(t, base)
	to := filepath.Join(base, "relayweft")
	writeFile(t, filepath.Join(to, "logs", "rw-debug.log"), "rw's own\n")
	if msg, err := migrateSwitchyard(base); msg != "" || err != nil {
		t.Fatalf("with rw's folder there: %q, %v", msg, err)
	}
	if got := tree(t, to); len(got) != 1 || got["logs/rw-debug.log"] != "rw's own\n" {
		t.Errorf("rw's folder changed: %v", got)
	}

	// A file in the way is not replaced either.
	base = t.TempDir()
	sySetup(t, base)
	writeFile(t, filepath.Join(base, "relayweft"), "a file")
	if msg, err := migrateSwitchyard(base); msg != "" || err != nil {
		t.Fatalf("with a file named relayweft: %q, %v", msg, err)
	}
}

func TestMigrateSwitchyardNothingToDo(t *testing.T) {
	base := t.TempDir()
	if msg, err := migrateSwitchyard(base); msg != "" || err != nil {
		t.Fatalf("no sy folder: %q, %v", msg, err)
	}
	if _, err := os.Stat(filepath.Join(base, "relayweft")); !os.IsNotExist(err) {
		t.Error("rw's folder was created without anything to copy")
	}
	// A file named switchyard is not sy's folder.
	writeFile(t, filepath.Join(base, "switchyard"), "x")
	if msg, err := migrateSwitchyard(base); msg != "" || err != nil {
		t.Fatalf("a file named switchyard: %q, %v", msg, err)
	}
}

// Two rw starting at once (the editor extension and a terminal): one
// copies, the other leaves the result alone, and nothing is half done.
func TestMigrateSwitchyardConcurrent(t *testing.T) {
	base := t.TempDir()
	sySetup(t, base)
	var wg sync.WaitGroup
	msgs := make([]string, 4)
	errs := make([]error, 4)
	for i := range msgs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			msgs[i], errs[i] = migrateSwitchyard(base)
		}()
	}
	wg.Wait()
	n := 0
	for i := range msgs {
		if errs[i] != nil {
			t.Errorf("start %d: %v", i, errs[i])
		}
		if msgs[i] != "" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d starts copied, want 1", n)
	}
	if got := tree(t, filepath.Join(base, "relayweft")); got["relayweft.yaml"] == "" || got["logs/rw-health.log"] == "" {
		t.Errorf("incomplete copy: %v", got)
	}
	leftovers, _ := filepath.Glob(filepath.Join(base, "relayweft.migrating-*"))
	if len(leftovers) > 0 {
		t.Errorf("temporary folders left: %v", leftovers)
	}
}

// isolateUserConfig points os.UserConfigDir at a fresh folder and returns
// it: %APPDATA% on Windows.
func isolateUserConfig(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Roaming")
	switch runtime.GOOS {
	case "windows":
		t.Setenv("APPDATA", dir)
	case "darwin", "ios":
		t.Setenv("HOME", dir)
		dir = filepath.Join(dir, "Library", "Application Support")
	case "plan9":
		t.Skip("no user config dir convention")
	default:
		t.Setenv("XDG_CONFIG_HOME", dir)
	}
	got, err := os.UserConfigDir()
	if err != nil || got != dir {
		t.Fatalf("UserConfigDir = %q, %v; want %q", got, err, dir)
	}
	return dir
}

// End to end through the user config dir (%APPDATA%\switchyard to
// %APPDATA%\relayweft on Windows): the copied config loads, and a repo's
// .switchyard.yaml renamed without changes keeps its trust.
func TestMigrateSwitchyardUserConfigAndTrust(t *testing.T) {
	cfgDir := isolateUserConfig(t)
	repo := filepath.Join(t.TempDir(), "my repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	repoFile := "verify:\n  commands: [go test ./...]\n"
	writeFile(t, filepath.Join(repo, ".switchyard.yaml"), repoFile)
	local := "hooks:\n  after_task: [echo done]\n"
	writeFile(t, filepath.Join(repo, "switchyard.yaml"), local)
	lh, err := localHash([]byte(local))
	if err != nil {
		t.Fatal(err)
	}
	// sy's trust list, keyed as sy keyed it (trustKey of its file names).
	trust := map[string]string{
		trustKey(filepath.Join(repo, ".switchyard.yaml")): contentHash([]byte(repoFile)),
		trustKey(filepath.Join(repo, "switchyard.yaml")):  lh,
	}
	b, _ := json.Marshal(trust)
	writeFile(t, filepath.Join(cfgDir, "switchyard", "trusted.json"), string(b))
	writeFile(t, filepath.Join(cfgDir, "switchyard", "switchyard.yaml"), "routing:\n  judge: true\n")

	// Before renaming: rw says the files are not read.
	if p := LegacyRepoFile(filepath.Join(repo, "sub")); p != filepath.Join(repo, ".switchyard.yaml") {
		t.Errorf("LegacyRepoFile = %q", p)
	}
	if info, err := ApplyRepo(Default(), repo); err != nil || info.Path != "" || info.Legacy != filepath.Join(repo, ".switchyard.yaml") {
		t.Errorf("ApplyRepo = %+v, %v", info, err)
	}
	if p := LegacyLocalFile(repo); p != filepath.Join(repo, "switchyard.yaml") {
		t.Errorf("LegacyLocalFile = %q", p)
	}

	msg, err := MigrateSwitchyard()
	if err != nil || !strings.Contains(msg, filepath.Join(cfgDir, "relayweft")) {
		t.Fatalf("MigrateSwitchyard = %q, %v", msg, err)
	}
	c, p, err := Load("")
	if err != nil || p != filepath.Join(cfgDir, "relayweft", FileName) || !c.Routing.Judge {
		t.Errorf("Load after the copy: %q, judge=%v, %v", p, c != nil && c.Routing.Judge, err)
	}

	// The user renames the files: unchanged content keeps its trust.
	for _, n := range [][2]string{{".switchyard.yaml", ".relayweft.yaml"}, {"switchyard.yaml", "relayweft.yaml"}} {
		if err := os.Rename(filepath.Join(repo, n[0]), filepath.Join(repo, n[1])); err != nil {
			t.Fatal(err)
		}
	}
	if !IsTrusted(filepath.Join(repo, ".relayweft.yaml"), []byte(repoFile)) {
		t.Error("the renamed repo file lost its trust")
	}
	if !IsLocalTrusted(filepath.Join(repo, "relayweft.yaml"), []byte(local)) {
		t.Error("the renamed local file lost its trust")
	}
	if IsTrusted(filepath.Join(repo, ".relayweft.yaml"), []byte(repoFile+"hooks:\n  after_task: [curl evil]\n")) {
		t.Error("changed content is trusted")
	}
	if info, err := ApplyRepo(Default(), repo); err != nil || !info.Trusted || info.Legacy != "" {
		t.Errorf("ApplyRepo after the rename = %+v, %v", info, err)
	}
	if LegacyRepoFile(repo) != "" || LegacyLocalFile(repo) != "" {
		t.Error("renamed files still reported as sy's")
	}
}

func TestMigrateTrustKey(t *testing.T) {
	for k, want := range map[string]string{
		"c:/a/.switchyard.yaml":        "c:/a/.relayweft.yaml",
		"c:/a/switchyard.yaml":         "c:/a/relayweft.yaml",
		"//srv/share/.switchyard.yaml": "//srv/share/.relayweft.yaml",
		".switchyard.yaml":             ".relayweft.yaml",
		"c:/a/my.switchyard.yaml":      "",
		"c:/switchyard/x.yaml":         "",
		"c:/a/.switchyard.yaml.bak":    "",
	} {
		got, ok := migrateTrustKey(k)
		if ok != (want != "") || got != want {
			t.Errorf("migrateTrustKey(%q) = %q, %v; want %q", k, got, ok, want)
		}
	}
}
