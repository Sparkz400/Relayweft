package diag

import "testing"

// FuzzParseRecord: any line parses without panicking, and every value the
// writer quotes reads back unchanged.
func FuzzParseRecord(f *testing.F) {
	for _, s := range []string{"", "tui", `exited: "x" = y`, "a b\tc\nd", "\xff\xfe", `\"`, "="} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		ParseRecord(v)
		ParseRecord("2026-10-04 12:00:00.000 " + v)
		line := "2026-10-04 12:00:00.000 end pid=7 k=" + quoteValue(v) + " last=1"
		r, ok := ParseRecord(line)
		if !ok || r.Fields["k"] != v || r.Fields["last"] != "1" {
			t.Fatalf("%q: %+v ok=%v", line, r, ok)
		}
	})
}
