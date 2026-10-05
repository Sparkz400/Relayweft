package config

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// docs/config.md is rendered from the code: the Config struct (keys and
// types), Default() (defaults), the field comments here and in
// internal/notify, the comments in default.yaml, and the trust rules of
// repo.go. TestConfigDoc fails when the committed file differs; rewrite it
// with
//
//	go test ./internal/config -run TestConfigDoc -update-docs
//
//go:generate go test -run ^TestConfigDoc$ . -update-docs

var updateDocs = flag.Bool("update-docs", false, "rewrite docs/config.md")

const configDocPath = "../../docs/config.md"

func TestConfigDoc(t *testing.T) {
	got, missing := renderConfigDoc(t)
	for _, k := range missing {
		t.Errorf("config key %s has no description: add a comment to its field (or a line comment in default.yaml)", k)
	}
	if *updateDocs {
		if err := os.WriteFile(configDocPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(configDocPath)
	if err != nil {
		t.Fatal(err)
	}
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(got, want) {
		t.Errorf("docs/config.md is out of date: run go test ./internal/config -run TestConfigDoc -update-docs and commit it")
	}
}

// Every top-level key the trust rules name exists, so a renamed key
// cannot silently drop out of the trust column.
func TestConfigDocTrustKeys(t *testing.T) {
	keys := map[string]bool{}
	ct := reflect.TypeOf(Config{})
	for i := 0; i < ct.NumField(); i++ {
		keys[yamlName(ct.Field(i))] = true
	}
	for _, k := range append(append([]string{}, commandKeys...), sandboxKey, strings.Split(bestOfKey, ".")[0], strings.Split(teamDirKey, ".")[0], strings.Split(webhooksKey, ".")[0]) {
		if !keys[k] {
			t.Errorf("trust rules name %q, which is not a config key", k)
		}
	}
}

// docRow is one key of the reference.
type docRow struct {
	key, typ, def, trust, desc string
}

// fieldDoc is a struct field's comments in the Go source.
type fieldDoc struct{ doc, line string }

// goFieldDocs reads the field comments of every struct type in the
// config and notify packages: "Type.Field" -> comments.
func goFieldDocs(t *testing.T) map[string]fieldDoc {
	out := map[string]fieldDoc{}
	for _, dir := range []string{".", "../notify"} {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, parser.ParseComments) //nolint:staticcheck // deprecated for module-aware loading; this reads known folders of plain files
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pkgs {
			for _, f := range p.Files {
				ast.Inspect(f, func(n ast.Node) bool {
					ts, ok := n.(*ast.TypeSpec)
					if !ok {
						return true
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						return true
					}
					for _, fld := range st.Fields.List {
						for _, name := range fld.Names {
							out[ts.Name.Name+"."+name.Name] = fieldDoc{fld.Doc.Text(), fld.Comment.Text()}
						}
					}
					return true
				})
			}
		}
	}
	return out
}

// yamlDocs are the comments of default.yaml by key path: line comments
// of the keys, and head comments (section introductions).
type yamlDocs struct {
	line, head map[string]string
}

func readYAMLDocs(t *testing.T) yamlDocs {
	var root yaml.Node
	// A Windows checkout may have CRLF, which yaml.v3 reads as blank
	// lines between comment lines.
	if err := yaml.Unmarshal(bytes.ReplaceAll(defaultYAML, []byte("\r\n"), []byte("\n")), &root); err != nil {
		t.Fatal(err)
	}
	d := yamlDocs{line: map[string]string{}, head: map[string]string{}}
	var walk func(n *yaml.Node, path string)
	walk = func(n *yaml.Node, path string) {
		if n.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			p := genericPath(path, k.Value)
			c := firstNonEmpty(k.LineComment, v.LineComment)
			if v.Kind == yaml.MappingNode && len(v.Content) > 0 {
				c = firstNonEmpty(c, v.Content[0].LineComment) // "key: # comment" before a block
			}
			// A comment on one entry of a map (providers.codex.disabled:
			// "never route to Codex") does not describe the key in general.
			if _, ok := d.line[p]; !ok && c != "" && !strings.Contains(p, "<") {
				d.line[p] = cleanYAMLComment(c)
			}
			if _, ok := d.head[p]; !ok && k.HeadComment != "" {
				// The last paragraph: those before it belong to
				// commented-out keys above (mcp: before sandbox:).
				h := strings.ReplaceAll(k.HeadComment, "\r\n", "\n")
				if i := strings.LastIndex(h, "\n\n"); i >= 0 {
					h = h[i+2:]
				}
				d.head[p] = cleanYAMLComment(h)
			}
			walk(v, p)
		}
	}
	walk(root.Content[0], "")
	return d
}

// genericPath is a key path with map entries replaced by their
// placeholder (roles.planner -> roles.<role>).
func genericPath(parent, key string) string {
	switch parent {
	case "roles":
		key = "<role>"
	case "providers":
		key = "<provider>"
	case "mcp.servers":
		key = "<server>"
	case "providers.<provider>.env", "workspace.repos", "verify.affected_commands":
		key = "<name>"
	case "roles.<role>":
		if key != "prefer" {
			key = "<provider>"
		}
	}
	if parent == "" {
		return key
	}
	return parent + "." + key
}

var reCommentHash = regexp.MustCompile(`(?m)^#+ ?`)

func cleanYAMLComment(c string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(reCommentHash.ReplaceAllString(c, "")), " "))
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

func yamlName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	return name
}

func yamlInline(f reflect.StructField) bool {
	_, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	return strings.Contains(opts, "inline")
}

// placeholders names map entries in key paths.
var placeholders = map[string]string{
	"roles":                    "<role>",
	"providers":                "<provider>",
	"mcp.servers":              "<server>",
	"workspace.repos":          "<name>",
	"verify.affected_commands": "<name>",
	"providers.<provider>.env": "<name>",
}

var durationType = reflect.TypeOf(Duration(0))

func typeName(t reflect.Type) string {
	if t == durationType {
		return "duration"
	}
	switch t.Kind() {
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int64:
		return "integer"
	case reflect.Float64:
		return "number"
	case reflect.String:
		return "string"
	case reflect.Pointer:
		return typeName(t.Elem())
	case reflect.Slice:
		return "list of " + plural(typeName(t.Elem()))
	case reflect.Map:
		return "map of " + plural(typeName(t.Elem()))
	case reflect.Struct:
		return "section"
	}
	return t.Kind().String()
}

func plural(s string) string {
	switch s {
	case "section":
		return "sections"
	case "integer":
		return "integers"
	case "string":
		return "strings"
	}
	return s
}

// formatDefault renders a default value, or "" for a section.
func formatDefault(v reflect.Value) string {
	if !v.IsValid() {
		return ""
	}
	if v.Type() == durationType {
		d := v.Interface().(Duration)
		if d == 0 {
			return "`0`"
		}
		s := d.D().String()
		if strings.HasSuffix(s, "m0s") {
			s = strings.TrimSuffix(s, "0s")
		}
		if strings.HasSuffix(s, "h0m") {
			s = strings.TrimSuffix(s, "0m")
		}
		return "`" + s + "`"
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return "unset"
		}
		return formatDefault(v.Elem())
	case reflect.String:
		if v.String() == "" {
			return `""`
		}
		return "`" + v.String() + "`"
	case reflect.Struct:
		return ""
	case reflect.Slice, reflect.Map:
		if v.Len() == 0 {
			if v.Kind() == reflect.Map {
				return "`{}`"
			}
			return "`[]`"
		}
		b, _ := yaml.Marshal(v.Interface())
		var n yaml.Node
		if yaml.Unmarshal(b, &n) == nil && len(n.Content) > 0 {
			setFlow(n.Content[0])
			if fb, err := yaml.Marshal(n.Content[0]); err == nil {
				if s := strings.TrimSpace(string(fb)); len(s) <= 70 {
					return "`" + s + "`"
				}
			}
		}
		return fmt.Sprintf("%d entries (`rw init --print`)", v.Len())
	}
	return fmt.Sprintf("`%v`", v.Interface())
}

func setFlow(n *yaml.Node) {
	n.Style = yaml.FlowStyle
	for _, c := range n.Content {
		setFlow(c)
	}
}

// trustRule says how a repo's .relayweft.yaml (or an untrusted
// ./relayweft.yaml) may set a key, from the rules in repo.go.
func trustRule(key string) string {
	under := func(prefix string) bool {
		return key == prefix || strings.HasPrefix(key, prefix+".") || strings.HasPrefix(key, prefix+"[]")
	}
	for _, k := range append(append([]string{}, commandKeys...), teamDirKey, webhooksKey) {
		if under(k) {
			return "needs trust"
		}
	}
	switch {
	case under(sandboxKey):
		return "stricter only, until trusted"
	case under(bestOfKey):
		return "lower only, until trusted"
	// ApplyRepo: a repo file may tighten the budget and lower rw watch's
	// rounds, never the other way, trusted or not.
	case under("budget"), under("watch.max_rounds"):
		return "stricter only"
	}
	return "yes"
}

// renderConfigDoc renders docs/config.md and lists the keys without a
// description.
func renderConfigDoc(t *testing.T) ([]byte, []string) {
	gdocs := goFieldDocs(t)
	ydocs := readYAMLDocs(t)
	var missing []string
	type section struct {
		key, intro string
		rows       []docRow
	}
	var sections []*section
	seenType := map[reflect.Type]string{}

	describe := func(owner reflect.Type, f reflect.StructField, key string) string {
		gd := gdocs[owner.Name()+"."+f.Name]
		desc := firstNonEmpty(ydocs.line[key], gd.doc, gd.line)
		if ydocs.line[key] != "" && gd.doc != "" {
			desc = gd.doc // the full comment; default.yaml's is its short form
		}
		return cleanGoDoc(desc, f.Name, yamlName(f))
	}

	var walk func(sec *section, t reflect.Type, v reflect.Value, path string, depth int)
	walk = func(sec *section, t reflect.Type, v reflect.Value, path string, depth int) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := yamlName(f)
			if name == "-" || !f.IsExported() {
				continue
			}
			var fv reflect.Value
			if v.IsValid() {
				fv = v.Field(i)
			}
			if yamlInline(f) {
				continue // RoleCfg.Extra: documented as roles.<role>.<provider>
			}
			key := name
			if path != "" {
				key = path + "." + name
			}
			if t == reflect.TypeOf(RoleCfg{}) && name != "prefer" {
				if name != "codex" {
					continue // claude and the rest: the same row
				}
				key = path + ".<provider>"
			}
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
				if fv.IsValid() && !fv.IsNil() {
					fv = fv.Elem()
				} else {
					fv = reflect.Value{}
				}
			}
			row := docRow{key: key, typ: typeName(f.Type), trust: trustRule(key), desc: describe(t, f, key)}
			if v.IsValid() {
				row.def = formatDefault(fv)
			}
			if row.desc == "" {
				missing = append(missing, key)
			}
			elem := ft
			suffix := ""
			switch ft.Kind() {
			case reflect.Map:
				elem, suffix = ft.Elem(), "."+placeholders[key]
				if placeholders[key] == "" {
					suffix = ".<name>"
				}
			case reflect.Slice:
				elem, suffix = ft.Elem(), "[]"
			}
			if elem.Kind() == reflect.Struct && elem != durationType {
				if first, ok := seenType[elem]; ok {
					if row.desc != "" && !strings.HasSuffix(row.desc, ".") {
						row.desc += "."
					}
					row.desc += fmt.Sprintf(" Same keys as `%s`.", first)
					sec.rows = append(sec.rows, row)
					continue
				}
				seenType[elem] = key + suffix
				sec.rows = append(sec.rows, row)
				childV := fv
				if suffix != "" {
					childV = reflect.Value{} // map entries and list items: no single default
				}
				walk(sec, elem, childV, key+suffix, depth+1)
				continue
			}
			sec.rows = append(sec.rows, row)
		}
	}

	ct := reflect.TypeOf(Config{})
	def := reflect.ValueOf(*Default())
	// The top-level sections first: a provider's sandbox has "the same keys
	// as sandbox", not the other way round.
	for i := 0; i < ct.NumField(); i++ {
		if ft := ct.Field(i).Type; ft.Kind() == reflect.Struct && ft != durationType {
			seenType[ft] = yamlName(ct.Field(i))
		}
	}
	for i := 0; i < ct.NumField(); i++ {
		f := ct.Field(i)
		name := yamlName(f)
		if name == "-" {
			continue
		}
		ft := f.Type
		sec := &section{key: name, intro: ydocs.head[name]}
		if sec.intro == "" && ft.Kind() == reflect.Struct {
			sec.intro = cleanGoDoc(gdocs["Config."+f.Name].doc, f.Name, name)
		}
		if sec.intro == "" && ft.Kind() == reflect.Struct {
			sec.intro = typeDoc(t, ft.Name())
		}
		sections = append(sections, sec)
		switch {
		case ft.Kind() == reflect.Struct && ft != durationType:
			walk(sec, ft, def.Field(i), name, 0)
		case ft.Kind() == reflect.Map && ft.Elem().Kind() == reflect.Struct:
			ph := placeholders[name]
			sec.rows = append(sec.rows, docRow{key: name, typ: typeName(ft), trust: trustRule(name), def: entryNames(def.Field(i)), desc: describe(ct, f, name)})
			if sec.rows[0].desc == "" {
				missing = append(missing, name)
			}
			seenType[ft.Elem()] = name + "." + ph
			walk(sec, ft.Elem(), reflect.Value{}, name+"."+ph, 0)
		default:
			row := docRow{key: name, typ: typeName(ft), trust: trustRule(name), def: formatDefault(def.Field(i)), desc: describe(ct, f, name)}
			if row.desc == "" {
				missing = append(missing, name)
			}
			sec.rows = append(sec.rows, row)
		}
	}

	var b strings.Builder
	b.WriteString(configDocIntro)
	b.WriteString("\n## Contents\n\n")
	for _, s := range sections {
		fmt.Fprintf(&b, "- [`%s`](#%s)\n", s.key, anchor(s.key))
	}
	for _, s := range sections {
		fmt.Fprintf(&b, "\n## %s\n\n", s.key)
		if s.intro != "" {
			b.WriteString(escapeMD(s.intro) + "\n\n")
		}
		b.WriteString("| Key | Type | Default | In a repo file | Description |\n|---|---|---|---|---|\n")
		for _, r := range s.rows {
			def := r.def
			if def == "" {
				def = "-"
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", r.key, r.typ, cell(def), r.trust, cell(r.desc))
		}
	}
	sort.Strings(missing)
	return []byte(b.String()), missing
}

func anchor(s string) string { return strings.ReplaceAll(strings.ToLower(s), "_", "_") }

// entryNames lists a default map's keys ("" when empty).
func entryNames(v reflect.Value) string {
	if v.Len() == 0 {
		return "`{}`"
	}
	var keys []string
	for _, k := range v.MapKeys() {
		keys = append(keys, k.String())
	}
	sort.Strings(keys)
	return "`" + strings.Join(keys, "`, `") + "`"
}

// typeDoc is a type's own doc comment.
func typeDoc(t *testing.T, name string) string {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, parser.ParseComments) //nolint:staticcheck // deprecated for module-aware loading; this reads one known folder of plain files
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pkgs {
		for _, f := range p.Files {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, s := range gd.Specs {
					if ts, ok := s.(*ast.TypeSpec); ok && ts.Name.Name == name {
						doc := gd.Doc.Text()
						if ts.Doc != nil {
							doc = ts.Doc.Text()
						}
						return cleanGoDoc(doc, name, "")
					}
				}
			}
		}
	}
	return ""
}

var (
	reGoFile = regexp.MustCompile(`\s*\((?:see |in )?[\w/]+\.go\)`)
	reSpaces = regexp.MustCompile(`\s+`)
)

// cleanGoDoc turns a Go comment into text for users: one line, the Go
// field name at its start replaced by the key, no source file names.
func cleanGoDoc(s, goName, key string) string {
	s = reSpaces.ReplaceAllString(strings.TrimSpace(s), " ")
	s = reGoFile.ReplaceAllString(s, "")
	if key != "" {
		for _, sep := range []string{" ", ": ", ", "} {
			if rest, ok := strings.CutPrefix(s, goName+sep); ok {
				s = "`" + key + "`" + sep + rest
				break
			}
		}
	}
	return s
}

func cell(s string) string {
	return strings.ReplaceAll(escapeMD(s), "|", `\|`)
}

// escapeMD keeps <placeholders> outside code spans from being read as
// HTML tags.
func escapeMD(s string) string {
	parts := strings.Split(s, "`")
	for i := 0; i < len(parts); i += 2 {
		parts[i] = strings.NewReplacer("<", "&lt;", ">", "&gt;").Replace(parts[i])
	}
	return strings.Join(parts, "`")
}

const configDocIntro = `# Configuration reference

<!-- Generated from internal/config (the Config struct, default.yaml and
their comments) by: go test ./internal/config -run TestConfigDoc -update-docs
Do not edit by hand: CI fails when it is out of date. -->

Every key of ` + "`relayweft.yaml`" + ` and of a repository's ` + "`.relayweft.yaml`" + `.
` + "`rw init --print`" + ` prints the commented default file.

**Where rw reads it.** ` + "`--config <file>`" + `, else ` + "`./relayweft.yaml`" + `, else
` + "`<user config dir>/relayweft/relayweft.yaml`" + ` (` + "`%APPDATA%\\relayweft`" + ` on
Windows, ` + "`~/.config/relayweft`" + ` on Linux, ` + "`~/Library/Application Support/relayweft`" + `
on macOS), else the built-in defaults. A file only needs the keys it
changes; the rest keep their defaults. Then a repository's
` + "`.relayweft.yaml`" + ` (in the repo root or the project folder) is layered on
top, and command-line flags last:

    built-in defaults < your config < learned routes < .relayweft.yaml < flags

Maps (` + "`roles`" + `, ` + "`providers`" + `, ` + "`mcp.servers`" + `) merge per key, so a repo file with
` + "`roles: {worker: {prefer: claude}}`" + ` keeps the worker's routes.

**The "In a repo file" column.** A ` + "`.relayweft.yaml`" + ` comes from whoever
pushed to the repository, and a ` + "`./relayweft.yaml`" + ` may have come with a
clone, so they may not set everything. Your own config and ` + "`--config`" + `
always apply in full.

- **yes**: applies.
- **needs trust**: runs commands, reaches other folders or sends data
  somewhere; ignored until you review the file with ` + "`rw trust`" + ` (any
  change to the file needs a new ` + "`rw trust`" + `; ` + "`rw trust --revoke`" + ` withdraws it).
- **stricter only, until trusted**: an untrusted file may make it
  stricter (turn the sandbox on, cut its network), not weaker.
- **lower only, until trusted**: an untrusted file may lower it, not
  raise it above your own setting.
- **stricter only**: a repo file may tighten your limit, never loosen
  it, trusted or not.

Durations are written like ` + "`90s`" + `, ` + "`30m`" + `, ` + "`1h30m`" + `. In key paths, ` + "`<role>`" + ` is
one of planner, worker, worker_high, explorer, researcher, reviewer, judge;
` + "`<provider>`" + ` a provider name (codex, claude, gemini, ...); ` + "`[]`" + ` an item of a list.
`
