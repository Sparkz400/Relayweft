package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.0.0", "1.0.1", true},
		{"1.0.1", "1.0.0", false},
		{"1.0.0", "1.0.0", false},
		{"v1.2.0", "1.10.0", true}, // numeric, not lexical
		{"1.10.0", "1.9.9", false},
		{"1.2", "1.2.1", true},
		{"1.2.0", "1.2", false},
		{"2.0.0", "10.0.0", true},
		{"1.0.0-rc1", "1.0.0", true},
		{"1.0.0", "1.0.0-rc1", false},
		{"1.0.0-rc1", "1.0.0-rc2", true},
		{"1.0.0+build5", "1.0.0", false},
		{"dev", "0.0.1", true},
		{"abc1234", "1.0.0", true}, // CI builds stamp a commit hash
		{"1.0.0", "garbage", false},
		{"dev", "garbage", false},
	}
	for _, c := range cases {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Errorf("versionLess(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	m := parseChecksums([]byte("ABCDEF  rw-linux-amd64\n0123 *rw-windows-amd64.exe\n\nbad line here x\n"))
	if m["rw-linux-amd64"] != "abcdef" || m["rw-windows-amd64.exe"] != "0123" || len(m) != 2 {
		t.Fatalf("parseChecksums = %v", m)
	}
}

// fakeRelease serves a latest-release JSON plus its assets. Set sum to
// override the checksum written for the binary.
type fakeRelease struct {
	tag     string
	binary  []byte
	sum     string
	status  int  // non-zero: the API answers with this status
	signed  bool // the release has checksums.txt.sigstore.json (v0.4.0 and later)
	gotAuth string
}

func (f *fakeRelease) start(t *testing.T) *httptest.Server {
	t.Helper()
	name := assetName(runtime.GOOS, runtime.GOARCH)
	sum := f.sum
	if sum == "" {
		sum = fmt.Sprintf("%x", sha256.Sum256(f.binary))
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			f.gotAuth = r.Header.Get("Authorization")
			if f.status != 0 {
				w.WriteHeader(f.status)
				return
			}
			assets := []ghAsset{
				{Name: name, BrowserDownloadURL: srv.URL + "/dl/" + name},
				{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/dl/checksums.txt"},
			}
			if f.signed {
				// Never downloaded: rw update does not verify it.
				assets = append(assets, ghAsset{Name: signatureAsset, BrowserDownloadURL: srv.URL + "/dl/" + signatureAsset})
			}
			json.NewEncoder(w).Encode(ghRelease{
				TagName: f.tag,
				HTMLURL: srv.URL + "/notes",
				Assets:  assets,
			})
		case "/dl/" + name:
			w.Write(f.binary)
		case "/dl/checksums.txt":
			fmt.Fprintf(w, "%s  %s\n%s  rw-other-thing\n", sum, name, strings.Repeat("0", 64))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// withUpdater points the updater at srv and a fake binary in a temp dir,
// so tests never touch the real executable. It returns the fake binary's
// path and the captured output.
func withUpdater(t *testing.T, srv *httptest.Server, cur, stdin string) (string, *bytes.Buffer) {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	exe := filepath.Join(t.TempDir(), "rw.exe")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	oldAPI, oldTarget, oldIn, oldOut, oldVer, oldQuery := updateAPI, updateTarget, updateIn, updateOut, version, updateOwnerQuery
	t.Cleanup(func() {
		updateAPI, updateTarget, updateIn, updateOut, version, updateOwnerQuery = oldAPI, oldTarget, oldIn, oldOut, oldVer, oldQuery
	})
	updateOwnerQuery = func(string, ...string) (string, error) { return "", errors.New("not owned") }
	updateAPI = srv.URL + "/latest"
	updateTarget = func() (string, error) { return exe, nil }
	updateIn = strings.NewReader(stdin)
	updateOut = out
	version = cur
	return exe, out
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpdateCheck(t *testing.T) {
	f := &fakeRelease{tag: "v1.4.0", binary: []byte("new")}
	srv := f.start(t)
	exe, out := withUpdater(t, srv, "1.3.2", "")
	if err := cmdUpdate([]string{"--check"}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"current: 1.3.2", "latest:  1.4.0", "update is available", "/notes"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	if readFile(t, exe) != "old binary" {
		t.Error("--check changed the binary")
	}

	out.Reset()
	version = "1.4.0"
	if err := cmdUpdate([]string{"--check"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "up to date") {
		t.Errorf("want up to date, got:\n%s", out)
	}

	out.Reset()
	version = "dev"
	if err := cmdUpdate([]string{"--check"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "development build") || !strings.Contains(out.String(), "update is available") {
		t.Errorf("dev build output:\n%s", out)
	}
}

func TestUpdateSendsToken(t *testing.T) {
	f := &fakeRelease{tag: "v1.0.0", binary: []byte("new")}
	srv := f.start(t)
	withUpdater(t, srv, "1.0.0", "")
	t.Setenv("GH_TOKEN", "secret")
	if err := cmdUpdate([]string{"--check"}); err != nil {
		t.Fatal(err)
	}
	if f.gotAuth != "Bearer secret" {
		t.Errorf("Authorization = %q", f.gotAuth)
	}
}

func TestUpdateReplacesBinary(t *testing.T) {
	f := &fakeRelease{tag: "v2.0.0", binary: []byte("brand new binary")}
	srv := f.start(t)
	exe, out := withUpdater(t, srv, "1.0.0", "y\n")
	if err := cmdUpdate(nil); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := readFile(t, exe); got != "brand new binary" {
		t.Fatalf("binary = %q", got)
	}
	if !strings.Contains(out.String(), "sha256 verified") {
		t.Errorf("output:\n%s", out)
	}
	// A release from before signing (v0.3.0 and older) updates as before,
	// without a provenance hint.
	if strings.Contains(out.String(), "attestation") {
		t.Errorf("unsigned release got a provenance hint:\n%s", out)
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Error(".new left behind")
	}
	if runtime.GOOS == "windows" {
		if readFile(t, exe+".old") != "old binary" {
			t.Error(".old should hold the previous binary")
		}
		cleanupOldBinary()
		if _, err := os.Stat(exe + ".old"); !os.IsNotExist(err) {
			t.Error("cleanupOldBinary left .old")
		}
	}
}

func TestUpdateSignedReleaseHint(t *testing.T) {
	f := &fakeRelease{tag: "v2.0.0", binary: []byte("signed release"), signed: true}
	srv := f.start(t)
	exe, out := withUpdater(t, srv, "1.0.0", "y\n")
	if err := cmdUpdate(nil); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := readFile(t, exe); got != "signed release" {
		t.Fatalf("binary = %q", got)
	}
	// The command checks the installed file, with the path as it is (no
	// doubled backslashes on Windows).
	want := `gh attestation verify "` + exe + `" --repo Sparkz400/Relayweft`
	if !strings.Contains(out.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}

	// The signature changes nothing about the checksum check.
	f2 := &fakeRelease{tag: "v2.0.0", binary: []byte("tampered"), sum: strings.Repeat("ab", 32), signed: true}
	srv2 := f2.start(t)
	exe2, out2 := withUpdater(t, srv2, "1.0.0", "y\n")
	if err := cmdUpdate(nil); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if readFile(t, exe2) != "old binary" || strings.Contains(out2.String(), "attestation") {
		t.Errorf("a mismatch replaced the binary or printed the hint:\n%s", out2)
	}
}

func TestUpdateDeclined(t *testing.T) {
	f := &fakeRelease{tag: "v2.0.0", binary: []byte("new")}
	srv := f.start(t)
	exe, out := withUpdater(t, srv, "1.0.0", "n\n")
	if err := cmdUpdate(nil); err != nil {
		t.Fatal(err)
	}
	if readFile(t, exe) != "old binary" || !strings.Contains(out.String(), "Cancelled") {
		t.Errorf("declined update changed things:\n%s", out)
	}
}

func TestUpdateChecksumMismatch(t *testing.T) {
	f := &fakeRelease{tag: "v2.0.0", binary: []byte("tampered"), sum: strings.Repeat("ab", 32)}
	srv := f.start(t)
	exe, _ := withUpdater(t, srv, "1.0.0", "")
	err := cmdUpdate([]string{"--yes"})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want checksum mismatch, got %v", err)
	}
	if readFile(t, exe) != "old binary" {
		t.Error("binary replaced despite mismatch")
	}
	for _, suffix := range []string{".new", ".old"} {
		if _, err := os.Stat(exe + suffix); !os.IsNotExist(err) {
			t.Errorf("%s created despite mismatch", suffix)
		}
	}
}

func TestUpdate404(t *testing.T) {
	f := &fakeRelease{status: http.StatusNotFound}
	srv := f.start(t)
	withUpdater(t, srv, "1.0.0", "")
	err := cmdUpdate([]string{"--check"})
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("want 404 hint about tokens, got %v", err)
	}
}

// Both replace strategies run on every OS so the Windows rename dance is
// covered by Linux CI too.
func TestReplaceBinaryBothStyles(t *testing.T) {
	for _, windows := range []bool{false, true} {
		dir := t.TempDir()
		exe := filepath.Join(dir, "rw")
		os.WriteFile(exe, []byte("v1"), 0o755)
		os.WriteFile(exe+".old", []byte("stale"), 0o755)
		if _, err := replaceBinary(exe, []byte("v2"), windows); err != nil {
			t.Fatalf("windows=%v: %v", windows, err)
		}
		if readFile(t, exe) != "v2" {
			t.Errorf("windows=%v: not replaced", windows)
		}
		if windows && readFile(t, exe+".old") != "v1" {
			t.Errorf("windows style should keep the previous binary as .old")
		}
		if runtime.GOOS != "windows" {
			if fi, _ := os.Stat(exe); fi.Mode().Perm()&0o100 == 0 {
				t.Errorf("windows=%v: new binary not executable: %v", windows, fi.Mode())
			}
		}
	}
}

// fakeOwners answers package database queries: tool name -> output. A tool
// that is missing from the map fails, like an uninstalled one.
func fakeOwners(t *testing.T, owners map[string]string) *[]string {
	t.Helper()
	old := updateOwnerQuery
	t.Cleanup(func() { updateOwnerQuery = old })
	var asked []string
	updateOwnerQuery = func(name string, args ...string) (string, error) {
		asked = append(asked, name+" "+strings.Join(args, " "))
		if out, ok := owners[name]; ok {
			return out, nil
		}
		return "", errors.New("exit status 1")
	}
	return &asked
}

func TestPackageManagerByPath(t *testing.T) {
	asked := fakeOwners(t, nil)
	t.Setenv("SCOOP", "")
	t.Setenv("SCOOP_GLOBAL", "")
	cases := []struct{ path, want string }{
		{"/opt/homebrew/Cellar/relayweft/0.2.0/bin/rw", "Homebrew"},
		{"/usr/local/Cellar/relayweft/0.2.0/bin/rw", "Homebrew"},
		{"/home/linuxbrew/.linuxbrew/Cellar/relayweft/0.2.0/bin/rw", "Homebrew"},
		{`C:\Users\me\scoop\apps\rw\0.2.0\rw.exe`, "Scoop"},
		{`D:\Scoop\apps\rw\current\rw.exe`, "Scoop"},
		{`C:\Users\me\AppData\Local\Microsoft\WinGet\Packages\Sparkz400.Relayweft_Microsoft.Winget.Source_8wekyb3d8bbwe\rw.exe`, "winget"},
		{`C:\Program Files\WinGet\Packages\Sparkz400.Relayweft_Microsoft.Winget.Source_8wekyb3d8bbwe\rw.exe`, "winget"},
		{`C:\ProgramData\scoop\apps\rw\current\rw.exe`, "Scoop"},
		{`C:\tools\rw.exe`, ""},
		{"/home/me/bin/rw", ""},
		{"/home/me/go/bin/rw", ""},
		{"/usr/local/bin/rw", ""}, // asks the package databases, none owns it
		// Only the package managers' own folders for rw count, not a
		// hand-downloaded rw that happens to sit under a similar path.
		{"/home/me/Cellar/bin/rw", ""},
		{"/opt/homebrew/Cellar/other/1.0/bin/rw", ""},
		{"/opt/homebrew/Cellar/relayweft/0.2.0/libexec/rw", ""},
		{`C:\Users\me\scoop\apps\other\1.0\rw.exe`, ""},
		{`C:\Users\me\scoop\apps\rw.exe`, ""},
		{`C:\Users\me\scoop\apps\rw\1.0\bin\rw.exe`, ""},
		{`C:\work\apps\rw\1.0\rw.exe`, ""}, // not a Scoop root
		{`C:\Users\me\AppData\Local\Microsoft\WinGet\Packages\Other.Tool_Microsoft.Winget.Source_8wekyb3d8bbwe\rw.exe`, ""},
		{`C:\Users\me\AppData\Local\Microsoft\WinGet\Packages\rw.exe`, ""},
		{`C:\Users\me\AppData\Local\Microsoft\WinGet\Links\rw.exe`, ""},
	}
	for _, c := range cases {
		pm, ok := packageManager(c.path, "amd64")
		if pm.name != c.want || ok != (c.want != "") {
			t.Errorf("packageManager(%q) = %q, %v; want %q", c.path, pm.name, ok, c.want)
		}
	}
	// Only the /usr/ path is looked up in the package databases.
	for _, q := range *asked {
		if !strings.Contains(q, "/usr/local/bin/rw") {
			t.Errorf("unexpected package query %q", q)
		}
	}
	if len(*asked) != 4 {
		t.Errorf("want dpkg-query, rpm, pacman and apk asked about /usr/local/bin/rw, got %q", *asked)
	}
}

func TestPackageManagerByOwner(t *testing.T) {
	cases := []struct {
		tool, out, wantName, wantHow string
	}{
		{"dpkg-query", "relayweft: /usr/bin/rw", "a .deb package (relayweft)", "sudo apt install ./relayweft-linux-arm64.deb"},
		{"dpkg-query", "diversion by foo from: /usr/bin/rw\ndiversion by foo to: /usr/bin/rw.real\nrelayweft: /usr/bin/rw", "a .deb package (relayweft)", "apt install"},
		{"rpm", "relayweft\n", "an .rpm package (relayweft)", "sudo dnf install ./relayweft-linux-arm64.rpm"},
		{"pacman", "relayweft-bin\n", "pacman (relayweft-bin)", "update the relayweft-bin package"},
		{"apk", "relayweft\n", "an .apk package (relayweft)", "apk add --allow-untrusted ./relayweft-linux-arm64.apk"},
	}
	for _, c := range cases {
		fakeOwners(t, map[string]string{c.tool: c.out})
		pm, ok := packageManager("/usr/bin/rw", "arm64")
		if !ok || pm.name != c.wantName || !strings.Contains(pm.how, c.wantHow) {
			t.Errorf("%s %q: got %+v, %v; want %q with %q", c.tool, c.out, pm, ok, c.wantName, c.wantHow)
		}
	}
	// Multi-arch names and files shared by several packages.
	fakeOwners(t, map[string]string{"dpkg-query": "relayweft:amd64, other: /usr/bin/rw"})
	if pm, ok := packageManager("/usr/bin/rw", "amd64"); !ok || pm.name != "a .deb package (relayweft:amd64, other)" {
		t.Errorf("multi-owner: got %+v, %v", pm, ok)
	}

	// Output that names no package: diversions (also a local one, left
	// behind after the package was removed), translated text, another path,
	// and errors printed on stdout.
	for _, c := range []struct{ tool, out string }{
		{"dpkg-query", "diversion by foo from: /usr/bin/rw"},
		{"dpkg-query", "local diversion from: /usr/bin/rw\nlocal diversion to: /usr/bin/rw.distrib"},
		{"dpkg-query", "lokale Umleitung von: /usr/bin/rw\nlokale Umleitung zu: /usr/bin/rw.distrib"},
		{"dpkg-query", "Umleitung durch foo von: /usr/bin/rw"},
		{"dpkg-query", "relayweft: /usr/bin/rw-other"},
		{"rpm", "file /usr/bin/rw is not owned by any package"},
		{"pacman", "error: No package owns /usr/bin/rw"},
		{"apk", "ERROR: /usr/bin/rw: Could not find owner package"},
	} {
		fakeOwners(t, map[string]string{c.tool: c.out})
		if pm, ok := packageManager("/usr/bin/rw", "amd64"); ok {
			t.Errorf("%s %q: got %+v, want no package manager", c.tool, c.out, pm)
		}
	}
}

// A Scoop installed to a custom root ($SCOOP, $SCOOP_GLOBAL).
func TestPackageManagerScoopRoots(t *testing.T) {
	fakeOwners(t, nil)
	t.Setenv("SCOOP", `D:\Tools\Pkgs\`)
	t.Setenv("SCOOP_GLOBAL", `E:/GlobalApps`)
	for path, want := range map[string]bool{
		`D:\Tools\Pkgs\apps\rw\current\rw.exe`: true,
		`d:\tools\pkgs\apps\rw\0.2.0\rw.exe`:   true,
		`E:\GlobalApps\apps\rw\0.2.0\rw.exe`:   true,
		`D:\Tools\Other\apps\rw\0.2.0\rw.exe`:  false,
		`D:\Tools\Pkgs\apps\other\1\rw.exe`:    false,
	} {
		if _, ok := packageManager(path, "amd64"); ok != want {
			t.Errorf("packageManager(%q) = %v, want %v", path, ok, want)
		}
	}
}

// rw update must not replace a binary that a package manager installed:
// the manager's records would name the old version and its next upgrade
// could fail or roll rw back. Scoop used to get only a note before the
// binary was replaced anyway.
func TestUpdateLeavesPackageManagersAlone(t *testing.T) {
	for _, c := range []struct{ dir, want string }{
		{filepath.Join("Cellar", "relayweft", "1.0.0", "bin"), "brew upgrade relayweft"},
		{filepath.Join("scoop", "apps", "rw", "1.0.0"), "scoop update rw"},
		{filepath.Join("WinGet", "Packages", "Sparkz400.Relayweft_x"), "winget upgrade Sparkz400.Relayweft"},
	} {
		f := &fakeRelease{tag: "v2.0.0", binary: []byte("new")}
		srv := f.start(t)
		_, out := withUpdater(t, srv, "1.0.0", "y\n")
		exe := filepath.Join(t.TempDir(), c.dir, "rw")
		os.MkdirAll(filepath.Dir(exe), 0o755)
		os.WriteFile(exe, []byte("old binary"), 0o755)
		updateTarget = func() (string, error) { return exe, nil }

		if err := cmdUpdate([]string{"--check"}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), c.want) || strings.Contains(out.String(), "run `rw update`") {
			t.Errorf("--check should point to %q:\n%s", c.want, out)
		}

		err := cmdUpdate([]string{"--yes"})
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "/notes") {
			t.Errorf("want a refusal naming %q and the release, got %v", c.want, err)
		}
		if readFile(t, exe) != "old binary" {
			t.Errorf("%s: binary replaced under the package manager", c.dir)
		}

		out.Reset()
		if err := cmdUpdate([]string{"--yes", "--force"}); err != nil {
			t.Fatalf("--force: %v\n%s", err, out)
		}
		if readFile(t, exe) != "new" || !strings.Contains(out.String(), "--force") {
			t.Errorf("--force should replace the binary with a note:\n%s", out)
		}
	}
}

func TestUpdateLeavesLinuxPackagesAlone(t *testing.T) {
	f := &fakeRelease{tag: "v2.0.0", binary: []byte("new")}
	srv := f.start(t)
	withUpdater(t, srv, "1.0.0", "y\n")
	updateTarget = func() (string, error) { return "/usr/bin/rw", nil }
	fakeOwners(t, map[string]string{"rpm": "relayweft"})
	err := cmdUpdate([]string{"--yes"})
	if err == nil || !strings.Contains(err.Error(), "an .rpm package (relayweft)") || !strings.Contains(err.Error(), "dnf install") {
		t.Fatalf("want a refusal for the rpm package, got %v", err)
	}
}

// --check needs no binary path: it only reports.
func TestUpdateCheckWithoutTarget(t *testing.T) {
	f := &fakeRelease{tag: "v2.0.0", binary: []byte("new")}
	srv := f.start(t)
	_, out := withUpdater(t, srv, "1.0.0", "")
	updateTarget = func() (string, error) { return "", errors.New("no executable") }
	if err := cmdUpdate([]string{"--check"}); err != nil || !strings.Contains(out.String(), "run `rw update`") {
		t.Fatalf("--check: %v\n%s", err, out)
	}
	if err := cmdUpdate([]string{"--yes"}); err == nil || !strings.Contains(err.Error(), "cannot locate") {
		t.Fatalf("want a locate error, got %v", err)
	}
}
