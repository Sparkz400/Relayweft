package repodocs

import "testing"

// Azure DevOps reads the pull request template from .azuredevops/ and
// .vsts/ too.
func TestPRTemplateAzureDevOps(t *testing.T) {
	for _, p := range []string{".azuredevops/pull_request_template.md", ".vsts/pull_request_template.md"} {
		root := t.TempDir()
		write(t, root, p, "## Summary\n")
		if rel, text := PRTemplate(root); rel != p || text != "## Summary\n" {
			t.Errorf("%s: found %q %q", p, rel, text)
		}
	}
}
