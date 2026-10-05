package io.github.sparkz400.switchyard.it

import io.github.sparkz400.switchyard.classPathOf
import io.github.sparkz400.switchyard.core.Json
import io.github.sparkz400.switchyard.core.asObj
import org.junit.Assume
import java.io.File
import java.time.LocalDateTime
import java.time.format.DateTimeFormatter
import java.util.concurrent.TimeUnit

/**
 * The scratch world of an integration test: a real sy (SY_EXE, or built
 * from this repository with `go build ./cmd/sy`), its config and state in
 * scratch APPDATA/XDG folders, the scripted Claude CLI (FakeClaude) as its
 * only agent, and a git repository for it to work in. Nothing touches your
 * own sy state or quota.
 *
 * Without SY_EXE and without Go the tests are skipped, unless SY_IT_REQUIRED=1
 * (CI), which makes that a failure.
 */
class ItEnv(name: String, repoDir: File? = null) {
    val run: File = File(base, "run-" + LocalDateTime.now().format(DateTimeFormatter.ofPattern("MMdd-HHmmss-SSS")) + "-" + name)
    val state = File(run, "fake-agent")

    /** A folder name with a space and a non-ASCII letter, like many real ones (or the test project's folder). */
    val repo = repoDir ?: File(run, "workspace/my project ä")
    val sy: String = syExe()
    val env: Map<String, String>

    init {
        val roaming = File(run, "appdata/Roaming")
        val local = File(run, "appdata/Local")
        val xdg = File(run, "appdata/config")
        val home = File(run, "home")
        for (d in listOf(roaming, local, xdg, home, state, repo)) d.mkdirs()
        val java = ProcessHandle.current().info().command().get()
        val cp = listOf(FakeClaude::class.java, Json::class.java, Unit::class.java).map { classPathOf(it) }.distinct().joinToString(File.pathSeparator)
        val shim = File(run, if (WINDOWS) "bin/claude.cmd" else "bin/claude")
        shim.parentFile.mkdirs()
        if (WINDOWS) {
            shim.writeText("@\"$java\" -cp \"$cp\" ${FakeClaude::class.java.name} \"${state.path}\" %*\r\n")
        } else {
            shim.writeText("#!/bin/sh\nexec '$java' -cp '$cp' ${FakeClaude::class.java.name} '${state.path}' \"$@\"\n")
            shim.setExecutable(true)
        }
        val cfgDir = File(if (WINDOWS) roaming else xdg, "switchyard")
        cfgDir.mkdirs()
        File(cfgDir, "switchyard.yaml").writeText(
            listOf(
                "providers:",
                "  claude:",
                "    command: ${Json.write(shim.path)}",
                "  codex:",
                "    disabled: true",
                "orchestrator:",
                "  approve_plan: true",
                "  review_changes: true",
                "  max_cpu_percent: 0        # the test machine may be busy: never wait for it",
                "  min_free_memory_mb: 0",
                "  min_free_disk_gb: 0",
                "notify:",
                "  enabled: false",
                "",
            ).joinToString("\n"),
        )
        val e = HashMap(System.getenv())
        if (WINDOWS) {
            e["APPDATA"] = roaming.path
            e["LOCALAPPDATA"] = local.path
        } else {
            e["XDG_CONFIG_HOME"] = xdg.path
            e["XDG_CACHE_HOME"] = File(run, "appdata/cache").path
            // macOS keeps user folders under $HOME/Library, whatever XDG says.
            if (System.getProperty("os.name").startsWith("Mac")) e["HOME"] = home.path
        }
        env = e

        // The workspace: a git repo. Windows keeps Git for Windows' default
        // (core.autocrlf=true: CRLF in the working tree, LF in git's diffs).
        git("init", "-q", "-b", "main")
        git("config", "user.name", "sy jetbrains test")
        git("config", "user.email", "jetbrains-test@localhost")
        git("config", "commit.gpgsign", "false")
        git("config", "core.autocrlf", if (WINDOWS) "true" else "false")
        val eol = if (WINDOWS) "\r\n" else "\n"
        File(repo, Fake.NOTES).writeText(ORIG_NOTES.joinToString(eol) + eol)
        File(repo, "README.md").writeText("# test project$eol")
        git("add", "-A")
        git("commit", "-q", "-m", "base")
    }

    fun git(vararg args: String) {
        val p = ProcessBuilder(listOf("git") + args).directory(repo).redirectErrorStream(true).start()
        val out = p.inputStream.readBytes().toString(Charsets.UTF_8)
        if (!p.waitFor(60, TimeUnit.SECONDS) || p.exitValue() != 0) throw IllegalStateException("git ${args.joinToString(" ")}: $out")
    }

    /** Removes the run's scratch folder (kept when SY_JB_TEST_KEEP=1, and when a test failed). */
    fun cleanup() {
        if (System.getenv("SY_JB_TEST_KEEP") != "1") run.deleteRecursively()
    }

    /** The scripted agent's calls so far. */
    fun calls(): List<Map<String, Any?>> {
        val f = File(state, "calls.log")
        if (!f.exists()) return emptyList()
        return f.readLines().filter { it.isNotBlank() }.map { Json.parse(it).asObj() }
    }

    fun hangPid(): Long? = File(state, "hang.pid").takeIf { it.exists() }?.readText()?.trim()?.toLongOrNull()

    fun clearHang() {
        File(state, "hang.pid").delete()
    }

    /** A file in the repository with LF line ends, or null. */
    fun readRepo(rel: String): String? = File(repo, rel).takeIf { it.exists() }?.readText()?.replace("\r\n", "\n")

    /** sy processes started from this test's sy executable that are still running. */
    fun syProcesses(): List<ProcessHandle> {
        val want = File(sy).canonicalPath
        return ProcessHandle.allProcesses().filter { p ->
            p.info().command().map { c -> runCatching { File(c).canonicalPath.equals(want, ignoreCase = WINDOWS) }.getOrDefault(false) }.orElse(false)
        }.toList()
    }

    companion object {
        val WINDOWS = System.getProperty("os.name").startsWith("Windows")
        val ORIG_NOTES = List(Fake.NOTES_LINES) { "line ${it + 1}" }
        val base: File = File(System.getenv("SY_JB_TEST_DIR") ?: File(System.getProperty("java.io.tmpdir"), "sy-jetbrains-test").path)
        private var built: String? = null

        /** The sy to test: SY_EXE, or one built from this repository (once per test JVM). */
        @Synchronized
        fun syExe(): String {
            built?.let { return it }
            System.getenv("SY_EXE")?.takeIf { it.isNotBlank() }?.let {
                built = File(it).absolutePath
                return built!!
            }
            val root = File(System.getProperty("user.dir")).absoluteFile.parentFile.parentFile
            val out = File(base, if (WINDOWS) "sy.exe" else "sy")
            base.mkdirs()
            val ok = try {
                val p = ProcessBuilder("go", "build", "-o", out.path, "./cmd/sy").directory(root).redirectErrorStream(true).start()
                val log = p.inputStream.readBytes().toString(Charsets.UTF_8)
                if (!p.waitFor(10, TimeUnit.MINUTES) || p.exitValue() != 0) throw IllegalStateException("go build failed:\n$log")
                true
            } catch (e: java.io.IOException) {
                false // no Go on PATH
            }
            if (!ok) {
                if (System.getenv("SY_IT_REQUIRED") == "1") throw IllegalStateException("SY_IT_REQUIRED=1 but there is no SY_EXE and no Go to build sy")
                Assume.assumeTrue("no SY_EXE and no Go: the integration tests against a real sy are skipped", false)
            }
            built = out.path
            return out.path
        }

        fun <T : Any> waitFor(what: String, ms: Long = 60_000, fn: () -> T?): T {
            val end = System.currentTimeMillis() + ms
            var last: Throwable? = null
            while (true) {
                try {
                    val v = fn()
                    if (v != null && v != false) return v
                } catch (e: Throwable) {
                    last = e
                }
                if (System.currentTimeMillis() > end) throw AssertionError("timed out waiting for $what" + (last?.let { ": $it" } ?: ""))
                Thread.sleep(150)
            }
        }
    }
}
