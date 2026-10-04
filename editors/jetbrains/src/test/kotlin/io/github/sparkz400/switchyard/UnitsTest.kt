package io.github.sparkz400.switchyard

import io.github.sparkz400.switchyard.core.FileView
import io.github.sparkz400.switchyard.core.FollowUp
import io.github.sparkz400.switchyard.core.Json
import io.github.sparkz400.switchyard.core.JsonException
import io.github.sparkz400.switchyard.core.LocateEnv
import io.github.sparkz400.switchyard.core.Plan
import io.github.sparkz400.switchyard.core.PlanDraft
import io.github.sparkz400.switchyard.core.Selection
import io.github.sparkz400.switchyard.core.SseMessage
import io.github.sparkz400.switchyard.core.SseParser
import io.github.sparkz400.switchyard.core.StateView
import io.github.sparkz400.switchyard.core.Subtask
import io.github.sparkz400.switchyard.core.SyEvent
import io.github.sparkz400.switchyard.core.TaskModel
import io.github.sparkz400.switchyard.core.AgentStatus
import io.github.sparkz400.switchyard.core.clientArgs
import io.github.sparkz400.switchyard.core.decisionBody
import io.github.sparkz400.switchyard.core.locateSy
import io.github.sparkz400.switchyard.core.parseFollowUp
import io.github.sparkz400.switchyard.core.parseHello
import io.github.sparkz400.switchyard.core.readProjectFile
import io.github.sparkz400.switchyard.core.resolveInside
import io.github.sparkz400.switchyard.core.selectionSummary
import io.github.sparkz400.switchyard.core.setFile
import io.github.sparkz400.switchyard.core.setHunk
import io.github.sparkz400.switchyard.core.splitArgLines
import io.github.sparkz400.switchyard.core.windowsArg
import io.github.sparkz400.switchyard.core.windowsQuote
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test
import java.nio.file.Files

class UnitsTest {
    @Test
    fun jsonRoundTripsWhatSySends() {
        val v = Json.parse("""{"a":[1,2.5,-3e2,true,false,null],"s":"x\"y\\z\n\u00e4\u2028","o":{}}""")
        val m = v as Map<*, *>
        assertEquals(listOf(1L, 2.5, -300.0, true, false, null), m["a"])
        assertEquals("x\"y\\z\nä\u2028", m["s"])
        assertEquals(emptyMap<String, Any>(), m["o"])
        val w = Json.parse("""{"a":[1,2.5,true,null,"ä"],"b":{"c":[]}}""")
        assertEquals(w, Json.parse(Json.write(w)))
        // Whole numbers are written without a fraction: Go refuses 1.0 for an int.
        assertEquals("[-300,2.5]", Json.write(listOf(-300.0, 2.5)))
        // What the plugin writes is plain JSON a strict parser accepts.
        assertEquals("""{"t":"a\"b\\c\n\u0001\u2028","n":[1,2]}""", Json.write(mapOf("t" to "a\"b\\c\n\u0001\u2028", "n" to listOf(1, 2))))
    }

    @Test
    fun jsonRefusesBrokenInput() {
        for (bad in listOf("", "{", "[1,]", "{\"a\" 1}", "\"\u0001\"", "tru", "1 2", "{\"a\":1}x")) {
            assertThrows(bad, JsonException::class.java) { Json.parse(bad) }
        }
    }

    @Test
    fun sseParserHandlesSplitChunksCrlfCommentsAndRetry() {
        val got = ArrayList<SseMessage>()
        val p = SseParser { got.add(it) }
        p.push("retry: 1500\n\n: ping\n\nevent: st")
        p.push("ate\ndata: {\"a\":1}\r")
        p.push("\n\r\nevent: ev\ndata: line1\ndata: line2\n\ndata: plain\n\n")
        assertEquals(1500L, p.retry)
        assertEquals(
            listOf(SseMessage("state", "{\"a\":1}"), SseMessage("ev", "line1\nline2"), SseMessage("message", "plain")),
            got,
        )
        // A dropped connection loses the half message, not the next one.
        p.push("event: ev\ndata: half")
        p.reset()
        p.push("event: ev\ndata: whole\n\n")
        assertEquals(SseMessage("ev", "whole"), got.last())
    }

    private val hex = "ab".repeat(32)

    @Test
    fun parseHelloAcceptsTheClientHelloAndRefusesAnythingElse() {
        val h = parseHello("""{"switchyard":"web-client","protocol":1,"version":"v1","url":"http://127.0.0.1:1234","addr":"127.0.0.1:1234","bootstrap":"$hex","dir":"C:\\x","pid":9}""")
        assertEquals("http://127.0.0.1:1234", h.url)
        assertEquals("C:\\x", h.dir)
        assertEquals(9L, h.pid)
        fun refused(line: String, want: String) {
            val e = assertThrows(IllegalStateException::class.java) { parseHello(line) }
            assertTrue("${e.message} should mention $want", e.message!!.contains(want))
        }
        refused("Switchyard web UI on http://127.0.0.1:1", "not the client hello")
        refused("""{"switchyard":"web-client","protocol":2,"url":"http://127.0.0.1:1","bootstrap":"$hex"}""", "protocol 2")
        refused("""{"switchyard":"web-client","protocol":1,"url":"http://10.0.0.1:1","bootstrap":"$hex"}""", "non-loopback")
        refused("""{"switchyard":"web-client","protocol":1,"url":"http://127.0.0.1.evil.com:1","bootstrap":"$hex"}""", "non-loopback")
        refused("""{"switchyard":"web-client","protocol":1,"url":"http://127.0.0.1:1/x","bootstrap":"$hex"}""", "non-loopback")
        refused("""{"switchyard":"web-client","protocol":1,"url":"http://127.0.0.1:1","bootstrap":"x"}""", "bootstrap")
        refused("""{"switchyard":"web-client","protocol":1,"url":"http://127.0.0.1:1","bootstrap":"${hex.uppercase()}"}""", "bootstrap")
        refused("[1]", "unexpected first line")
    }

    @Test
    fun clientArgsPassArgumentsAsTheyAre() {
        assertEquals(
            listOf("web", "--client", "--dir", "C:\\My Projects\\a b", "--threads", "2", "--route", "worker=codex:gpt \"x\""),
            clientArgs("C:\\My Projects\\a b", listOf("--threads", "2", "", "--route", "worker=codex:gpt \"x\"")),
        )
        assertEquals(listOf("--threads", "2", "--demo"), splitArgLines("--threads\r\n2\n\n  --demo  \n"))
    }

    @Test
    fun windowsArgumentsSurviveJavasCommandLine() {
        // ProcessBuilder quotes arguments with spaces itself; inner quotes
        // must be escaped here, or sy gets a mangled argument.
        assertEquals("plain", windowsArg("plain"))
        assertEquals("C:\\My Projects\\a b", windowsArg("C:\\My Projects\\a b"))
        assertEquals("\"worker=codex:gpt \\\"x\\\"\"", windowsArg("worker=codex:gpt \"x\""))
        assertEquals("\"a\\\"b\"", windowsArg("a\"b"))
        assertEquals("\"a\\\\\\\"b\"", windowsArg("a\\\"b"))
        assertEquals("\"\"", windowsQuote(""))
        assertEquals("\"a b\\\\\"", windowsQuote("a b\\"))
        assertThrows(IllegalArgumentException::class.java) { windowsArg("say \"hi\" C:\\dir\\") }
    }

    @Test
    fun locateSyOnWindowsWantsAnExeOnPathThenTheGoAndScoopFolders() {
        val files = hashSetOf("C:\\tools\\sy.cmd", "C:\\Users\\me\\go\\bin\\sy.exe")
        val le = LocateEnv(true, mapOf("Path" to "C:\\tools;\"C:\\Program Files\\x\""), "C:\\Users\\me") { it in files }
        assertEquals("C:\\Users\\me\\go\\bin\\sy.exe", locateSy("", le).path)
        files.add("C:\\Program Files\\x\\sy.exe")
        assertEquals("C:\\Program Files\\x\\sy.exe", locateSy("", le).path)
        assertNull(locateSy("D:\\sy\\sy", le).path)
        files.add("D:\\sy\\sy.exe")
        assertEquals("D:\\sy\\sy.exe", locateSy("D:\\sy\\sy", le).path)
        assertEquals("C:\\Users\\me\\go\\bin\\sy.exe", locateSy("~\\go\\bin\\sy.exe", le).path)
        // WinGet's links folder.
        val le2 = LocateEnv(true, mapOf("LOCALAPPDATA" to "C:\\Users\\me\\AppData\\Local"), "C:\\Users\\me") {
            it == "C:\\Users\\me\\AppData\\Local\\Microsoft\\WinGet\\Links\\sy.exe"
        }
        assertEquals("C:\\Users\\me\\AppData\\Local\\Microsoft\\WinGet\\Links\\sy.exe", locateSy("", le2).path)
    }

    @Test
    fun locateSyOnUnix() {
        val files = hashSetOf("/home/me/go/bin/sy")
        val le = LocateEnv(false, mapOf("PATH" to "/usr/bin:/bin"), "/home/me") { it in files }
        val r = locateSy("", le)
        assertEquals("/home/me/go/bin/sy", r.path)
        assertTrue(r.tried.contains("/usr/bin/sy"))
        assertNull(locateSy("/opt/sy", le).path)
        files.add("/usr/bin/sy")
        assertEquals("/usr/bin/sy", locateSy("", le).path)
    }

    @Test
    fun parseFollowUpMatchesTheServer() {
        assertEquals(FollowUp("s1", "fix the test"), parseFollowUp("@s1 fix the test"))
        assertEquals(FollowUp("", "again"), parseFollowUp("@ again"))
        assertEquals(FollowUp("", "more"), parseFollowUp("@last more"))
        assertNull(parseFollowUp("@types/node is missing"))
        assertNull(parseFollowUp("plain task"))
    }

    private fun fv(path: String, splittable: Boolean, hunks: Int) =
        FileView(path, "M", 1, 1, false, "", splittable, "", List(hunks) { "h$it" })

    @Test
    fun decisionBodyMirrorsTheWebPage() {
        val files = listOf(fv("a.go", true, 3), fv("b.go", false, 0), fv("c.go", true, 2))
        val sel = Selection(files)
        assertEquals(listOf("a.go", "b.go", "c.go"), decisionBody(files, sel).apply)
        assertEquals(emptyMap<String, List<Int>>(), decisionBody(files, sel).hunks)
        setHunk(files, sel, 0, 1, false)
        assertFalse(setHunk(files, sel, 1, 0, false))
        setHunk(files, sel, 2, 0, false)
        setHunk(files, sel, 2, 1, false)
        val b = decisionBody(files, sel)
        assertEquals(listOf("a.go"), b.apply)
        assertEquals(mapOf("a.go" to listOf(0, 2)), b.hunks)
        assertEquals("1 of 3 file(s), 1 partly", selectionSummary(files, sel))
        setFile(files, sel, 2, true)
        assertEquals(listOf("a.go", "c.go"), decisionBody(files, sel).apply)
        assertEquals("""{"apply":["a.go","c.go"],"hunks":{"a.go":[0,2]}}""", Json.write(decisionBody(files, sel).toJson()))
    }

    @Test
    fun taskModelFoldsEventsIntoATreeAndLogLines() {
        val m = TaskModel()
        val ts = "2026-10-04T12:00:00.123456789+02:00"
        assertEquals("task: do it", m.fold(SyEvent(kind = "task_start", text = "do it", ts = ts)))
        m.fold(SyEvent(kind = "started", agentId = "main", provider = "claude", model = "opus", ts = ts))
        m.fold(SyEvent(kind = "done", agentId = "main", ok = true, ts = ts))
        m.fold(SyEvent(kind = "queued", agentId = "s1", parentId = "main", role = "worker", text = "step one", ts = ts))
        m.fold(SyEvent(kind = "started", agentId = "s1", provider = "codex", model = "gpt-5", ts = ts))
        m.fold(SyEvent(kind = "started", agentId = "reviewer", provider = "claude", model = "opus", ts = ts))
        m.fold(SyEvent.from(Json.parse("""{"kind":"usage","agent_id":"s1","tokens":{"input":100,"cached":40,"output":10},"ts":"$ts"}""")))
        assertTrue(m.fold(SyEvent(kind = "edit", agentId = "s1", text = "a.go", ts = ts))!!.matches(Regex("s1.*edit: a\\.go")))
        assertEquals(AgentStatus.RUNNING, m.nodes["s1"]!!.status)
        assertEquals(70L, m.nodes["s1"]!!.tokens)
        assertEquals(listOf("s1", "reviewer"), m.children("main").map { it.id })
        m.fold(SyEvent(kind = "done", agentId = "s1", ok = true, ts = ts))
        assertEquals("failed: failed", m.fold(SyEvent(kind = "task_done", ok = false, text = "failed", ts = ts)))
        assertEquals(AgentStatus.OK, m.nodes["s1"]!!.status)
        assertEquals(AgentStatus.KILLED, m.nodes["reviewer"]!!.status)
        assertEquals(AgentStatus.FAILED, m.nodes["main"]!!.status)
        assertEquals(1791108000123L, io.github.sparkz400.switchyard.core.eventMillis(SyEvent(kind = "x", ts = ts)))
    }

    @Test
    fun stateViewDecodesTheServersSnapshot() {
        val st = StateView.from(
            Json.parse(
                """{"version":"v0.3","dir":"D:\\p","project":"p","demo":false,"running":true,"cancelling":false,"paused":false,
                "phase":"working","task":"t","queue":[{"id":2,"label":"next","kind":"task"}],
                "approvals":[{"id":"r1","type":"changes","created":"x","changes":{"step_id":"edit","title":"T","round":1,
                  "files":[{"path":"a.txt","status":"M","added":1,"deleted":1,"patch":"@@ -1 +1 @@\n-a\n+b\n","splittable":false}]}}],
                "running_agents":null,"last":{"ok":true,"text":"done","took":"3s"},"extra":{"ignored":true}}""",
            ),
        )
        assertTrue(st.running)
        assertEquals(1, st.queue.size)
        assertEquals("edit", st.approvals[0].changes!!.stepId)
        assertEquals(emptyList<String>(), st.runningAgents)
        assertEquals("3s", st.last!!.took)
    }

    @Test
    fun planDraftEditsLikeTheWebPage() {
        val plan = Plan(
            "s",
            listOf(
                Subtask("look", "look", "explore", "p1"),
                Subtask("edit", "edit", "edit", "p2", files = listOf("a"), dependsOn = listOf("look")),
                Subtask("test", "test", "", "p3", dependsOn = listOf("edit", "look")),
            ),
            listOf("repo-a", "repo-b"),
        )
        val d = PlanDraft(plan)
        assertEquals("edit", d[2].kind) // an empty kind reads as edit
        assertTrue(d.move(2, 0))
        assertEquals(listOf("test", "look", "edit"), d.subtasks.map { it.id })
        assertFalse(d.move(0, 5))
        d.delete(1) // look: the dependencies on it go too
        assertEquals(listOf("test", "edit"), d.subtasks.map { it.id })
        assertEquals(listOf("edit"), d[0].dependsOn)
        assertEquals(emptyList<String>(), d[1].dependsOn)
        // The id stays; a dependency on itself or an unknown id is dropped.
        d.update(1, d[1].copy(id = "renamed", title = "new title", dependsOn = listOf("edit", "ghost", "test")))
        assertEquals("edit", d[1].id)
        assertEquals("new title", d[1].title)
        assertEquals(listOf("test"), d[1].dependsOn)
        val i = d.add()
        assertEquals("step-3", d[i].id)
        assertNotNull(d.problem()) // the new subtask is empty
        d.update(i, d[i].copy(prompt = "do it"))
        assertNull(d.problem())
        val json = Json.write(d.toPlan().toJson())
        assertTrue(json, json.contains("\"repos\":[\"repo-a\",\"repo-b\"]"))
        assertTrue(json, json.contains("\"depends_on\":[]"))
        repeat(3) { d.delete(0) }
        assertEquals("No subtasks left: add one, or reject the plan to cancel the task.", d.problem())
    }

    @Test
    fun projectFilesAreNeverReadOutsideTheProject() {
        val root = Files.createTempDirectory("sy-jb-test")
        val dir = Files.createDirectory(root.resolve("proj"))
        Files.writeString(dir.resolve("in.txt"), "inside\r\n")
        Files.writeString(root.resolve("secret.txt"), "outside")
        Files.createDirectories(dir.resolve("sub"))
        Files.writeString(dir.resolve("sub/x.txt"), "x")
        assertEquals("inside\r\n", readProjectFile(dir.toString(), "in.txt"))
        assertEquals("x", readProjectFile(dir.toString(), "sub/x.txt"))
        assertNull(readProjectFile(dir.toString(), "../secret.txt"))
        assertNull(readProjectFile(dir.toString(), "sub/../../secret.txt"))
        assertNull(readProjectFile(dir.toString(), root.resolve("secret.txt").toString()))
        assertNull(readProjectFile(dir.toString(), ""))
        assertNull(readProjectFile(dir.toString(), "missing.txt"))
        assertNull(resolveInside(dir.toString(), "."))
        // A symbolic link that points out of the project (when the OS lets us make one).
        val link = dir.resolve("link.txt")
        val made = try {
            Files.createSymbolicLink(link, root.resolve("secret.txt"))
            true
        } catch (_: Exception) {
            false
        }
        if (made) assertNull(readProjectFile(dir.toString(), "link.txt"))
    }
}
