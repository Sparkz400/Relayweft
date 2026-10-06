// Command gen builds the Hugo content of the docs site from website/pages
// and the Markdown that already lives in the repository (README.md,
// ROADMAP.md, docs/, editors/...), so nothing is written twice.
//
// Run it from website/ (build.sh does):
//
//	go run ./gen [-rw path/to/rw] [-require-cli]
//
// A page in website/pages is Markdown with Hugo front matter. These
// directives, each an HTML comment on its own line, pull text in:
//
//	<!-- include README.md#budgets -->        a section: the heading and everything up to the next heading of its level
//	<!-- include ROADMAP.md -->                a whole file (its H1 is dropped; the page has a title)
//	<!-- include docs/plan.md|plan.md -->      the first of these that exists
//	options: level=N (the section heading's level here, default 2), body (leave the heading out),
//	         intro (stop at the first subheading), item="**Team mode" (one list item of the section),
//	         para="**Estimates.**" (one paragraph of the section),
//	         code=yaml (the file as a code block), optional (nothing if the file is missing)
//	<!-- if-exists PATH --> ... <!-- end -->   kept only if PATH exists (if-missing: only if not)
//	<!-- cli -->                               the CLI reference, from rw --help (-rw or $RW_BIN)
//	<!-- pages docs/bench -->                  a list of that folder's pages; they become this page's children
//
// Links are resolved where the text came from and checked: a link to a
// file or section that is on the site points there, anything else in the
// repo points at GitHub, and a link to a missing file or heading fails the
// build. In a page, a link starting with "/" names a repo path; other links
// are relative to the page.
//
// docs/**/*.md that no page includes are added under "More" with a
// warning, and the images and videos under docs/ are copied to the site.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func main() {
	var o options
	flag.StringVar(&o.repo, "repo", "..", "repository root")
	flag.StringVar(&o.pages, "pages", "pages", "the site's own pages")
	flag.StringVar(&o.out, "out", "_gen", "output folder (content/ and static/ are replaced)")
	flag.StringVar(&o.rw, "rw", os.Getenv("RW_BIN"), "rw binary for the CLI reference (default $RW_BIN)")
	flag.BoolVar(&o.requireCLI, "require-cli", false, "fail without an rw binary instead of leaving a note")
	flag.StringVar(&o.github, "github", "https://github.com/Sparkz400/Relayweft", "repository URL for links to files that are not on the site")
	flag.StringVar(&o.branch, "branch", "main", "branch for links to GitHub")
	flag.Parse()
	g, err := newGen(o)
	if err == nil {
		err = g.run()
	}
	for _, w := range g.warnings() {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

type options struct {
	repo, pages, out, rw, github, branch string
	requireCLI                           bool
}

// page is one page of the generated site.
type page struct {
	content string // path under content/, e.g. "docs/guides/web.md"
	front   string // front matter, without the --- lines
	body    string // source text (directives not yet expanded)
	src     string // where the text comes from: "pages/<path>" or a repo path
	text    string // the generated Markdown
	anchors map[string]bool
	incl    map[string]bool // repo files this page includes (all or part)
}

// url is the page's URL path under the site root ("/docs/guides/web/").
func (p *page) url() string {
	c := strings.TrimSuffix(p.content, ".md")
	if path.Base(c) == "_index" {
		c = path.Dir(c)
		if c == "." {
			return "/"
		}
	}
	return "/" + strings.ToLower(c) + "/"
}

type gen struct {
	o        options
	pages    []*page
	bySource map[string]*page // "pages/<path>" -> page
	whole    map[string]*page // repo path included whole -> page
	section  map[string]*page // "path#anchor" -> page with that section
	sectBody map[string]*page // the same, for sections included without their heading
	pulled   map[string]*page // docs/*.md not included -> its own page
	assets   map[string]string
	children map[string]*page // docs folder -> page listing it
	warns    []string
	errs     []string
}

func newGen(o options) (*gen, error) {
	g := &gen{o: o, bySource: map[string]*page{}, whole: map[string]*page{}, section: map[string]*page{}, sectBody: map[string]*page{},
		pulled: map[string]*page{}, assets: map[string]string{}, children: map[string]*page{}}
	err := filepath.WalkDir(o.pages, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		rel, _ := filepath.Rel(o.pages, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		front, body, ok := splitFront(string(b))
		if !ok {
			return fmt.Errorf("%s: no front matter (--- title: ... ---)", p)
		}
		pg := &page{content: filepath.ToSlash(rel), front: front, body: body, src: "pages/" + filepath.ToSlash(rel), incl: map[string]bool{}}
		g.pages = append(g.pages, pg)
		g.bySource[pg.src] = pg
		return nil
	})
	return g, err
}

func (g *gen) warnings() []string { return g.warns }

func (g *gen) errorf(format string, a ...any) { g.errs = append(g.errs, fmt.Sprintf(format, a...)) }

func splitFront(s string) (front, body string, ok bool) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return "", s, false
	}
	end := strings.Index(s[4:], "\n---\n")
	if end < 0 {
		return "", s, false
	}
	return s[4 : 4+end], s[4+end+5:], true
}

func (g *gen) exists(repoPath string) bool {
	_, err := os.Stat(filepath.Join(g.o.repo, filepath.FromSlash(repoPath)))
	return err == nil
}

func (g *gen) read(repoPath string) (string, error) {
	b, err := os.ReadFile(filepath.Join(g.o.repo, filepath.FromSlash(repoPath)))
	return strings.ReplaceAll(string(b), "\r\n", "\n"), err
}

// --- directives ------------------------------------------------------------

var (
	reDirective = regexp.MustCompile(`^<!--\s*(include|if-exists|if-missing|end|cli|pages)\b\s*(.*?)\s*-->\s*$`)
	reOption    = regexp.MustCompile(`(\w[\w-]*)(?:=("[^"]*"|\S+))?`)
)

type include struct {
	file, anchor string
	level        int
	body, intro  bool
	item, para   string
	code         string
	optional     bool
}

func parseInclude(arg string) (include, error) {
	words := strings.Fields(arg)
	if len(words) == 0 {
		return include{}, fmt.Errorf("include needs a path")
	}
	// The path is the first word (it may hold "#", "|", "/" and ".").
	first := words[0]
	inc := include{level: 2}
	rest := strings.TrimSpace(strings.TrimPrefix(arg, first))
	inc.file, inc.anchor, _ = strings.Cut(first, "#")
	for _, m := range reOption.FindAllStringSubmatch(rest, -1) {
		val := strings.Trim(m[2], `"`)
		switch m[1] {
		case "level":
			n, err := strconv.Atoi(val)
			if err != nil || n < 2 || n > 6 {
				return inc, fmt.Errorf("level=%s: want 2 to 6", val)
			}
			inc.level = n
		case "body":
			inc.body = true
		case "intro":
			inc.intro = true
		case "item":
			inc.item = val
		case "para":
			inc.para = val
		case "code":
			inc.code = val
		case "optional":
			inc.optional = true
		default:
			return inc, fmt.Errorf("unknown option %q", m[1])
		}
	}
	return inc, nil
}

// pick returns the first of "a|b|c" that exists ("" if none does).
func (g *gen) pick(files string) string {
	for _, f := range strings.Split(files, "|") {
		if g.exists(f) {
			return f
		}
	}
	return ""
}

// condition evaluates if-exists / if-missing.
func (g *gen) condition(kind, arg string) bool {
	return g.exists(strings.TrimSpace(arg)) == (kind == "if-exists")
}

// scan walks a page's directives that are in effect and calls fn for
// each line (directive or not) that stays.
func (g *gen) scan(pg *page, fn func(line, kind, arg string)) {
	var keep []bool // stack of if-blocks
	on := func() bool {
		for _, k := range keep {
			if !k {
				return false
			}
		}
		return true
	}
	var f fence
	for _, l := range strings.Split(pg.body, "\n") {
		if f.step(l) {
			if on() {
				fn(l, "", "")
			}
			continue
		}
		m := reDirective.FindStringSubmatch(strings.TrimSpace(l))
		if m == nil {
			if on() {
				fn(l, "", "")
			}
			continue
		}
		switch m[1] {
		case "if-exists", "if-missing":
			keep = append(keep, g.condition(m[1], m[2]))
		case "end":
			if len(keep) == 0 {
				g.errorf("%s: <!-- end --> without an if-block", pg.src)
				continue
			}
			keep = keep[:len(keep)-1]
		default:
			if on() {
				fn(l, m[1], m[2])
			}
		}
	}
	if len(keep) > 0 {
		g.errorf("%s: an if-block has no <!-- end -->", pg.src)
	}
}

// --- the run -------------------------------------------------------------

func (g *gen) run() error {
	// 1. Which repo files and sections each page shows.
	for _, pg := range append([]*page(nil), g.pages...) {
		g.scan(pg, func(_, kind, arg string) {
			switch kind {
			case "include":
				inc, err := parseInclude(arg)
				if err != nil {
					g.errorf("%s: %v", pg.src, err)
					return
				}
				file := g.pick(inc.file)
				if file == "" {
					if !inc.optional {
						g.errorf("%s: include %s: no such file", pg.src, inc.file)
					}
					return
				}
				pg.incl[file] = true
				switch {
				case inc.code != "", inc.item != "", inc.para != "":
				case inc.anchor == "":
					if _, dup := g.whole[file]; !dup {
						g.whole[file] = pg
					}
				case !inc.body:
					if _, dup := g.section[file+"#"+inc.anchor]; !dup {
						g.section[file+"#"+inc.anchor] = pg
					}
				default:
					if _, dup := g.sectBody[file+"#"+inc.anchor]; !dup {
						g.sectBody[file+"#"+inc.anchor] = pg
					}
				}
			case "pages":
				g.children[path.Clean(strings.TrimSpace(arg))] = pg
			}
		})
	}
	// 2. docs/: pages for Markdown no page includes, and the media files.
	if err := g.walkDocs(); err != nil {
		return err
	}
	// 3. Expand every page.
	for _, pg := range g.pages {
		g.expand(pg)
	}
	// 4. Resolve and check the links, now that every page's anchors are known.
	for _, pg := range g.pages {
		pg.anchors = anchors(pg.text)
	}
	for _, pg := range g.pages {
		pg.text = g.resolve(pg)
	}
	if len(g.errs) > 0 {
		sort.Strings(g.errs)
		return fmt.Errorf("%d problem(s):\n  %s", len(g.errs), strings.Join(g.errs, "\n  "))
	}
	return g.write()
}

var mediaExt = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true,
	".webp": true, ".webm": true, ".mp4": true, ".cast": true}

func (g *gen) walkDocs() error {
	var more bool
	err := filepath.WalkDir(filepath.Join(g.o.repo, "docs"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(g.o.repo, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(path.Ext(rel))
		if mediaExt[ext] {
			g.assets[rel] = "/" + strings.TrimPrefix(rel, "docs/")
			return nil
		}
		if ext != ".md" || g.whole[rel] != nil {
			return nil
		}
		text, err := g.read(rel)
		if err != nil {
			return err
		}
		if err := noShortcodes(text); err != nil {
			g.errorf("%s: %v", rel, err)
		}
		title, body := dropTitle(text)
		if title == "" {
			title = strings.TrimSuffix(path.Base(rel), ".md")
		}
		var content string
		if parent, ok := g.children[path.Dir(rel)]; ok {
			content = path.Join(path.Dir(parent.content), path.Base(rel))
		} else {
			content = path.Join("docs/more", strings.TrimPrefix(rel, "docs/"))
			g.warns = append(g.warns, fmt.Sprintf("%s is on no page, so it is under More; include it from a page in website/pages", rel))
			more = true
		}
		pg := &page{content: content, front: "title: " + strconv.Quote(title), src: rel,
			text: g.rewrite(body, rel), incl: map[string]bool{rel: true}}
		g.pulled[rel] = pg
		g.pages = append(g.pages, pg)
		return nil
	})
	if more {
		g.pages = append(g.pages, &page{content: "docs/more/_index.md", src: "(generated)", incl: map[string]bool{},
			front: "title: More\nweight: 90", text: "Pages from `docs/` that no other page shows yet.\n"})
	}
	return err
}

// expand runs the directives of a page from website/pages.
func (g *gen) expand(pg *page) {
	if !strings.HasPrefix(pg.src, "pages/") {
		return
	}
	var b strings.Builder
	g.scan(pg, func(line, kind, arg string) {
		switch kind {
		case "":
			b.WriteString(line + "\n")
		case "include":
			inc, _ := parseInclude(arg)
			if file := g.pick(inc.file); file != "" {
				text, err := g.extract(file, inc)
				if err != nil {
					g.errorf("%s: include %s: %v", pg.src, arg, err)
					return
				}
				b.WriteString(text + "\n")
			}
		case "cli":
			b.WriteString(g.cli() + "\n")
		case "pages":
			b.WriteString(g.listPages(path.Clean(strings.TrimSpace(arg))) + "\n")
		}
	})
	pg.text = g.rewritePage(b.String(), pg)
}

// noShortcodes refuses repo text that Hugo would read as a shortcode.
func noShortcodes(text string) error {
	if strings.Contains(text, "{{<") || strings.Contains(text, "{{%") {
		return fmt.Errorf(`it has "{{<" or "{{%%", which Hugo would read as a shortcode`)
	}
	return nil
}

// extract returns the included text, its links already rewritten.
func (g *gen) extract(file string, inc include) (string, error) {
	text, err := g.read(file)
	if err != nil {
		return "", err
	}
	if err := noShortcodes(text); err != nil {
		return "", err
	}
	if inc.code != "" {
		return codeBlock(text, inc.code), nil
	}
	lines := strings.Split(text, "\n")
	if inc.anchor == "" {
		_, rest := dropTitle(text)
		return g.rewrite(rest, file), nil
	}
	hs := headings(lines)
	at := -1
	for i, h := range hs {
		if h.anchor == inc.anchor {
			at = i
			break
		}
	}
	if at < 0 {
		return "", fmt.Errorf("%s has no heading #%s", file, inc.anchor)
	}
	h := hs[at]
	end := len(lines)
	for _, n := range hs[at+1:] {
		if n.level <= h.level || inc.intro {
			end = n.line
			break
		}
	}
	sec := lines[h.line:end]
	delta := inc.level - h.level
	if inc.body {
		sec = sec[1:]
		delta = inc.level - (h.level + 1)
	}
	if inc.item != "" {
		item, ok := listItem(sec, inc.item)
		if !ok {
			return "", fmt.Errorf("no list item starting with %q in %s#%s", inc.item, file, inc.anchor)
		}
		sec = item
	}
	if inc.para != "" {
		par, ok := paragraph(sec, inc.para)
		if !ok {
			return "", fmt.Errorf("no paragraph starting with %q in %s#%s", inc.para, file, inc.anchor)
		}
		sec = par
	}
	sec = shiftHeadings(sec, delta, 2)
	return g.rewrite(strings.Trim(strings.Join(sec, "\n"), "\n"), file), nil
}

var reListItem = regexp.MustCompile(`^(\s*)([-*+]|\d+[.)])\s+`)

// listItem finds the list item whose text starts with prefix and returns
// it as plain paragraphs (its own nested lists stay lists).
func listItem(lines []string, prefix string) ([]string, bool) {
	for i, l := range lines {
		m := reListItem.FindStringSubmatch(l)
		if m == nil || !strings.HasPrefix(l[len(m[0]):], prefix) {
			continue
		}
		indent := len(m[0])
		out := []string{l[indent:]}
		for _, n := range lines[i+1:] {
			if strings.TrimSpace(n) != "" && len(n)-len(strings.TrimLeft(n, " \t")) < indent {
				break
			}
			if len(n) >= indent {
				n = n[indent:]
			} else {
				n = strings.TrimLeft(n, " \t")
			}
			out = append(out, n)
		}
		return out, true
	}
	return nil, false
}

// paragraph finds the paragraph (outside code blocks and lists) that
// starts with prefix.
func paragraph(lines []string, prefix string) ([]string, bool) {
	var f fence
	for i, l := range lines {
		if f.step(l) || !strings.HasPrefix(l, prefix) {
			continue
		}
		end := i + 1
		for end < len(lines) && strings.TrimSpace(lines[end]) != "" {
			end++
		}
		return lines[i:end], true
	}
	return nil, false
}

// --- links -----------------------------------------------------------------

// Links are first rewritten to "rw:<repo path>#anchor" (text from the
// repo) or "rwpage:<page source>#anchor" (a link between site pages), and
// resolved once every page exists.

func isExternal(t string) bool {
	if strings.HasPrefix(t, "//") || strings.HasPrefix(t, "mailto:") {
		return true
	}
	return reScheme.MatchString(t) && !strings.HasPrefix(t, "rw:") && !strings.HasPrefix(t, "rwpage:")
}

var reScheme = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)

// rewrite turns the links of text that comes from repo file src into
// rw: links.
func (g *gen) rewrite(text, src string) string {
	return mapLinks(text, func(t string, _ bool) string {
		if isExternal(t) {
			return t
		}
		p, frag, _ := strings.Cut(t, "#")
		if p == "" {
			return "rw:" + src + "#" + frag
		}
		p = path.Clean(path.Join(path.Dir(src), p))
		if strings.HasPrefix(p, "../") || p == ".." {
			g.errorf("%s: link %s leaves the repository", src, t)
			return t
		}
		return "rw:" + p + fragPart(frag)
	})
}

// rewritePage does the same for a page's own text: "/x" is a repo path,
// other paths are relative to the page.
func (g *gen) rewritePage(text string, pg *page) string {
	return mapLinks(text, func(t string, _ bool) string {
		if isExternal(t) || strings.HasPrefix(t, "rw:") || strings.HasPrefix(t, "#") {
			return t
		}
		p, frag, _ := strings.Cut(t, "#")
		if strings.HasPrefix(p, "/") {
			return "rw:" + path.Clean(strings.TrimPrefix(p, "/")) + fragPart(frag)
		}
		return "rwpage:" + path.Clean(path.Join(path.Dir(pg.src), p)) + fragPart(frag)
	})
}

func fragPart(f string) string {
	if f == "" {
		return ""
	}
	return "#" + f
}

// resolve replaces the rw: and rwpage: links of a page with URLs and
// checks them.
func (g *gen) resolve(pg *page) string {
	return mapLinks(pg.text, func(t string, image bool) string {
		switch {
		case strings.HasPrefix(t, "#"):
			if !pg.anchors[t[1:]] {
				g.errorf("%s: no heading %s on this page", pg.src, t)
			}
			return t
		case strings.HasPrefix(t, "rwpage:"):
			p, frag, _ := strings.Cut(strings.TrimPrefix(t, "rwpage:"), "#")
			to, ok := g.bySource[p]
			if !ok {
				g.errorf("%s: link to %s: no such page", pg.src, strings.TrimPrefix(p, "pages/"))
				return t
			}
			return g.pageLink(pg, to, frag, t)
		case strings.HasPrefix(t, "rw:"):
			return g.repoLink(pg, strings.TrimPrefix(t, "rw:"), image)
		}
		return t
	})
}

func (g *gen) pageLink(from, to *page, frag, orig string) string {
	if frag != "" && !to.anchors[frag] {
		g.errorf("%s: link %s: %s has no heading #%s", from.src, orig, to.url(), frag)
	}
	if to == from && frag != "" {
		return "#" + frag
	}
	return to.url() + fragPart(frag)
}

func (g *gen) repoLink(from *page, ref string, image bool) string {
	p, frag, _ := strings.Cut(ref, "#")
	// The heading is on this page.
	if frag != "" && from.incl[p] && from.anchors[frag] {
		return "#" + frag
	}
	if to := g.section[p+"#"+frag]; to != nil && frag != "" {
		return g.pageLink(from, to, frag, ref)
	}
	if to := g.sectBody[p+"#"+frag]; to != nil && frag != "" {
		return g.pageLink(from, to, "", ref) // the heading is not there, the text is
	}
	if to := g.whole[p]; to != nil {
		return g.pageLink(from, to, frag, ref)
	}
	if to := g.pulled[p]; to != nil {
		return g.pageLink(from, to, frag, ref)
	}
	if u, ok := g.assets[p]; ok {
		return u
	}
	st, err := os.Stat(filepath.Join(g.o.repo, filepath.FromSlash(p)))
	if err != nil {
		g.errorf("%s: link to %s: no such file in the repository", from.src, ref)
		return ref
	}
	if frag != "" && strings.HasSuffix(p, ".md") {
		text, _ := g.read(p)
		if !anchors(text)[frag] {
			g.errorf("%s: link to %s: %s has no heading #%s", from.src, ref, p, frag)
		}
	}
	switch {
	case st.IsDir():
		return fmt.Sprintf("%s/tree/%s/%s", g.o.github, g.o.branch, p)
	case image:
		return fmt.Sprintf("%s/raw/%s/%s", g.o.github, g.o.branch, p)
	}
	return fmt.Sprintf("%s/blob/%s/%s%s", g.o.github, g.o.branch, p, fragPart(frag))
}

// listPages is the list of a folder's pages, newest file name first.
func (g *gen) listPages(dir string) string {
	var rels []string
	for rel := range g.pulled {
		if path.Dir(rel) == dir {
			rels = append(rels, rel)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(rels)))
	var b strings.Builder
	for _, rel := range rels {
		title, _ := strconv.Unquote(strings.TrimPrefix(g.pulled[rel].front, "title: "))
		fmt.Fprintf(&b, "- [%s](/%s)\n", title, rel)
	}
	if len(rels) == 0 {
		b.WriteString("Nothing here yet.\n")
	}
	return b.String()
}

// --- output ----------------------------------------------------------------

func (g *gen) write() error {
	for _, d := range []string{"content", "static"} {
		if err := os.RemoveAll(filepath.Join(g.o.out, d)); err != nil {
			return err
		}
	}
	for _, pg := range g.pages {
		dst := filepath.Join(g.o.out, "content", filepath.FromSlash(pg.content))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		out := "---\n" + strings.TrimSpace(pg.front) + "\n---\n\n" + strings.TrimLeft(pg.text, "\n")
		if err := os.WriteFile(dst, []byte(out), 0o644); err != nil {
			return err
		}
	}
	for rel, u := range g.assets {
		b, err := os.ReadFile(filepath.Join(g.o.repo, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		dst := filepath.Join(g.o.out, "static", filepath.FromSlash(strings.TrimPrefix(u, "/")))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("gen: %d pages, %d media files -> %s\n", len(g.pages), len(g.assets), g.o.out)
	return nil
}
