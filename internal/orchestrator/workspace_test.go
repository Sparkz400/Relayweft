package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/runner"
)

// isolateUserConfig keeps `rw trust` records of a test out of the real
// user config dir.
func isolateUserConfig(t *testing.T) {
	d := t.TempDir()
	for _, k := range []string{"XDG_CONFIG_HOME", "APPDATA", "HOME"} {
		t.Setenv(k, d)
	}
}

// trustedVerify writes a trusted .relayweft.yaml with verify commands.
func trustedVerify(t *testing.T, dir string, cmds ...string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"verify": map[string]any{"commands": cmds}})
	p := filepath.Join(dir, config.RepoFileName)
	if err := os.WriteFile(p, b, 0o644); err != nil { // JSON is YAML
		t.Fatal(err)
	}
	if err := config.Trust(p); err != nil {
		t.Fatal(err)
	}
}

// twoRepoPlan plans step a in the primary and step b in repo web.
func twoRepoPlan(bRepo string) string {
	return planJSON(
		map[string]any{"id": "a", "title": "api side", "kind": "edit", "prompt": "write api.txt", "files": []string{"api.txt"}, "repo": "primary"},
		map[string]any{"id": "b", "title": "web side", "kind": "edit", "prompt": "write web.txt", "files": []string{"web.txt"}, "repo": bRepo},
	)
}

// A task that changes two repos: each writer works in a worktree of its own
// repo in parallel, both trees get the changes, neither HEAD moves, the
// final review sees both diffs, each repo's checks run in that repo, and
// one undo (from the primary) reverts both repos.
func TestMultiRepoTask(t *testing.T) {
	isolateUserConfig(t)
	api, web := gitRepo(t), gitRepo(t)
	apiHead, webHead := headOf(t, api), headOf(t, web)
	trustedVerify(t, web, fileCheck("web.txt"))

	var mu sync.Mutex
	dirs := map[string]string{}
	prompts := map[string]string{}
	allowed := map[string][]string{}
	var planPrompt, finalPrompt string
	running := 0
	parallel := false
	set := both(func(s runner.Spec) runner.Result {
		switch {
		case strings.Contains(s.Prompt, runner.MarkerPlan):
			planPrompt = s.Prompt
			return runner.Result{Final: twoRepoPlan("web")}
		case strings.Contains(s.Prompt, runner.MarkerFinalReview):
			finalPrompt = s.Prompt
			return approve()
		case strings.Contains(s.Prompt, runner.MarkerPlanReview):
			return approve()
		}
		mu.Lock()
		dirs[s.StepID], prompts[s.StepID], allowed[s.StepID] = s.Dir, s.Prompt, s.AllowedCommands
		running++
		if running > 1 {
			parallel = true
		}
		mu.Unlock()
		// Both agents are in flight at once (or the other finished already).
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			p := parallel || len(dirs) == 2
			mu.Unlock()
			if p {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		name := map[string]string{"a": "api.txt", "b": "web.txt"}[s.StepID]
		os.WriteFile(filepath.Join(s.Dir, name), []byte(s.StepID+"\n"), 0o644)
		mu.Lock()
		running--
		mu.Unlock()
		return runner.Result{Final: "wrote " + name, Files: []string{name}}
	})
	o, _ := newOrc(t, api, set, func(c *config.Config) { c.Verify.Commands = []string{fileCheck("api.txt")} })
	o.opts.Repos = []Repo{{Name: "web", Dir: web}}
	res := o.Run(context.Background(), longTask)
	if !res.OK || !strings.Contains(res.Summary, "checks pass") {
		t.Fatalf("task: %+v", res)
	}

	// The planner saw both repos.
	for _, want := range []string{"WORKSPACE", `repo "web" at ` + web, `repo "primary" at ` + api, `"repo": "<name>"`} {
		if !strings.Contains(planPrompt, want) {
			t.Errorf("planner prompt lacks %q", want)
		}
	}
	// Each writer ran in a pool worktree of its own repo, at the same time.
	if !within(dirs["a"], poolDir(api)) || !within(dirs["b"], poolDir(web)) {
		t.Errorf("dirs = %v; want a in %s, b in %s", dirs, poolDir(api), poolDir(web))
	}
	if !parallel {
		t.Error("the two repos' writers did not run in parallel")
	}
	if !strings.Contains(prompts["b"], `You work in repo "web"`) || !strings.Contains(prompts["b"], web) {
		t.Errorf("step b's prompt does not place it in repo web:\n%s", prompts["b"])
	}
	if strings.Join(allowed["b"], ",") != fileCheck("web.txt") || strings.Join(allowed["a"], ",") != fileCheck("api.txt") {
		t.Errorf("allowed commands: a=%v b=%v (each repo's own checks)", allowed["a"], allowed["b"])
	}
	// Both trees are updated, nothing crossed over, HEADs untouched.
	if read(t, filepath.Join(api, "api.txt")) != "a\n" || read(t, filepath.Join(web, "web.txt")) != "b\n" {
		t.Error("changes not applied to both trees")
	}
	for _, p := range []string{filepath.Join(api, "web.txt"), filepath.Join(web, "api.txt")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s exists: a change landed in the wrong repo", p)
		}
	}
	if headOf(t, api) != apiHead || headOf(t, web) != webHead {
		t.Error("a HEAD moved")
	}
	// The final review got both repos' diffs, labelled, and both repos' checks.
	for _, want := range []string{"=== repo primary (" + api, "=== repo web (" + web, "+a", "+b", "api.txt", "web.txt",
		"PASSED: " + fileCheck("api.txt"), "PASSED: web: " + fileCheck("web.txt")} {
		if !strings.Contains(finalPrompt, want) {
			t.Errorf("final review prompt lacks %q:\n%s", want, finalPrompt)
		}
	}
	// History records the repos.
	st, err := LoadTask(res.UndoKey)
	if err != nil || len(st.Repos) != 1 || st.Repos[0].Name != "web" || st.Plan == nil || st.Plan.Subtasks[1].Repo != "web" {
		t.Fatalf("state = %+v, %v", st, err)
	}

	// One undo from the primary reverts both repos.
	plan, err := PreviewUndo(api, res.UndoKey, false)
	if err != nil || len(plan.Others) != 1 || plan.Others[0].Repo != "web" || plan.TotalChanges() != 2 {
		t.Fatalf("preview = %+v, %v", plan, err)
	}
	if _, err := Undo(api, res.UndoKey, false, false); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(api, "api.txt"), filepath.Join(web, "web.txt")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived the undo", p)
		}
	}
	if l, _ := UndoList(web); len(l) != 1 || !l[0].Undone {
		t.Errorf("web's part not marked undone: %+v", l)
	}
	// And redo puts both back.
	if _, err := Undo(api, res.UndoKey, true, false); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(api, "api.txt")) != "a\n" || read(t, filepath.Join(web, "web.txt")) != "b\n" {
		t.Error("redo did not restore both repos")
	}
}

// Without worktrees (one thread) each writer works in its own repo's main
// tree, and the fix round runs once per changed repo, inside that repo.
func TestMultiRepoMainTreesAndFixPerRepo(t *testing.T) {
	isolateUserConfig(t)
	api, web := gitRepo(t), gitRepo(t)
	var mu sync.Mutex
	dirs := map[string]string{}
	reviews := 0
	set := both(func(s runner.Spec) runner.Result {
		switch {
		case strings.Contains(s.Prompt, runner.MarkerPlan):
			return runner.Result{Final: twoRepoPlan("web")}
		case strings.Contains(s.Prompt, runner.MarkerFinalReview):
			mu.Lock()
			defer mu.Unlock()
			if reviews++; reviews == 1 {
				return runner.Result{Final: `{"approve": false, "advice": "add a newline"}`}
			}
			return approve()
		case strings.Contains(s.Prompt, runner.MarkerPlanReview):
			return approve()
		}
		mu.Lock()
		dirs[s.StepID] = s.Dir
		mu.Unlock()
		os.WriteFile(filepath.Join(s.Dir, s.StepID+".txt"), []byte("x\n"), 0o644)
		return runner.Result{Final: "ok"}
	})
	o, _ := newOrc(t, api, set, func(c *config.Config) { c.Orchestrator.MaxThreads = 1 })
	o.opts.Repos = []Repo{{Name: "web", Dir: web}}
	res := o.Run(context.Background(), longTask)
	if !res.OK {
		t.Fatalf("task: %+v", res)
	}
	if !samePath(dirs["a"], api) || !samePath(dirs["b"], web) {
		t.Errorf("dirs = %v", dirs)
	}
	if !samePath(dirs["fix-1"], api) || !samePath(dirs["fix-1-web"], web) {
		t.Errorf("fix rounds ran in %q and %q; want one per changed repo", dirs["fix-1"], dirs["fix-1-web"])
	}
	if _, err := os.Stat(filepath.Join(web, "fix-1-web.txt")); err != nil {
		t.Error("web's fix agent did not work in web")
	}
}

// Resuming a multi-repo task works in the repos it started with and skips
// finished steps.
func TestMultiRepoResume(t *testing.T) {
	isolateUserConfig(t)
	api, web := gitRepo(t), gitRepo(t)
	var mu sync.Mutex
	var ran []string
	var bDir string
	set := both(func(s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, "[RW:") && !strings.Contains(s.Prompt, runner.MarkerStep) {
			return approve()
		}
		mu.Lock()
		ran = append(ran, s.StepID)
		if s.StepID == "b" {
			bDir = s.Dir
		}
		mu.Unlock()
		os.WriteFile(filepath.Join(s.Dir, "web.txt"), []byte("b\n"), 0o644)
		return runner.Result{Final: "done " + s.StepID}
	})
	o, _ := newOrc(t, api, set, nil) // this rw has no --repo: the state's repos count
	st := &TaskState{ID: "multi-resume", Task: longTask, Dir: api, Status: "running", Created: time.Now(),
		Repos: []Repo{{Name: "web", Dir: web}},
		Plan: &Plan{Summary: "p", Repos: []string{PrimaryRepo, "web"}, Subtasks: []Subtask{
			{ID: "a", Title: "a", Kind: router.KindEdit, Prompt: "a"},
			{ID: "b", Title: "b", Kind: router.KindEdit, Prompt: "b", DependsOn: []string{"a"}, Repo: "web"},
		}},
		Results: map[string]StepState{"a": {OK: true, Final: "a was done"}}}
	st.save()
	res := o.RunWith(context.Background(), "", TaskOptions{Resume: st})
	if !res.OK || strings.Join(ran, ",") != "b" {
		t.Fatalf("resume: %+v ran=%v", res, ran)
	}
	if !within(bDir, web) && !within(bDir, poolDir(web)) {
		t.Errorf("b ran in %s, want repo web", bDir)
	}
	if read(t, filepath.Join(web, "web.txt")) != "b\n" {
		t.Error("b's change did not land in web")
	}
	if _, err := os.Stat(filepath.Join(api, "web.txt")); err == nil {
		t.Error("b's change landed in the primary")
	}
}

func TestMultiRepoPlanRejectsUnknownRepo(t *testing.T) {
	p := Plan{Repos: []string{PrimaryRepo, "web"}, Subtasks: []Subtask{{ID: "a", Prompt: "x", Repo: "Web"}, {ID: "b", Prompt: "y", Repo: "primary"}}}
	got, err := NormalizePlan(p)
	if err != nil || got.Subtasks[0].Repo != "web" || got.Subtasks[1].Repo != "" {
		t.Fatalf("valid repos: %+v %v", got.Subtasks, err)
	}
	p.Subtasks[0].Repo = "nope"
	var re *RepoError
	if _, err := NormalizePlan(p); !errors.As(err, &re) || re.Repo != "nope" {
		t.Fatalf("unknown repo accepted: %v", err)
	}
	// A single-repo plan ignores repo names (the planner may make them up).
	got, err = ParsePlan(twoRepoPlan("nope"))
	if err != nil || got.Subtasks[1].Repo != "" || got.Repos != nil {
		t.Fatalf("single-repo parse: %+v %v", got, err)
	}

	// The orchestrator asks the planner again, naming the mistake.
	api, web := gitRepo(t), gitRepo(t)
	var mu sync.Mutex
	var plans []string
	set := both(func(s runner.Spec) runner.Result {
		switch {
		case strings.Contains(s.Prompt, runner.MarkerPlan):
			mu.Lock()
			defer mu.Unlock()
			plans = append(plans, s.Prompt)
			if len(plans) == 1 {
				return runner.Result{Final: twoRepoPlan("frontend")}
			}
			return runner.Result{Final: twoRepoPlan("web")}
		case strings.Contains(s.Prompt, "[RW:") && !strings.Contains(s.Prompt, runner.MarkerStep):
			return approve()
		}
		return runner.Result{Final: "ok"}
	})
	o, _ := newOrc(t, api, set, nil)
	o.opts.Repos = []Repo{{Name: "web", Dir: web}}
	if res := o.Run(context.Background(), longTask); !res.OK {
		t.Fatalf("%+v", res)
	}
	if len(plans) != 2 || !strings.Contains(plans[1], `unknown repo "frontend"`) {
		t.Fatalf("planner calls = %d; second prompt:\n%s", len(plans), plans[len(plans)-1])
	}

	// A person who moves a step to an unknown repo stops the task.
	o2, _ := newOrc(t, api, set, func(c *config.Config) { c.Orchestrator.ApprovePlan = true })
	o2.opts.Repos = []Repo{{Name: "web", Dir: web}}
	var shown Plan
	withApprover(o2, &fakeApprover{plan: func(p Plan) (Plan, bool) {
		shown = p
		p.Subtasks[0].Repo = "ghost"
		return p, true
	}})
	mu.Lock()
	plans = nil
	mu.Unlock()
	res := o2.Run(context.Background(), longTask)
	if res.OK || !strings.Contains(res.Summary, `unknown repo "ghost"`) {
		t.Fatalf("%+v", res)
	}
	if strings.Join(shown.Repos, ",") != "primary,web" || shown.Subtasks[1].Repo != "web" {
		t.Errorf("approver saw %+v", shown)
	}
}

func TestResolveWorkspace(t *testing.T) {
	api, web := gitRepo(t), gitRepo(t)
	plain := t.TempDir()
	rel, _ := filepath.Rel(api, web)
	cfgWeb := []WorkspaceEntry{{Name: "web", Path: rel}}

	repos, skipped, err := ResolveWorkspace(api, cfgWeb, nil)
	if err != nil || len(skipped) != 0 || len(repos) != 1 || repos[0].Name != "web" || !samePath(repos[0].Dir, web) {
		t.Fatalf("config path relative to the project: %+v %v %v", repos, skipped, err)
	}
	// A flag replaces the config entry of the same name.
	if _, _, err := ResolveWorkspace(api, cfgWeb, []string{"web=" + plain}); err == nil || !strings.Contains(err.Error(), "is not a git repository") {
		t.Fatalf("not a git repo: %v", err)
	}
	for flag, want := range map[string]string{
		"web":                                    "want name=path",
		"=x":                                     "want name=path",
		"Web=" + web:                             "lowercase",
		"primary=" + web:                         "reserved",
		"web=" + filepath.Join(plain, "missing"): "is not a directory",
		"self=" + api:                            "same git repository",
	} {
		if _, _, err := ResolveWorkspace(api, nil, []string{flag}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("--repo %s: %v, want %q", flag, err, want)
		}
	}
	if _, _, err := ResolveWorkspace(plain, nil, []string{"web=" + web}); err == nil || !strings.Contains(err.Error(), "project folder") {
		t.Errorf("primary not a git repo: %v", err)
	}
	if repos, _, err := ResolveWorkspace(plain, nil, nil); err != nil || repos != nil {
		t.Errorf("no repos: %v %v", repos, err)
	}
	if got := WorkspaceLabel(api, []Repo{{Name: "web"}, {Name: "docs"}}); got != filepath.Base(api)+" + web, docs" {
		t.Errorf("label %q", got)
	}
}

// A committed workspace that does not fit this machine (the teammate has
// no ../web, or it is not a repo, or the name is bad) is skipped with a
// reason instead of stopping rw; the good entries are kept. --repo flags
// still fail hard.
func TestResolveWorkspaceSkipsBadConfigRepos(t *testing.T) {
	api, web := gitRepo(t), gitRepo(t)
	plain := t.TempDir()
	repos, skipped, err := ResolveWorkspace(api, []WorkspaceEntry{
		{Name: "web", Path: web},
		{Name: "gone", Path: filepath.Join(plain, "missing"), Origin: "/p/.relayweft.yaml: workspace.repos.gone"},
		{Name: "plain", Path: plain},
		{Name: "Bad", Path: web},
		{Name: "self", Path: api},
	}, nil)
	if err != nil {
		t.Fatalf("a bad config repo stopped startup: %v", err)
	}
	if len(repos) != 1 || repos[0].Name != "web" {
		t.Errorf("repos = %+v", repos)
	}
	joined := strings.Join(skipped, "\n")
	for _, want := range []string{"/p/.relayweft.yaml: workspace.repos.gone", "is not a directory", "workspace.repos.plain", "not a git repository", "lowercase", "same git repository", "skipped"} {
		if !strings.Contains(joined, want) {
			t.Errorf("skipped lacks %q:\n%s", want, joined)
		}
	}
	if len(skipped) != 4 {
		t.Errorf("%d skipped, want 4:\n%s", len(skipped), joined)
	}
	// Without a git project folder every config repo is skipped, not fatal.
	if repos, skipped, err := ResolveWorkspace(plain, []WorkspaceEntry{{Name: "web", Path: web}}, nil); err != nil || repos != nil || len(skipped) != 1 {
		t.Errorf("plain project: %+v %v %v", repos, skipped, err)
	}
	// A flag that clashes with a config entry wins; the entry is skipped.
	repos, skipped, err = ResolveWorkspace(api, []WorkspaceEntry{{Name: "other", Path: web}}, []string{"web=" + web})
	if err != nil || len(repos) != 1 || repos[0].Name != "web" || len(skipped) != 1 {
		t.Errorf("flag vs config: %+v %v %v", repos, skipped, err)
	}
}

// A relative config path is taken from the entry's Base (the folder of the
// .relayweft.yaml that set it), not from the project folder rw runs in.
func TestResolveWorkspaceRelativeToConfigFile(t *testing.T) {
	root := t.TempDir()
	api := filepath.Join(root, "api")
	sub := filepath.Join(api, "sub")
	web := filepath.Join(root, "web")
	for _, d := range []string{api, web} {
		os.MkdirAll(d, 0o755)
		gitIn(t, d, "init", "-q")
	}
	os.MkdirAll(sub, 0o755)
	// rw runs in api/sub; the repo file in api says ../web.
	repos, skipped, err := ResolveWorkspace(sub, []WorkspaceEntry{{Name: "web", Path: "../web", Base: api}}, nil)
	if err != nil || len(skipped) != 0 || len(repos) != 1 || !samePath(repos[0].Dir, web) {
		t.Fatalf("relative to the file's folder: %+v %v %v", repos, skipped, err)
	}
	// Without a Base it is the project folder (the user's config).
	if repos, _, _ := ResolveWorkspace(api, []WorkspaceEntry{{Name: "web", Path: "../web"}}, nil); len(repos) != 1 || !samePath(repos[0].Dir, web) {
		t.Errorf("relative to the project: %+v", repos)
	}
}

// Nested and aliased repos are refused: an extra repo that is the parent
// (or a child) of the primary or of another listed repo, a symlink to the
// primary, and a linked worktree of the primary (same git common dir).
func TestResolveWorkspaceRejectsNestedAndAliases(t *testing.T) {
	proj := gitRepo(t)
	link := filepath.Join(t.TempDir(), "lnk")
	if err := os.Symlink(proj, link); err == nil {
		if _, _, err := ResolveWorkspace(proj, nil, []string{"same=" + link}); err == nil || !strings.Contains(err.Error(), "same git repository") {
			t.Errorf("symlink to the primary: %v", err)
		}
	}
	wt := filepath.Join(t.TempDir(), "wt")
	gitIn(t, proj, "worktree", "add", "-q", "--detach", wt)
	if r, _, err := ResolveWorkspace(proj, nil, []string{"wt=" + wt}); err == nil || !strings.Contains(err.Error(), "worktree of the same git repository") {
		t.Errorf("linked worktree of the primary: %+v %v", r, err)
	}
	parent := t.TempDir()
	gitIn(t, parent, "init", "-q")
	nested := filepath.Join(parent, "child")
	os.MkdirAll(nested, 0o755)
	gitIn(t, nested, "init", "-q")
	if r, _, err := ResolveWorkspace(nested, nil, []string{"mono=" + parent}); err == nil || !strings.Contains(err.Error(), "contains repo") {
		t.Errorf("extra is the parent of the primary: %+v %v", r, err)
	}
	if r, _, err := ResolveWorkspace(parent, nil, []string{"child=" + nested}); err == nil || !strings.Contains(err.Error(), "is inside repo") {
		t.Errorf("extra is inside the primary: %+v %v", r, err)
	}
	// Between two extra repos too (config entries: skipped, not fatal).
	other := gitRepo(t)
	r, skipped, err := ResolveWorkspace(other, []WorkspaceEntry{{Name: "a", Path: parent}, {Name: "b", Path: nested}}, nil)
	if err != nil || len(r) != 1 || r[0].Name != "a" || len(skipped) != 1 || !strings.Contains(skipped[0], "is inside repo \"a\"") {
		t.Errorf("nested extras: %+v %v %v", r, skipped, err)
	}
}

// A repo that stops being a git repository refuses the task up front.
func TestMultiRepoMissingRepoRefused(t *testing.T) {
	api := gitRepo(t)
	gone := t.TempDir()
	set := both(func(s runner.Spec) runner.Result { return approve() })
	o, _ := newOrc(t, api, set, nil)
	o.opts.Repos = []Repo{{Name: "web", Dir: gone}}
	res := o.Run(context.Background(), longTask)
	if res.OK || !strings.Contains(res.Summary, "not a git repository") {
		t.Fatalf("%+v", res)
	}
}
