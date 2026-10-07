package main

import (
	"os"
	"testing"
)

// This opt-in check downloads a public, released artifact and uses the real
// Sigstore verifier. It never installs it or calls an agent. Ordinary tests
// are offline; run RW_VERIFY_LIVE=1 go test ./cmd/rw -run TestUpdateLive.
func TestUpdateLive(t *testing.T) {
	if os.Getenv("RW_VERIFY_LIVE") != "1" {
		t.Skip("set RW_VERIFY_LIVE=1 for public release verification")
	}
	old := updateAPI
	updateAPI = "https://api.github.com/repos/Sparkz400/Relayweft/releases/tags/v0.4.0"
	t.Cleanup(func() { updateAPI = old })
	rel, err := latestRelease()
	if err != nil {
		t.Fatal(err)
	}
	name := "rw-linux-amd64"
	a, ok := findAsset(rel, name)
	if !ok {
		t.Fatal("release has no binary")
	}
	binary, err := download(a)
	if err != nil {
		t.Fatal(err)
	}
	a, ok = findAsset(rel, "checksums.txt")
	if !ok {
		t.Fatal("release has no checksums")
	}
	sums, err := download(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyUpdate(rel, name, binary, sums); err != nil {
		t.Fatal("genuine release rejected:", err)
	}
	binary[len(binary)/2] ^= 1
	if err := verifyUpdate(rel, name, binary, sums); err == nil {
		t.Fatal("tampered binary accepted")
	}
	binary[len(binary)/2] ^= 1
	sums = append(sums, []byte("tampered\n")...)
	if err := verifyUpdate(rel, name, binary, sums); err == nil {
		t.Fatal("tampered checksums accepted")
	}
}
