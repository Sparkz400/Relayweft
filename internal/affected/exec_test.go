package affected

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// With Input.Exec set (the sandbox is on), the tools that read the agents'
// folder (go list, cargo metadata) run through it, never here.
func TestSelectUsesExec(t *testing.T) {
	oldGo, oldCargo := goList, cargoMetadata
	defer func() { goList, cargoMetadata = oldGo, oldCargo }()
	goList = func(context.Context, string, ...string) ([]byte, error) {
		t.Error("go list ran here")
		return nil, errors.New("no")
	}
	cargoMetadata = func(context.Context, string) ([]byte, error) {
		t.Error("cargo metadata ran here")
		return nil, errors.New("no")
	}
	var ran []string
	exec := func(_ context.Context, dir string, argv []string) ([]byte, error) {
		ran = append(ran, strings.Join(argv[:2], " "))
		return nil, errors.New("not in the image")
	}
	dir := goTree(t)
	p := Select(context.Background(), "go test ./...", "", Input{Root: dir, Dir: dir, Files: []string{"a/a.go"}, Exec: exec})
	if !p.Full || !strings.Contains(p.Why, "not in the image") {
		t.Errorf("go: %+v", p)
	}
	cdir := tree(t, map[string]string{"Cargo.toml": "[workspace]\n", "core/src/lib.rs": ""})
	p = Select(context.Background(), "cargo test", "", Input{Root: cdir, Dir: cdir, Files: []string{"core/src/lib.rs"}, Exec: exec})
	if !p.Full || !strings.Contains(p.Why, "not in the image") {
		t.Errorf("cargo: %+v", p)
	}
	if strings.Join(ran, ",") != "go list,cargo metadata" {
		t.Errorf("ran through Exec: %v", ran)
	}
}
