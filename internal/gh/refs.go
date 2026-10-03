package gh

import (
	"regexp"
	"strconv"
)

// reCloses matches GitHub's closing keywords followed by a same-repository
// issue number: "Closes #12", "fixes #3", "Resolved #7".
var reCloses = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s+#(\d+)\b`)

// ClosedBy returns the issue numbers the pull requests' bodies close.
func ClosedBy(pulls []Pull) map[int]bool {
	out := map[int]bool{}
	for _, p := range pulls {
		for _, m := range reCloses.FindAllStringSubmatch(p.Body, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil {
				out[n] = true
			}
		}
	}
	return out
}
