package forge

import (
	"fmt"
	"strings"
)

// A line diff (Myers' algorithm) for a forge that serves file versions
// but no diff (Azure DevOps), written as git writes unified hunks.

// diffOp is one line of a diff: ' ' kept, '-' removed, '+' added.
type diffOp struct {
	kind byte
	line string // with its line end, if it has one
}

// maxDiffEdits caps the edits Myers' algorithm looks for (its memory
// grows with their square); beyond it the changed middle is written as
// removed, then added: still a correct diff, just not the shortest.
const maxDiffEdits = 1000

// splitLines splits s after each "\n"; the last line may lack one.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	ls := strings.SplitAfter(s, "\n")
	if ls[len(ls)-1] == "" {
		ls = ls[:len(ls)-1]
	}
	return ls
}

// diffLines turns a into b: kept, removed and added lines.
func diffLines(a, b []string) []diffOp {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var out []diffOp
	for _, l := range a[:pre] {
		out = append(out, diffOp{' ', l})
	}
	out = append(out, myers(a[pre:len(a)-suf], b[pre:len(b)-suf])...)
	for _, l := range a[len(a)-suf:] {
		out = append(out, diffOp{' ', l})
	}
	return out
}

// replaced writes all of a as removed and all of b as added.
func replaced(a, b []string) []diffOp {
	out := make([]diffOp, 0, len(a)+len(b))
	for _, l := range a {
		out = append(out, diffOp{'-', l})
	}
	for _, l := range b {
		out = append(out, diffOp{'+', l})
	}
	return out
}

// myers finds a shortest edit script (E. Myers, "An O(ND) Difference
// Algorithm and Its Variations", 1986), keeping each step's frontier to
// walk back.
func myers(a, b []string) []diffOp {
	n, m := len(a), len(b)
	if n == 0 || m == 0 {
		return replaced(a, b)
	}
	most := n + m
	off := most + 1
	v := make([]int, 2*most+3) // v[off+k]: the furthest x on diagonal k
	var trace [][]int          // trace[d]: v[k] for k in [-d-1, d+1] before step d
	for d := 0; d <= most; d++ {
		if d > maxDiffEdits {
			return replaced(a, b)
		}
		trace = append(trace, append([]int(nil), v[off-d-1:off+d+2]...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1] // down: add b[y]
			} else {
				x = v[off+k-1] + 1 // right: remove a[x]
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x, y = x+1, y+1
			}
			v[off+k] = x
			if x >= n && y >= m {
				return walkBack(a, b, trace)
			}
		}
	}
	return replaced(a, b)
}

// walkBack reads the edit script off the frontiers, from the end.
func walkBack(a, b []string, trace [][]int) []diffOp {
	x, y := len(a), len(b)
	var rev []diffOp
	for d := len(trace) - 1; d >= 0; d-- {
		at := func(k int) int { return trace[d][k+d+1] }
		k := x - y
		pk := k - 1
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			pk = k + 1
		}
		px := at(pk)
		py := px - pk
		for x > px && y > py {
			rev = append(rev, diffOp{' ', a[x-1]})
			x, y = x-1, y-1
		}
		if d > 0 {
			if x == px {
				rev = append(rev, diffOp{'+', b[y-1]})
			} else {
				rev = append(rev, diffOp{'-', a[x-1]})
			}
		}
		x, y = px, py
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// unifiedHunks is the diff of two texts as git writes its hunks, with
// three lines of context; "" when they are the same.
func unifiedHunks(old, cur string) string {
	const ctx = 3
	ops := diffLines(splitLines(old), splitLines(cur))
	// oldAt[i], newAt[i]: the lines of each side before ops[i].
	oldAt, newAt := make([]int, len(ops)+1), make([]int, len(ops)+1)
	for i, o := range ops {
		oldAt[i+1], newAt[i+1] = oldAt[i], newAt[i]
		if o.kind != '+' {
			oldAt[i+1]++
		}
		if o.kind != '-' {
			newAt[i+1]++
		}
	}
	span := func(at, n int) string {
		if n == 1 {
			return fmt.Sprint(at + 1)
		}
		if n == 0 {
			return fmt.Sprintf("%d,0", at) // git names the line before
		}
		return fmt.Sprintf("%d,%d", at+1, n)
	}
	var b strings.Builder
	for i := 0; ; {
		for i < len(ops) && ops[i].kind == ' ' {
			i++
		}
		if i == len(ops) {
			break
		}
		// One hunk holds the changes less than 2*ctx+1 kept lines apart.
		end := i
		for j := i; j < len(ops); j++ {
			if ops[j].kind != ' ' {
				end = j + 1
			} else if j-end >= 2*ctx {
				break
			}
		}
		start, stop := max(i-ctx, 0), min(end+ctx, len(ops))
		fmt.Fprintf(&b, "@@ -%s +%s @@\n", span(oldAt[start], oldAt[stop]-oldAt[start]), span(newAt[start], newAt[stop]-newAt[start]))
		for _, o := range ops[start:stop] {
			b.WriteByte(o.kind)
			b.WriteString(o.line)
			if !strings.HasSuffix(o.line, "\n") {
				b.WriteString("\n\\ No newline at end of file\n")
			}
		}
		i = stop
	}
	return b.String()
}
