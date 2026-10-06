package main

import (
	"os"
	"path/filepath"
)

// The demo task, typed into rw in the recording.
//
//	Make the parser keep trailing empty fields and add a --strict flag that rejects malformed lines

// planJSON is the planner's answer: look first, then two edits that run in
// parallel. The recording adds "Document it in the README." to the strict
// step in plan approval, and the strict agent then edits the README too.
const planJSON = `{
  "summary": "Map the parser, then fix the field splitting and add --strict in parallel.",
  "subtasks": [
    {"id": "map", "title": "Map the parser", "kind": "explore",
     "prompt": "Find where lines are split into fields and where bad lines are handled.", "files": []},
    {"id": "fields", "title": "Keep trailing empty fields", "kind": "edit",
     "prompt": "In splitFields, stop trimming the trailing separator, so \"a,b,\" has three fields. Add a test.",
     "files": ["parse.go", "parse_test.go"], "depends_on": ["map"]},
    {"id": "strict", "title": "Add a --strict flag", "kind": "edit",
     "prompt": "Add a --strict flag to main.go: stop at the first malformed line with exit code 1.",
     "files": ["main.go"], "depends_on": ["map"]}
  ]
}`

// setup writes the sample project (before the task) into dir.
func setup(dir string) error {
	files := map[string]string{
		"go.mod":        "module example.com/inventory\n\ngo 1.22\n",
		"main.go":       mainBefore,
		"parse.go":      parseBefore,
		"parse_test.go": parseTestBefore,
		"README.md":     readmeBefore,
		"stock.csv":     "apples,12,crate\npears,4,\nplums\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

const mainBefore = `// Command inventory prints a stock list from key,value lines.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
)

func main() {
	sep := flag.String("sep", ",", "field separator")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: inventory [--sep ,] <file>")
		os.Exit(2)
	}
	f, err := os.Open(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		rec, err := parseLine(sc.Text(), rune((*sep)[0]))
		if err != nil {
			fmt.Fprintf(os.Stderr, "line %d skipped: %v\n", n, err)
			continue
		}
		fmt.Printf("%-10s %q\n", rec.Key, rec.Values)
	}
}
`

const mainAfter = `// Command inventory prints a stock list from key,value lines.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
)

func main() {
	sep := flag.String("sep", ",", "field separator")
	strict := flag.Bool("strict", false, "stop at the first malformed line (exit code 1)")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: inventory [--sep ,] [--strict] <file>")
		os.Exit(2)
	}
	f, err := os.Open(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		rec, err := parseLine(sc.Text(), rune((*sep)[0]))
		if err != nil && *strict {
			fmt.Fprintf(os.Stderr, "line %d: %v\n", n, err)
			os.Exit(1)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "line %d skipped: %v\n", n, err)
			continue
		}
		fmt.Printf("%-10s %q\n", rec.Key, rec.Values)
	}
}
`

const parseBefore = `package main

import (
	"errors"
	"strings"
)

// Record is one parsed input line.
type Record struct {
	Key    string
	Values []string
}

// splitFields splits a line at sep. Quoted fields may contain sep.
func splitFields(line string, sep rune) []string {
	line = strings.TrimRight(line, string(sep))
	var fields []string
	var cur strings.Builder
	inQuote := false
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == sep && !inQuote:
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(fields, cur.String())
}

// parseLine turns one line into a Record.
func parseLine(line string, sep rune) (Record, error) {
	fields := splitFields(line, sep)
	if len(fields) < 2 {
		return Record{}, errors.New("too few fields")
	}
	return Record{Key: fields[0], Values: fields[1:]}, nil
}
`

// parseAfter has two hunks: the fix, and a debug print the agent left in.
// The recording unticks the second one in change review.
const parseAfter = `package main

import (
	"errors"
	"strings"
)

// Record is one parsed input line.
type Record struct {
	Key    string
	Values []string
}

// splitFields splits a line at sep. Quoted fields may contain sep.
// Trailing empty fields are kept: "a,b," has three fields.
func splitFields(line string, sep rune) []string {
	var fields []string
	var cur strings.Builder
	inQuote := false
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == sep && !inQuote:
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(fields, cur.String())
}

// parseLine turns one line into a Record.
func parseLine(line string, sep rune) (Record, error) {
	fields := splitFields(line, sep)
	println("DEBUG fields:", len(fields))
	if len(fields) < 2 {
		return Record{}, errors.New("too few fields")
	}
	return Record{Key: fields[0], Values: fields[1:]}, nil
}
`

const parseTestBefore = `package main

import "testing"

func TestParseLine(t *testing.T) {
	rec, err := parseLine("apples,12,crate", ',')
	if err != nil || rec.Key != "apples" || len(rec.Values) != 2 {
		t.Fatalf("parseLine = %+v, %v", rec, err)
	}
}
`

const parseTestAfter = parseTestBefore + `
func TestSplitFieldsKeepsTrailingEmpty(t *testing.T) {
	got := splitFields("pears,4,", ',')
	if len(got) != 3 || got[2] != "" {
		t.Fatalf("splitFields = %q, want 3 fields", got)
	}
}
`

const readmeBefore = `# inventory

Prints a stock list from lines of comma-separated fields.

    go run . stock.csv

Lines with fewer than two fields are skipped with a warning.
`

const readmeAfter = `# inventory

Prints a stock list from lines of comma-separated fields.

    go run . stock.csv

Lines with fewer than two fields are skipped with a warning.
With --strict, the first such line stops the run with exit code 1.
`
