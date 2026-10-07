package web

import (
	"os/exec"
	"testing"
)

func TestRecoveryMemoryControllers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is needed for DOM controller tests")
	}
	if b, err := exec.Command(node, "--test", "ui_dom_test.cjs").CombinedOutput(); err != nil {
		t.Fatalf("UI controllers: %v\n%s", err, b)
	}
}
