package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/sessionlog"
)

func TestReportPreservedAlternatives(t *testing.T) {
	d := &Data{}
	d.fromRecords([]sessionlog.Record{
		{Type: sessionlog.TypeMerge, OK: sessionlog.Bool(false), Text: "not used (step kept winner); this work is kept on rw/saved"},
		{Type: sessionlog.TypeMerge, OK: sessionlog.Bool(false), Text: "conflict in parser.go"},
	})
	if len(d.Merges) != 2 || !d.Merges[0].Saved || !d.Merges[0].OK || d.Merges[1].OK {
		t.Fatal(d.Merges)
	}
	var out bytes.Buffer
	if err := d.HTML(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), ">saved</span>") || !strings.Contains(out.String(), `class="pill fail">merge`) {
		t.Fatal("HTML conflates a saved alternative with a conflict")
	}
}
