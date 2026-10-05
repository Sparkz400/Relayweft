package config

import (
	"regexp"
	"strings"
)

var (
	reProviderHead = regexp.MustCompile(`^  ([a-z][a-z0-9_-]*):`)
	reDisabledLine = regexp.MustCompile(`^(    disabled:\s*)(true|false)(.*)$`)
)

// WithEnabled turns the named providers on (true) or off (false) in the
// commented default config text, for `sy setup`. Comments and the other
// providers stay as they are.
func WithEnabled(yamlText []byte, on map[string]bool) []byte {
	lines := strings.Split(string(yamlText), "\n")
	inProviders, current := false, ""
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "providers:"):
			inProviders = true
			continue
		case l != "" && l[0] != ' ' && l[0] != '#' && l[0] != '\r':
			inProviders = false // the next top-level key
		}
		if !inProviders {
			continue
		}
		if m := reProviderHead.FindStringSubmatch(l); m != nil {
			current = m[1]
			continue
		}
		enable, ok := on[current]
		if !ok {
			continue
		}
		if m := reDisabledLine.FindStringSubmatch(l); m != nil {
			v := "true"
			if enable {
				v = "false"
			}
			lines[i] = m[1] + v + m[3]
		}
	}
	return []byte(strings.Join(lines, "\n"))
}
