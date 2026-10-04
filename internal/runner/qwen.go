package runner

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/limits"
)

// NewQwen returns a runner for Qwen Code, `qwen --output-format stream-json`
// (tested with Qwen Code 0.24.7). Its stream is Claude Code's format, so it
// shares the Claude parser. The prompt is read from stdin.
func NewQwen(cfg config.ProviderCfg, det *limits.Detector) *Exec {
	return &Exec{
		Provider: event.Qwen,
		Kind:     event.Qwen,
		Cfg:      cfg,
		Detector: det,
		args:     func(s Spec) []string { return QwenArgs(cfg, s) },
		// Qwen's input_tokens already include the cached part.
		parser:   func() lineParser { return &claudeParser{inputHasCache: true, noCost: true} },
		precheck: qwenPrecheck(cfg),
	}
}

// qwenPrecheck refuses to run Qwen Code in a folder with a .qwen settings
// folder of its own: Qwen would load it (MCP servers and discovery
// commands run commands), whatever the role. allow_repo_settings in your
// own config lifts this.
func qwenPrecheck(cfg config.ProviderCfg) func(Spec) error {
	return func(s Spec) error {
		if cfg.AllowRepoSettings || s.Dir == "" {
			return nil
		}
		if _, err := os.Stat(filepath.Join(s.Dir, ".qwen")); err == nil {
			return errors.New("this repo has its own .qwen settings, which Qwen Code would load (they can run commands); " +
				"review them and set providers.qwen.allow_repo_settings, or route this work to another provider")
		}
		return nil
	}
}

// QwenArgs builds the argument list. Read-only agents run in plan mode
// (no edits, no commands: the write and shell tools are removed); writers
// in auto-edit, which approves edits but no shell commands except the
// verify commands.
func QwenArgs(cfg config.ProviderCfg, s Spec) []string {
	args := []string{"--output-format", "stream-json"}
	if s.Model != "" {
		args = append(args, "-m", s.Model)
	}
	if s.Resume != "" {
		args = append(args, "--resume", s.Resume)
	}
	mode := cfg.WritePermissionMode
	if mode == "" {
		mode = "auto-edit"
	}
	if s.ReadOnly {
		mode = "plan"
	}
	args = append(args, "--approval-mode", mode)
	if s.MCP != nil && s.MCP.ConfigFile != "" {
		// Same {"mcpServers": ...} file as Claude's.
		args = append(args, "--mcp-config", s.MCP.ConfigFile)
	}
	args = append(args, cfg.ExtraArgs...)
	if !s.ReadOnly && len(s.AllowedCommands) > 0 {
		// Variadic: keep it last so it cannot swallow other arguments.
		args = append(args, "--allowed-tools")
		for _, c := range s.AllowedCommands {
			args = append(args, "run_shell_command("+c+")")
		}
	}
	return args
}
