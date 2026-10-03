package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Package vars so tests can point the updater at an httptest server, a fake
// binary and canned stdin/stdout without touching the real executable.
var (
	updateAPI = "https://api.github.com/repos/sparkz400/switchyard/releases/latest"
	// The default transport is kept on purpose: it honours HTTPS_PROXY,
	// which corporate Windows machines often need.
	updateHTTP = &http.Client{Timeout: 5 * time.Minute}
	// updateTarget returns the binary to replace. It resolves symlinks so a
	// winget "Links" symlink updates the real file, not the link.
	updateTarget = func() (string, error) {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(exe)
	}
	updateIn  io.Reader = os.Stdin
	updateOut io.Writer = os.Stdout
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
	fs := flag.NewFlagSet("sy update", flag.ContinueOnError)
	check := fs.Bool("check", false, "only show whether a newer release exists")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: sy update [--check] [--yes]

Downloads the latest release from GitHub, verifies its SHA-256 against
checksums.txt and replaces this sy binary. Set GITHUB_TOKEN or GH_TOKEN
if the repository is private.
`)
	}
	if err := fs.Parse(args); err != nil {
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
		fmt.Fprintln(out, "sy is up to date.")
		return nil
	}
	if *check {
		fmt.Fprintln(out, "An update is available: run `sy update`.")
		if rel.HTMLURL != "" {
			fmt.Fprintln(out, "release notes:", rel.HTMLURL)
		}
		return nil
	}

	target, err := updateTarget()
	if err != nil {
		return fmt.Errorf("cannot locate the running sy binary: %w", err)
	}
	if strings.Contains(strings.ToLower(filepath.ToSlash(target)), "/scoop/apps/") {
		fmt.Fprintln(out, "note: sy was installed with scoop; `scoop update sy` keeps scoop's records in sync.")
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
	if err := replaceBinary(target, data, windows); err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated %s from %s to %s (sha256 verified).\n", target, cur, latest)
	if windows {
		fmt.Fprintf(out, "The previous version is kept as %s.old and removed on the next start.\n", target)
		fmt.Fprintf(out, "To roll back now: close sy, then move %s.old back to %s.\n", target, target)
	} else {
		fmt.Fprintln(out, "To roll back, download an older release from https://github.com/sparkz400/switchyard/releases.")
	}
	return nil
}

// cleanupOldBinary removes the <exe>.old left behind by a Windows update,
// where the running file could only be renamed, not deleted. It is best
// effort and silent: a failure just means we try again next start.
func cleanupOldBinary() {
	target, err := updateTarget()
	if err != nil {
		return
	}
	os.Remove(target + ".old")
}

// replaceBinary swaps target for data. windows selects the rename dance
// needed there: a running .exe cannot be overwritten or deleted but can be
// renamed, so it is moved to .old first. It is a parameter (not a GOOS
// check) so both paths are tested on every platform.
func replaceBinary(target string, data []byte, windows bool) error {
	tmp := target + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return fmt.Errorf("cannot write %s (is the folder writable?): %w", tmp, err)
	}
	if !windows {
		if err := os.Rename(tmp, target); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("replace %s: %w", target, err)
		}
		return nil
	}
	old := target + ".old"
	os.Remove(old) // a leftover from an earlier update
	if err := os.Rename(target, old); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("move running binary aside: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		// Put the old binary back so sy keeps working.
		if rerr := os.Rename(old, target); rerr != nil {
			return fmt.Errorf("install new binary: %w (and restoring failed: %v; the old binary is %s)", err, rerr, old)
		}
		os.Remove(tmp)
		return fmt.Errorf("install new binary: %w", err)
	}
	return nil
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

func githubToken() string {
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
	n := "sy-" + goos + "-" + goarch
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
