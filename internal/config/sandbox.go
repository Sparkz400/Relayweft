package config

import (
	"fmt"
	"path"
	"reflect"
	"slices"
	"strings"

	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/proc"
)

// Sandbox modes: the container runtime, or off.
const (
	SandboxOff    = "off"
	SandboxDocker = "docker"
	SandboxPodman = "podman"
)

// DefaultSandboxImage is the image built from packaging/sandbox.
const DefaultSandboxImage = "relayweft-sandbox"

// sandboxKey is the setting a repo file may tighten but not loosen
// without trust.
const sandboxKey = "sandbox"

// SandboxCfg runs agents, and the verify commands rw runs, in a container
// with only the step's folder writable (docs/sandbox.md). It is off by
// default. The top-level section applies to every provider; a provider's
// own section (providers.<name>.sandbox) adds to it: its mode, image,
// network and command replace the top-level ones, its env, credentials and
// mounts are added, and its roles replace the top-level roles.
type SandboxCfg struct {
	Mode    string `yaml:"mode,omitempty"`    // off (default), docker or podman
	Image   string `yaml:"image,omitempty"`   // default relayweft-sandbox
	Network string `yaml:"network,omitempty"` // on (default) or off
	// Env names variables passed from your environment into the container
	// (only those that are set). Nothing else of your environment goes in.
	Env []string `yaml:"env,omitempty"`
	// Credentials are files mounted read-only at the same place under the
	// container's home, e.g. ~/.codex/auth.json. They must be under your
	// home folder.
	Credentials []string `yaml:"credentials,omitempty"`
	// Mounts are more folders or files (read-only unless writable).
	Mounts []SandboxMount `yaml:"mounts,omitempty"`
	// Roles limits the sandbox to these roles (empty = every role).
	Roles []string `yaml:"roles,omitempty"`
	// Command is the CLI's name in the image (provider sections; default:
	// the provider's command without folder and .cmd/.exe).
	Command string `yaml:"command,omitempty"`
}

// SandboxMount is one extra mount.
type SandboxMount struct {
	Path     string `yaml:"path"`               // on this machine; ~ is your home
	Target   string `yaml:"target,omitempty"`   // in the container (default: the same place under its home, for paths under yours)
	Writable bool   `yaml:"writable,omitempty"` // mounted read-write
}

// On reports whether the sandbox runs anything.
func (s SandboxCfg) On() bool { return s.Mode == SandboxDocker || s.Mode == SandboxPodman }

// NetworkOff reports whether the container gets no network at all.
func (s SandboxCfg) NetworkOff() bool { return s.Network == "off" }

// ImageName is the image, or the default one.
func (s SandboxCfg) ImageName() string {
	if s.Image != "" {
		return s.Image
	}
	return DefaultSandboxImage
}

// with returns s with a provider's own section o layered on top.
func (s SandboxCfg) with(o SandboxCfg) SandboxCfg {
	if o.Mode != "" {
		s.Mode = o.Mode
	}
	if o.Image != "" {
		s.Image = o.Image
	}
	if o.Network != "" {
		s.Network = o.Network
	}
	if o.Command != "" {
		s.Command = o.Command
	}
	if len(o.Roles) > 0 {
		s.Roles = o.Roles
	}
	s.Env = appendNew(slices.Clone(s.Env), o.Env...)
	s.Credentials = appendNew(slices.Clone(s.Credentials), o.Credentials...)
	s.Mounts = append(slices.Clone(s.Mounts), o.Mounts...)
	return s
}

func appendNew(xs []string, add ...string) []string {
	for _, a := range add {
		if !slices.Contains(xs, a) {
			xs = append(xs, a)
		}
	}
	return xs
}

// ProviderSandbox is a provider's sandbox: the top-level section with the
// provider's own on top.
func (c *Config) ProviderSandbox(provider string) SandboxCfg {
	s := c.Sandbox
	if pc, ok := c.Providers[provider]; ok && pc.Sandbox != nil {
		s = s.with(*pc.Sandbox)
	}
	return s
}

// Covers reports whether the sandbox runs the agents of role.
func (s SandboxCfg) Covers(role string) bool {
	return s.On() && (len(s.Roles) == 0 || slices.Contains(s.Roles, role))
}

// CheckSandbox is the sandbox for the commands rw runs on code agents
// wrote (verify commands, after_merge and after_task hooks): the
// top-level section when it is on, otherwise that of the first enabled
// provider whose writing agents run in a sandbox, without that provider's
// sign-in variables and credential files. false when no writing agent
// runs in a sandbox: then those commands run on this machine, as before.
func (c *Config) CheckSandbox() (SandboxCfg, bool) {
	if c.Sandbox.On() {
		return c.Sandbox, true
	}
	for _, p := range c.Enabled() {
		s := c.ProviderSandbox(p)
		if s.Covers(event.RoleWorker) || s.Covers(event.RoleWorkerHigh) {
			s.Env, s.Credentials, s.Command, s.Roles = c.Sandbox.Env, nil, "", nil
			return s, true
		}
	}
	return SandboxCfg{}, false
}

// SecretVars lists the ${NAME}s the MCP server uses that are rw's forge or
// CI tokens. In a sandbox the server runs in the container with the
// values in its config file or environment, so the token would go in.
func (s MCPServer) SecretVars() []string {
	var out []string
	s.Expanded(func(n string) (string, bool) {
		if proc.IsChildSecret(n) && !slices.Contains(out, n) {
			out = append(out, n)
		}
		return "", true
	})
	slices.Sort(out)
	return out
}

// SandboxedProviders lists the enabled providers whose agents run in a
// sandbox for at least one role, with that sandbox.
func (c *Config) SandboxedProviders() map[string]SandboxCfg {
	out := map[string]SandboxCfg{}
	for _, name := range c.Enabled() {
		if s := c.ProviderSandbox(name); s.On() {
			out[name] = s
		}
	}
	return out
}

// validate checks one sandbox section; where names it in messages.
func (s SandboxCfg) validate(where string) []string {
	var errs []string
	bad := func(format string, args ...any) { errs = append(errs, where+": "+fmt.Sprintf(format, args...)) }
	switch s.Mode {
	case "", SandboxOff, SandboxDocker, SandboxPodman:
	default:
		bad("mode must be off, docker or podman, got %q", s.Mode)
	}
	switch s.Network {
	case "", "on", "off":
	default:
		bad("network must be on or off, got %q (an allow-list is not supported)", s.Network)
	}
	if s.Image != "" && (strings.HasPrefix(s.Image, "-") || strings.ContainsAny(s.Image, " \t\r\n")) {
		bad("image %q is not an image name", s.Image)
	}
	if s.Command != "" && (strings.HasPrefix(s.Command, "-") || strings.ContainsAny(s.Command, " \t\r\n\"'")) {
		bad("command %q must be a plain command name", s.Command)
	}
	for _, n := range s.Env {
		switch {
		case !envKey.MatchString(n):
			bad("env name %q may use only letters, digits and _", n)
		case proc.IsChildSecret(n):
			// The forge and CI tokens stay with rw (docs/ci.md).
			bad("%s is rw's forge or CI token and never goes into a sandbox", n)
		}
	}
	for _, p := range s.Credentials {
		if strings.TrimSpace(p) == "" {
			bad("an empty credentials entry")
		}
	}
	for i, m := range s.Mounts {
		if strings.TrimSpace(m.Path) == "" {
			bad("mounts[%d] needs a path", i)
		}
		if m.Target != "" {
			if !strings.HasPrefix(m.Target, "/") || strings.ContainsAny(m.Target, ",\"\r\n") {
				bad("mounts[%d]: target %q must be an absolute path in the container", i, m.Target)
			} else if t := path.Clean(m.Target); reservedTarget(t) {
				bad("mounts[%d]: target %s is where rw mounts the step's folder or its own files", i, t)
			}
		}
		if dockerSocket(m.Path) {
			bad("mounts[%d]: %s would give the agent control of the container runtime", i, m.Path)
		}
	}
	for _, r := range s.Roles {
		if !slices.Contains(event.Roles, r) {
			bad("unknown role %q in roles", r)
		}
	}
	return errs
}

// reservedTarget reports whether t (clean) is a container path rw uses
// itself: the step's folder and rw's own mounts.
func reservedTarget(t string) bool {
	for _, r := range []string{"/work", "/rw"} {
		if t == r || strings.HasPrefix(t, r+"/") {
			return true
		}
	}
	return t == "/"
}

// dockerSocket reports whether p is the container runtime's socket or pipe.
func dockerSocket(p string) bool {
	q := strings.ToLower(strings.ReplaceAll(p, `\`, "/"))
	return strings.HasSuffix(q, "docker.sock") || strings.HasSuffix(q, "podman.sock") || strings.Contains(q, "/pipe/docker") || strings.Contains(q, "/pipe/podman")
}

// validateSandbox checks the top-level section and every provider's.
func (c *Config) validateSandbox() []string {
	errs := c.Sandbox.validate("sandbox")
	if c.Sandbox.Command != "" {
		errs = append(errs, "sandbox: command belongs in a provider's sandbox section")
	}
	for _, name := range sortedKeys(c.Providers) {
		if pc := c.Providers[name]; pc.Sandbox != nil {
			errs = append(errs, pc.Sandbox.validate("providers."+name+".sandbox")...)
		}
	}
	return errs
}

// untrustedSandbox is what an untrusted file (a repo's .relayweft.yaml, a
// ./relayweft.yaml that came with a clone) may do to your sandbox section
// mine; file is the section after reading the file over it. It may turn
// the sandbox on, cut its network and widen it to more roles. It may not
// turn it off, pick its image, or pass more of your environment, files or
// folders in: those make the sandbox weaker and need `rw trust`. changed
// reports that something the file set was dropped.
func untrustedSandbox(mine, file SandboxCfg) (out SandboxCfg, changed bool) {
	out = mine
	if !mine.On() && file.On() {
		out.Mode = file.Mode
		out.Roles = file.Roles
	}
	if file.NetworkOff() {
		out.Network = "off"
	}
	if mine.On() && len(mine.Roles) > 0 && (len(file.Roles) == 0 || isSuperset(file.Roles, mine.Roles)) {
		out.Roles = file.Roles
	}
	return out, !reflect.DeepEqual(out, file)
}

func isSuperset(a, b []string) bool {
	for _, x := range b {
		if !slices.Contains(a, x) {
			return false
		}
	}
	return true
}
