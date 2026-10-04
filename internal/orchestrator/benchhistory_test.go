package orchestrator

import "testing"

func TestParseHistory(t *testing.T) {
	out := "\x1eaaa\x1fppp\x1fAdd quoted fields\n\nBody line.\n\x1f\n\n3\t1\tcsv/parse.go\n-\t-\tlogo.png\n10\t0\tcsv/parse_test.go\n" +
		"\x1ebbb\x1fp1 p2\x1fMerge branch x\n\x1f\n\n1\t1\ta.go\n" + // merge
		"\x1eccc\x1f\x1froot\n\x1f\n\n1\t0\ta.go\n" + // root commit
		"\x1eddd\x1fqqq\x1fOdd path\n\x1f\n\n1\t0\t\"a\tb.go\"\n" + // quoted path
		"\x1eeee\x1fqqq\x1fEmpty\n\x1f\n"
	got := parseHistory(out)
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	c := got[0]
	if c.SHA != "aaa" || c.Parent != "ppp" || c.Message != "Add quoted fields\n\nBody line." || len(c.Files) != 3 {
		t.Fatalf("got %+v", c)
	}
	if c.Files[0] != (HistoryFile{Path: "csv/parse.go", Lines: 4}) || !c.Files[1].Binary || c.Files[2].Lines != 10 {
		t.Errorf("files %+v", c.Files)
	}
}
