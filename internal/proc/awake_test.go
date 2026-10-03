package proc

import "testing"

// Holds nest; releasing twice is harmless. (On Windows this really sets
// and clears the execution state; on macOS it starts caffeinate.)
func TestKeepAwakeNests(t *testing.T) {
	a := KeepAwake()
	b := KeepAwake()
	if !Awake() {
		t.Fatal("not awake")
	}
	a()
	a()
	if !Awake() {
		t.Fatal("released by a double release of one hold")
	}
	b()
	if Awake() {
		t.Fatal("still awake after every release")
	}
}
