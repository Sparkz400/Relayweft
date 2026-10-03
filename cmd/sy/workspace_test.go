package main

import (
	"flag"
	"github.com/sparkz400/switchyard/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/router"
)

// --repo is accepted by every task command (sy, sy run, sy web share the
// common flags) and resolved by setup; the config's workspace.repos is
// relative to the project folder and a flag of the same name replaces it.
func TestRepoFlag(t *testing.T) {
	isolate(t)
	api, web, docs := gitInit(t), gitInit(t), gitInit(t)
	chdir(t, api)
	parse := func(args ...string) (*common, error) {
		fs := flag.NewFlagSet("sy run", flag.ContinueOnError)
		var c common
		c.register(fs)
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		_, _, err := c.setup()
		return &c, err
	}
	c, err := parse("--repo", "web="+web, "--repo", "docs="+docs)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.workspace) != 2 || c.workspace[0].Name != "docs" || c.workspace[1].Name != "web" || c.workspace[1].Dir != web {
		t.Fatalf("workspace = %+v", c.workspace)
	}
	if c, err := parse(); err != nil || c.workspace != nil {
		t.Fatalf("no --repo: %+v %v", c, err)
	}

	plain := t.TempDir()
	if _, err := parse("--repo", "web="+plain); err == nil || !strings.Contains(err.Error(), "is not a git repository") {
		t.Errorf("non-git repo: %v", err)
	}
	if _, err := parse("--repo", "web"); err == nil || !strings.Contains(err.Error(), "name=path") {
		t.Errorf("missing path: %v", err)
	}

	// From .switchyard.yaml: folders agents write to, so only once trusted.
	rel, _ := filepath.Rel(api, web)
	repoFile := filepath.Join(api, ".switchyard.yaml")
	os.WriteFile(repoFile, []byte("workspace:\n  repos:\n    web: "+filepath.ToSlash(rel)+"\n"), 0o644)
	if c, err := parse(); err != nil || len(c.workspace) != 0 {
		t.Fatalf("untrusted workspace applied: %+v %v", c, err)
	}
	if err := config.Trust(repoFile); err != nil {
		t.Fatal(err)
	}
	c, err = parse()
	if err != nil || len(c.workspace) != 1 || c.workspace[0].Name != "web" {
		t.Fatalf("config workspace: %+v %v", c, err)
	}
	c, err = parse("--repo", "web="+docs)
	if err != nil || len(c.workspace) != 1 || c.workspace[0].Dir != docs {
		t.Fatalf("flag over config: %+v %v", c, err)
	}
}

// A committed .switchyard.yaml whose workspace does not fit this machine
// (no ../web here) lets sy start, with a warning; its relative paths are
// taken from the file's folder, also when sy runs in a subfolder.
func TestRepoFileWorkspaceMissingRepoAndSubfolder(t *testing.T) {
	isolate(t)
	api, web := gitInit(t), gitInit(t)
	sub := filepath.Join(api, "sub")
	os.MkdirAll(sub, 0o755)
	rel, _ := filepath.Rel(api, web)
	repoFile := filepath.Join(api, ".switchyard.yaml")
	os.WriteFile(repoFile, []byte("workspace:\n  repos:\n    web: "+filepath.ToSlash(rel)+"\n    gone: ../definitely-not-here\n"), 0o644)
	if err := config.Trust(repoFile); err != nil {
		t.Fatal(err)
	}
	// The warning may name the file through its resolved folder (macOS: /private/var).
	realRepoFile, _ := filepath.EvalSymlinks(repoFile)
	parse := func(args ...string) (*common, error) {
		fs := flag.NewFlagSet("sy run", flag.ContinueOnError)
		var c common
		c.register(fs)
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		_, _, err := c.setup()
		return &c, err
	}
	for _, d := range []string{api, sub} {
		chdir(t, d)
		c, err := parse()
		if err != nil {
			t.Fatalf("from %s: a missing workspace repo stopped sy: %v", d, err)
		}
		if len(c.workspace) != 1 || c.workspace[0].Name != "web" || !sameDir(c.workspace[0].Dir, web) {
			t.Errorf("from %s: workspace = %+v", d, c.workspace)
		}
		if len(c.workspaceSkipped) != 1 || !strings.Contains(c.workspaceSkipped[0], "workspace.repos.gone") || !(strings.Contains(c.workspaceSkipped[0], repoFile) || strings.Contains(c.workspaceSkipped[0], realRepoFile)) {
			t.Errorf("from %s: skipped = %q", d, c.workspaceSkipped)
		}
	}
	// --dir works the same way.
	chdir(t, t.TempDir())
	if c, err := parse("--dir", sub); err != nil || len(c.workspace) != 1 || !sameDir(c.workspace[0].Dir, web) {
		t.Errorf("--dir sub: %+v %v", c, err)
	}
	// A bad --repo flag still fails.
	if _, err := parse("--dir", api, "--repo", "x=../definitely-not-here"); err == nil {
		t.Error("a missing --repo folder was accepted")
	}
}

func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && strings.EqualFold(ra, rb)
}

func planWithRepos() orchestrator.Plan {
	return orchestrator.Plan{Summary: "s", Repos: []string{orchestrator.PrimaryRepo, "web"},
		Subtasks: []orchestrator.Subtask{{ID: "a", Title: "a", Kind: router.KindEdit, Prompt: "a"}}}
}

func TestTermApproverMovesStepToRepo(t *testing.T) {
	in := strings.NewReader("o 1 web\ny\n")
	var out strings.Builder
	a := newTermApprover(in, &out)
	p := planWithRepos()
	got, ok := a.ApprovePlan(t.Context(), "task", p)
	if !ok || got.Subtasks[0].Repo != "web" {
		t.Fatalf("plan %+v ok=%v\n%s", got.Subtasks, ok, out.String())
	}
	if !strings.Contains(out.String(), " in primary") || !strings.Contains(out.String(), "o N repo") {
		t.Errorf("repo not shown:\n%s", out.String())
	}
}
