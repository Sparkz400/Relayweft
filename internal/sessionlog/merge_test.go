package sessionlog

import "testing"

func TestSavedBranchesAreNotFailedMerges(t *testing.T) {
	records := []Record{
		{Type: TypeMerge, OK: Bool(false), Text: "not used (step kept candidate); this work is kept on rw/saved"},
		{Type: TypeMerge, Text: "agent moved HEAD; kept on rw/agent-head"},
		{Type: TypeMerge, OK: Bool(true), Text: "merged"},
		{Type: TypeMerge, OK: Bool(false), Text: "conflict in file.go"},
	}
	got := Aggregate(records, Filter{})
	if got.Merges != 2 || got.MergeFail != 1 {
		t.Fatalf("saved work counted as a failed merge: merges=%d failed=%d", got.Merges, got.MergeFail)
	}
}
