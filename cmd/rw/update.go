package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Package vars so tests can point the updater at an httptest server, a fake
// binary and canned stdin/stdout without touching the real executable.
var (
	updateAPI = "https://api.github.com/repos/sparkz400/relayweft/releases/latest"
	// The default transport is kept on purpose: it honours HTTPS_PROXY,
	// which corporate Windows machines often need.
	updateHTTP = &http.Client{Timeout: 5 * time.Minute}
	// updateTarget returns the binary to replace. It resolves symlinks so
	// a link (winget's "Links", Homebrew's bin) leads to the real file,
	// whose folder tells which package manager installed it.
	updateTarget = func() (string, error) {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(exe)
	}
	updateIn  io.Reader = os.Stdin
	updateOut io.Writer = os.Stdout
	// updateOwnerQuery runs a package database query (dpkg-query, rpm,
	// pacman, apk) and returns its output. It fails when the tool is not
	// installed or the file belongs to no package. LC_ALL=C keeps the
	// output untranslated, so it can be parsed.
	updateOwnerQuery = func(name string, args ...string) (string, error) {
		if _, err := exec.LookPath(name); err != nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
)

// maxDownload caps a release asset so a broken redirect cannot fill the disk.
const maxDownload = 256 << 20

type ghRelease struct {
	TagName string    `json:"tag_name"`
	HTMLURL string    `json:"html_url"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	URL                string `json:"url"` // API URL; works for private repos with a token
	BrowserDownloadURL string `json:"browser_download_url"`
}

func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("rw update", flag.ContinueOnError)
	check := fs.Bool("check", false, "only show whether a newer release exists")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	force := fs.Bool("force", false, "replace the binary even if a package manager installed it")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: rw update [--check] [--yes] [--force]

Downloads the latest release from GitHub, verifies its SHA-256 against
checksums.txt and replaces this rw binary. Set GITHUB_TOKEN or GH_TOKEN
if the repository is private.

If Homebrew, Scoop, winget or a Linux package (.deb, .rpm, .apk, AUR)
installed rw, it says how to update with that instead and changes nothing.
`)
	}
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	out := updateOut

	rel, err := latestRelease()
	if err != nil {
		return err
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	cur := strings.TrimPrefix(version, "v")
	newer := versionLess(cur, latest)

	fmt.Fprintf(out, "current: %s\nlatest:  %s\n", cur, latest)
	if _, ok := parseVersion(cur); !ok {
		fmt.Fprintln(out, "note: this is a development build; any release counts as newer.")
	}
	if !newer {
		fmt.Fprintln(out, "rw is up to date.")
		return nil
	}
	target, targetErr := updateTarget()
	var pm pkgManager
	var managed bool
	if targetErr == nil {
		pm, managed = packageManager(target, runtime.GOARCH)
	}
	if *check {
		if managed {
			fmt.Fprintf(out, "An update is available. rw was installed with %s: %s.\n", pm.name, pm.how)
		} else {
			fmt.Fprintln(out, "An update is available: run `rw update`.")
		}
		if rel.HTMLURL != "" {
			fmt.Fprintln(out, "release notes:", rel.HTMLURL)
		}
		return nil
	}
	if targetErr != nil {
		return fmt.Errorf("cannot locate the running rw binary: %w", targetErr)
	}
	if managed {
		// Replacing the binary under a package manager leaves its records
		// wrong, and its next upgrade may fail or roll rw back.
		if !*force {
			return fmt.Errorf("this rw was installed with %s, so `rw update` leaves it alone: %s (release: %s). `rw update --force` replaces the binary anyway", pm.name, pm.how, releaseURL(rel))
		}
		fmt.Fprintf(out, "note: rw was installed with %s; replacing it anyway (--force). Its records still name the old version.\n", pm.name)
	}

	name := assetName(runtime.GOOS, runtime.GOARCH)
	asset, ok := findAsset(rel, name)
	if !ok {
		return fmt.Errorf("release %s has no %s asset for %s/%s", rel.TagName, name, runtime.GOOS, runtime.GOARCH)
	}
	sums, ok := findAsset(rel, "checksums.txt")
	if !ok {
		// Never install an unverified binary.
		return fmt.Errorf("release %s has no checksums.txt; refusing to install an unverified binary", rel.TagName)
	}

	if !*yes {
		fmt.Fprintf(out, "Replace %s with %s? [y/N] ", target, latest)
		ans, _ := bufio.NewReader(updateIn).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			fmt.Fprintln(out, "Cancelled.")
			return nil
		}
	}

	fmt.Fprintf(out, "downloading %s ...\n", name)
	data, err := download(asset)
	if err != nil {
		return err
	}
	sumData, err := download(sums)
	if err != nil {
		return err
	}
	want, ok := parseChecksums(sumData)[name]
	if !ok {
		return fmt.Errorf("checksums.txt has no entry for %s; refusing to install", name)
	}
	got := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(got[:]), want) {
		return fmt.Errorf("checksum mismatch for %s (got %x, want %s); refusing to install", name, got, want)
	}

	windows := runtime.GOOS == "windows"
	old, err := replaceBinary(target, data, windows)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated %s from %s to %s (sha256 verified).\n", target, cur, latest)
	if windows {
		fmt.Fprintf(out, "The previous version is kept as %s and removed on the next start.\n", old)
		fmt.Fprintf(out, "To roll back now: close rw, then move %s back to %s.\n", old, target)
	} else {
		fmt.Fprintln(out, "To roll back, download an older release from https://github.com/sparkz400/relayweft/releases.")
	}
	return nil
}

// pkgManager is a package manager that installed rw and how to update with it.
type pkgManager struct {
	name string // "Homebrew", "a .deb package (relayweft)"
	how  string // what to do instead of `rw update`
}

var (
	// Homebrew installs the formula as <prefix>/Cellar/relayweft/<version>/bin/rw.
	brewPath = regexp.MustCompile(`/Cellar/relayweft/[^/]+/bin/rw$`)
	// Scoop: <root>/apps/rw/<version or current>/<file>; the default roots
	// are ~/scoop and C:\ProgramData\scoop. Lowercased paths.
	scoopApp = regexp.MustCompile(`/apps/rw/[^/]+/[^/]+$`)
	// winget portable: ...\WinGet\Packages\Sparkz400.Relayweft_<source>\<file>.
	wingetPath = regexp.MustCompile(`/winget/packages/sparkz400\.relayweft_[^/]+/[^/]+$`)
	// dpkg-query -S prints "pkg[, pkg...]: path"; diversion lines ("local
	// diversion from: ...", "diversion by x to: ...") do not fit this.
	debOwners = regexp.MustCompile(`^[a-z0-9][a-z0-9+.:-]*(, [a-z0-9][a-z0-9+.:-]*)*$`)
	// An rpm, pacman or apk package name.
	pkgName = regexp.MustCompile(`^[A-Za-z0-9@_+][A-Za-z0-9@._+-]*$`)
)

// packageManager reports whether a package manager installed the rw binary
// at target (symlinks already resolved). Homebrew, Scoop and winget are told
// by the folders they install rw to. A binary under /usr/ is looked up in
// the system's package database (dpkg, rpm, pacman, apk), since the .deb,
// .rpm, .apk and AUR packages all install /usr/bin/rw.
func packageManager(target, goarch string) (pkgManager, bool) {
	// Backslashes too: tests pass Windows paths on every OS.
	p := strings.ReplaceAll(target, `\`, "/")
	lower := strings.ToLower(p)
	switch {
	case brewPath.MatchString(p):
		return pkgManager{"Homebrew", "run `brew upgrade relayweft`"}, true
	case isScoopApp(lower):
		return pkgManager{"Scoop", "run `scoop update rw`"}, true
	case wingetPath.MatchString(lower):
		return pkgManager{"winget", "run `winget upgrade Sparkz400.Relayweft`"}, true
	case !strings.HasPrefix(p, "/usr/"):
		return pkgManager{}, false
	}
	// The release's package assets are named relayweft-linux-<arch>.<ext>.
	asset := func(ext string) string { return "relayweft-linux-" + goarch + "." + ext }
	if out, err := updateOwnerQuery("dpkg-query", "-S", p); err == nil {
		for _, line := range strings.Split(out, "\n") {
			pkg, path, ok := strings.Cut(strings.TrimSpace(line), ": ")
			if ok && path == p && debOwners.MatchString(pkg) {
				return pkgManager{"a .deb package (" + pkg + ")",
					"download " + asset("deb") + " from the release and run `sudo apt install ./" + asset("deb") + "`"}, true
			}
		}
	}
	if pkg, ok := ownerName("rpm", "-qf", "--queryformat", "%{NAME}\n", p); ok {
		return pkgManager{"an .rpm package (" + pkg + ")",
			"download " + asset("rpm") + " from the release and run `sudo dnf install ./" + asset("rpm") + "`"}, true
	}
	if pkg, ok := ownerName("pacman", "-Qqo", p); ok {
		return pkgManager{"pacman (" + pkg + ")",
			"update the " + pkg + " package with your AUR helper (for example `yay -Syu`) or makepkg"}, true
	}
	if pkg, ok := ownerName("apk", "info", "-q", "--who-owns", p); ok {
		return pkgManager{"an .apk package (" + pkg + ")",
			"download " + asset("apk") + " from the release and run `sudo apk add --allow-untrusted ./" + asset("apk") + "`"}, true
	}
	return pkgManager{}, false
}

// ownerName runs a query that prints only the owning package's name, and
// accepts the answer only if it is one.
func ownerName(name string, args ...string) (string, bool) {
	out, err := updateOwnerQuery(name, args...)
	if err != nil {
		return "", false
	}
	pkg := firstLine(out)
	return pkg, pkgName.MatchString(pkg)
}

// isScoopApp reports whether lower (a lowercased, slash-separated path) is
// rw's own app folder in a Scoop root: the default ones (a "scoop" folder)
// or $SCOOP and $SCOOP_GLOBAL.
func isScoopApp(lower string) bool {
	m := scoopApp.FindStringIndex(lower)
	if m == nil {
		return false
	}
	root := lower[:m[0]]
	if strings.HasSuffix(root, "/scoop") {
		return true
	}
	for _, env := range []string{"SCOOP", "SCOOP_GLOBAL"} {
		r := strings.TrimRight(strings.ToLower(strings.ReplaceAll(os.Getenv(env), `\`, "/")), "/")
		if r != "" && root == r {
			return true
		}
	}
	return false
}

func releaseURL(rel *ghRelease) string {
	if rel.HTMLURL != "" {
		return rel.HTMLURL
	}
	return "https://github.com/sparkz400/relayweft/releases/tag/" + rel.TagName
}

// cleanupOldBinary removes the <exe>.old (and .old-N) files left behind by
// Windows updates, where the running file could only be renamed, not
// deleted. It is best effort and silent: a file still in use (another rw
// runs it) is tried again next start.
func cleanupOldBinary() {
	target, err := updateTarget()
	if err != nil {
		return
	}
	os.Remove(target + ".old")
	more, _ := filepath.Glob(target + ".old-*")
	for _, p := range more {
		os.Remove(p)
	}
}

// replaceBinary swaps target for data and returns where the previous
// binary was kept (Windows only). windows selects the rename dance needed
// there: a running .exe cannot be overwritten or deleted but can be
// renamed, so it is moved to .old first. It is a parameter (not a GOOS
// check) so both paths are tested on every platform.
func replaceBinary(target string, data []byte, windows bool) (string, error) {
	tmp := target + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return "", fmt.Errorf("cannot write %s (is the folder writable?): %w", tmp, err)
	}
	if !windows {
		if err := os.Rename(tmp, target); err != nil {
			os.Remove(tmp)
			return "", fmt.Errorf("replace %s: %w", target, err)
		}
		return "", nil
	}
	old := oldBinaryName(target)
	if err := os.Rename(target, old); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("move running binary aside: %w", err)
	}
	err := os.Rename(tmp, target)
	for i := 0; err != nil && windows && i < 5; i++ {
		// A virus scanner often holds a freshly written .exe for a moment.
		time.Sleep(time.Duration(200*(i+1)) * time.Millisecond)
		err = os.Rename(tmp, target)
	}
	if err != nil {
		// Put the old binary back so rw keeps working.
		if rerr := os.Rename(old, target); rerr != nil {
			return "", fmt.Errorf("install new binary: %w (and restoring failed: %v; the old binary is %s)", err, rerr, old)
		}
		os.Remove(tmp)
		return "", fmt.Errorf("install new binary: %w", err)
	}
	return old, nil
}

// oldBinaryName is where the running binary is moved aside: <exe>.old, or
// <exe>.old-N when an earlier .old cannot be removed because it still runs
// (a rw web left open across two updates); Windows cannot rename over a
// running .exe.
func oldBinaryName(target string) string {
	old := target + ".old"
	if err := os.Remove(old); err == nil || os.IsNotExist(err) {
		return old
	}
	for i := 1; ; i++ {
		p := fmt.Sprintf("%s.old-%d", target, i)
		if err := os.Remove(p); err == nil || os.IsNotExist(err) {
			return p
		}
	}
}

func latestRelease() (*ghRelease, error) {
	req, err := http.NewRequest("GET", updateAPI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	authorize(req)
	resp, err := updateHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("check for updates: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized && githubToken() != "" {
		// A stale or wrong token must not block updates of a public repo.
		fmt.Fprintln(updateOut, "note: GitHub rejected the token in GITHUB_TOKEN/GH_TOKEN (401); trying without it")
		tokenRejected = true
		return latestRelease()
	}
	if resp.StatusCode == http.StatusNotFound {
		hint := "If the repository is private, set GITHUB_TOKEN or GH_TOKEN to a token that can read it."
		if githubToken() != "" {
			hint = "The token in GITHUB_TOKEN/GH_TOKEN may lack access to the repository."
		}
		return nil, fmt.Errorf("no release found (404 from %s). %s", updateAPI, hint)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("check for updates: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("check for updates: bad response: %w", err)
	}
	if rel.TagName == "" {
		return nil, errors.New("check for updates: release has no tag")
	}
	return &rel, nil
}

// download fetches an asset. With a token it uses the API URL, which is
// the only one that works for private repos; Go drops the Authorization
// header when GitHub redirects to its storage host, as it must.
func download(a ghAsset) ([]byte, error) {
	url := a.BrowserDownloadURL
	if (githubToken() != "" && a.URL != "") || url == "" {
		url = a.URL
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	authorize(req)
	resp, err := updateHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", a.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", a.Name, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", a.Name, err)
	}
	if len(data) > maxDownload {
		return nil, fmt.Errorf("download %s: larger than %d MB", a.Name, maxDownload>>20)
	}
	return data, nil
}

// tokenRejected is set when GitHub answered 401 to the token: later
// requests in this run go without it.
var tokenRejected bool

func githubToken() string {
	if tokenRejected {
		return ""
	}
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return t
	}
	return os.Getenv("GH_TOKEN")
}

func authorize(req *http.Request) {
	if t := githubToken(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
}

// assetName must match the names release.yml uploads.
func assetName(goos, goarch string) string {
	n := "rw-" + goos + "-" + goarch
	if goos == "windows" {
		n += ".exe"
	}
	return n
}

func findAsset(rel *ghRelease, name string) (ghAsset, bool) {
	for _, a := range rel.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return ghAsset{}, false
}

// parseChecksums reads sha256sum output ("<hex>  <name>", or "<hex> *<name>"
// in binary mode) into name -> lowercase hex.
func parseChecksums(data []byte) map[string]string {
	m := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		m[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
	}
	return m
}

// semver is a release version: numeric dot parts and an optional
// pre-release suffix.
type semver struct {
	parts []int
	pre   string
}

// parseVersion parses "1.2.3" or "v1.2.3-rc1". Anything else (like "dev"
// or a commit hash from a CI build) is not a release version.
func parseVersion(s string) (semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i] // build metadata never affects ordering
	}
	var v semver
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s, v.pre = s[:i], s[i+1:]
	}
	if s == "" {
		return v, false
	}
	for _, p := range strings.Split(s, ".") {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v.parts = append(v.parts, n)
	}
	return v, true
}

// versionLess reports whether a is older than b. A non-release current
// version (a dev build) is always older, so dev builds can update; an
// unparseable latest tag never counts as newer.
func versionLess(a, b string) bool {
	vb, okb := parseVersion(b)
	if !okb {
		return false
	}
	va, oka := parseVersion(a)
	if !oka {
		return true
	}
	for i := 0; i < len(va.parts) || i < len(vb.parts); i++ {
		var x, y int
		if i < len(va.parts) {
			x = va.parts[i]
		}
		if i < len(vb.parts) {
			y = vb.parts[i]
		}
		if x != y {
			return x < y
		}
	}
	// Same numbers: a pre-release is older than the release itself.
	switch {
	case va.pre != "" && vb.pre == "":
		return true
	case va.pre == "" || vb.pre == "":
		return false
	}
	return va.pre < vb.pre
}
