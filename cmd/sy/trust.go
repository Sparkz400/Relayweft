package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/sparkz400/switchyard/internal/config"
)

// cmdTrust shows what a repo's .switchyard.yaml would run and trusts it.
func cmdTrust(args []string) error {
	fs := flag.NewFlagSet("sy trust", flag.ExitOnError)
	dirFlag := fs.String("dir", "", "project directory (default current directory)")
	yes := fs.Bool("yes", false, "trust without asking")
	revoke := fs.Bool("revoke", false, "stop trusting the file")
	fs.Parse(args)
	dir, err := absDir(*dirFlag)
	if err != nil {
		return err
	}
	p := config.FindRepoFile(dir)
	if p == "" {
		return errors.New("no " + config.RepoFileName + " in this project (sy init --repo creates one)")
	}
	if *revoke {
		if err := config.Untrust(p); err != nil {
			return err
		}
		fmt.Println("no longer trusted:", p)
		return nil
	}
	cmds, err := config.CommandSettings(p)
	if err != nil {
		return err
	}
	if len(cmds) == 0 {
		fmt.Println(p, "runs no commands; it applies without trust.")
		return nil
	}
	fmt.Printf("%s runs these on your machine:\n\n%s\n\n", p, strings.Join(cmds, "\n"))
	if !*yes {
		fmt.Print("Trust this exact content? Any later change asks again. [y/N] ")
		ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			return nil
		}
	}
	if err := config.Trust(p); err != nil {
		return err
	}
	fmt.Println("trusted:", p)
	return nil
}

const repoTemplate = `# Switchyard settings for this repository. sy layers them over each
# person's own config (flags win over both). Only set what this repo needs;
# everything else stays as each person has it.
#
# Commands below (verify, hooks) run only after each person reviews them
# with ` + "`sy trust`" + ` (and again after every change to this file).

verify:
  commands: VERIFY

# hooks:
#   after_merge: ["gofmt -w ."]
#   after_task: []

# roles:
#   worker: {prefer: codex}
#   reviewer: {claude: {model: opus, effort: high}}

# orchestrator:
#   approve_plan: true
#   review_changes: false
`

func initRepo(force bool) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	root := wd
	if r, err := gitRoot(wd); err == nil {
		root = r
	}
	path := root + string(os.PathSeparator) + config.RepoFileName
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s exists (use --force to overwrite)", path)
	}
	checks := config.DetectVerify(root)
	q := make([]string, len(checks))
	for i, c := range checks {
		q[i] = fmt.Sprintf("%q", c)
	}
	body := strings.Replace(repoTemplate, "VERIFY", "["+strings.Join(q, ", ")+"]", 1)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return err
	}
	if err := config.Trust(path); err != nil {
		return err
	}
	fmt.Println("wrote", path, "(trusted for you; commit it to share)")
	if len(checks) > 0 {
		fmt.Println("verify commands detected:", strings.Join(checks, ", "))
	}
	return nil
}

// gitRoot is the top level of the git repository containing dir.
func gitRoot(dir string) (string, error) {
	out, err := runGit(dir, "rev-parse", "--show-toplevel")
	return strings.TrimSpace(out), err
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}
