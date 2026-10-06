package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Upgrading from Switchyard (`sy`)":                          "upgrading-from-switchyard-sy",
		"Per-repo settings: `.relayweft.yaml`":                      "per-repo-settings-relayweftyaml",
		"The browser UI: `rw web` and `rw app`":                     "the-browser-ui-rw-web-and-rw-app",
		"Bench: does Relayweft beat a single agent on *your* work?": "bench-does-relayweft-beat-a-single-agent-on-your-work",
		"The repo's own conventions":                                "the-repos-own-conventions",
		"How it works (and the decisions made for v1)":              "how-it-works-and-the-decisions-made-for-v1",
		"A [link](x.md) and <b>tags</b>":                            "a-link-and-tags",
		"Ünïcode stays":                                             "ünïcode-stays",
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHeadingsSkipCodeAndNumberDuplicates(t *testing.T) {
	text := "# Title\n\n## Usage\n\n```sh\n# not a heading\n```\n\n## Usage\n~~~\n## nor this\n~~~\n### Deep ###\n"
	var got []string
	for _, h := range headings(strings.Split(text, "\n")) {
		got = append(got, h.anchor)
	}
	want := []string{"title", "usage", "usage-1", "deep"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("anchors = %q, want %q", got, want)
	}
}

func TestMapLinksSkipsCode(t *testing.T) {
	text := "See [a](a.md) and `[b](b.md)` and ![img](i.png \"t\").\n```\n[c](c.md)\n```\n[ref]: r.md\n``code `[d](d.md)` ``"
	var seen []string
	out := mapLinks(text, func(target string, image bool) string {
		seen = append(seen, target)
		return "X" + target
	})
	if want := []string{"a.md", "i.png", "r.md"}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("targets = %q, want %q", seen, want)
	}
	for _, s := range []string{"[a](Xa.md)", "`[b](b.md)`", `![img](Xi.png "t")`, "[c](c.md)", "[ref]: Xr.md", "``code `[d](d.md)` ``"} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
}

func TestListItemAndParagraph(t *testing.T) {
	lines := strings.Split("Intro:\n- **One.** first\n  more of one\n  - nested\n- **Two.** second\n\n**Para.** text\nstill para\n\nnext", "\n")
	item, ok := listItem(lines, "**One.**")
	if !ok || strings.Join(item, "\n") != "**One.** first\nmore of one\n- nested" {
		t.Fatalf("item = %q, %v", item, ok)
	}
	par, ok := paragraph(lines, "**Para.**")
	if !ok || strings.Join(par, "\n") != "**Para.** text\nstill para" {
		t.Fatalf("paragraph = %q, %v", par, ok)
	}
}

// A small repo and site: includes, link rewriting and the checks.
func TestGenerate(t *testing.T) {
	repo := t.TempDir()
	write := func(p, s string) {
		t.Helper()
		p = filepath.Join(repo, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "# Proj\n\n## Budgets\n\nSee [providers](docs/providers.md#keys), [hooks](#hooks), [cost](#cost) and [code](internal/x.go).\n\n### Team\n\nteam text\n\n## Hooks\n\nhook text ![shot](docs/web/a.png)\n\n## Cost\n\ncost text\n")
	write("docs/providers.md", "# Providers\n\n## Keys\n\nBack to [budgets](../README.md#budgets).\n")
	write("docs/web/a.png", "png")
	write("docs/bench/2026-01-01-x.md", "# Bench one\n\ntext\n")
	write("docs/orphan.md", "# Orphan\n")
	write("internal/x.go", "package x\n")
	write("site/pages/docs/_index.md", "---\ntitle: Docs\n---\n\n<!-- include README.md#budgets -->\n\n<!-- include README.md#hooks -->\n\n[guide](guide.md#keys) and [repo file](/internal/x.go).\n\n<!-- if-missing CHANGELOG.md -->\nno changelog\n<!-- end -->\n<!-- if-exists CHANGELOG.md -->\nchangelog\n<!-- end -->\n")
	write("site/pages/docs/guide.md", "---\ntitle: Guide\n---\n\n<!-- include docs/providers.md -->\n<!-- include docs/plan.md|README.md#cost body -->\n")
	write("site/pages/docs/bench/_index.md", "---\ntitle: Bench\n---\n\n<!-- pages docs/bench -->\n")

	g, err := newGen(options{repo: repo, pages: filepath.Join(repo, "site/pages"), out: filepath.Join(repo, "site/_gen"),
		github: "https://github.com/o/r", branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.run(); err != nil {
		t.Fatal(err)
	}
	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join(repo, "site/_gen", filepath.FromSlash(p)))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	docs := read("content/docs/_index.md")
	for _, want := range []string{
		"## Budgets",
		"[providers](/docs/guide/#keys)", // a whole include elsewhere
		"[hooks](#hooks)",                // on this page
		"[cost](/docs/guide/)",           // included without its heading: no anchor there
		"(https://github.com/o/r/blob/main/internal/x.go)", // not on the site
		"![shot](/web/a.png)",
		"[guide](/docs/guide/#keys)",
		"[repo file](https://github.com/o/r/blob/main/internal/x.go)",
		"no changelog",
	} {
		if !strings.Contains(docs, want) {
			t.Errorf("docs page lacks %q:\n%s", want, docs)
		}
	}
	if !strings.Contains(docs, "### Team") {
		t.Error("the subsection of an included section is missing")
	}
	if strings.Contains(docs, "\nchangelog\n") {
		t.Error("an if-exists block for a missing file was kept")
	}
	guide := read("content/docs/guide.md")
	if !strings.Contains(guide, "[budgets](/docs/#budgets)") || strings.Contains(guide, "# Providers") || !strings.Contains(guide, "cost text") {
		t.Errorf("guide page:\n%s", guide)
	}
	if b := read("content/docs/bench/2026-01-01-x.md"); !strings.Contains(b, `title: "Bench one"`) {
		t.Errorf("bench page:\n%s", b)
	}
	if !strings.Contains(read("content/docs/bench/_index.md"), "[Bench one](/docs/bench/2026-01-01-x/)") {
		t.Error("the bench list does not link the bench page")
	}
	read("content/docs/more/orphan.md")
	if read("static/web/a.png") != "png" {
		t.Error("media file not copied")
	}
	if len(g.warnings()) != 1 || !strings.Contains(g.warnings()[0], "docs/orphan.md") {
		t.Errorf("warnings = %q", g.warnings())
	}

	// Broken links and anchors fail the build.
	write("docs/providers.md", "# Providers\n\n[gone](missing.md) [no anchor](../README.md#nope) [here](#nowhere)\n")
	g, _ = newGen(options{repo: repo, pages: filepath.Join(repo, "site/pages"), out: filepath.Join(repo, "site/_gen"), github: "x", branch: "main"})
	err = g.run()
	if err == nil {
		t.Fatal("broken links passed")
	}
	for _, want := range []string{"docs/missing.md: no such file", "README.md has no heading #nope", "no heading #nowhere"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}
