package web

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestRecoveryProjectPaths(t *testing.T) {
	dir := t.TempDir()
	if !sameDir(dir, dir+string(filepath.Separator)+"child"+string(filepath.Separator)+"..") {
		t.Fatal("same project using a normalized path was rejected")
	}
	if runtime.GOOS == "linux" && sameDir("/repo/A", "/repo/a") {
		t.Fatal("different case-sensitive projects were conflated")
	}
}
