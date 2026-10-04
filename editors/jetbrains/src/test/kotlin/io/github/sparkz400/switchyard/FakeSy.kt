package io.github.sparkz400.switchyard

import io.github.sparkz400.switchyard.core.Json
import java.io.File
import java.util.concurrent.TimeUnit

/**
 * A stand-in for `sy web --client` (SyProcessTest starts it with the test
 * JVM's java): it prints a hello line and answers stdin commands like
 * cmd/sy/webclient.go, or misbehaves on purpose.
 *
 *   FakeSy ok <args...>        hello (the args echoed in "dir"), answers link/bootstrap, exits when stdin closes
 *   FakeSy stubborn <pidfile>  hello, starts a child, ignores stdin closing (must be killed)
 *   FakeSy crash               writes an error to stderr and exits 3 before the hello
 *   FakeSy noise               prints something that is not a hello
 *   FakeSy sleep               sleeps (the stubborn one's child)
 */
object FakeSy {
    const val BOOTSTRAP = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

    private fun hello(dir: String) {
        println(
            Json.write(
                mapOf(
                    "switchyard" to "web-client", "protocol" to 1, "version" to "fake", "url" to "http://127.0.0.1:9",
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
                System.err.println("sy web --client: stdin closed, stopping")
            }
            "stubborn" -> {
                val java = ProcessHandle.current().info().command().get()
                val child = ProcessBuilder(java, "-cp", System.getProperty("java.class.path"), FakeSy::class.java.name, "sleep")
                    .redirectErrorStream(true).start()
                File(args[1]).writeText(child.pid().toString())
                hello("stubborn")
                // Never reads stdin: closing it does not stop this one.
                Thread.sleep(TimeUnit.MINUTES.toMillis(10))
            }
            "crash" -> {
                System.err.println("loading config")
                System.err.println("error: switchyard.yaml: line 3: bad indentation")
                System.exit(3)
            }
            "noise" -> {
                println("Switchyard web UI on http://127.0.0.1:9")
                System.out.flush()
                Thread.sleep(TimeUnit.MINUTES.toMillis(10))
            }
            "sleep" -> Thread.sleep(TimeUnit.MINUTES.toMillis(10))
            else -> System.exit(2)
        }
    }
}
