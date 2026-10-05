package report

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"strconv"
	"strings"

	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/proc"
)

// Diff is the task's change: its before -> after snapshots.
type Diff struct {
	Before, After string
	Undone        bool // the task was undone since (sy undo --redo puts it back)
	Files         []FileDiff
	Add, Del      int
	Truncated     bool // lines were left out (limits)
	Hidden        int  // files whose content is not shown at all
}

// FileDiff is one file's change.
type FileDiff struct {
	Path      string
	Status    string // A, M, D, T
	Add, Del  int
	Binary    bool
	Lines     []Line
	Truncated bool
	Lang      string
}

// Line is one diff line, split into highlighted tokens.
type Line struct {
	Kind   string // add, del, ctx, hunk, note
	Tokens []Token
}

// Token is a piece of a line with a highlight class ("" = plain).
type Token struct{ Class, Text string }

// Text is the line without its tokens split.
func (l Line) Text() string {
	var b strings.Builder
	for _, t := range l.Tokens {
		b.WriteString(t.Text)
	}
	return b.String()
}

// loadDiff reads the diff of the task's undo snapshots.
func loadDiff(dir, key string, o Options) (*Diff, error) {
	list, err := orchestrator.UndoList(dir)
	if err != nil {
		return nil, err
	}
	var t *orchestrator.UndoTask
	for i := range list {
		if list[i].Key == key {
			t = &list[i]
		}
	}
	if t == nil {
		return nil, errors.New("no undo snapshots for this task (only the newest 30 tasks per working tree are kept)")
	}
	d := &Diff{Before: t.Before, After: t.After, Undone: t.Undone}
	maxFile, maxAll, maxBytes := o.MaxFileLines, o.MaxDiffLines, o.MaxDiffBytes
	if maxFile <= 0 {
		maxFile = defMaxFileLines
	}
	if maxAll <= 0 {
		maxAll = defMaxDiffLines
	}
	if maxBytes <= 0 {
		maxBytes = defMaxDiffBytes
	}
	base := append(append([]string(nil), proc.GitGuard...), "-c", "core.quotepath=false", "diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "--ignore-submodules=all")
	gitOut := func(limit int64, args ...string) ([]byte, bool, error) {
		cmd := exec.Command("git", append(append([]string(nil), base...), args...)...)
		cmd.Dir = dir
		out, err := cmd.StdoutPipe()
		if err != nil {
			return nil, false, err
		}
		if err := cmd.Start(); err != nil {
			return nil, false, err
		}
		data, _ := io.ReadAll(io.LimitReader(out, limit+1))
		cut := int64(len(data)) > limit
		if cut {
			data = data[:limit]
			cmd.Process.Kill()
		}
		werr := cmd.Wait()
		if werr != nil && !cut {
			return nil, false, fmt.Errorf("git diff: %w", werr)
		}
		return data, cut, nil
	}
	num, _, err := gitOut(16<<20, "--numstat", "-z", t.Before, t.After)
	if err != nil {
		return nil, err
	}
	index := map[string]int{}
	for _, rec := range strings.Split(string(num), "\x00") {
		f := strings.SplitN(rec, "\t", 3)
		if len(f) < 3 {
			continue
		}
		fd := FileDiff{Path: f[2], Status: "M", Lang: langOf(f[2])}
		if f[0] == "-" {
			fd.Binary = true
		} else {
			fd.Add, _ = strconv.Atoi(f[0])
			fd.Del, _ = strconv.Atoi(f[1])
		}
		d.Add += fd.Add
		d.Del += fd.Del
		index[fd.Path] = len(d.Files)
		d.Files = append(d.Files, fd)
	}
	ns, _, err := gitOut(16<<20, "--name-status", "-z", t.Before, t.After)
	if err != nil {
		return nil, err
	}
	f := strings.Split(string(ns), "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		if j, ok := index[f[i+1]]; ok && f[i] != "" {
			d.Files[j].Status = f[i][:1]
		}
	}
	patch, cut, err := gitOut(maxBytes, t.Before, t.After)
	if err != nil {
		return nil, err
	}
	if cut {
		d.Truncated = true
	}
	// Split the patch per file. With --no-renames both header paths are the
	// same; a path git had to quote falls back to the file order, which is
	// the same in --numstat and the patch.
	chunks := splitPatch(string(patch))
	shown := 0
	for ci, c := range chunks {
		j, ok := index[c.path]
		if !ok {
			if ci >= len(d.Files) {
				continue
			}
			j = ci
		}
		fd := &d.Files[j]
		if fd.Binary {
			continue
		}
		for _, l := range c.lines {
			if len(fd.Lines) >= maxFile || shown >= maxAll {
				fd.Truncated, d.Truncated = true, true
				break
			}
			fd.Lines = append(fd.Lines, diffLine(l, fd.Lang))
			shown++
		}
	}
	for i := range d.Files {
		fd := &d.Files[i]
		if !fd.Binary && len(fd.Lines) == 0 && fd.Add+fd.Del > 0 {
			fd.Truncated = true
			d.Hidden++
		}
	}
	return d, nil
}

type chunk struct {
	path  string
	lines []string
}

func splitPatch(p string) []chunk {
	var out []chunk
	var cur *chunk
	inBody := false
	for _, l := range strings.Split(p, "\n") {
		if strings.HasPrefix(l, "diff --git ") {
			out = append(out, chunk{path: headerPath(l)})
			cur = &out[len(out)-1]
			inBody = false
			continue
		}
		if cur == nil {
			continue
		}
		if strings.HasPrefix(l, "@@") {
			inBody = true
		}
		if inBody && l != "" {
			cur.lines = append(cur.lines, strings.TrimSuffix(l, "\r"))
		}
	}
	return out
}

// headerPath reads "diff --git a/p b/p" (both sides equal without renames).
func headerPath(l string) string {
	rest := strings.TrimPrefix(l, "diff --git ")
	if !strings.HasPrefix(rest, "a/") || len(rest)%2 != 1 {
		return ""
	}
	half := (len(rest) - 1) / 2
	a, b := rest[:half], rest[half+1:]
	if strings.TrimPrefix(a, "a/") != strings.TrimPrefix(b, "b/") || !strings.HasPrefix(b, "b/") {
		return ""
	}
	return a[2:]
}

func diffLine(l, lang string) Line {
	switch {
	case strings.HasPrefix(l, "@@"):
		return Line{Kind: "hunk", Tokens: []Token{{Text: l}}}
	case strings.HasPrefix(l, `\`):
		return Line{Kind: "note", Tokens: []Token{{Text: l}}}
	}
	kind, sign := "ctx", " "
	switch l[0] {
	case '+':
		kind, sign = "add", "+"
	case '-':
		kind, sign = "del", "-"
	}
	toks := append([]Token{{Class: "sg", Text: sign}}, highlight(l[1:], lang)...)
	return Line{Kind: kind, Tokens: toks}
}

// langOf picks a highlighting family from the file name.
func langOf(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".go", ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx", ".java", ".c", ".h", ".cc", ".cpp", ".hpp", ".cs", ".rs", ".kt", ".swift", ".php", ".scala", ".dart", ".css", ".scss":
		return "c"
	case ".py", ".sh", ".bash", ".rb", ".yaml", ".yml", ".toml", ".pl", ".r", ".ps1", ".mk", ".conf", ".ini", ".cfg":
		return "hash"
	case ".sql", ".lua", ".hs":
		return "dash"
	}
	switch strings.ToLower(path.Base(p)) {
	case "makefile", "dockerfile", ".gitignore", ".env":
		return "hash"
	}
	return ""
}

var keywords = map[string]bool{}

func init() {
	for _, k := range strings.Fields(`break case catch class const continue def default defer do elif else enum
		except export extends false finally fn for from func function go if impl import in interface let
		match mut nil None null package pass private protected pub public raise range return select self
		static struct super switch this throw true True False try type typeof var void while with yield
		async await lambda not and or is new delete package use mod trait where loop local then end`) {
		keywords[k] = true
	}
}

// highlight splits a line into tokens: comments, strings, numbers and
// keywords. It is deliberately simple (one line at a time, no state across
// lines) and never changes the text, only how it is colored.
func highlight(s, lang string) []Token {
	if lang == "" || len(s) > 2000 {
		return []Token{{Text: s}}
	}
	var out []Token
	plain := func(t string) {
		if t == "" {
			return
		}
		if n := len(out); n > 0 && out[n-1].Class == "" {
			out[n-1].Text += t
			return
		}
		out = append(out, Token{Text: t})
	}
	i := 0
	for i < len(s) {
		c := s[i]
		rest := s[i:]
		switch {
		case lang == "c" && (strings.HasPrefix(rest, "//") || strings.HasPrefix(rest, "/*")),
			lang == "hash" && c == '#',
			lang == "dash" && strings.HasPrefix(rest, "--"):
			out = append(out, Token{Class: "com", Text: rest})
			return out
		case c == '"' || c == '\'' || c == '`':
			j := i + 1
			for j < len(s) && s[j] != c {
				if s[j] == '\\' && c != '`' {
					j++
				}
				j++
			}
			j = min(j+1, len(s))
			out = append(out, Token{Class: "str", Text: s[i:j]})
			i = j
		case isDigit(c) && (i == 0 || !isWord(s[i-1])):
			j := i
			for j < len(s) && (isWord(s[j]) || s[j] == '.') {
				j++
			}
			out = append(out, Token{Class: "num", Text: s[i:j]})
			i = j
		case isWord(c):
			j := i
			for j < len(s) && isWord(s[j]) {
				j++
			}
			w := s[i:j]
			switch {
			case keywords[w]:
				out = append(out, Token{Class: "kw", Text: w})
			case j < len(s) && s[j] == '(':
				out = append(out, Token{Class: "fn", Text: w})
			default:
				plain(w)
			}
			i = j
		default:
			j := i + 1
			for j < len(s) && !isWord(s[j]) && !isDigit(s[j]) && !strings.ContainsRune("\"'`#/-", rune(s[j])) {
				j++
			}
			plain(s[i:j])
			i = j
		}
	}
	return out
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isWord(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || isDigit(c) || c >= 0x80
}
