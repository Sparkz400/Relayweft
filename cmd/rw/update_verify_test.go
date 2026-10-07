package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestUpdateVerificationFailurePreservesBinary(t *testing.T) {
	f := &fakeRelease{tag: "v2.0.0", binary: []byte("new")}
	exe, _ := withUpdater(t, f.start(t), "1.0.0", "")
	updateVerify = func(*ghRelease, string, []byte, []byte) error { return errors.New("untrusted signer") }
	if err := cmdUpdate([]string{"--yes", "--force"}); err == nil || !strings.Contains(err.Error(), "untrusted signer") {
		t.Fatalf("%v", err)
	}
	if readFile(t, exe) != "old binary" {
		t.Fatal("changed the binary before verification")
	}
	for _, suffix := range []string{".new", ".old"} {
		if _, err := os.Stat(exe + suffix); !os.IsNotExist(err) {
			t.Fatalf("left %s", suffix)
		}
	}
}

func TestVerifyUpdatePolicyAndTampering(t *testing.T) {
	sha := strings.Repeat("a", 40)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"object":{"type":"commit","sha":%q}}`, sha)
	}))
	defer srv.Close()
	oldAPI, oldCmd := updateRepoAPI, updateVerifyCommand
	t.Cleanup(func() { updateRepoAPI, updateVerifyCommand = oldAPI, oldCmd })
	updateRepoAPI = srv.URL
	rel := &ghRelease{TagName: "v2.0.0", Assets: []ghAsset{{Name: signatureAsset}}}
	for _, failure := range []string{"", "signature", "checksum attestation", "changed file"} {
		t.Run(failure, func(t *testing.T) {
			calls := 0
			var work string
			updateVerifyCommand = func(_ context.Context, dir string, args []string) error {
				calls++
				work = dir
				for flag, value := range map[string]string{"--repo": releaseRepo, "--hostname": "github.com", "--source-digest": sha, "--predicate-type": "https://slsa.dev/provenance/v1", "--cert-oidc-issuer": "https://token.actions.githubusercontent.com"} {
					i := slices.Index(args, flag)
					if i < 0 || i+1 >= len(args) || args[i+1] != value {
						t.Errorf("policy %s: %v", flag, args)
					}
				}
				if !slices.Contains(args, "--deny-self-hosted-runners") {
					t.Error("missing runner policy")
				}
				if filepath.Dir(args[2]) != dir {
					t.Error("artifact not isolated")
				}
				if failure == "signature" || failure == "checksum attestation" && calls == 2 {
					return errors.New("invalid signature")
				}
				if failure == "changed file" {
					return os.WriteFile(args[2], []byte("tampered"), 0600)
				}
				return nil
			}
			err := verifyUpdate(rel, "rw.exe", []byte("new"), []byte("sums"))
			if (err == nil) != (failure == "") {
				t.Fatalf("%s: %v", failure, err)
			}
			if failure == "" && calls != 2 {
				t.Fatalf("verified %d artifacts", calls)
			}
			if _, err := os.Stat(work); !os.IsNotExist(err) {
				t.Fatal("verification files left behind")
			}
		})
	}
	if err := verifyUpdate(&ghRelease{TagName: "v2.0.0"}, "rw.exe", nil, nil); err == nil {
		t.Fatal("unsigned release accepted")
	}
	if err := verifyUpdate(&ghRelease{TagName: "v2.0.0/../../other", Assets: rel.Assets}, "rw.exe", nil, nil); err == nil {
		t.Fatal("invalid tag accepted")
	}
}

func TestReleaseTagResolution(t *testing.T) {
	for _, kind := range []string{"annotated", "blob", "cycle", "invalid sha", "unavailable"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if kind == "unavailable" {
					w.WriteHeader(503)
					return
				}
				typ, sha := "commit", strings.Repeat("b", 40)
				if kind == "cycle" || kind == "annotated" && strings.Contains(r.URL.Path, "/ref/") {
					typ = "tag"
				}
				if kind == "blob" {
					typ = "blob"
				}
				if kind == "invalid sha" {
					sha = "main"
				}
				fmt.Fprintf(w, `{"object":{"type":%q,"sha":%q}}`, typ, sha)
			}))
			defer srv.Close()
			old := updateRepoAPI
			updateRepoAPI = srv.URL
			defer func() { updateRepoAPI = old }()
			_, err := updateReleaseCommit(context.Background(), "v2.0.0")
			if (err == nil) != (kind == "annotated") {
				t.Fatalf("%v", err)
			}
		})
	}
}
