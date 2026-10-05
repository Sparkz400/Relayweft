package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/proc"
)

//go:embed testimage/Dockerfile testimage/claude
var testImage embed.FS

// SelftestImage is the tag of the selftest image for the current files.
func SelftestImage() string {
	h := sha256.New()
	for _, n := range []string{"Dockerfile", "claude"} {
		h.Write([]byte(n))
		h.Write(testImageFile(n))
	}
	return "switchyard-sandbox-selftest:" + hex.EncodeToString(h.Sum(nil))[:12]
}

// testImageFile reads an embedded file with Unix line endings (a Windows
// checkout may have turned them into CRLF, which sh cannot run).
func testImageFile(name string) []byte {
	b, _ := testImage.ReadFile("testimage/" + name)
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

// Usable reports whether the runtime rt is installed and runs Linux
// containers, and says why not.
func Usable(rt string) (bin string, why string) {
	bin, err := LookPath(rt)
	if err != nil {
		return "", rt + " is not installed"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "info", "--format", "{{.OSType}}")
	proc.Background(cmd)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if rt == "podman" && err == nil && s == "" {
		s = "linux" // podman info has no OSType: it only runs Linux containers
	}
	switch {
	case err != nil:
		return "", fmt.Sprintf("%s is not running (%s)", rt, lastLine(s))
	case s != "linux":
		return "", fmt.Sprintf("%s runs %s containers, not Linux ones", rt, s)
	}
	return bin, ""
}

// BuildSelftestImage builds the selftest image (SelftestImage) with the
// runtime at bin, unless it is there already, and returns its tag.
func BuildSelftestImage(bin string) (string, error) {
	tag := SelftestImage()
	inspect := exec.Command(bin, "image", "inspect", "--format", "{{.Id}}", tag)
	proc.Background(inspect)
	if inspect.Run() == nil {
		return tag, nil
	}
	dir, err := os.MkdirTemp("", "sy-sandbox-image-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	for _, n := range []string{"Dockerfile", "claude"} {
		if err := os.WriteFile(filepath.Join(dir, n), testImageFile(n), 0o755); err != nil {
			return "", err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "build", "-t", tag, dir)
	proc.Background(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build %s: %v: %s", tag, err, lastLine(string(out)))
	}
	return tag, nil
}

// Running lists the names of this user's sy containers that exist now.
func Running(bin string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "ps", "-a", "--filter", "label=switchyard.owner="+owner(), "--format", "{{.Names}}")
	proc.Background(cmd)
	out, err := cmd.Output()
	return strings.Fields(string(out)), err
}
