package affected

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// jestMapper is one moduleNameMapper entry as `jest --showConfig` prints
// it: [regex, path or paths], with <rootDir> already replaced.
type jestMapper struct {
	re *regexp.Regexp
	to []string
}

// readMappers reads the project's moduleNameMapper, or says why it cannot.
func (p *jestProject) readMappers() string {
	for _, raw := range p.ModuleNameMapper {
		var pair []json.RawMessage
		var src string
		if json.Unmarshal(raw, &pair) != nil || len(pair) != 2 || json.Unmarshal(pair[0], &src) != nil {
			return "rw cannot read jest's moduleNameMapper"
		}
		var to []string
		var one string
		if json.Unmarshal(pair[1], &one) == nil {
			to = []string{one}
		} else if json.Unmarshal(pair[1], &to) != nil || len(to) == 0 {
			return "rw cannot read jest's moduleNameMapper"
		}
		re, err := regexp.Compile(src)
		if err != nil {
			// JavaScript-only syntax (lookbehind, backreferences).
			return fmt.Sprintf("rw cannot read jest's moduleNameMapper pattern %q", clipStr(src, 60))
		}
		p.mappers = append(p.mappers, jestMapper{re: re, to: to})
	}
	return ""
}

// reJSGroupRef is $1 in a moduleNameMapper path.
var reJSGroupRef = regexp.MustCompile(`\$\d+`)

// resolve lists the dir-relative files an import of spec from f can load
// that matter to jest's index, or says why rw cannot tell. A bare import
// of a package from outside the check folder matters to no index here.
func (p *jestProject) resolve(t *jsTree, f, spec string) ([]string, string) {
	// jest maps every module name first, relative ones too; the first
	// pattern that matches wins.
	for _, m := range p.mappers {
		sub := m.re.FindStringSubmatch(spec)
		if sub == nil {
			continue
		}
		// jest loads the mapped path (the first of several that resolves;
		// rw checks them all), with $1 ... taken from the match.
		var out []string
		for _, to := range m.to {
			mapped := reJSGroupRef.ReplaceAllStringFunc(to, func(g string) string {
				if n, err := strconv.Atoi(g[1:]); err == nil && n < len(sub) {
					return sub[n]
				}
				return "undefined"
			})
			ts, why := p.mappedTargets(t, mapped)
			if why != "" {
				return nil, why
			}
			out = append(out, ts...)
		}
		return out, ""
	}
	if strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") {
		tg, ok := t.resolveRel(f, spec)
		if !ok {
			return nil, "outside the check folder"
		}
		return tg, ""
	}
	if pk, ok := t.byName[jsPkgName(spec)]; ok {
		// A workspace package: jest resolves it to its real folder.
		return t.under(pk.Dir), ""
	}
	return nil, ""
}

// mappedTargets resolves a moduleNameMapper result: an absolute path (in
// jest's own paths, the container's in the sandbox) or a module name.
func (p *jestProject) mappedTargets(t *jsTree, mapped string) ([]string, string) {
	m := strings.ReplaceAll(mapped, `\`, "/")
	abs := strings.HasPrefix(m, "/") || len(m) > 2 && m[1] == ':' && m[2] == '/'
	switch {
	case abs:
		cwd := strings.TrimRight(strings.ReplaceAll(p.Cwd, `\`, "/"), "/") + "/"
		if hasPrefixFold(m, cwd) {
			rel := strings.TrimSuffix(m[len(cwd):], "/")
			if rel == "" {
				rel = "."
			}
			return t.existing(rel), ""
		}
		if strings.Contains(m, "/node_modules/") {
			return nil, ""
		}
		return nil, "which jest's moduleNameMapper maps to " + clipStr(mapped, 80) + ", outside the check folder"
	case strings.HasPrefix(m, "."):
		return nil, "which jest's moduleNameMapper maps to the relative path " + clipStr(mapped, 80)
	}
	if pk, ok := t.byName[jsPkgName(m)]; ok {
		return t.under(pk.Dir), ""
	}
	return nil, ""
}
