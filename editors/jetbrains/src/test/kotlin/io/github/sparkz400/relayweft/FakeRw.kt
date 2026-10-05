package io.github.sparkz400.relayweft

import io.github.sparkz400.relayweft.core.Json
import java.io.File
import java.util.concurrent.TimeUnit

/**
 * A stand-in for `rw web --client` (RwProcessTest starts it with the test
 * JVM's java): it prints a hello line and answers stdin commands like
 * cmd/rw/webclient.go, or misbehaves on purpose.
 *
 *   FakeRw ok <args...>        hello (the args echoed in "dir"), answers link/bootstrap, exits when stdin closes
 *   FakeRw stubborn <pidfile>  hello, starts a child, ignores stdin closing (must be killed)
 *   FakeRw crash               writes an error to stderr and exits 3 before the hello
 *   FakeRw noise               prints something that is not a hello
 *   FakeRw sleep               sleeps (the stubborn one's child)
 */
object FakeRw {
    const val BOOTSTRAP = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

    private fun hello(dir: String) {
        println(
            Json.write(
                mapOf(
                    "relayweft" to "web-client", "protocol" to 1, "version" to "fake", "url" to "http://127.0.0.1:9",
                    "addr" to "127.0.0.1:9", "bootstrap" to BOOTSTRAP, "dir" to dir, "pid" to ProcessHandle.current().pid(),
                ),
            ),
        )
        System.out.flush()
    }

    @JvmStatic
    fun main(args: Array<String>) {
        when (args.firstOrNull()) {
            "ok" -> {
                hello(Json.write(args.drop(1)))
                val r = System.`in`.bufferedReader()
                while (true) {
                    val l = r.readLine() ?: break
                    val out = when (l.trim()) {
                        "link" -> mapOf("link" to "http://127.0.0.1:9/#b=$BOOTSTRAP")
                        "bootstrap" -> mapOf("bootstrap" to BOOTSTRAP)
                        else -> mapOf("error" to "unknown command \"${l.trim()}\" (want link or bootstrap)")
                    }
                    println(Json.write(out))
                    System.out.flush()
                }
                System.err.println("rw web --client: stdin closed, stopping")
            }
            "stubborn" -> {
                val java = ProcessHandle.current().info().command().get()
                val child = ProcessBuilder(java, "-cp", System.getProperty("java.class.path"), FakeRw::class.java.name, "sleep")
                    .redirectErrorStream(true).start()
                File(args[1]).writeText(child.pid().toString())
                hello("stubborn")
                // Never reads stdin: closing it does not stop this one.
                Thread.sleep(TimeUnit.MINUTES.toMillis(10))
            }
            "crash" -> {
                System.err.println("loading config")
                System.err.println("error: relayweft.yaml: line 3: bad indentation")
                System.exit(3)
            }
            "noise" -> {
                println("Relayweft web UI on http://127.0.0.1:9")
                System.out.flush()
                Thread.sleep(TimeUnit.MINUTES.toMillis(10))
            }
            "sleep" -> Thread.sleep(TimeUnit.MINUTES.toMillis(10))
            else -> System.exit(2)
        }
    }
}
