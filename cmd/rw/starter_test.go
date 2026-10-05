package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestStarterSet(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := filepath.Join(t.TempDir(), "starter")
	if err := writeStarter(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "bench.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var bf benchFile
	if err := yaml.Unmarshal(data, &bf); err != nil || len(bf.Tasks) != 5 || len(bf.Modes) != 3 {
		t.Fatalf("bench.yaml: %v %+v", err, bf)
	}
	if _, err := os.Stat(filepath.Join(dir, "inventory", "__init__.py")); err != nil {
		t.Fatal("underscore files were not embedded")
	}
	if out, err := exec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("starter not committed cleanly: %v %s", err, out)
	}
	if err := writeStarter(dir); err == nil {
		t.Fatal("overwrote an existing directory")
	}
	py := "python3"
	if runtime.GOOS == "windows" {
		py = "python"
	}
	if _, err := exec.LookPath(py); err != nil {
		t.Skip("no python: checks not run")
	}
	// Every check fails before the task is done (so a pass means something).
	for _, task := range bf.Tasks {
		cmd := exec.Command(py, strings.Fields(task.Check)[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
		if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "CHECK FAILED") {
			t.Errorf("%s: check passes or crashes on the untouched project: %v\n%s", task.Name, err, out)
		}
	}
}
