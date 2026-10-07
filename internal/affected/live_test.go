package affected

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/proc"
)

// These opt-in checks install real dependencies in disposable workspaces.
// See docs/bench/2026-10-07-affected-layouts.md for prerequisites and commands.
func liveCommand(t *testing.T, dir, command string, succeeds bool) string {
	t.Helper()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := proc.Shell(ctx, command)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("%s: %v\n%s", command, ctx.Err(), out)
	}
	if (err == nil) != succeeds {
		t.Fatalf("%s: expected success=%v, error=%v\n%s", command, succeeds, err, out)
	}
	t.Logf("%s: success=%v (%.2fs)", command, err == nil, time.Since(start).Seconds())
	return string(out)
}

func liveWrite(t *testing.T, dir, file, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(file)), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLiveYarnPnP(t *testing.T) {
	if os.Getenv("RW_AFFECTED_LIVE_YARN") != "1" {
		t.Skip("set RW_AFFECTED_LIVE_YARN=1 with Yarn 4.6.0 on PATH")
	}
	t.Log(strings.TrimSpace(liveCommand(t, t.TempDir(), "yarn --version", true)))
	for _, runner := range []string{"jest", "vitest"} {
		t.Run(runner, func(t *testing.T) {
			script, version := "jest --runInBand", "29.7.0"
			header := "const {value} = require('@probe/core');\n"
			if runner == "vitest" {
				script, version = "vitest run --maxWorkers=1 --minWorkers=1", "3.2.7"
				header = "import {test, expect} from 'vitest';\nimport {value} from '@probe/core';\n"
			}
			dir := tree(t, map[string]string{
				"package.json":               fmt.Sprintf(`{"name":"pnp-probe","private":true,"packageManager":"yarn@4.6.0","workspaces":["packages/*"],"scripts":{"test":%q},"devDependencies":{%q:%q},"dependencies":{"@probe/core":"workspace:*"}}`, script, runner, version),
				".yarnrc.yml":                "nodeLinker: pnp\n",
				"packages/core/package.json": `{"name":"@probe/core","version":"1.0.0","main":"index.cjs"}`,
				"packages/core/index.cjs":    "exports.value = () => 1;\n",
				"tests/consumer.test.js":     header + "test('consumer detects core bug', () => expect(value()).toBe(1));\n",
				"tests/other.test.js":        "",
			})
			other := "test('unrelated control', () => expect(2).toBe(2));\n"
			if runner == "vitest" {
				other = "import {test, expect} from 'vitest';\n" + other
			}
			liveWrite(t, dir, "tests/other.test.js", other)
			liveCommand(t, dir, "yarn install", true)
			if _, err := os.Stat(filepath.Join(dir, "node_modules")); !os.IsNotExist(err) {
				t.Fatal("expected a real PnP install without node_modules")
			}
			liveCommand(t, dir, "yarn test", true)
			liveWrite(t, dir, "packages/core/index.cjs", "exports.value = () => 2;\n")
			fullOutput := liveCommand(t, dir, "yarn test", false)
			plan := sel(t, dir, "yarn test", "packages/core/index.cjs")
			t.Logf("plan: %+v", plan)
			if plan.Full || len(plan.Run) != 1 {
				t.Fatalf("expected narrowing for plain PnP workspace: %+v", plan)
			}
			checkAllowed(t, dir, "yarn test", "", plan)
			narrowOutput := liveCommand(t, dir, plan.Run[0], false)
			for _, out := range []string{fullOutput, narrowOutput} {
				if !strings.Contains(out, "consumer detects core bug") {
					t.Fatalf("missing expected failing test:\n%s", out)
				}
			}
			if !strings.Contains(fullOutput, "other.test.js") || strings.Contains(narrowOutput, "other.test.js") {
				t.Fatalf("expected unrelated suite only in full output\nfull:\n%s\nnarrow:\n%s", fullOutput, narrowOutput)
			}
			liveWrite(t, dir, "packages/core/index.cjs", "exports.value = () => 1;\n")
			liveCommand(t, dir, plan.Run[0], true)
			// Peer dependencies give this workspace a virtual PnP path.
			liveWrite(t, dir, "packages/core/package.json", fmt.Sprintf(`{"name":"@probe/core","version":"1.0.0","main":"index.cjs","peerDependencies":{%q:"*"}}`, runner))
			liveCommand(t, dir, "yarn install", true)
			liveCommand(t, dir, "yarn test", true)
			liveWrite(t, dir, "packages/core/index.cjs", "exports.value = () => 2;\n")
			fullOutput = liveCommand(t, dir, "yarn test", false)
			plan = sel(t, dir, "yarn test", "packages/core/index.cjs")
			t.Logf("virtual workspace plan: %+v", plan)
			if !plan.Full || !strings.Contains(plan.Why, "virtual path") {
				t.Fatalf("expected fallback for virtual PnP workspace: %+v", plan)
			}
			narrowOutput = liveCommand(t, dir, plan.Command, false)
			if !strings.Contains(fullOutput, "consumer detects core bug") || !strings.Contains(narrowOutput, "consumer detects core bug") {
				t.Fatalf("missing virtual workspace failure\nfull:\n%s\nnarrow:\n%s", fullOutput, narrowOutput)
			}
		})
	}
}

func TestLiveGradleProjectDir(t *testing.T) {
	if os.Getenv("RW_AFFECTED_LIVE_GRADLE") != "1" {
		t.Skip("set RW_AFFECTED_LIVE_GRADLE=1 with Gradle and JDK 21 on PATH")
	}
	t.Setenv("DEBUG", "") // Gradle's Windows launcher otherwise echoes its script
	t.Log(liveCommand(t, t.TempDir(), "gradle --version", true))
	for _, kotlin := range []bool{false, true} {
		t.Run(fmt.Sprintf("kotlin=%v", kotlin), func(t *testing.T) {
			settings, body := "settings.gradle", "include 'core', 'consumer', 'other'\nproject(':core').projectDir = file('components/core')\nproject(':consumer').projectDir = new File(settingsDir, 'apps/consumer')\n"
			if kotlin {
				settings, body = "settings.gradle.kts", "include(\"core\", \"consumer\", \"other\")\nproject(\":core\").projectDir = file(\"components/core\")\nproject(\":consumer\").projectDir = File(rootDir, \"apps/consumer\")\n"
			}
			files := map[string]string{
				settings: body,
				"components/core/src/main/java/probe/Core.java":       "package probe; public class Core { public static int value() { return 1; } }",
				"apps/consumer/src/test/java/probe/ConsumerTest.java": "package probe; import org.junit.Test; import static org.junit.Assert.*; public class ConsumerTest { @Test public void detectsCoreBug() { assertEquals(1, Core.value()); } }",
				"other/src/test/java/probe/OtherTest.java":            "package probe; import org.junit.Test; import static org.junit.Assert.*; public class OtherTest { @Test public void unrelatedControl() { assertEquals(2, 2); } }",
			}
			for _, d := range []string{"components/core", "apps/consumer", "other"} {
				files[d+"/build.gradle"] = "plugins { id 'java-library' }\nrepositories { mavenCentral() }\ndependencies { testImplementation 'junit:junit:4.13.2' }\n"
			}
			files["apps/consumer/build.gradle"] += "dependencies { implementation project (path: ':core') }\n"
			if kotlin {
				delete(files, "apps/consumer/build.gradle")
				files["apps/consumer/build.gradle.kts"] = "plugins { `java-library` }\nrepositories { mavenCentral() }\ndependencies { testImplementation(\"junit:junit:4.13.2\"); implementation(project (path = \":core\")) }\n"
			}
			dir := tree(t, files)
			command := "gradle test --no-daemon --console=plain --max-workers=2 --rerun-tasks --continue"
			liveCommand(t, dir, command, true)
			liveWrite(t, dir, "components/core/src/main/java/probe/Core.java", "package probe; public class Core { public static int value() { return 2; } }")
			fullOutput := liveCommand(t, dir, command, false)
			plan := sel(t, dir, command, "components/core/src/main/java/probe/Core.java")
			t.Logf("plan: %+v", plan)
			if plan.Full || len(plan.Run) != 1 || strings.Contains(plan.Run[0], ":other:test") {
				t.Fatalf("expected core and consumer tasks: %+v", plan)
			}
			checkAllowed(t, dir, command, "", plan)
			narrowOutput := liveCommand(t, dir, plan.Run[0], false)
			for _, out := range []string{fullOutput, narrowOutput} {
				if !strings.Contains(out, "ConsumerTest > detectsCoreBug FAILED") {
					t.Fatalf("missing expected failing test:\n%s", out)
				}
			}
			if !strings.Contains(fullOutput, "> Task :other:test") || strings.Contains(narrowOutput, "> Task :other:test") {
				t.Fatalf("expected unrelated task only in full output\nfull:\n%s\nnarrow:\n%s", fullOutput, narrowOutput)
			}
			if !kotlin {
				liveWrite(t, dir, "apps/consumer/build.gradle", "plugins { id 'java-library' }\nrepositories { mavenCentral() }\ndef target = ':core'\ndependencies { testImplementation 'junit:junit:4.13.2'; implementation project(target) }\n")
				plan = sel(t, dir, command, "components/core/src/main/java/probe/Core.java")
				if !plan.Full {
					t.Fatalf("expected fallback for computed dependency: %+v", plan)
				}
				if out := liveCommand(t, dir, plan.Command, false); !strings.Contains(out, "ConsumerTest > detectsCoreBug FAILED") {
					t.Fatalf("missing failure with computed dependency:\n%s", out)
				}
			}
		})
	}
}
