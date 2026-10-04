package io.github.sparkz400.switchyard.core

import java.io.Closeable
import java.io.IOException
import java.io.InputStream
import java.io.InputStreamReader
import java.net.URI
import java.net.http.HttpClient
import java.net.http.HttpRequest
import java.net.http.HttpResponse
import java.nio.charset.StandardCharsets
import java.time.Duration
import java.util.concurrent.atomic.AtomicBoolean

// sy web's API, spoken like a browser tab speaks it: a single-use
// bootstrap is traded for a session (POST /api/session), and every /api
// request carries the session in X-Switchyard-Session. The client sends no
// Origin (it is not a web page), no cookies and goes through no proxy; sy
// checks the Host header and the session on every request. Only a loopback
// address is ever contacted.

const val SESSION_HEADER = "X-Switchyard-Session"

class ApiException(val status: Int, message: String) : IOException(message)

class SyApi(base: String, private val reauth: () -> String) : Closeable {
    private val base: URI

    init {
        val u = try {
            URI(base)
        } catch (e: Exception) {
            throw IllegalArgumentException("refusing a bad sy address: $base")
        }
        if (u.scheme != "http" || u.host !in setOf("127.0.0.1", "localhost", "[::1]") || u.port <= 0 ||
            !(u.path.isNullOrEmpty()) || u.query != null || u.userInfo != null
        ) {
            throw IllegalArgumentException("refusing a non-loopback sy address: $base")
        }
        this.base = u
    }

    private val client: HttpClient = HttpClient.newBuilder()
        .version(HttpClient.Version.HTTP_1_1)
        .proxy(HttpClient.Builder.NO_PROXY)
        .followRedirects(HttpClient.Redirect.NEVER)
        .connectTimeout(Duration.ofSeconds(10))
        .build()

    private val lock = Any()

    @Volatile
    private var session = ""

    val hasSession: Boolean get() = session.isNotEmpty()

    private fun uri(path: String): URI {
        require(path.startsWith("/api/")) { "not an API path: $path" }
        return URI.create(base.toString() + path)
    }

    private fun request(path: String, withSession: Boolean): HttpRequest.Builder {
        val b = HttpRequest.newBuilder(uri(path)).header("Accept", "application/json")
        val s = session
        if (withSession && s.isNotEmpty()) b.header(SESSION_HEADER, s)
        return b
    }

    private class Raw(val status: Int, val body: String)

    private fun raw(method: String, path: String, body: String?, withSession: Boolean): Raw {
        val b = request(path, withSession).timeout(Duration.ofSeconds(30))
        if (body != null) {
            b.header("Content-Type", "application/json")
            b.method(method, HttpRequest.BodyPublishers.ofString(body, StandardCharsets.UTF_8))
        } else {
            b.method(method, HttpRequest.BodyPublishers.noBody())
        }
        val res = try {
            client.send(b.build(), HttpResponse.BodyHandlers.ofString(StandardCharsets.UTF_8))
        } catch (e: InterruptedException) {
            Thread.currentThread().interrupt()
            throw IOException("interrupted")
        } catch (e: java.net.http.HttpTimeoutException) {
            throw IOException("sy did not answer in time")
        } catch (e: java.net.ConnectException) {
            throw IOException("could not reach sy at $base (is it still running?)")
        }
        return Raw(res.statusCode(), res.body() ?: "")
    }

    /** Trades a bootstrap for a session. */
    fun login(bootstrap: String) {
        val r = raw("POST", "/api/session", Json.write(mapOf("bootstrap" to bootstrap)), false)
        if (r.status != 200) throw ApiException(r.status, errorText(r.status, r.body))
        val s = try {
            Json.parse(r.body).asObj().str("session")
        } catch (e: JsonException) {
            ""
        }
        if (s.isEmpty()) throw IOException("sy returned no session")
        session = s
    }

    /** Logs in again with a fresh bootstrap, once for all callers that saw the same dropped session. */
    private fun refreshSession(stale: String) {
        synchronized(lock) {
            if (session != stale && session.isNotEmpty()) return // someone else already did
            login(reauth())
        }
    }

    /** A JSON request; it logs in again once when the session was dropped. Returns the decoded body. */
    fun call(method: String, path: String, body: Any? = null): Any? {
        val data = if (body == null) (if (method == "POST") "{}" else null) else Json.write(body)
        var used = session
        var r = raw(method, path, data, true)
        if (r.status == 401) {
            refreshSession(used)
            used = session
            r = raw(method, path, data, true)
        }
        if (r.status !in 200..299) throw ApiException(r.status, errorText(r.status, r.body))
        if (r.body.isBlank()) return null
        return try {
            Json.parse(r.body)
        } catch (e: JsonException) {
            throw IOException("sy sent a bad answer to $path: ${e.message}")
        }
    }

    /**
     * Follows the event stream on its own thread until the returned handle
     * is closed. It reconnects after a dropped connection (sy then sends a
     * reset and the replay again) and logs in again when the session was
     * dropped. onMessage and onStatus run on the stream's thread.
     */
    fun stream(onMessage: (SseMessage) -> Unit, onStatus: (connected: Boolean, error: String?) -> Unit): Closeable {
        val closed = AtomicBoolean(false)
        var current: InputStream? = null
        val t = Thread({
            val parser = SseParser(onMessage)
            var refused = 0
            while (!closed.get()) {
                var err: String? = null
                var wait = true
                try {
                    val used = session
                    val req = request("/api/events", true).setHeader("Accept", "text/event-stream").GET().build()
                    val res = client.send(req, HttpResponse.BodyHandlers.ofInputStream())
                    val body = res.body()
                    when (res.statusCode()) {
                        200 -> {
                            refused = 0
                            synchronized(closed) { current = body }
                            if (closed.get()) {
                                body.close()
                                break
                            }
                            onStatus(true, null)
                            InputStreamReader(body, StandardCharsets.UTF_8).use { rd ->
                                val buf = CharArray(16 * 1024)
                                while (!closed.get()) {
                                    val n = rd.read(buf)
                                    if (n < 0) break
                                    parser.push(java.nio.CharBuffer.wrap(buf, 0, n))
                                }
                            }
                        }
                        401 -> {
                            body.close()
                            refreshSession(used)
                            // Reconnect at once the first time, then wait.
                            wait = refused++ > 0
                            if (wait) err = "the session was refused again"
                        }
                        else -> {
                            val text = body.use { String(it.readNBytes(2000), StandardCharsets.UTF_8) }
                            err = "event stream: " + errorText(res.statusCode(), text)
                        }
                    }
                } catch (e: InterruptedException) {
                    break
                } catch (e: Exception) {
                    if (closed.get()) break
                    err = e.message ?: e.javaClass.simpleName
                }
                if (closed.get()) break
                onStatus(false, err)
                parser.reset()
                if (wait) {
                    try {
                        Thread.sleep(parser.retry ?: 1500L)
                    } catch (e: InterruptedException) {
                        break
                    }
                }
            }
        }, "Switchyard event stream")
        t.isDaemon = true
        t.start()
        return Closeable {
            if (closed.compareAndSet(false, true)) {
                synchronized(closed) {
                    try {
                        current?.close()
                    } catch (_: IOException) {
                    }
                }
                t.interrupt()
            }
        }
    }

    override fun close() {
        client.shutdownNow()
    }
}

/** The server's {"error": "..."} text, or the status and the start of the body. */
fun errorText(status: Int, body: String): String {
    try {
        val e = Json.parse(body).asObj().str("error")
        if (e.isNotEmpty()) return e
    } catch (_: JsonException) {
        // not JSON
    }
    val b = body.trim().take(200)
    return "HTTP $status" + if (b.isNotEmpty()) ": $b" else ""
}
