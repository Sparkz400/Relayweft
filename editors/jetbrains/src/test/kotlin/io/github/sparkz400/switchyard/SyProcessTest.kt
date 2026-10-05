package io.github.sparkz400.switchyard

import io.github.sparkz400.switchyard.core.Json
import io.github.sparkz400.switchyard.core.SyNotFoundException
import io.github.sparkz400.switchyard.core.SyProcess
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File
import java.io.IOException
import java.nio.file.Files
import java.util.Collections
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

/** SyProcess against FakeSy, a scripted `sy web --client` run with this JVM's java. */
class SyProcessTest {
    private val java = ProcessHandle.current().info().command().get()

    // Only what FakeSy needs (the full test classpath is too long for a
    // Windows command line).
    private val cp = listOf(FakeSy::class.java, Json::class.java, Unit::class.java)
        .map { classPathOf(it) }
        .distinct()
        .joinToString(File.pathSeparator)

    private val stderr: MutableList<String> = Collections.synchronizedList(ArrayList())

    private fun start(vararg args: String, timeoutMs: Long = 30_000): SyProcess =
        SyProcess.start(java, listOf("-cp", cp, FakeSy::class.java.name) + args, File("."), { stderr.add(it) }, helloTimeoutMs = timeoutMs)

    @Test
    fun readsTheHelloAnswersCommandsAndStopsWhenStdinCloses() {
        val p = start("ok")
        assertEquals("fake", p.hello.version)
        assertEquals(FakeSy.BOOTSTRAP, p.hello.bootstrap)
        assertTrue(p.request("link").str("link").startsWith("http://127.0.0.1:9/#b="))
        assertEquals(FakeSy.BOOTSTRAP, p.request("bootstrap").str("bootstrap"))
        var code = -99
        val exited = CountDownLatch(1)
        p.onExit {
            code = it
            exited.countDown()
        }
        p.stop(10_000)
        assertFalse(p.running)
        assertTrue(exited.await(5, TimeUnit.SECONDS))
        assertEquals(0, code)
        assertTrue(stderr.joinToString("\n"), stderr.any { it.contains("stdin closed") })
        assertThrows(IOException::class.java) { p.request("link") }
    }

    private fun Map<String, Any?>.str(k: String) = this[k] as String

    @Test
    fun argumentsArriveUnchangedOnEveryPlatform() {
        // Spaces, inner quotes and trailing backslashes: Java's ProcessBuilder
        // mangles some of them on Windows unless they are quoted first.
        val args = listOf("C:\\My Projects\\a b", "worker=codex:gpt \"x\"", "a\"b", "trailing\\", "two  spaces", "--flag=")
        val p = start("ok", *args.toTypedArray())
        try {
            assertEquals(args, Json.parse(p.hello.dir))
        } finally {
            p.stop(5_000)
        }
    }

    @Test
    fun killsSyAndItsChildrenWhenStdinClosingDoesNotStopIt() {
        val pidFile = Files.createTempFile("fake-sy-child", ".pid").toFile()
        val p = start("stubborn", pidFile.path)
        val child = ProcessHandle.of(pidFile.readText().trim().toLong()).get()
        assertTrue(child.isAlive)
        val t0 = System.currentTimeMillis()
        p.stop(1_500)
        assertFalse("sy still runs", p.running)
        assertTrue(System.currentTimeMillis() - t0 >= 1_400)
        val end = System.currentTimeMillis() + 10_000
        while (child.isAlive && System.currentTimeMillis() < end) Thread.sleep(50)
        assertFalse("sy's child outlived it", child.isAlive)
    }

    @Test
    fun anExitBeforeTheHelloShowsSysLastErrorLines() {
        val e = assertThrows(IOException::class.java) { start("crash") }
        assertTrue(e.message, e.message!!.contains("exited (code 3) before it was ready"))
        assertTrue(e.message, e.message!!.contains("switchyard.yaml: line 3: bad indentation"))
    }

    @Test
    fun aFirstLineThatIsNotAHelloIsRefusedAndSyIsStopped() {
        val e = assertThrows(IOException::class.java) { start("noise") }
        assertTrue(e.message, e.message!!.contains("not the client hello"))
    }

    @Test
    fun aMissingHelloTimesOutAndSyIsStopped() {
        val e = assertThrows(IOException::class.java) { start("sleep", timeoutMs = 1_500) }
        assertTrue(e.message, e.message!!.contains("did not start within"))
    }

    @Test
    fun stopWhileStartingCancelsTheStartAndStopsSy() {
        // Review fix: Stop could not cancel a start (up to the 60 s hello timeout).
        val t0 = System.currentTimeMillis()
        assertThrows(io.github.sparkz400.switchyard.core.StartCancelledException::class.java) {
            SyProcess.start(java, listOf("-cp", cp, FakeSy::class.java.name, "sleep"), File("."), {}, cancelled = { System.currentTimeMillis() - t0 > 500 })
        }
        assertTrue(System.currentTimeMillis() - t0 < 10_000)
    }

    @Test
    fun aRelativeExecutableIsRefused() {
        // Review fix: a relative path would be resolved against the project folder.
        val e = assertThrows(IOException::class.java) { SyProcess.start("bin/sy", listOf("web"), File("."), {}) }
        assertTrue(e.message, e.message!!.contains("absolute"))
    }

    @Test
    fun aMissingExecutableSaysNotFound() {
        val missing = File(Files.createTempDirectory("no-sy").toFile(), if (System.getProperty("os.name").startsWith("Windows")) "sy.exe" else "sy")
        assertThrows(SyNotFoundException::class.java) {
            SyProcess.start(missing.path, listOf("web", "--client"), File("."), {})
        }
    }
}

/** The jar or class folder a class was loaded from. */
fun classPathOf(c: Class<*>): String {
    val res = "/" + c.name.replace('.', '/') + ".class"
    val url = c.getResource(res)?.toString() ?: throw IllegalStateException("cannot find $res")
    return when {
        url.startsWith("jar:") -> File(java.net.URI(url.removePrefix("jar:").substringBefore("!/"))).path
        url.startsWith("file:") -> File(java.net.URI(url.removeSuffix(res))).path
        else -> throw IllegalStateException("cannot use $url on a class path")
    }
}
