package main

import "testing"

func TestCIRunnerScriptProtected(t *testing.T) {
	if got := ciFiles([]string{"M .relayweft-ci/forge-job.sh"}); len(got) != 1 {
		t.Fatal("watch would push a change to the privileged CI runner")
	}
}
