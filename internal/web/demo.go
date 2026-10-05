package web

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sparkz400/relayweft/internal/orchestrator"
)

// Demo runs have no git repo, so the orchestrator never asks for a change
// review. POST /api/demo/review (demo mode only) opens one with a sample
// change set so the review panel can be tried.
func (s *Server) handleDemoReview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		go func() {
			select {
			case <-s.stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		d := s.ap.ReviewChanges(ctx, demoChangeSet())
		msg := fmt.Sprintf("demo review answered: apply %v", d.Apply)
		if len(d.Hunks) > 0 {
			msg += fmt.Sprintf(", hunks %v", d.Hunks)
		}
		if d.Feedback != "" {
			msg = "demo review answered with feedback: " + d.Feedback
		}
		s.notice("info", msg)
	}()
	writeJSON(w, map[string]string{"message": "sample change review opened"})
}

func demoChangeSet() orchestrator.ChangeSet {
	parse := `diff --git a/internal/parse.go b/internal/parse.go
index 3b18e51..a9c4d2f 100644
--- a/internal/parse.go
+++ b/internal/parse.go
@@ -12,9 +12,12 @@ import (
 // splitFields splits one input line at the separator. Quoted fields may
 // contain the separator.
 func splitFields(line string, sep rune) []string {
-	line = strings.TrimRight(line, string(sep))
 	var fields []string
 	var cur strings.Builder
+	// Keep trailing empty fields: "a,b," has three fields.
+	if line == "" {
+		return []string{""}
+	}
 	inQuote := false
 	for _, r := range line {
 		switch {
@@ -41,7 +44,15 @@ func parseLine(line string, opts Options) (Record, error) {
 	fields := splitFields(line, opts.Sep)
 	if len(fields) < 2 {
-		return Record{}, errors.New("too few fields")
+		if opts.Strict {
+			return Record{}, fmt.Errorf("line %q: want at least 2 fields, got %d", line, len(fields))
+		}
+		return Record{Raw: line}, nil
 	}
-	return Record{Key: fields[0], Values: fields[1:]}, nil
+	rec := Record{Key: fields[0], Values: fields[1:]}
+	if opts.Strict && rec.Key == "" {
+		return Record{}, fmt.Errorf("line %q: empty key", line)
+	}
+	return rec, nil
 }
`
	test := `diff --git a/internal/parse_test.go b/internal/parse_test.go
new file mode 100644
index 0000000..5d1e0aa
--- /dev/null
+++ b/internal/parse_test.go
@@ -0,0 +1,18 @@
+package internal
+
+import "testing"
+
+func TestSplitFieldsKeepsTrailing(t *testing.T) {
+	got := splitFields("a,b,", ',')
+	if len(got) != 3 || got[2] != "" {
+		t.Fatalf("splitFields = %q, want 3 fields", got)
+	}
+}
+
+func TestStrictRejectsShortLines(t *testing.T) {
+	_, err := parseLine("lonely", Options{Sep: ',', Strict: true})
+	if err == nil {
+		t.Fatal("want an error in strict mode")
+	}
+}
`
	readme := `diff --git a/README.md b/README.md
index 1f2e3d4..5a6b7c8 100644
--- a/README.md
+++ b/README.md
@@ -20,6 +20,10 @@ Usage:
   inventory report data.csv
   inventory stock --low 5 data.csv

+Flags:
+
+  --strict    reject malformed lines instead of skipping them
+
 Lines that cannot be parsed are skipped with a warning.
`
	return orchestrator.ChangeSet{
		StepID: "parser", Title: "Fix field splitting", Round: 1,
		Summary: "Kept trailing empty fields in splitFields, added --strict handling in parseLine and tests for both.",
		Files: []orchestrator.FileChange{
			{Path: "internal/parse.go", Status: "M", Added: 13, Deleted: 3, Patch: parse},
			{Path: "internal/parse_test.go", Status: "A", Added: 18, Patch: test},
			{Path: "README.md", Status: "M", Added: 4, Patch: readme},
		},
	}
}
