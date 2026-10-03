package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDetectVerify(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"empty", nil, nil},
		{"go", map[string]string{"go.mod": "module x"}, []string{"go build ./...", "go test ./..."}},
		{"npm placeholder", map[string]string{"package.json": `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`}, nil},
		{"pnpm", map[string]string{"package.json": `{"scripts":{"test":"vitest"}}`, "pnpm-lock.yaml": ""}, []string{"pnpm test"}},
		{"npm", map[string]string{"package.json": `{"scripts":{"test":"jest"}}`}, []string{"npm test"}},
		{"pytest", map[string]string{"pyproject.toml": "[tool.pytest.ini_options]\n"}, []string{"python -m pytest -q"}},
		{"cargo+dotnet", map[string]string{"Cargo.toml": "", "app.csproj": ""}, []string{"cargo test", "dotnet test"}},
		{"maven", map[string]string{"pom.xml": ""}, []string{"mvn -q test"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for n, body := range c.files {
				os.WriteFile(filepath.Join(dir, n), []byte(body), 0o644)
			}
			if got := DetectVerify(dir); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestWithVerify(t *testing.T) {
	out := WithVerify(DefaultYAML(), []string{"go test ./...", `say "hi"`})
	c, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"go test ./...", `say "hi"`}; !reflect.DeepEqual(c.Verify.Commands, want) {
		t.Fatalf("commands %q", c.Verify.Commands)
	}
	if string(WithVerify(DefaultYAML(), nil)) != string(DefaultYAML()) {
		t.Fatal("nil commands changed the text")
	}
}
