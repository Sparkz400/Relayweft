package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestSandboxValidate(t *testing.T) {
	cases := map[string]string{
		"sandbox: {mode: kubernetes}":                                                     "mode must be off, docker or podman",
		"sandbox: {mode: docker, network: allow-list}":                                    "an allow-list is not supported",
		"sandbox: {mode: docker, env: [GITHUB_TOKEN]}":                                    "never goes into a sandbox",
		"sandbox: {mode: docker, env: [CI_JOB_TOKEN]}":                                    "never goes into a sandbox",
		"sandbox: {mode: docker, env: [\"A-B\"]}":                                         "may use only letters",
		"sandbox: {mode: docker, image: \"--privileged\"}":                                "is not an image name",
		"sandbox: {mode: docker, mounts: [{path: /x, target: /work/x}]}":                  "where rw mounts",
		"sandbox: {mode: docker, mounts: [{path: /x, target: /rw}]}":                      "where rw mounts",
		"sandbox: {mode: docker, mounts: [{path: /x, target: rel}]}":                      "absolute path",
		"sandbox: {mode: docker, mounts: [{path: /var/run/docker.sock, target: /d}]}":     "control of the container runtime",
		"sandbox: {mode: docker, mounts: [{path: '//./pipe/docker_engine', target: /d}]}": "control of the container runtime",
		"sandbox: {mode: docker, roles: [boss]}":                                          "unknown role",
		"sandbox: {mode: docker, command: claude}":                                        "belongs in a provider",
		"providers: {claude: {sandbox: {command: \"-x\"}}}":                               "plain command name",
	}
	for body, want := range cases {
		c, err := parseFile("t.yaml", []byte(body))
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", body, err, want)
		}
	}
	c, _ := parseFile("t.yaml", []byte("sandbox: {mode: docker, network: off, env: [MY_KEY], credentials: [~/.codex/auth.json], mounts: [{path: ~/cache, target: /cache}], roles: [worker]}"))
	if err := c.Validate(); err != nil {
		t.Errorf("a good section: %v", err)
	}
	if def := Default(); def.Sandbox.On() || len(def.SandboxedProviders()) != 0 {
		t.Error("the sandbox must be off by default")
	}
}

// A provider's section adds to the top-level one; roles limit it.
func TestProviderSandbox(t *testing.T) {
	c, err := parseFile("t.yaml", []byte(`
sandbox: {mode: docker, image: mine, env: [SHARED], roles: [worker, explorer]}
providers:
  claude: {sandbox: {command: my-claude}}
  codex: {sandbox: {mode: off}}
  gemini: {sandbox: {env: [EXTRA]}}
`))
	if err != nil || c.Validate() != nil {
		t.Fatal(err, c.Validate())
	}
	s := c.ProviderSandbox("claude")
	// The preset's sign-in variables stay (merged per field), after the
	// top-level ones.
	if s.Mode != "docker" || s.ImageName() != "mine" || s.Command != "my-claude" ||
		!slices.Equal(s.Env, []string{"SHARED", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}) {
		t.Errorf("claude sandbox = %+v", s)
	}
	// A list replaces the preset's list, like every list in the file.
	if g := c.ProviderSandbox("gemini"); !slices.Equal(g.Env, []string{"SHARED", "EXTRA"}) {
		t.Errorf("gemini env = %v", g.Env)
	}
	if !s.Covers("worker") || s.Covers("planner") {
		t.Error("roles do not limit the sandbox")
	}
	if c.ProviderSandbox("codex").On() {
		t.Error("a provider's mode: off must take it out of the sandbox")
	}
	if _, ok := c.SandboxedProviders()["codex"]; ok {
		t.Error("codex listed as sandboxed")
	}
	if (SandboxCfg{}).ImageName() != DefaultSandboxImage {
		t.Error("default image")
	}
}

// The commands rw runs on agents' code use the top-level sandbox, else
// that of a provider whose writers are sandboxed, without its sign-in.
func TestCheckSandbox(t *testing.T) {
	if _, on := Default().CheckSandbox(); on {
		t.Error("on by default")
	}
	c, _ := parseFile("t.yaml", []byte("sandbox: {env: [MINE]}\nproviders: {codex: {sandbox: {mode: docker, image: cx, credentials: [~/.codex/auth.json]}}}\n"))
	s, on := c.CheckSandbox()
	if !on || s.Mode != "docker" || s.ImageName() != "cx" || len(s.Credentials) != 0 || !slices.Equal(s.Env, []string{"MINE"}) {
		t.Errorf("provider writer sandbox: %v %+v", on, s)
	}
	// A provider sandboxed only for read-only roles writes nothing.
	c, _ = parseFile("t.yaml", []byte("providers: {codex: {sandbox: {mode: docker, roles: [explorer]}}}\n"))
	if _, on := c.CheckSandbox(); on {
		t.Error("read-only sandbox counted")
	}
	c, _ = parseFile("t.yaml", []byte("sandbox: {mode: podman}\n"))
	if s, on := c.CheckSandbox(); !on || s.Mode != "podman" {
		t.Errorf("top-level: %v %+v", on, s)
	}
}

// An untrusted repo file may make the sandbox stricter, never weaker: it
// may turn it on (with your image) and cut its network, but not turn it
// off, pick the image, or pass more of your environment or files in.
func TestRepoFileSandboxNeedsTrustToLoosen(t *testing.T) {
	isolateTrust(t)
	apply := func(user, repo string) (*Config, RepoInfo) {
		t.Helper()
		root := t.TempDir()
		os.Mkdir(filepath.Join(root, ".git"), 0o755)
		os.WriteFile(filepath.Join(root, RepoFileName), []byte(repo), 0o644)
		c, err := parseFile("user.yaml", []byte(user))
		if err != nil {
			t.Fatal(err)
		}
		info, err := ApplyRepo(c, root)
		if err != nil {
			t.Fatal(err)
		}
		return c, info
	}
	ignored := func(info RepoInfo) bool { return slices.Contains(info.Ignored, "sandbox") }

	// Turning it on, and network off: stricter, applies.
	c, info := apply("", "sandbox: {mode: docker, network: off}\n")
	if !c.Sandbox.On() || !c.Sandbox.NetworkOff() || ignored(info) || c.Sandbox.ImageName() != DefaultSandboxImage {
		t.Errorf("turning it on: %+v %v", c.Sandbox, info.Ignored)
	}
	// Turning it on with another image: on, with your image.
	c, info = apply("", "sandbox: {mode: docker, image: evil/image}\n")
	if !c.Sandbox.On() || c.Sandbox.ImageName() != DefaultSandboxImage || !ignored(info) {
		t.Errorf("repo image: %+v %v", c.Sandbox, info.Ignored)
	}
	user := "sandbox: {mode: docker, image: mine, env: [MINE], roles: [worker]}\n"
	for name, repo := range map[string]string{
		"off":         "sandbox: {mode: off}\n",
		"runtime":     "sandbox: {mode: podman}\n",
		"image":       "sandbox: {image: evil/image}\n",
		"env":         "sandbox: {env: [AWS_SECRET_ACCESS_KEY]}\n",
		"credentials": "sandbox: {credentials: [~/.ssh/id_ed25519]}\n",
		"mounts":      "sandbox: {mounts: [{path: ~/, target: /h, writable: true}]}\n",
		"anchor":      "x: &s {mode: off}\nsandbox: *s\n",
		"merge key":   "x: &s {mode: off}\nsandbox: {<<: *s}\n",
		"provider":    "providers: {claude: {sandbox: {mode: off}}}\n",
	} {
		c, info := apply(user, repo)
		mine, _ := parseFile("user.yaml", []byte(user))
		if !reflect.DeepEqual(c.Sandbox, mine.Sandbox) || !reflect.DeepEqual(c.Providers["claude"].Sandbox, mine.Providers["claude"].Sandbox) {
			t.Errorf("%s: an untrusted repo file changed the sandbox: %+v", name, c.Sandbox)
		}
		if len(info.Ignored) == 0 {
			t.Errorf("%s: not reported as ignored", name)
		}
	}
	// Widening to every role is stricter.
	c, info = apply(user, "sandbox: {roles: []}\n")
	if len(c.Sandbox.Roles) != 0 || ignored(info) {
		t.Errorf("roles: [] = %v %v", c.Sandbox.Roles, info.Ignored)
	}

	// Trusted, it may do all of that.
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, ".git"), 0o755)
	repo := filepath.Join(root, RepoFileName)
	os.WriteFile(repo, []byte("sandbox: {mode: off}\n"), 0o644)
	if err := Trust(repo); err != nil {
		t.Fatal(err)
	}
	c, _ = parseFile("user.yaml", []byte(user))
	if _, err := ApplyRepo(c, root); err != nil || c.Sandbox.On() {
		t.Errorf("a trusted repo file could not turn it off: %v %+v", err, c.Sandbox)
	}
	// rw trust shows it.
	if lines, _ := CommandSettings(repo); len(lines) != 1 || !strings.Contains(lines[0], "sandbox") {
		t.Errorf("CommandSettings = %q", lines)
	}
}

// A ./relayweft.yaml that came with a clone is guarded the same way.
func TestUntrustedLocalConfigSandbox(t *testing.T) {
	isolateTrust(t)
	writeUserConfig(t, "sandbox: {mode: docker}\n")
	t.Chdir(t.TempDir())
	os.WriteFile(FileName, []byte("sandbox: {mode: off}\n"), 0o644)
	c, _, ignored, err := LoadInfo("")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Sandbox.On() || !slices.Contains(ignored, "sandbox") {
		t.Errorf("an untrusted local file turned the sandbox off: %+v %v", c.Sandbox, ignored)
	}
	if err := TrustLocal(FileName); err != nil {
		t.Fatal(err)
	}
	if c, _, _, _ = LoadInfo(""); c.Sandbox.On() {
		t.Error("the trusted local file's sandbox: off did not apply")
	}
}
