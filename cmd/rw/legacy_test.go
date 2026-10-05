package main

import (
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/forge"
)

// What sy (Switchyard, v0.2.0 and older) posted keeps counting as rw's
// own after the rename: rw watch must not answer sy's comments as review
// feedback, and a claim of a team member still on sy must hold.
func TestSwitchyardMarkersStillCount(t *testing.T) {
	if !ownComment("**high**: a finding\n<!-- switchyard -->") || !ownComment("x\n"+rwMark) {
		t.Error("a comment sy or rw posted is taken for review feedback")
	}
	if ownComment("please fix the switchyard bug") {
		t.Error("a comment that only mentions switchyard is taken for rw's own")
	}

	until := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	body := "Switchyard is working on this issue.\n\n<!-- switchyard:queue id=0123456789abcdef machine=desk-1 state=working until=2026-10-05T12:00:00Z -->\n<!-- switchyard -->"
	if !isQueueComment(body) {
		t.Fatal("sy's claim comment is not seen as a claim")
	}
	c, ok := parseClaim(forge.Comment{ID: 7, Author: "me", Body: body})
	if !ok || c.id != "0123456789abcdef" || c.machine != "desk-1" || c.state != "working" || !c.until.Equal(until) {
		t.Errorf("sy's claim = %+v, %v", c, ok)
	}
	// rw's own markers, as before.
	c, ok = parseClaim(forge.Comment{ID: 8, Body: queueMarker("fedcba9876543210", "laptop", "working", until)})
	if !ok || c.machine != "laptop" {
		t.Errorf("rw's claim = %+v, %v", c, ok)
	}
}
