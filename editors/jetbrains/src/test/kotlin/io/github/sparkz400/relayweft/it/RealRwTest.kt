package io.github.sparkz400.relayweft.it

import io.github.sparkz400.relayweft.core.ApprovalRequest
import io.github.sparkz400.relayweft.core.Json
import io.github.sparkz400.relayweft.core.PlanDraft
import io.github.sparkz400.relayweft.core.Selection
import io.github.sparkz400.relayweft.core.StateView
import io.github.sparkz400.relayweft.core.RwApi
import io.github.sparkz400.relayweft.core.RwEvent
import io.github.sparkz400.relayweft.core.RwProcess
import io.github.sparkz400.relayweft.core.TaskModel
import io.github.sparkz400.relayweft.core.AgentStatus
import io.github.sparkz400.relayweft.core.asObj
import io.github.sparkz400.relayweft.core.clientArgs
import io.github.sparkz400.relayweft.core.decisionBody
import io.github.sparkz400.relayweft.core.documentText
import io.github.sparkz400.relayweft.core.hunkAt
import io.github.sparkz400.relayweft.core.messageOf
import io.github.sparkz400.relayweft.core.readProjectFile
import io.github.sparkz400.relayweft.core.reconstruct
import io.github.sparkz400.relayweft.core.setHunk
import io.github.sparkz400.relayweft.core.str
import io.github.sparkz400.relayweft.it.ItEnv.Companion.ORIG_NOTES
import io.github.sparkz400.relayweft.it.ItEnv.Companion.WINDOWS
import io.github.sparkz400.relayweft.it.ItEnv.Companion.waitFor
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.Closeable
import java.io.File
import java.net.Socket
import java.util.Collections

/**
 * The plugin's client code (RwProcess, RwApi, the event stream, the task
 * model, plan drafts and the hunk mapping) against a real `rw web --client`
 * whose agent is the scripted CLI in FakeClaude: plan editing, hunk review
 * on a CRLF checkout (Windows), follow-up, feedback, reject, cancel and
 * stop, and no process left afterwards. The same flow as the VS Code
 * extension's integration suite.
 */
class RealRwTest {
    private val task = "Please edit the notes file in this repository and add a new file next to it with a greeting"

    private class Client(val proc: RwProcess, val api: RwApi) : Closeable {
        @Volatile
        var state: StateView? = null

        @Volatile
        var connected = false
        val model = TaskModel()
        val log: MutableList<String> = Collections.synchronizedList(ArrayList())
        private val stream = api.stream({ m ->
            synchronized(model) {
                when (m.event) {
                    "state" -> state = StateView.from(Json.parse(m.data))
                    "reset" -> model.reset()
                    "ev" -> model.fold(RwEvent.from(Json.parse(m.data)))?.let { log.add(it) }
                }
            }
        }, { ok, _ -> connected = ok })

        fun approval(type: String, not: String? = null): ApprovalRequest? = state?.approvals?.firstOrNull { it.type == type && it.id != not }

        fun idle(): StateView? = state?.takeIf { !it.running && it.last != null }

        override fun close() = stream.close()
    }

    private val stderr: MutableList<String> = Collections.synchronizedList(ArrayList())

    private fun start(env: ItEnv): Client {
        val proc = RwProcess.start(env.rw, clientArgs(env.repo.path, emptyList()), env.repo, { stderr.add(it) }, env = env.env)
        val api = RwApi(proc.hello.url) { proc.request("bootstrap").str("bootstrap") }
        api.login(proc.hello.bootstrap)
        return Client(proc, api)
    }

    /** One raw HTTP/1.1 request, so the test can send what a browser or another site would. */
    private fun raw(port: Int, method: String, path: String, headers: Map<String, String>, body: String = ""): Pair<Int, String> {
        Socket("127.0.0.1", port).use { s ->
            s.soTimeout = 10_000
            val b = body.toByteArray()
            val req = StringBuilder("$method $path HTTP/1.1\r\n")
            for ((k, v) in headers) req.append("$k: $v\r\n")
            req.append("Content-Length: ${b.size}\r\nConnection: close\r\n\r\n")
            s.getOutputStream().write(req.toString().toByteArray() + b)
            val text = s.getInputStream().readBytes().toString(Charsets.UTF_8)
            return text.substringAfter(' ').substringBefore(' ').toInt() to text.substringAfter("\r\n\r\n")
        }
    }

    @Test
    fun theWholeFlowAgainstARealRw() {
        val env = ItEnv("flow")
        val c = start(env)
        try {
            flow(env, c)
        } catch (e: Throwable) {
            println("--- rw stderr ---\n" + stderr.joinToString("\n"))
            println("--- log ---\n" + c.log.joinToString("\n"))
            println("--- fake agent calls ---\n" + env.calls().joinToString("\n") { Json.write(it) })
            throw e
        } finally {
            c.close()
            if (c.proc.running) c.proc.stop(5_000)
            c.api.close()
        }
        // No rw (or agent) outlives the stop.
        waitFor("no rw process left", 15_000) { env.rwProcesses().isEmpty() }
        env.cleanup()
    }

    private fun flow(env: ItEnv, c: Client) {
        val h = c.proc.hello
        assertEquals(File(env.repo.path).canonicalPath.lowercase(), File(h.dir).canonicalPath.lowercase())
        waitFor("the event stream") { c.connected }
        val st = waitFor("the first state") { c.state }
        assertFalse(st.demo)

        // rw's security checks are as they were: a bootstrap is single use,
        // and a foreign Origin or Host is refused even with a valid session.
        val port = h.url.substringAfterLast(':').toInt()
        val host = "127.0.0.1:$port"
        val bs = c.proc.request("bootstrap").str("bootstrap")
        val json = mapOf("Host" to host, "Content-Type" to "application/json")
        val (code, body) = raw(port, "POST", "/api/session", json, Json.write(mapOf("bootstrap" to bs)))
        assertEquals(body, 200, code)
        val session = Json.parse(body).asObj().str("session")
        assertEquals(401, raw(port, "POST", "/api/session", json, Json.write(mapOf("bootstrap" to bs))).first)
        assertEquals(200, raw(port, "GET", "/api/state", mapOf("Host" to host, "X-Relayweft-Session" to session)).first)
        assertEquals(401, raw(port, "GET", "/api/state", mapOf("Host" to host)).first)
        assertEquals(403, raw(port, "GET", "/api/state", mapOf("Host" to host, "X-Relayweft-Session" to session, "Origin" to "http://evil.example")).first)
        assertEquals(403, raw(port, "GET", "/api/state", mapOf("Host" to "evil.example:$port", "X-Relayweft-Session" to session)).first)
        assertTrue(Regex("""^http://127\.0\.0\.1:\d+/#b=[0-9a-f]{64}$""").matches(c.proc.request("link").str("link")))

        // A task: the plan waits; delete the explore step and edit the other one's prompt.
        assertTrue(messageOf(c.api.call("POST", "/api/task", mapOf("text" to task))).startsWith("started"))
        val planReq = waitFor("a plan approval") { c.approval("plan") }
        val draft = PlanDraft(planReq.plan!!)
        assertEquals(listOf("look", "edit"), draft.subtasks.map { it.id })
        draft.delete(0)
        draft.update(0, draft[0].copy(prompt = draft[0].prompt.replace(Fake.PLAN_TEXT, Fake.EDITED_PLAN_TEXT)))
        val ans = c.api.call("POST", "/api/approvals/${planReq.id}/plan", mapOf("ok" to true, "plan" to draft.toPlan().toJson()))
        assertEquals("plan approved: 1 subtasks", messageOf(ans))

        // The change review: the hunks map onto the file on disk (CRLF on Windows).
        val req = waitFor("a change review", 120_000) { c.approval("changes") }
        val files = req.changes!!.files
        assertEquals(listOf("added.txt", Fake.NOTES), files.map { it.path }.sorted())
        val ni = files.indexOfFirst { it.path == Fake.NOTES }
        val notes = files[ni]
        assertTrue("notes.txt should be splittable", notes.splittable)
        assertEquals(2, notes.hunks.size)
        val disk = readProjectFile(env.repo.path, Fake.NOTES)!!
        if (WINDOWS) assertTrue("a core.autocrlf checkout has CRLF", disk.contains("\r\n"))
        val rec = reconstruct(notes.patch, notes.status, disk)
        assertTrue("the diff fell back to hunks only", rec.whole)
        val after = documentText(rec.after).split("\n")
        for (n in Fake.EDITED_LINES) assertEquals(Fake.edited(n), after[n - 1])
        assertEquals(ORIG_NOTES.joinToString("\n") + "\n", documentText(rec.before))
        val second = hunkAt(rec.ranges, Fake.EDITED_LINES[1] - 1)
        assertEquals(1, second)
        val sel = Selection(files)
        setHunk(files, sel, ni, second, false)
        val decision = decisionBody(files, sel)
        assertEquals(mapOf(Fake.NOTES to listOf(0)), decision.hunks)
        val applied = c.api.call("POST", "/api/approvals/${req.id}/changes", decision.toJson())
        assertEquals("applying 2 file(s), 1 of them partly (selected hunks)", messageOf(applied))
        val done = waitFor("the task to finish", 120_000) { c.idle() }
        assertTrue("task failed: " + done.last?.text, done.last?.ok == true)
        val want = ORIG_NOTES.toMutableList()
        want[Fake.EDITED_LINES[0] - 1] = Fake.edited(Fake.EDITED_LINES[0])
        assertEquals("only the accepted hunk is in notes.txt", want.joinToString("\n") + "\n", env.readRepo(Fake.NOTES))
        assertEquals("the edited plan reached the agent", Fake.EDITED_PLAN_TEXT + "\n", env.readRepo("added.txt"))
        assertFalse("the deleted explore step ran", env.calls().any { it["what"] == "explore" })
        synchronized(c.model) {
            assertEquals(AgentStatus.OK, c.model.nodes["edit"]?.status)
            assertFalse(c.model.nodes.containsKey("look"))
        }
        val afterTask1 = env.readRepo(Fake.NOTES)

        // A follow-up with the agent that edited.
        val n = env.calls().size
        assertTrue(messageOf(c.api.call("POST", "/api/task", mapOf("text" to "@edit please also double-check line three"))).isNotEmpty())
        waitFor("the follow-up call") { env.calls().drop(n).firstOrNull { (it["prompt"] as String).contains("double-check line three") } }
        val fu = env.calls().drop(n).first { (it["prompt"] as String).contains("double-check line three") }
        // rw resumes the edit agent's session (in its pool worktree since
        // ROADMAP 2.5, where Claude keys sessions by folder); when that is not
        // possible, a fresh agent gets the earlier agent's context instead.
        val editCall = env.calls().first { (it["what"] as String).startsWith("edit ") }
        if (fu["resumed"] != "") {
            assertEquals("the follow-up resumed another session", editCall["sid"], fu["resumed"])
            assertEquals(File(editCall["cwd"] as String).canonicalPath, File(fu["cwd"] as String).canonicalPath)
        } else {
            assertTrue("the fresh agent did not get the earlier context", (fu["prompt"] as String).contains("edit the notes"))
        }
        waitFor("the follow-up to finish", 120_000) { c.state?.running == false }

        // Feedback sends the changes back; the second round is rejected.
        c.api.call("POST", "/api/task", mapOf("text" to "$task (second round)"))
        val p2 = waitFor("a plan approval") { c.approval("plan") }
        c.api.call("POST", "/api/approvals/${p2.id}/plan", mapOf("ok" to true, "plan" to p2.plan!!.toJson()))
        val r1 = waitFor("a change review", 120_000) { c.approval("changes") }
        assertEquals(1, r1.changes!!.round)
        assertEquals(
            "sent back to the agent with your feedback",
            messageOf(c.api.call("POST", "/api/approvals/${r1.id}/changes", mapOf("feedback" to "use a friendlier greeting"))),
        )
        val r2 = waitFor("the second round", 120_000) { c.approval("changes", not = r1.id) }
        assertEquals(2, r2.changes!!.round)
        assertTrue("the agent did not get the feedback", env.calls().any { (it["prompt"] as String).contains("use a friendlier greeting") })
        assertEquals(
            "all changes rejected (kept on a branch)",
            messageOf(c.api.call("POST", "/api/approvals/${r2.id}/changes", mapOf("apply" to emptyList<String>(), "hunks" to emptyMap<String, Any>()))),
        )
        waitFor("the task to finish", 120_000) { c.idle()?.takeIf { it.approvals.isEmpty() } }
        assertEquals("rejected changes must not land", afterTask1, env.readRepo(Fake.NOTES))

        // A rejected plan cancels the task: no agent works.
        val n3 = env.calls().size
        c.api.call("POST", "/api/task", mapOf("text" to "$task (third time)"))
        val p3 = waitFor("a plan approval") { c.approval("plan") }
        assertEquals(
            "plan not approved: cancelling the task",
            messageOf(c.api.call("POST", "/api/approvals/${p3.id}/plan", mapOf("ok" to false, "plan" to mapOf("summary" to "", "subtasks" to emptyList<Any>())))),
        )
        waitFor("the task to end", 60_000) { c.idle()?.takeIf { it.approvals.isEmpty() } }
        assertFalse("an agent ran after the plan was rejected", env.calls().drop(n3).any { (it["what"] as String).startsWith("edit") })

        // Cancel kills the running agent; rw keeps running.
        env.clearHang()
        c.api.call("POST", "/api/task", mapOf("text" to "${Fake.HANG} wait here"))
        val agent = waitFor("the hanging agent") { env.hangPid() }
        waitFor("the task to run") { c.state?.running }
        assertTrue(messageOf(c.api.call("POST", "/api/cancel")).startsWith("cancelling"))
        waitFor("the task to stop", 30_000) { c.state?.running == false }
        waitFor("the agent process to exit", 15_000) { ProcessHandle.of(agent).map { !it.isAlive }.orElse(true) }
        assertTrue(c.proc.running)

        // Stop (stdin closed): rw and its working agent exit.
        env.clearHang()
        c.api.call("POST", "/api/task", mapOf("text" to "${Fake.HANG} and again"))
        val agent2 = waitFor("the hanging agent") { env.hangPid() }
        c.proc.stop(10_000)
        assertFalse("rw still runs after stop", c.proc.running)
        waitFor("the agent to exit with rw", 15_000) { ProcessHandle.of(agent2).map { !it.isAlive }.orElse(true) }
    }
}
