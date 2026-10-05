package forge

import (
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// lcs is the length of the longest common subsequence (the reference a
// shortest edit script is checked against).
func lcs(a, b []string) int {
	prev := make([]int, len(b)+1)
	for i := range a {
		cur := make([]int, len(b)+1)
		for j := range b {
			if a[i] == b[j] {
				cur[j+1] = prev[j] + 1
			} else {
				cur[j+1] = max(prev[j+1], cur[j])
			}
		}
		prev = cur
	}
	return prev[len(b)]
}

func randLines(r *rand.Rand, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = string(rune('a'+r.Intn(4))) + "\n"
	}
	return out
}

// The edit script turns a into b and is as short as can be.
func TestDiffLinesShortest(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 500; i++ {
		a, b := randLines(r, r.Intn(30)), randLines(r, r.Intn(30))
		ops := diffLines(a, b)
		var gotA, gotB []string
		edits := 0
		for _, o := range ops {
			if o.kind != '+' {
				gotA = append(gotA, o.line)
			}
			if o.kind != '-' {
				gotB = append(gotB, o.line)
			}
			if o.kind != ' ' {
				edits++
			}
		}
		if strings.Join(gotA, "") != strings.Join(a, "") || strings.Join(gotB, "") != strings.Join(b, "") {
			t.Fatalf("ops do not rebuild the texts: %q -> %q: %v", a, b, ops)
		}
		if want := len(a) + len(b) - 2*lcs(a, b); edits != want {
			t.Fatalf("%d edits, the shortest has %d: %q -> %q", edits, want, a, b)
		}
	}
	// Past the cap the diff is still right, just not the shortest.
	a, b := randLines(r, 3000), randLines(r, 3000)
	for i := range b {
		b[i] = "x" + b[i]
	}
	if ops := diffLines(a, b); len(ops) != 6000 || ops[0].kind != '-' || ops[5999].kind != '+' {
		t.Fatalf("over the cap: %d ops", len(ops))
	}
}

// git applies the hunks: their line numbers, context and "no newline"
// markers are what git expects.
func TestUnifiedHunksApply(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	r := rand.New(rand.NewSource(2))
	cases := [][2]string{
		{"", "new\n"},
		{"old\n", ""},
		{"a\nb\nc", "a\nb\nc\n"},
		{"a\nb\nc\n", "a\nB\nc"},
		{strings.Repeat("same\n", 20) + "x\n" + strings.Repeat("same\n", 6) + "y\n" + strings.Repeat("same\n", 7) + "z\n" + strings.Repeat("same\n", 20), strings.Repeat("same\n", 20) + "X\n" + strings.Repeat("same\n", 6) + "Y\n" + strings.Repeat("same\n", 7) + "Z\n" + strings.Repeat("same\n", 20)},
		{"crlf\r\nline\r\n", "crlf\r\nLINE\r\n"},
	}
	for i := 0; i < 6; i++ {
		a := randLines(r, 40)
		b := append([]string(nil), a...)
		for j := 0; j < 5; j++ {
			k := r.Intn(len(b))
			switch r.Intn(3) {
			case 0:
				b[k] = "changed\n"
			case 1:
				b = append(b[:k], b[k+1:]...)
			default:
				b = append(b[:k], append([]string{"inserted\n"}, b[k:]...)...)
			}
		}
		cases = append(cases, [2]string{strings.Join(a, ""), strings.Join(b, "")})
	}
	if h := unifiedHunks("same\n", "same\n"); h != "" {
		t.Fatalf("no change gave %q", h)
	}
	if h := unifiedHunks(cases[4][0], cases[4][1]); strings.Count(h, "@@ -") != 2 {
		t.Fatalf("changes 6 lines apart share a hunk, 7 apart do not:\n%s", h)
	}
	dir := t.TempDir()
	for i, c := range cases {
		os.Remove(filepath.Join(dir, "f"))
		from := "a/f"
		if c[0] != "" {
			os.WriteFile(filepath.Join(dir, "f"), []byte(c[0]), 0o644)
		} else {
			from = "/dev/null"
		}
		to := "b/f"
		if c[1] == "" {
			to = "/dev/null"
		}
		patch := "diff --git a/f b/f\n"
		if from == "/dev/null" {
			patch += "new file mode 100644\n"
		}
		if to == "/dev/null" {
			patch += "deleted file mode 100644\n"
		}
		patch += "--- " + from + "\n+++ " + to + "\n" + unifiedHunks(c[0], c[1])
		os.WriteFile(filepath.Join(dir, "p.diff"), []byte(patch), 0o644)
		cmd := exec.Command("git", "-c", "core.autocrlf=false", "apply", "p.diff")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("case %d: git apply: %v\n%s\n%s", i, err, out, patch)
		}
		got, _ := os.ReadFile(filepath.Join(dir, "f"))
		if string(got) != c[1] {
			t.Fatalf("case %d: applied %q, want %q\n%s", i, got, c[1], patch)
		}
	}
}
