package workflow

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"gopkg.in/yaml.v3"
)

func TestWorkflowLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("APPDATA", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	for _, d := range Builtins() {
		data, _ := yaml.Marshal(d)
		if err := Save(data, false); err != nil {
			t.Fatal(err)
		}
		if err := Save(data, false); !os.IsExist(err) {
			t.Fatal("overwrote a saved workflow")
		}
	}
	ds, err := List()
	if err != nil || len(ds) != 4 {
		t.Fatalf("%v %+v", err, ds)
	}
	d, err := Load("bugfix")
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := d.Render("repair \"quotes\" and $() safely")
	if err != nil || !strings.Contains(prompt, "$()") {
		t.Fatal("task text was interpreted")
	}
	c := config.Default()
	c.Verify.Commands = []string{"go test ./..."}
	c.Budget.TaskUSD = 2
	c.Budget.TaskTokens = 100
	d.Checks = []string{"go test ./...", "go vet ./..."}
	if err = d.Apply(c); err != nil {
		t.Fatal(err)
	}
	if c.Budget.TaskUSD != 2 || c.Budget.TaskTokens != 100 || !c.Orchestrator.ReviewChanges || !reflect.DeepEqual(c.Verify.Commands, []string{"go test ./...", "go vet ./..."}) {
		t.Fatalf("bad config: %+v", c.Budget)
	}
	c.Verify.Commands = nil
	d.Checks = nil
	if err = d.Apply(c); err == nil {
		t.Fatal("required checks absent")
	}
	if _, err = Load("../secrets"); err == nil {
		t.Fatal("path traversal accepted")
	}
	dir, _ := Dir()
	if _, err = os.Stat(filepath.Join(dir, "bugfix.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowRejectsInvalidFiles(t *testing.T) {
	base := "name: example\ndescription: test\nprompt: '{{task}}'\n"
	for _, extra := range []string{"unknown: true\n", "task_usd: -.1\n", "task_usd: .nan\n", "task_tokens: -1\n", "---\nname: other\n", "checks: ['']\n"} {
		if _, err := Parse([]byte(base + extra)); err == nil {
			t.Errorf("accepted %s", extra)
		}
	}
}
