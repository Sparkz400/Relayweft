package main

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Markdown helpers: just enough of GitHub-flavoured Markdown to find
// headings, sections and links outside code blocks.

// fence tracks fenced code blocks line by line.
type fence struct {
	char byte
	n    int
}

// step reports whether line is inside a code block (the fence lines
// themselves count as inside) and updates the state.
func (f *fence) step(line string) bool {
	t := strings.TrimLeft(line, " \t")
	if f.n == 0 {
		if c, n := fenceRun(t); n >= 3 && !(c == '`' && strings.ContainsRune(t[n:], '`')) {
			f.char, f.n = c, n
			return true
		}
		return false
	}
	if c, n := fenceRun(t); c == f.char && n >= f.n && strings.TrimSpace(t[n:]) == "" {
		f.n = 0
	}
	return true
}

func fenceRun(t string) (byte, int) {
	if t == "" || (t[0] != '`' && t[0] != '~') {
		return 0, 0
	}
	n := 0
	for n < len(t) && t[n] == t[0] {
		n++
	}
	return t[0], n
}

var reHeading = regexp.MustCompile(`^ {0,3}(#{1,6})[ \t]+(.*?)[ \t]*(?:[ \t]#+)?[ \t]*$`)

// heading is an ATX heading: its line index, level, text and anchor.
type heading struct {
	line   int
	level  int
	text   string
	anchor string
}

// headings lists the ATX headings outside code blocks with the anchors
// GitHub and Hugo (autoHeadingIDType: github) give them.
func headings(lines []string) []heading {
	var out []heading
	var f fence
	seen := map[string]int{}
	for i, l := range lines {
		if f.step(l) {
			continue
		}
		m := reHeading.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		a := slug(m[2])
		if n := seen[a]; n > 0 {
			seen[a] = n + 1
			a = fmt.Sprintf("%s-%d", a, n)
		} else {
			seen[a] = 1
		}
		out = append(out, heading{line: i, level: len(m[1]), text: m[2], anchor: a})
	}
	return out
}

var (
	reMdLink  = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	reHTMLTag = regexp.MustCompile(`<[^>]+>`)
)

// slug is GitHub's anchor for a heading's text: lower case, letters,
// digits, "-" and "_" kept, spaces turned into "-", everything else gone.
func slug(text string) string {
	text = reMdLink.ReplaceAllString(text, "$1")
	text = reHTMLTag.ReplaceAllString(text, "")
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case unicode.IsLetter(r), unicode.IsNumber(r), r == '_', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

// anchors is the set of heading anchors of a Markdown text.
func anchors(text string) map[string]bool {
	out := map[string]bool{}
	for _, h := range headings(strings.Split(text, "\n")) {
		out[h.anchor] = true
	}
	return out
}

// shiftHeadings moves every heading outside code blocks by delta levels,
// keeping them between minLevel and 6.
func shiftHeadings(lines []string, delta, minLevel int) []string {
	if delta == 0 {
		return lines
	}
	out := make([]string, len(lines))
	var f fence
	for i, l := range lines {
		out[i] = l
		if f.step(l) {
			continue
		}
		m := reHeading.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		lvl := min(6, max(minLevel, len(m[1])+delta))
		out[i] = strings.Repeat("#", lvl) + " " + m[2]
	}
	return out
}

var (
	reInlineLink = regexp.MustCompile(`(!?)\[((?:[^\[\]]|\[[^\[\]]*\])*)\]\(\s*(<[^>]*>|[^)\s]+)((?:\s+"[^"]*")?)\s*\)`)
	reRefDef     = regexp.MustCompile(`^( {0,3}\[[^\]]+\]:[ \t]*)(\S+)(.*)$`)
)

// mapLinks calls fn for every link and image target outside code blocks
// and code spans, and replaces the target with what fn returns.
func mapLinks(text string, fn func(target string, image bool) string) string {
	lines := strings.Split(text, "\n")
	var f fence
	for i, l := range lines {
		if f.step(l) {
			continue
		}
		if m := reRefDef.FindStringSubmatch(l); m != nil {
			lines[i] = m[1] + fn(m[2], false) + m[3]
			continue
		}
		// Hide code spans, rewrite, put them back.
		masked, spans := maskCodeSpans(l)
		masked = reInlineLink.ReplaceAllStringFunc(masked, func(s string) string {
			m := reInlineLink.FindStringSubmatch(s)
			target := strings.TrimSuffix(strings.TrimPrefix(m[3], "<"), ">")
			return m[1] + "[" + m[2] + "](" + fn(target, m[1] == "!") + m[4] + ")"
		})
		for j, s := range spans {
			masked = strings.Replace(masked, fmt.Sprintf("\x00%d\x00", j), s, 1)
		}
		lines[i] = masked
	}
	return strings.Join(lines, "\n")
}

// maskCodeSpans replaces each code span of a line with "\x00<n>\x00" and
// returns the spans. A span opens with a run of backticks and closes with
// the next run of the same length.
func maskCodeSpans(l string) (string, []string) {
	var b strings.Builder
	var spans []string
	for i := 0; i < len(l); {
		if l[i] != '`' {
			b.WriteByte(l[i])
			i++
			continue
		}
		n := 0
		for i+n < len(l) && l[i+n] == '`' {
			n++
		}
		end := -1
		for j := i + n; j < len(l); {
			if l[j] != '`' {
				j++
				continue
			}
			k := 0
			for j+k < len(l) && l[j+k] == '`' {
				k++
			}
			if k == n {
				end = j + k
				break
			}
			j += k
		}
		if end < 0 { // no closing run: literal backticks
			b.WriteString(l[i : i+n])
			i += n
			continue
		}
		spans = append(spans, l[i:end])
		fmt.Fprintf(&b, "\x00%d\x00", len(spans)-1)
		i = end
	}
	return b.String(), spans
}

// dropTitle removes a leading H1 (and the blank lines after it) and
// returns its text.
func dropTitle(text string) (title, rest string) {
	lines := strings.Split(text, "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i == len(lines) || !strings.HasPrefix(lines[i], "# ") {
		return "", text
	}
	title = strings.TrimSpace(strings.TrimPrefix(lines[i], "# "))
	i++
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	return title, strings.Join(lines[i:], "\n")
}

// codeBlock wraps text in a fence longer than any backtick run in it.
func codeBlock(text, lang string) string {
	n := 3
	for _, m := range regexp.MustCompile("`+").FindAllString(text, -1) {
		n = max(n, len(m)+1)
	}
	f := strings.Repeat("`", n)
	return f + lang + "\n" + strings.TrimRight(text, "\n") + "\n" + f
}
