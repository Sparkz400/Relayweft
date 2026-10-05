package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/proc"
)

// noteUntrustedLocal says which settings of an untrusted ./relayweft.yaml
// were ignored (see config.LoadInfo).
func noteUntrustedLocal(w io.Writer, path string, ignored []string) {
	if len(ignored) > 0 {
		fmt.Fprintf(w, "note: %s sets %s, which run commands; they are ignored (yours apply) until you review and trust them: rw trust\n",
			path, strings.Join(ignored, ", "))
	}
}

// migrateSwitchyard copies sy's user folder (Switchyard, v0.2.0 and
// older) to rw's on rw's first start, and says so on w.
func migrateSwitchyard(w io.Writer) {
	msg, err := config.MigrateSwitchyard()
	if err != nil {
		// rw's folder gets created below, so there is no next try: say
		// how to finish by hand. sy's folder is unchanged.
		fmt.Fprintf(w, "warning: %v\nrw starts without your Switchyard settings and history. To bring them over, quit rw and copy the old folder's contents into rw's (switchyard.yaml becomes relayweft.yaml, logs/sy-*.log become logs/rw-*.log).\n", err)
		return
	}
	if msg != "" {
		fmt.Fprintln(w, msg)
	}
}

// noteLegacyFiles points out config files under sy's names (Switchyard,
// v0.2.0 and older), which rw does not read: a ./switchyard.yaml in the
// current folder (when no --config is given) and the repo's
// .switchyard.yaml (info.Legacy).
func noteLegacyFiles(w io.Writer, configPath string, info config.RepoInfo) {
	if configPath == "" {
		if wd, err := os.Getwd(); err == nil {
			if p := config.LegacyLocalFile(wd); p != "" {
				fmt.Fprintf(w, "note: %s is not read: rw's config file is %s. Rename it (and run `rw trust` if it runs commands).\n", p, config.FileName)
			}
		}
	}
	if info.Legacy != "" {
		fmt.Fprintf(w, "note: %s is not read: rw's repo file is %s. Rename it (and run `rw trust` if it runs commands).\n", info.Legacy, config.RepoFileName)
	}
}

// cmdTrust shows what a repo's .relayweft.yaml, and a ./relayweft.yaml in
// the project folder, would run and trusts them.
func cmdTrust(args []string) error {
	fs := flag.NewFlagSet("rw trust", flag.ExitOnError)
	dirFlag := fs.String("dir", "", "project directory (default current directory)")
	yes := fs.Bool("yes", false, "trust without asking")
	revoke := fs.Bool("revoke", false, "stop trusting the file")
	parseFlags(fs, args)
	dir, err := absDir(*dirFlag)
	if err != nil {
		return err
	}
	local := filepath.Join(dir, config.FileName)
	if _, err := os.Stat(local); err != nil {
		local = ""
	}
	p := config.FindRepoFile(dir)
	if p == "" && local == "" {
		return errors.New("no " + config.RepoFileName + " or " + config.FileName + " in this project (rw init --repo creates one)")
	}
	if local != "" {
		if err := trustLocal(local, *yes, *revoke); err != nil || p == "" {
			return err
		}
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

// writeRepoFile writes root's .relayweft.yaml with these checks and trusts
// it (you wrote it through rw).
func writeRepoFile(root string, checks []string) (string, error) {
	path := root + string(os.PathSeparator) + config.RepoFileName
	q := make([]string, len(checks))
	for i, c := range checks {
		q[i] = fmt.Sprintf("%q", c)
	}
	body := strings.Replace(repoTemplate, "VERIFY", "["+strings.Join(q, ", ")+"]", 1)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return path, err
	}
	return path, config.Trust(path)
}

const repoTemplate = `# Relayweft settings for this repository. rw layers them over each
# person's own config (flags win over both). Only set what this repo needs;
# everything else stays as each person has it.
#
# Commands below (verify, hooks) run only after each person reviews them
# with ` + "`rw trust`" + ` (and again after every change to this file).

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
	if _, err := writeRepoFile(root, checks); err != nil {
		return err
	}
	fmt.Println("wrote", path, "(trusted for you; commit it to share)")
	if len(checks) > 0 {
		fmt.Println("verify commands detected:", strings.Join(checks, ", "))
	}
	return nil
}

// trustLocal is rw trust for a ./relayweft.yaml: it is a whole config,
// and only what it sets for the settings that run commands is trusted, so
// later edits to its routes or toggles keep the trust.
func trustLocal(p string, yes, revoke bool) error {
	if revoke {
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
	fmt.Printf("%s runs these on your machine (instead of your own config's):\n\n%s\n\n", p, strings.Join(cmds, "\n"))
	if !yes {
		fmt.Print("Trust these settings? A later change to them asks again. [y/N] ")
		ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			return nil
		}
	}
	if err := config.TrustLocal(p); err != nil {
		return err
	}
	fmt.Println("trusted:", p)
	return nil
}

// gitRoot is the top level of the git repository containing dir.
func gitRoot(dir string) (string, error) {
	out, err := runGit(dir, "rev-parse", "--show-toplevel")
	return strings.TrimSpace(out), err
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append(append([]string(nil), proc.GitGuard...), args...)...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}
