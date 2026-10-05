package io.github.sparkz400.switchyard

import com.sun.net.httpserver.HttpExchange
import com.sun.net.httpserver.HttpServer
import io.github.sparkz400.switchyard.core.ApiException
import io.github.sparkz400.switchyard.core.Json
import io.github.sparkz400.switchyard.core.SESSION_HEADER
import io.github.sparkz400.switchyard.core.SseMessage
import io.github.sparkz400.switchyard.core.SyApi
import io.github.sparkz400.switchyard.core.asObj
import io.github.sparkz400.switchyard.core.str
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.OutputStream
import java.net.InetAddress
import java.net.InetSocketAddress
import java.util.Collections
import java.util.concurrent.CopyOnWriteArrayList
import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.TimeUnit

/**
 * SyApi against a fake sy web that checks what internal/web/security.go
 * checks: the Host header, the session on every /api route, JSON bodies,
 * and single-use bootstraps.
 */
class SyApiTest {
    private class Req(val method: String, val path: String, val headers: Map<String, List<String>>, val body: String)

    private class Fake {
        val server: HttpServer = HttpServer.create(InetSocketAddress(InetAddress.getLoopbackAddress(), 0), 0)
        val port get() = server.address.port
        val bootstraps: MutableSet<String> = Collections.synchronizedSet(HashSet())
        val sessions: MutableSet<String> = Collections.synchronizedSet(HashSet())
        val requests = CopyOnWriteArrayList<Req>()
        val streams = LinkedBlockingQueue<OutputStream>()
        var seq = 0

        fun bootstrap(): String = "b${++seq}".padEnd(64, '0').also { bootstraps.add(it) }

        init {
            server.createContext("/") { ex -> handle(ex) }
            server.executor = java.util.concurrent.Executors.newCachedThreadPool { r -> Thread(r).apply { isDaemon = true } }
            server.start()
        }

        private fun reply(ex: HttpExchange, code: Int, v: Any?) {
            val b = Json.write(v).toByteArray()
            ex.responseHeaders.add("Content-Type", "application/json")
            ex.sendResponseHeaders(code, b.size.toLong())
            ex.responseBody.use { it.write(b) }
        }

        private fun handle(ex: HttpExchange) {
            val body = ex.requestBody.readBytes().toString(Charsets.UTF_8)
            val path = ex.requestURI.path
            requests.add(Req(ex.requestMethod, path, ex.requestHeaders.mapKeys { it.key.lowercase() }, body))
            if (ex.requestHeaders.getFirst("Host") != "127.0.0.1:$port") return reply(ex, 403, mapOf("error" to "forbidden host"))
            if (ex.requestHeaders.getFirst("Origin") != null) return reply(ex, 403, mapOf("error" to "cross-origin request refused"))
            if (ex.requestMethod == "POST" && ex.requestHeaders.getFirst("Content-Type")?.startsWith("application/json") != true) {
                return reply(ex, 415, mapOf("error" to "want Content-Type: application/json"))
            }
            if (path == "/api/session") {
                val b = Json.parse(body).asObj().str("bootstrap")
                if (!bootstraps.remove(b)) return reply(ex, 401, mapOf("error" to "this link is not valid for this sy (was sy restarted?)"))
                val s = "s${++seq}"
                sessions.add(s)
                return reply(ex, 200, mapOf("session" to s))
            }
            if (ex.requestHeaders.getFirst(SESSION_HEADER) !in sessions) return reply(ex, 401, mapOf("error" to "no session"))
            when (path) {
                "/api/state" -> reply(ex, 200, mapOf("version" to "fake"))
                "/api/task" -> reply(ex, 200, mapOf("status" to "started", "message" to "started: " + Json.parse(body).asObj().str("text")))
                "/api/broken" -> reply(ex, 500, mapOf("error" to "the orchestrator is still busy - try again in a moment"))
                "/api/events" -> {
                    ex.responseHeaders.add("Content-Type", "text/event-stream")
                    ex.sendResponseHeaders(200, 0)
                    val out = ex.responseBody
                    out.write("retry: 100\n\nevent: state\ndata: {\"version\":\"fake\"}\n\nevent: reset\ndata: {}\n\n".toByteArray())
                    out.write("event: ev\ndata: {\"kind\":\"log\",\"text\":\"héllo\"}\n\nevent: synced\ndata: {}\n\n".toByteArray())
                    out.flush()
                    streams.add(out)
                }
                else -> reply(ex, 404, mapOf("error" to "not found"))
            }
        }
    }

    private val fake = Fake()
    private val url get() = "http://127.0.0.1:${fake.port}"

    @After
    fun stop() {
        fake.server.stop(0)
    }

    @Test
    fun logsInWithTheBootstrapAndSendsTheSessionOnEveryRequest() {
        val api = SyApi(url) { fake.bootstrap() }
        api.login(fake.bootstrap())
        assertEquals("fake", api.call("GET", "/api/state").asObj().str("version"))
        assertEquals("started: hi ä", api.call("POST", "/api/task", mapOf("text" to "hi ä")).asObj().str("message"))
        val task = fake.requests.last()
        assertEquals("application/json", task.headers["content-type"]?.first())
        assertEquals("{\"text\":\"hi ä\"}", task.body)
        for (r in fake.requests) {
            // Never an Origin (it is not a web page), never a cookie, always our Host.
            assertNull(r.headers["origin"])
            assertNull(r.headers["cookie"])
            assertEquals("127.0.0.1:${fake.port}", r.headers["host"]?.first())
        }
        assertNull(fake.requests.first { it.path == "/api/session" }.headers[SESSION_HEADER.lowercase()])
        api.close()
    }

    @Test
    fun aDroppedSessionLogsInAgainOnceWithAFreshBootstrap() {
        var asked = 0
        val api = SyApi(url) {
            asked++
            fake.bootstrap()
        }
        api.login(fake.bootstrap())
        fake.sessions.clear() // sy forgot the session (e.g. 32 newer pages)
        assertEquals("fake", api.call("GET", "/api/state").asObj().str("version"))
        assertEquals(1, asked)
        // A used bootstrap is refused: the error says so.
        val used = fake.bootstrap()
        api.login(used)
        val e = assertThrows(ApiException::class.java) { api.login(used) }
        assertEquals(401, e.status)
        assertTrue(e.message!!.contains("not valid"))
        api.close()
    }

    @Test
    fun serverErrorsCarrySysMessage() {
        val api = SyApi(url) { fake.bootstrap() }
        api.login(fake.bootstrap())
        val e = assertThrows(ApiException::class.java) { api.call("POST", "/api/broken") }
        assertEquals(500, e.status)
        assertEquals("the orchestrator is still busy - try again in a moment", e.message)
        api.close()
    }

    @Test
    fun refusesAddressesThatAreNotLoopback() {
        for (bad in listOf("http://10.0.0.1:1", "https://127.0.0.1:1", "http://127.0.0.1.evil.com:1", "http://127.0.0.1:1/x", "http://u@127.0.0.1:1", "http://127.0.0.1")) {
            assertThrows(bad, IllegalArgumentException::class.java) { SyApi(bad) { "" } }
        }
        assertThrows(IllegalArgumentException::class.java) { SyApi(url) { "" }.call("GET", "http://evil/") }
    }

    @Test
    fun ignoresTheSystemProxy() {
        // An IDE or a machine with an HTTP proxy configured must not send
        // sy's session to the proxy.
        val old = System.getProperty("http.proxyHost") to System.getProperty("http.proxyPort")
        System.setProperty("http.proxyHost", "192.0.2.1")
        System.setProperty("http.proxyPort", "9")
        try {
            val api = SyApi(url) { fake.bootstrap() }
            api.login(fake.bootstrap())
            assertEquals("fake", api.call("GET", "/api/state").asObj().str("version"))
            api.close()
        } finally {
            if (old.first == null) System.clearProperty("http.proxyHost") else System.setProperty("http.proxyHost", old.first!!)
            if (old.second == null) System.clearProperty("http.proxyPort") else System.setProperty("http.proxyPort", old.second!!)
        }
    }

    @Test
    fun theEventStreamReplaysReconnectsAndLogsInAgain() {
        val api = SyApi(url) { fake.bootstrap() }
        api.login(fake.bootstrap())
        val got = LinkedBlockingQueue<SseMessage>()
        val status = LinkedBlockingQueue<Boolean>()
        val h = api.stream({ got.add(it) }, { ok, _ -> status.add(ok) })
        fun next(): SseMessage = got.poll(10, TimeUnit.SECONDS) ?: throw AssertionError("no message")
        assertEquals(true, status.poll(10, TimeUnit.SECONDS))
        assertEquals(listOf("state", "reset", "ev", "synced"), List(4) { next().event })
        // The connection drops (sy restarted its stream): the client reconnects and gets the replay again.
        fake.streams.poll(10, TimeUnit.SECONDS)!!.close()
        assertEquals(false, status.poll(10, TimeUnit.SECONDS))
        assertEquals(true, status.poll(10, TimeUnit.SECONDS))
        assertEquals("state", next().event)
        assertEquals("reset", next().event)
        assertEquals("{\"kind\":\"log\",\"text\":\"héllo\"}", next().data)
        assertEquals("synced", next().event)
        // The session was dropped: the stream logs in again by itself.
        fake.sessions.clear()
        fake.streams.poll(10, TimeUnit.SECONDS)!!.close()
        assertEquals("state", next().event)
        val events = fake.requests.filter { it.path == "/api/events" }
        assertTrue(events.size >= 3)
        assertTrue(events.all { it.headers["accept"]?.first() == "text/event-stream" && it.headers[SESSION_HEADER.lowercase()] != null })
        h.close()
        api.close()
    }
}
