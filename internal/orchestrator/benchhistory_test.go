package orchestrator

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The history scan must stay cheap on repos full of big binaries: commits
// that cannot become tasks are listed by name only (their blobs are never
// read: here they are deleted, so reading one fails), and the candidates'
// big files count as binary without being loaded.
func TestHistoryReadsNoBlobsOutsideCandidates(t *testing.T) {
	dir := gitRepo(t)
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(msg string) string {
		t.Helper()
		tgit(t, dir, "add", "-A")
		tgit(t, dir, "commit", "-q", "-m", msg)
		return tgit(t, dir, "rev-parse", "HEAD")
	}
	big := make([]byte, 3<<20)
	rand.New(rand.NewSource(1)).Read(big)
	write("level.unity", big)
	commit("Add the level")
	var gone []string
	for i := range 3 {
		big[i] ^= 0xff
		write("level.unity", big)
		write("notes.txt", []byte(strings.Repeat("x", i+1)))
		commit("Tweak the level lighting")
		gone = append(gone, tgit(t, dir, "rev-parse", "HEAD:level.unity"))
	}
	write("a.go", []byte("package a\n\nfunc A() {}\n"))
	write("b.go", []byte("package a\n\nfunc B() {}\n"))
	write("a_test.go", []byte("package a\n"))
	write("gen.go", bytes.Repeat([]byte("// generated\n"), 200_000)) // 2.6 MB of text
	cand := commit("Add A and B with a test")
	// Reading any of these blobs now fails.
	for _, id := range gone {
		if err := os.Remove(filepath.Join(dir, ".git", "objects", id[:2], id[2:])); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := NewBenchWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	hasTest := func(c HistoryCommit) bool {
		for _, f := range c.Files {
			if strings.HasSuffix(f.Path, "_test.go") {
				return true
			}
		}
		return false
	}
	list, err := ws.History(50, hasTest)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 5 || list[0].SHA != cand || list[0].Uncounted {
		t.Fatalf("history %+v", list)
	}
	for _, c := range list[1:] {
		if !c.Uncounted || len(c.Files) == 0 {
			t.Errorf("non-candidate %+v", c)
		}
	}
	files := map[string]HistoryFile{}
	for _, f := range list[0].Files {
		files[f.Path] = f
	}
	if files["a.go"].Lines != 3 || files["a.go"].Binary || !files["gen.go"].Binary {
		t.Errorf("candidate files %+v", list[0].Files)
	}
}

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
