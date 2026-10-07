package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/proc"
)

const releaseRepo = "Sparkz400/Relayweft"
const releaseWorkflow = "https://github.com/" + releaseRepo + "/.github/workflows/release.yml"

var updateRepoAPI = "https://api.github.com/repos/" + releaseRepo
var updateVerify = verifyUpdate
var updateVerifyCommand = func(ctx context.Context, dir string, args []string) error {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return fmt.Errorf("install GitHub CLI (gh) with attestation support to verify updates: %w", err)
	}
	cmd := exec.CommandContext(ctx, gh, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GH_HOST=github.com", "GH_PROMPT_DISABLED=1")
	proc.Prepare(cmd)
	// Do not include verifier output: external errors can contain credentials.
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("GitHub CLI attestation verification failed: %w; check gh authentication, network access and CLI version", err)
	}
	return nil
}

var releaseTagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?$`)
var releaseSHAPattern = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)

// verifyUpdate verifies the signed SLSA attestations of both bytestrings before
// the installer touches the current binary. gh maintains Sigstore trust roots
// and checks signatures, artifact digests and transparency timestamps. Policy
// additionally binds the signing workflow and the exact release-tag commit.
// There is no checksum-only fallback, including for old unsigned releases.
func verifyUpdate(rel *ghRelease, name string, binary, sums []byte) error {
	if !releaseTagPattern.MatchString(rel.TagName) {
		return fmt.Errorf("invalid release tag %q; refusing to install", rel.TagName)
	}
	if _, ok := findAsset(rel, signatureAsset); !ok {
		return fmt.Errorf("release %s has no signing metadata; refusing to install", rel.TagName)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	sha, err := updateReleaseCommit(ctx, rel.TagName)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "rw-update-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	identity := "^" + regexp.QuoteMeta(releaseWorkflow) + "@(refs/heads/main|" + regexp.QuoteMeta("refs/tags/"+rel.TagName) + ")$"
	for _, a := range []struct {
		name string
		data []byte
	}{{name, binary}, {"checksums.txt", sums}} {
		p := filepath.Join(dir, a.name)
		if err := os.WriteFile(p, a.data, 0o600); err != nil {
			return err
		}
		args := []string{"attestation", "verify", p, "--hostname", "github.com", "--repo", releaseRepo,
			"--cert-identity-regex", identity, "--cert-oidc-issuer", "https://token.actions.githubusercontent.com",
			"--source-digest", sha, "--predicate-type", "https://slsa.dev/provenance/v1", "--deny-self-hosted-runners"}
		if err := updateVerifyCommand(ctx, dir, args); err != nil {
			return fmt.Errorf("verify %s: %w; the installed binary was not changed", a.name, err)
		}
		// Verify the bytes that will actually be installed, even if a verifier
		// or another process unexpectedly changes the temporary file.
		got, err := os.ReadFile(p)
		if err != nil || string(got) != string(a.data) {
			return fmt.Errorf("%s changed during verification; refusing to install", a.name)
		}
	}
	return nil
}

func updateReleaseCommit(ctx context.Context, tag string) (string, error) {
	u := updateRepoAPI + "/git/ref/tags/" + tag
	for i := 0; i < 5; i++ {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		authorize(req)
		res, err := updateHTTP.Do(req)
		if err != nil {
			return "", fmt.Errorf("resolve release tag: %w", err)
		}
		var obj struct {
			Object struct {
				Type string `json:"type"`
				SHA  string `json:"sha"`
			} `json:"object"`
		}
		err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&obj)
		res.Body.Close()
		if res.StatusCode != 200 || err != nil || !releaseSHAPattern.MatchString(obj.Object.SHA) {
			return "", fmt.Errorf("cannot resolve release tag %s to a commit; refusing to install", tag)
		}
		switch obj.Object.Type {
		case "commit":
			return strings.ToLower(obj.Object.SHA), nil
		case "tag":
			u = updateRepoAPI + "/git/tags/" + obj.Object.SHA
		default:
			return "", fmt.Errorf("release tag points to %s, not a commit", obj.Object.Type)
		}
	}
	return "", fmt.Errorf("release tag nesting exceeds the verification limit")
}
