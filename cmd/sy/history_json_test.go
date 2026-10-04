package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/orchestrator"
)

func TestPrintHistoryJSON(t *testing.T) {
	created := time.Date(2026, 10, 4, 2, 30, 0, 0, time.UTC)
	hist := []orchestrator.TaskState{
		{ID: "t2", Task: "Fix GitHub issue #7: x", Dir: "/r", Status: "done", Created: created, Summary: "fixed", CostLine: "12k tokens", Plan: &orchestrator.Plan{}},
		{ID: "t1", Task: "other", Dir: "/r", Status: "failed", Created: created.Add(-time.Hour)},
	}
	var b bytes.Buffer
	if err := printHistoryJSON(&b, hist); err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatalf("%v\n%s", err, b.String())
	}
	if len(got) != 2 || got[0]["id"] != "t2" || got[0]["status"] != "done" || got[0]["cost"] != "12k tokens" || got[1]["status"] != "failed" {
		t.Errorf("got %v", got)
	}
	if _, ok := got[0]["plan"]; ok {
		t.Errorf("the whole state leaked into the listing: %v", got[0])
	}
	if got[0]["created"] != "2026-10-04T02:30:00Z" {
		t.Errorf("created = %v", got[0]["created"])
	}

	// No tasks is an empty list, not null: scripts can iterate it.
	b.Reset()
	if err := printHistoryJSON(&b, nil); err != nil {
		t.Fatal(err)
	}
	if s := bytes.TrimSpace(b.Bytes()); string(s) != "[]" {
		t.Errorf("empty history = %s", s)
	}
}
