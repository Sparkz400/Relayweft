package main

import (
	"bufio"
	"embed"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/orchestrator"
)

// Shell completion. The scripts (completions/) are thin: on every Tab
// they run the hidden `rw __complete <words...>`, which answers from the
// commands table (commands.go), so subcommands and flags cannot drift
// from what rw accepts. Dynamic values (provider names, models, task ids)
// come from the config and the task state files only: no agent, no git,
// no network, and any failure prints nothing.
//
// The answer is one directive line, then one candidate per line as
// value<TAB>description. Directives:
//
//	default  the candidates
//	nospace  the candidates, without a space after them (role=, codex:)
//	files    let the shell complete file names
//	dirs     let the shell complete folder names

// completeCmd is the hidden subcommand the scripts call.
const completeCmd = "__complete"

// completeWordsEnv carries the words instead of arguments: PowerShell
// 5.1 drops empty arguments to native programs and mangles embedded
// quotes, so its script passes them here, each ended by \x1f.
const completeWordsEnv = "RW_COMPLETE_WORDS"

const completeWordsSep = "\x1f"

//go:embed completions/rw.bash completions/rw.zsh completions/rw.fish completions/rw.ps1
var completionFS embed.FS

// completionShells maps a shell name to its script.
var completionShells = map[string]string{
	"bash":       "completions/rw.bash",
	"zsh":        "completions/rw.zsh",
	"fish":       "completions/rw.fish",
	"powershell": "completions/rw.ps1",
	"pwsh":       "completions/rw.ps1",
}

const completionHelp = `Usage: rw completion bash|zsh|fish|powershell

Prints a script that makes Tab complete rw's subcommands, flags, provider
names, models and task ids. Install it once:

  bash        echo 'source <(rw completion bash)' >> ~/.bashrc
              (or: rw completion bash > ~/.local/share/bash-completion/completions/rw)
  zsh         rw completion zsh > "${fpath[1]}/_rw"    then start a new shell
              (or, after compinit in ~/.zshrc: source <(rw completion zsh))
  fish        rw completion fish > ~/.config/fish/completions/rw.fish
  PowerShell  add this line to your profile (notepad $PROFILE):
              rw completion powershell | Out-String | Invoke-Expression

The Linux packages and Homebrew install the bash, zsh and fish scripts
for you. Windows PowerShell 5.1 and PowerShell 7 both work.
`

func cmdCompletion(args []string) error {
	fs := flag.NewFlagSet("rw completion", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, completionHelp) }
	parseFlags(fs, args)
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("name one shell: bash, zsh, fish or powershell")
	}
	script, err := completionScript(fs.Arg(0))
	if err != nil {
		return err
	}
	_, err = os.Stdout.WriteString(script)
	return err
}

// completionScript returns the script for a shell, with LF line endings
// whatever the checkout did to the file.
func completionScript(shell string) (string, error) {
	p, ok := completionShells[strings.ToLower(shell)]
	if !ok {
		return "", fmt.Errorf("unknown shell %q: want bash, zsh, fish or powershell", shell)
	}
	b, err := completionFS.ReadFile(p)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(string(b), "\r", ""), nil
}

// cmdComplete answers one completion request on stdout. It never fails
// loudly: a shell would show the error in the middle of the prompt.
func cmdComplete(args []string) {
	defer func() { _ = recover() }()
	words := args
	if v, ok := os.LookupEnv(completeWordsEnv); ok && len(args) == 0 {
		words = strings.Split(strings.TrimSuffix(v, completeWordsSep), completeWordsSep)
	}
	res := complete(words)
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	writeCompletion(w, res)
}

func writeCompletion(w *bufio.Writer, res completion) {
	d := res.directive
	if d == "" {
		d = "default"
	}
	// The writes go to the shell; a failed one has nowhere to be reported,
	// and the shell then just offers nothing.
	_, _ = w.WriteString(d + "\n")
	clean := strings.NewReplacer("\t", " ", "\n", " ", "\r", " ")
	for _, c := range res.cands {
		// Values come from files in the user's folders (task ids, undo
		// keys, provider and model names): one with a control character,
		// such as an escape sequence, is never sent to the terminal.
		if strings.IndexFunc(c.value, unicode.IsControl) >= 0 {
			continue
		}
		_, _ = w.WriteString(clean.Replace(c.value))
		if c.desc != "" {
			_, _ = w.WriteString("\t" + clean.Replace(c.desc))
		}
		_, _ = w.WriteString("\n")
	}
}

type candidate struct{ value, desc string }

type completion struct {
	directive string // "" (default), nospace, files or dirs
	cands     []candidate
}

// complete answers for the words after "rw"; the last one is the word
// being completed ("" after a space).
func complete(words []string) completion {
	if len(words) == 0 {
		words = []string{""}
	}
	cur := words[len(words)-1]
	before := words[:len(words)-1]
	sub := ""
	if len(before) > 0 && !strings.HasPrefix(before[0], "-") {
		sub, before = before[0], before[1:]
	}
	cmd, ok := findCommand(sub)
	if !ok || strings.HasPrefix(sub, "__") {
		return completion{}
	}
	if sub == "" && len(before) == 0 && !strings.HasPrefix(cur, "-") {
		return subcommandCompletion(cur)
	}
	fs, _ := commandFlags(cmd)
	lookup := func(name string) *flag.Flag {
		if fs == nil {
			return nil
		}
		return fs.Lookup(name)
	}
	// What the words so far say: flag values (--dir, --config) and how
	// many positional arguments there are.
	st := completeState{values: map[string]string{}}
	var expect *flag.Flag
	dashdash := false
	for _, w := range before {
		switch {
		case expect != nil:
			st.values[expect.Name] = w
			expect = nil
		case dashdash || !isFlagWord(w):
			st.positional++
		case w == "--":
			dashdash = true
		default:
			name, val, hasVal := strings.Cut(strings.TrimLeft(w, "-"), "=")
			f := lookup(name)
			switch {
			case f == nil:
			case hasVal:
				st.values[name] = val
			case !isBoolFlag(f):
				expect = f
			}
		}
	}
	if expect != nil {
		return valueCompletion(cmd, expect, cur, st)
	}
	if !dashdash && (isFlagWord(cur) || cur == "-") {
		trimmed := strings.TrimLeft(cur, "-")
		if name, val, ok := strings.Cut(trimmed, "="); ok {
			f := lookup(name)
			if f == nil || isBoolFlag(f) {
				return completion{}
			}
			res := valueCompletion(cmd, f, val, st)
			pre := cur[:len(cur)-len(val)]
			for i := range res.cands {
				res.cands[i].value = pre + res.cands[i].value
			}
			return res
		}
		return flagCompletion(fs, cur)
	}
	return positionalCompletion(cmd, cur, st)
}

// completeState is what the words before the current one set.
type completeState struct {
	values     map[string]string // flag values given, by flag name
	positional int               // positional arguments given
}

func isFlagWord(w string) bool { return len(w) > 1 && w[0] == '-' }

func subcommandCompletion(cur string) completion {
	var res completion
	for _, c := range commands {
		if c.name != "" && strings.HasPrefix(c.name, cur) {
			res.cands = append(res.cands, candidate{c.name, c.desc})
		}
	}
	return res
}

func flagCompletion(fs *flag.FlagSet, cur string) completion {
	var res completion
	if fs == nil {
		return res
	}
	dashes := "--"
	if !strings.HasPrefix(cur, "--") && cur != "-" {
		dashes = "-" // Go flags take one dash too: keep what was typed
	}
	fs.VisitAll(func(f *flag.Flag) {
		if v := dashes + f.Name; strings.HasPrefix(v, cur) {
			res.cands = append(res.cands, candidate{v, oneLine(firstLine(f.Usage), 90)})
		}
	})
	return res
}

// valueKind is what a flag's value is, for completion.
type valueKind int

const (
	valNone      valueKind = iota
	valFile                // a file name
	valDir                 // a folder name
	valProvider            // a provider name
	valRouteSpec           // provider:model[:effort]
	valRoleRoute           // role=provider:model[:effort]
	valRolePref            // role=codex|claude|other|auto|<provider>
	valChoice              // one of choices
)

type valueHint struct {
	kind    valueKind
	choices []string
}

// flagValues says how to complete flag values, by flag name, or by
// "<command> <flag>" where a flag of that name means something else in
// that command. Flags not listed take free text (a test checks that each
// value flag was looked at: completeFreeFlags).
var flagValues = map[string]valueHint{
	"config":           {kind: valFile},
	"file":             {kind: valFile},
	"out":              {kind: valFile},
	"dir":              {kind: valDir},
	"logs":             {kind: valDir},
	"starter":          {kind: valDir},
	"selftest in":      {kind: valDir},
	"provider":         {kind: valProvider},
	"when-reset":       {kind: valProvider, choices: []string{"any"}},
	"single":           {kind: valRouteSpec},
	"route":            {kind: valRoleRoute},
	"prefer":           {kind: valRolePref},
	"selftest sandbox": {kind: valChoice, choices: []string{"auto", "off", "only"}},
	"schedule os":      {kind: valChoice, choices: []string{"windows", "darwin", "linux"}},
}

// completeFreeFlags are the value flags that take free text (numbers,
// durations, names, URLs, commands): nothing to complete.
var completeFreeFlags = []string{
	"api", "at", "base", "branch", "budget-day-usd", "budget-task-tokens", "budget-task-usd",
	"check", "check-timeout", "count", "days", "every", "files", "forget", "idle", "in",
	"issue", "issues", "lease", "limit", "max-files", "max-lines", "min-files", "min-use",
	"n", "name", "only", "port", "repo", "scan", "sessions", "setup", "since", "speed",
	"threads", "title",
}

func hintFor(cmd command, name string) valueHint {
	if h, ok := flagValues[cmd.name+" "+name]; ok {
		return h
	}
	return flagValues[name]
}

func valueCompletion(cmd command, f *flag.Flag, cur string, st completeState) completion {
	h := hintFor(cmd, f.Name)
	switch h.kind {
	case valFile:
		return completion{directive: "files"}
	case valDir:
		return completion{directive: "dirs"}
	case valChoice:
		return prefixed(cur, words(h.choices...))
	case valProvider:
		c := providerCandidates(st)
		return prefixed(cur, append(c, words(h.choices...)...))
	case valRouteSpec:
		return routeSpecCompletion(cur, "", st)
	case valRoleRoute, valRolePref:
		role, rest, ok := strings.Cut(cur, "=")
		if !ok {
			roles := event.Roles
			if h.kind == valRolePref {
				roles = append(append([]string(nil), roles...), "all")
			}
			res := completion{directive: "nospace"}
			for _, r := range roles {
				if strings.HasPrefix(r+"=", cur) {
					res.cands = append(res.cands, candidate{value: r + "="})
				}
			}
			return res
		}
		if h.kind == valRoleRoute {
			return routeSpecCompletion(rest, role+"=", st)
		}
		c := append(providerCandidates(st), words(config.PreferOptions...)...)
		res := prefixed(rest, c)
		for i := range res.cands {
			res.cands[i].value = role + "=" + res.cands[i].value
		}
		return res
	}
	return completion{}
}

// routeSpecCompletion completes provider:model[:effort] after pre.
func routeSpecCompletion(cur, pre string, st completeState) completion {
	prov, _, ok := strings.Cut(cur, ":")
	if !ok {
		res := completion{directive: "nospace"}
		for _, c := range providerCandidates(st) {
			if strings.HasPrefix(c.value, cur) {
				res.cands = append(res.cands, candidate{pre + c.value + ":", c.desc})
			}
		}
		return res
	}
	cfg := loadConfigQuiet(st)
	pc, found := cfg.Providers[prov]
	if !found {
		return completion{}
	}
	var res completion
	for _, m := range pc.Models {
		v := prov + ":" + m.ID
		if strings.HasPrefix(v, cur) {
			res.cands = append(res.cands, candidate{pre + v, strings.TrimSpace(m.Label + " " + m.Tier)})
		}
		if strings.HasPrefix(cur, v+":") {
			for _, e := range pc.Efforts {
				if ve := v + ":" + e; strings.HasPrefix(ve, cur) {
					res.cands = append(res.cands, candidate{value: pre + ve})
				}
			}
		}
	}
	return res
}

func positionalCompletion(cmd command, cur string, st completeState) completion {
	switch cmd.args {
	case argFiles:
		return completion{directive: "files"}
	case argShell:
		if st.positional == 0 {
			return prefixed(cur, words("bash", "zsh", "fish", "powershell"))
		}
	case argTask, argUndoKey:
		if st.positional == 0 {
			return prefixed(cur, taskCandidates(st, cmd.args == argUndoKey))
		}
	}
	return completion{}
}

func words(vs ...string) []candidate {
	out := make([]candidate, len(vs))
	for i, v := range vs {
		out[i] = candidate{value: v}
	}
	return out
}

func prefixed(cur string, cands []candidate) completion {
	var res completion
	for _, c := range cands {
		if strings.HasPrefix(c.value, cur) {
			res.cands = append(res.cands, c)
		}
	}
	return res
}

// loadConfigQuiet is the config the command line names (--config), or
// the usual one, or the defaults when it does not load.
func loadConfigQuiet(st completeState) *config.Config {
	cfg, _, err := config.Load(st.values["config"])
	if err != nil || cfg == nil {
		return config.Default()
	}
	return cfg
}

func providerCandidates(st completeState) []candidate {
	cfg := loadConfigQuiet(st)
	var out []candidate
	for _, p := range cfg.ProviderNames() {
		desc := cfg.Providers[p].Label
		if cfg.Providers[p].Disabled {
			desc = strings.TrimSpace(desc + " (disabled)")
		}
		out = append(out, candidate{p, desc})
	}
	return out
}

// completeTasks caps the task ids offered.
const completeTasks = 20

// taskCandidates are the recent tasks of the folder (--dir, else the
// current one), newest first; for rw undo only those it recorded. Other
// commands take any task: without one here, the newest of all are offered.
func taskCandidates(st completeState, undo bool) []candidate {
	dir := st.values["dir"]
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	states := orchestrator.Recent(dir, completeTasks)
	if len(states) == 0 && !undo {
		states = orchestrator.Recent("", completeTasks)
	}
	var out []candidate
	seen := map[string]bool{}
	for _, s := range states {
		id := s.ID
		if undo {
			id = s.UndoKey
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, candidate{id, s.Status + ": " + oneLine(s.Task, 60)})
	}
	return out
}
