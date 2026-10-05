package io.github.sparkz400.switchyard.core

import java.io.BufferedReader
import java.io.File
import java.io.IOException
import java.io.InputStreamReader
import java.nio.charset.StandardCharsets
import java.util.concurrent.CompletableFuture
import java.util.concurrent.CopyOnWriteArrayList
import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.TimeUnit
import java.util.concurrent.TimeoutException
import java.util.concurrent.atomic.AtomicBoolean

// The `sy web --client` child process: started without a shell (the
// arguments go to the program as they are), talked to through JSON lines
// on stdin/stdout, and stopped by closing its stdin (sy then cancels the
// running task and stops its agents like on Ctrl+C), with a kill of the
// process and its children as the last resort. If the IDE itself dies, the
// operating system closes the pipe and sy stops the same way.

class SyNotFoundException(message: String) : IOException(message)

/** The start was cancelled (Stop while sy started); sy was stopped. */
class StartCancelledException : IOException("the start was cancelled")

class SyProcess private constructor(
    private val process: Process,
    val hello: ClientHello,
    private val replies: LinkedBlockingQueue<String>,
) {
    private val exitListeners = CopyOnWriteArrayList<(Int) -> Unit>()
    private val requestLock = Any()

    init {
        process.onExit().thenAccept { p ->
            replies.offer(EXITED)
            val code = try {
                p.exitValue()
            } catch (_: IllegalThreadStateException) {
                -1
            }
            for (l in exitListeners) l(code)
        }
    }

    val pid: Long get() = process.pid()
    val running: Boolean get() = process.isAlive

    /** Called with the exit code once sy has exited (also when it already has). */
    fun onExit(fn: (Int) -> Unit) {
        val once = AtomicBoolean(false)
        val w: (Int) -> Unit = { c -> if (once.compareAndSet(false, true)) fn(c) }
        exitListeners.add(w)
        if (!process.isAlive) w(process.exitValue())
    }

    /** Sends a command ("link" or "bootstrap") and returns sy's JSON answer. */
    fun request(cmd: String, timeoutMs: Long = REPLY_TIMEOUT_MS): Map<String, Any?> {
        require(cmd == "link" || cmd == "bootstrap") { "unknown command $cmd" }
        synchronized(requestLock) {
            if (!process.isAlive) throw IOException("sy is not running")
            replies.removeIf { it != EXITED } // late answers of a request that timed out
            try {
                val w = process.outputStream
                w.write("$cmd\n".toByteArray(StandardCharsets.UTF_8))
                w.flush()
            } catch (e: IOException) {
                throw IOException("sy is not reading its input any more (${e.message})")
            }
            val line = replies.poll(timeoutMs, TimeUnit.MILLISECONDS) ?: throw IOException("sy did not answer")
            if (line == EXITED) {
                replies.offer(EXITED)
                throw IOException("sy stopped")
            }
            val v = try {
                Json.parse(line).asObj()
            } catch (e: JsonException) {
                throw IOException("sy answered something unexpected: ${line.take(200)}")
            }
            v.strOrNull("error")?.let { throw IOException(it) }
            return v
        }
    }

    /**
     * Stops sy: closes its stdin (a clean stop: the running task is
     * cancelled and its agents stopped) and kills sy and its children if it
     * is still running after graceMs. Blocks until sy has exited or was killed.
     */
    fun stop(graceMs: Long = 10_000) {
        closeInput()
        if (waitFor(graceMs)) return
        kill()
        waitFor(5_000)
    }

    /** Closes sy's stdin without waiting: sy stops on its own. */
    fun closeInput() {
        try {
            process.outputStream.close()
        } catch (_: IOException) {
        }
    }

    fun waitFor(ms: Long): Boolean = try {
        process.waitFor(ms, TimeUnit.MILLISECONDS)
    } catch (_: InterruptedException) {
        Thread.currentThread().interrupt()
        !process.isAlive
    }

    /** Kills sy and every process it started (the last resort). */
    fun kill() {
        val kids = try {
            process.descendants().toList()
        } catch (_: Exception) {
            emptyList()
        }
        process.destroyForcibly()
        for (k in kids) k.destroyForcibly()
    }

    companion object {
        const val HELLO_TIMEOUT_MS = 60_000L
        const val REPLY_TIMEOUT_MS = 10_000L
        private const val EXITED = "\u0000exited"

        /**
         * Starts sy and waits for its hello line. stderr lines go to onStderr
         * (sy's human-readable messages and warnings), on a reader thread.
         * env, when given, replaces the environment (tests).
         */
        fun start(
            exe: String,
            args: List<String>,
            cwd: File,
            onStderr: (String) -> Unit,
            env: Map<String, String>? = null,
            helloTimeoutMs: Long = HELLO_TIMEOUT_MS,
            windows: Boolean = System.getProperty("os.name").startsWith("Windows"),
            /** Polled while waiting for the hello: true stops sy and throws (Stop pressed while it starts). */
            cancelled: () -> Boolean = { false },
        ): SyProcess {
            // Only an absolute path: a relative one would be resolved against cwd (the project).
            if (!File(exe).isAbsolute) throw IOException("could not start $exe: the sy path must be absolute")
            val cmd = listOf(File(exe).absolutePath) + if (windows) args.map { windowsArg(it) } else args
            val pb = ProcessBuilder(cmd).directory(cwd)
            if (env != null) {
                pb.environment().clear()
                pb.environment().putAll(env)
            }
            val p = try {
                pb.start()
            } catch (e: IOException) {
                val m = e.message ?: ""
                if (m.contains("error=2") || m.contains("No such file") || m.contains("cannot find")) {
                    throw SyNotFoundException("could not start $exe: not found")
                }
                throw IOException("could not start $exe: $m")
            }
            val errTail = ArrayDeque<String>()
            val errDone = CompletableFuture<Unit>()
            daemon("Switchyard sy stderr") {
                try {
                    BufferedReader(InputStreamReader(p.errorStream, StandardCharsets.UTF_8)).use { r ->
                        while (true) {
                            val l = r.readLine() ?: break
                            synchronized(errTail) {
                                errTail.addLast(l)
                                if (errTail.size > 20) errTail.removeFirst()
                            }
                            try {
                                onStderr(l)
                            } catch (_: Exception) {
                            }
                        }
                    }
                } catch (_: IOException) {
                } finally {
                    errDone.complete(Unit)
                }
            }
            val hello = CompletableFuture<String>()
            val replies = LinkedBlockingQueue<String>()
            daemon("Switchyard sy stdout") {
                try {
                    BufferedReader(InputStreamReader(p.inputStream, StandardCharsets.UTF_8)).use { r ->
                        var first = true
                        while (true) {
                            val l = r.readLine() ?: break
                            if (first) {
                                first = false
                                hello.complete(l)
                            } else {
                                replies.offer(l)
                            }
                        }
                    }
                } catch (_: IOException) {
                } finally {
                    hello.complete("") // no hello: sy exited or closed stdout
                }
            }
            fun failWith(e: Exception): Nothing {
                if (p.isAlive) {
                    p.descendants().forEach { it.destroyForcibly() }
                    p.destroyForcibly()
                }
                throw e
            }
            val deadline = System.currentTimeMillis() + helloTimeoutMs
            var line: String? = null
            while (line == null) {
                if (cancelled()) failWith(StartCancelledException())
                line = try {
                    hello.get(minOf(200L, maxOf(1L, deadline - System.currentTimeMillis())), TimeUnit.MILLISECONDS)
                } catch (e: TimeoutException) {
                    if (System.currentTimeMillis() >= deadline) failWith(IOException("sy did not start within ${helloTimeoutMs / 1000} seconds"))
                    null
                } catch (e: InterruptedException) {
                    Thread.currentThread().interrupt()
                    failWith(IOException("interrupted while starting sy"))
                }
            }
            if (line.isEmpty()) {
                p.waitFor(5, TimeUnit.SECONDS)
                try {
                    errDone.get(2, TimeUnit.SECONDS)
                } catch (_: Exception) {
                }
                val why = synchronized(errTail) { errTail.filter { it.isNotBlank() }.takeLast(6).joinToString("\n") }
                val code = if (p.isAlive) "still running" else "code ${p.exitValue()}"
                failWith(IOException("sy exited ($code) before it was ready" + if (why.isNotEmpty()) ":\n$why" else ""))
            }
            val h = try {
                parseHello(line)
            } catch (e: IllegalStateException) {
                failWith(IOException(e.message))
            }
            return SyProcess(p, h, replies)
        }

        private fun daemon(name: String, body: () -> Unit) {
            val t = Thread(body, name)
            t.isDaemon = true
            t.start()
        }
    }
}
