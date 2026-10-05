package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/proc"
)

// installHints are what doctor suggests for a missing built-in CLI
// (providers.<name>.install_hint overrides them).
var installHints = map[string]string{
	event.Codex:  "npm install -g @openai/codex, then `codex login`",
	event.Claude: "npm install -g @anthropic-ai/claude-code, then run `claude` once to log in",
	event.Gemini: "npm install -g @google/gemini-cli, then set GEMINI_API_KEY and pick it in `gemini` (/auth)",
	event.Qwen:   "npm install -g @qwen-code/qwen-code",
}

// doctorProviders checks every configured provider's CLI: found, version,
// sign-in and the environment it needs. Disabled providers get one line.
// It returns the number of problems.
func doctorProviders(w io.Writer, cfg *config.Config, ok func(bool) string, warn string) int {
	problems := 0
	info := stMuted.Render("info")
	for _, p := range cfg.ProviderNames() {
		pc := cfg.Providers[p]
		kind := pc.KindOf(p)
		label := p
		if kind != p {
			label = p + " (" + kind + ")"
		}
		if pc.Disabled {
			fmt.Fprintf(w, "%s %-11s disabled (providers.%s.disabled)\n", info, p, p)
			continue
		}
		bin, err := proc.Resolve(pc.Command)
		if sb := cfg.ProviderSandbox(p); err != nil && sb.On() && len(sb.Roles) == 0 {
			// Every role runs it from the sandbox image (doctorSandbox).
			fmt.Fprintf(w, "%s %-11s runs in the %s sandbox only; not needed on this machine\n", info, p, sb.Mode)
			continue
		}
		if err != nil {
			problems++
			fmt.Fprintf(w, "%s %-11s %q not found on PATH", ok(false), p, pc.Command)
			hint := pc.InstallHint
			if hint == "" {
				hint = installHints[kind]
			}
			if hint != "" {
				fmt.Fprint(w, " - "+hint)
			}
			fmt.Fprintln(w)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		out, verr := exec.CommandContext(ctx, bin, "--version").Output()
		cancel()
		v := strings.TrimSpace(string(out))
		if verr != nil {
			fmt.Fprintf(w, "%s %-11s %s (version check failed: %v)\n", warn, p, bin, verr)
			continue
		}
		mark := ok(true)
		note := ""
		if pc.TestedVersion != "" && v != pc.TestedVersion {
			mark = warn
			note = stMuted.Render(fmt.Sprintf("  (tested with %s; output format may differ)", pc.TestedVersion))
		}
		if label != p {
			note += stMuted.Render("  " + label)
		}
		fmt.Fprintf(w, "%s %-11s %s  %s%s\n", mark, p, v, bin, note)
		if c := providerCaps(p, pc); c != "" {
			fmt.Fprintf(w, "%s %-11s %s\n", info, p, c)
		}
		if _, missing := pc.EnvFor(nil); len(missing) > 0 {
			problems++
			fmt.Fprintf(w, "%s %-11s needs %s set in your environment (providers.%s.env)\n", ok(false), p, strings.Join(missing, ", "), p)
		}
		switch kind {
		case event.Codex:
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			out, err := exec.CommandContext(ctx, bin, "login", "status").CombinedOutput()
			cancel()
			s := oneLine(string(out), 100)
			if err != nil {
				fmt.Fprintf(w, "%s %-11s login: %s - run `codex login`\n", warn, p, s)
			} else {
				fmt.Fprintf(w, "%s %-11s login: %s\n", ok(true), p, s)
			}
		case event.Claude:
			if endpoint(pc) != "" {
				break // another API (DeepSeek, Ollama) with its own key
			}
			env, _ := pc.EnvFor(nil) // e.g. CLAUDE_CONFIG_DIR
			login, detail := claudeLogin(func(args ...string) (string, string, error) {
				ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
				defer cancel()
				return setupExec(ctx, env, bin, args...)
			})
			switch login {
			case loginOK:
				fmt.Fprintf(w, "%s %-11s login: %s\n", ok(true), p, detail)
			case loginNo:
				fmt.Fprintf(w, "%s %-11s login: not logged in - run `claude auth login`\n", warn, p)
			default:
				fmt.Fprintf(w, "%s %-11s login: %s\n", warn, p, detail)
			}
		case event.Gemini:
			if msg := geminiAuthProblem(pc); msg != "" {
				problems++
				fmt.Fprintf(w, "%s %-11s %s\n", ok(false), p, msg)
			}
		}
		if base := endpoint(pc); base != "" {
			if err := reachable(base); err != nil {
				fmt.Fprintf(w, "%s %-11s %s is not answering (%v)%s\n", warn, p, base, err, startHint(base))
			} else {
				fmt.Fprintf(w, "%s %-11s endpoint %s\n", ok(true), p, base)
			}
		}
	}
	return problems
}

// providerCaps describes what a provider described in the config may do,
// and the roles it stands by for ("" = nothing to say).
func providerCaps(name string, pc config.ProviderCfg) string {
	var parts []string
	if pc.KindOf(name) == event.Generic && pc.Generic != nil {
		switch {
		case !pc.CanWrite(name):
			parts = append(parts, "read-only work only")
		case !pc.CanReadOnly(name):
			parts = append(parts, "writing work only (no read_only_args)")
		}
		if len(pc.Generic.ResumeArgs) == 0 {
			parts = append(parts, "no resume (follow-ups start fresh)")
		}
	}
	if len(pc.Standby) > 0 {
		parts = append(parts, "stands by for "+strings.Join(pc.Standby, ", ")+" when the others are nearly out")
	}
	return strings.Join(parts, "; ")
}

// geminiAuthProblem says what keeps Gemini CLI from signing in ("" = none
// found). Its own settings.json choice wins over the environment, and
// since 18 June 2026 Google no longer serves personal accounts (free, AI Pro,
// AI Ultra) through Gemini CLI: only an API key, Vertex AI or a Code Assist
// Standard/Enterprise licence work.
func geminiAuthProblem(pc config.ProviderCfg) string {
	has := func(k string) bool {
		if os.Getenv(k) != "" {
			return true
		}
		env, _ := pc.EnvFor(nil)
		for _, kv := range env {
			if strings.HasPrefix(kv, k+"=") && len(kv) > len(k)+1 {
				return true
			}
		}
		return false
	}
	selected := ""
	if home, err := os.UserHomeDir(); err == nil {
		if data, err := os.ReadFile(filepath.Join(home, ".gemini", "settings.json")); err == nil {
			var s struct {
				Security struct {
					Auth struct {
						SelectedType string `json:"selectedType"`
					} `json:"auth"`
				} `json:"security"`
			}
			if json.Unmarshal(data, &s) == nil {
				selected = s.Security.Auth.SelectedType
			}
		}
	}
	key := has("GEMINI_API_KEY") || has("GOOGLE_API_KEY")
	switch selected {
	case "oauth-personal":
		return "signs in with a personal Google account, which Gemini CLI no longer serves - set GEMINI_API_KEY, then choose \"Use Gemini API key\" in `gemini` (/auth)"
	case "gemini-api-key":
		if !key {
			return "set to use an API key, but GEMINI_API_KEY is not set"
		}
	case "":
		if !key && !has("GOOGLE_GENAI_USE_VERTEXAI") && !has("GOOGLE_GENAI_USE_GCA") {
			return "no sign-in - set GEMINI_API_KEY (a key from aistudio.google.com, with billing)"
		}
	}
	return ""
}

// endpoint is the API a provider's env points its CLI at, if any.
func endpoint(pc config.ProviderCfg) string {
	for _, k := range []string{"ANTHROPIC_BASE_URL", "OPENAI_BASE_URL"} {
		if v := pc.Env[k]; v != "" && !strings.Contains(v, "${") {
			return v
		}
	}
	return ""
}

// reachable checks that something listens at a URL's host and port.
func reachable(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("not a URL")
	}
	host := u.Host
	if u.Port() == "" {
		port := "443"
		if u.Scheme == "http" {
			port = "80"
		}
		host = net.JoinHostPort(u.Hostname(), port)
	}
	c, err := net.DialTimeout("tcp", host, 3*time.Second)
	if err != nil {
		return err
	}
	return c.Close()
}

// startHint says how to start a local model server.
func startHint(base string) string {
	if strings.Contains(base, ":11434") {
		return " - start Ollama"
	}
	return ""
}

// reviewerFor picks who gives rw review's second opinion on work writer
// did: the first other provider in routing order with a reviewer model
// that may stand in for another ("" = none, or writer is not a provider).
func reviewerFor(cfg *config.Config, writer string) string {
	if writer == "" || !cfg.IsProvider(writer) {
		return ""
	}
	for _, p := range cfg.Alternatives(writer) {
		if cfg.Roles[event.RoleReviewer].For(p).Model != "" && !cfg.Providers[p].OnlyPreferred {
			return p
		}
	}
	return ""
}
