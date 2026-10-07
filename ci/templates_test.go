package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func bashForTest(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		p := `C:\Program Files\Git\bin\bash.exe`
		if _, err := os.Stat(p); err == nil {
			return p
		}
		t.Skip("Git Bash is needed for Linux CI script checks")
	}
	p, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is needed")
	}
	return p
}

func TestPipelineTemplates(t *testing.T) {
	for _, name := range []string{"azure-pipelines.yml", "bitbucket-pipelines.yml"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := yaml.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "bash .relayweft-ci/forge-job.sh") || !strings.Contains(string(b), "relayweft-report") {
			t.Fatalf("%s misses runner/artifacts", name)
		}
		if name == "azure-pipelines.yml" {
			if v["trigger"] != "none" || v["pr"] != "none" {
				t.Fatal("Azure must not run on untrusted pushes/PRs")
			}
			if !strings.Contains(string(b), "persistCredentials: false") || !strings.Contains(string(b), "condition: always()") {
				t.Fatal("checkout/artifact policy missing")
			}
		} else {
			pipelines := v["pipelines"].(map[string]any)
			if len(pipelines) != 1 || pipelines["custom"] == nil {
				t.Fatal("Bitbucket must be manually triggered")
			}
		}
	}
	cmd := exec.Command(bashForTest(t), "-n", "forge-job.sh")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("shell syntax: %v: %s", err, b)
	}
}

func TestForgeJob(t *testing.T) {
	bash := bashForTest(t)
	for _, forge := range []string{"azure", "bitbucket"} {
		for _, exit := range []string{"0", "7"} {
			t.Run(forge+exit, func(t *testing.T) {
				dir := t.TempDir()
				for _, args := range [][]string{{"init", "-q", dir}, {"-C", dir, "remote", "add", "origin", "https://example.invalid/original"}} {
					if b, err := exec.Command("git", args...).CombinedOutput(); err != nil {
						t.Fatalf("git %v: %s", err, b)
					}
				}
				// No network or agent: this double exercises the actual job shell, its
				// argument boundaries, credential helper, report files and exit handling.
				fake := `#!/usr/bin/env bash
set -eu
case "$1" in
version) echo 'fixture rw' ;;
history)
 if [ -f ran ]; then echo '[{"id":"fixture-task"}]'; else echo '[]'; fi ;;
run)
 printf '%s\n' "$@" > args
 node -e 'const u=new URL(process.env.RW_CI_ORIGIN); console.log("protocol=https\nhost="+u.host+"\npath="+u.pathname.slice(1)+"\n")' | git -c credential.interactive=false credential fill > credential
 node -e 'const fs=require("fs"); if(!fs.readFileSync("credential","utf8").includes("password=fixture-secret")) process.exit(1)'
 rm credential
 touch ran
 echo 'task output'
 exit "${FIXTURE_EXIT}" ;;
report)
 while [ "$1" != --out ]; do shift; done
 printf 'report' > "$2" ;;
*) exit 80 ;;
esac
`
				p := filepath.Join(dir, "fake-rw")
				if err := os.WriteFile(p, []byte(fake), 0700); err != nil {
					t.Fatal(err)
				}
				script, err := filepath.Abs("forge-job.sh")
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(bash, filepath.ToSlash(script))
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "RW_FORGE="+forge, "AZURE_DEVOPS_TOKEN=fixture-secret", "BUILD_REPOSITORY_URI=https://dev.azure.com/org/project/_git/repo", "BITBUCKET_TOKEN=fixture@example.invalid:fixture-secret", "BITBUCKET_REPO_FULL_NAME=workspace/repo", "RW_BINARY="+filepath.ToSlash(p), "RW_SKIP_AGENT_INSTALL=true", "RW_PROVIDER=claude", "ANTHROPIC_API_KEY=fixture-key", "RW_ISSUE=42", "RW_BUDGET_TASK_USD=2.5", "FIXTURE_EXIT="+exit, "GIT_TERMINAL_PROMPT=0")
				b, err := cmd.CombinedOutput()
				if exit == "0" && err != nil {
					t.Fatalf("job: %v: %s", err, b)
				}
				if exit != "0" {
					ee, ok := err.(*exec.ExitError)
					if !ok || ee.ExitCode() != 7 {
						t.Fatalf("lost failure code: %v: %s", err, b)
					}
				}
				if strings.Contains(string(b), "fixture-secret") {
					t.Fatal("credential leaked in output")
				}
				for _, name := range []string{"rw-run.log", "fixture-task.html", "fixture-task.md"} {
					if _, err := os.Stat(filepath.Join(dir, "relayweft-report", name)); err != nil {
						t.Fatal(err)
					}
				}
				cfg, _ := os.ReadFile(filepath.Join(dir, ".git", "config"))
				if strings.Contains(string(cfg), "fixture-secret") || strings.Contains(string(cfg), "credential.cjs") {
					t.Fatal("credential/helper left in git config")
				}
				args, _ := os.ReadFile(filepath.Join(dir, "args"))
				if !strings.Contains(string(args), "--budget-task-usd\n2.5\n") || !strings.Contains(string(args), "--issue\n42\n") {
					t.Fatalf("arguments: %s", args)
				}
			})
		}
	}
}
