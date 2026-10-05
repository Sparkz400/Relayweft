package io.github.sparkz400.relayweft.it

import com.intellij.notification.Notification
import com.intellij.notification.Notifications
import com.intellij.openapi.editor.Editor
import com.intellij.openapi.editor.EditorFactory
import com.intellij.openapi.editor.markup.EffectType
import com.intellij.openapi.util.Disposer
import com.intellij.testFramework.HeavyPlatformTestCase
import com.intellij.testFramework.PlatformTestUtil
import com.intellij.ui.CheckedTreeNode
import io.github.sparkz400.relayweft.core.PlanDraft
import io.github.sparkz400.relayweft.ide.AgentsItem
import io.github.sparkz400.relayweft.ide.AgentsPanel
import io.github.sparkz400.relayweft.ide.ReviewItem
import io.github.sparkz400.relayweft.ide.ReviewPanel
import io.github.sparkz400.relayweft.ide.RwService
import io.github.sparkz400.relayweft.ide.RwSettings
import io.github.sparkz400.relayweft.it.ItEnv.Companion.ORIG_NOTES
import com.intellij.openapi.command.WriteCommandAction
import com.intellij.openapi.fileEditor.FileDocumentManager
import com.intellij.openapi.vfs.LocalFileSystem
import java.io.File
import java.util.Collections
import com.intellij.util.ui.tree.TreeUtil
import javax.swing.tree.DefaultMutableTreeNode
import javax.swing.tree.TreePath

/**
 * The plugin inside a headless IDE (the IntelliJ Platform test framework,
 * a real project on disk) against a real `rw web --client` with the
 * scripted agent: the service starts rw from the project folder, the
 * Agents and Review tabs show what rw sends, the plan is answered, a hunk
 * is rejected in the diff editor (gutter marks, strike-through), the
 * review is applied, a crash is noticed, and closing the project stops rw
 * and its working agent.
 */
class RwServiceIdeTest : HeavyPlatformTestCase() {
    private lateinit var env: ItEnv
    private lateinit var svc: RwService
    private val notes: MutableList<Notification> = Collections.synchronizedList(ArrayList())
    private val task = "Please edit the notes file in this repository and add a new file next to it with a greeting"

    override fun setUp() {
        super.setUp()
        val dir = File(project.basePath!!)
        env = ItEnv("ide", dir)
        RwSettings.get().state.rwPath = env.rw
        RwSettings.get().state.extraArgs = ""
        svc = RwService.get(project)
        svc.environment = env.env
        project.messageBus.connect(testRootDisposable).subscribe(Notifications.TOPIC, object : Notifications {
            override fun notify(notification: Notification) {
                notes.add(notification)
            }
        })
    }

    override fun tearDown() {
        try {
            if (svc.running) svc.stop(5_000).get()
        } finally {
            super.tearDown()
        }
    }

    /** Waits on the EDT, running queued events (the plugin's UI work) meanwhile. */
    private fun <T : Any> pumpUntil(what: String, ms: Long = 60_000, fn: () -> T?): T {
        val end = System.currentTimeMillis() + ms
        while (true) {
            PlatformTestUtil.dispatchAllInvocationEventsInIdeEventQueue()
            val v = try {
                fn()
            } catch (e: Exception) {
                null
            }
            if (v != null && v != false) return v
            if (System.currentTimeMillis() > end) {
                println("--- activity log ---\n" + svc.log.joinToString("\n"))
                println("--- notifications ---\n" + notes.joinToString("\n") { it.content })
                throw AssertionError("timed out waiting for $what")
            }
            Thread.sleep(50)
        }
    }

    private fun startRw() {
        svc.start()
        pumpUntil("rw to start and connect") { svc.running && svc.connected && svc.model.state != null }
    }

    private fun items(p: AgentsPanel): List<AgentsItem> {
        val out = ArrayList<AgentsItem>()
        fun walk(n: DefaultMutableTreeNode) {
            (n.userObject as? AgentsItem)?.let { out.add(it) }
            for (i in 0 until n.childCount) walk(n.getChildAt(i) as DefaultMutableTreeNode)
        }
        walk(p.tree.model.root as DefaultMutableTreeNode)
        return out
    }

    /** Review fix: Undo ran `rw undo` (and so git) in a project opened in safe mode. */
    fun testAnUntrustedProjectRunsNoRw() {
        svc.trustCheck = { false }
        val before = env.rwProcesses().size
        svc.undoLastTask()
        svc.start()
        PlatformTestUtil.dispatchAllInvocationEventsInIdeEventQueue()
        Thread.sleep(1_000)
        PlatformTestUtil.dispatchAllInvocationEventsInIdeEventQueue()
        assertFalse(svc.running || svc.starting)
        assertEquals(before, env.rwProcesses().size)
        assertEquals(2, notes.count { it.content.contains("not trusted") })
        assertFalse(svc.log.any { it.contains("rw undo") || it.startsWith("starting:") })
        assertFalse(svc.trusted())
        val ctx = com.intellij.openapi.actionSystem.impl.SimpleDataContext.getProjectContext(project)
        val ev = com.intellij.openapi.actionSystem.AnActionEvent.createEvent(ctx, null, "test", com.intellij.openapi.actionSystem.ActionUiKind.NONE, null)
        io.github.sparkz400.relayweft.ide.UndoLastTaskAction().update(ev)
        assertFalse("Undo must be off in an untrusted project", ev.presentation.isEnabled)
        svc.trustCheck = { true }
        io.github.sparkz400.relayweft.ide.UndoLastTaskAction().update(ev)
        assertTrue(ev.presentation.isEnabled)
        env.cleanup()
    }

    fun testTheWholeFlowInAHeadlessIde() {
        startRw()
        assertEquals(File(project.basePath!!).canonicalPath.lowercase(), File(svc.hello!!.dir).canonicalPath.lowercase())
        val agents = AgentsPanel(project, svc)
        Disposer.register(testRootDisposable, agents)
        val review = ReviewPanel(project, svc)
        Disposer.register(testRootDisposable, review)
        pumpUntil("the status row") { items(agents).firstOrNull() == AgentsItem.Status }

        // A task from the prompt's code path; the plan shows in the tree and is notified.
        svc.runTask(task)
        val plan = pumpUntil("a plan approval") { svc.approvals("plan").firstOrNull() }
        pumpUntil("the plan in the tree") { items(agents).any { it is AgentsItem.Approval && it.req.id == plan.id } }
        // A refresh with the same rows keeps the rows (and so a double click
        // in progress, hovers and the selection): found in a real IDE, where
        // every event reloaded the tree and the double click on a row was lost.
        val row = TreeUtil.findNode(agents.tree.model.root as DefaultMutableTreeNode) { (it.userObject as? AgentsItem.Approval)?.req?.id == plan.id }!!
        agents.tree.selectionPath = TreePath(row.path)
        agents.rebuild()
        assertSame(row, agents.tree.selectionPath?.lastPathComponent)
        assertTrue((row.userObject as AgentsItem.Approval).req.id == plan.id)
        pumpUntil("the plan notification") { notes.any { it.content.contains("Approve the plan (2 subtasks)") } }

        // Approve an edited plan (what the plan dialog sends).
        val draft = PlanDraft(plan.plan!!)
        val edit = draft.subtasks.indexOfFirst { it.id == "edit" }
        draft.update(edit, draft[edit].copy(prompt = draft[edit].prompt.replace(Fake.PLAN_TEXT, Fake.EDITED_PLAN_TEXT)))
        var answered: Boolean? = null
        svc.answerPlan(plan.id, draft.toPlan(), true) { answered = it }
        pumpUntil("the plan answer") { answered }
        pumpUntil("the plan to be gone") { svc.approvals("plan").isEmpty() }

        // The agents show in the tree while they work.
        pumpUntil("the edit agent in the tree") { items(agents).any { it is AgentsItem.Agent && it.id == "edit" } }

        // The change review: the Review tab and the diff editor.
        val r = pumpUntil("a change review", 120_000) { svc.review.pending.firstOrNull() }
        val id = r.req.id
        val ni = r.files.indexOfFirst { it.path == Fake.NOTES }
        assertTrue(r.files[ni].splittable)
        pumpUntil("the review notification") { notes.any { it.content.contains("Review the changes of edit") } }
        svc.review.openDiff(id, ni)
        val afterDoc = pumpUntil("the diff documents") { r.docs[ni]?.second?.document }
        assertTrue("the diff fell back to hunks only", r.recs[ni]!!.whole)
        val lines = afterDoc.text.split("\n")
        for (n in Fake.EDITED_LINES) assertEquals(Fake.edited(n), lines[n - 1])
        assertFalse(afterDoc.text.contains('\r'))
        // The diff viewer's editors, or one made here if the headless IDE shows none.
        var own: Editor? = null
        val editor = EditorFactory.getInstance().getEditors(afterDoc, project).firstOrNull()
            ?: EditorFactory.getInstance().createViewer(afterDoc, project).also { own = it }
        try {
            fun gutters() = editor.markupModel.allHighlighters.mapNotNull { it.gutterIconRenderer?.tooltipText }
            fun struck() = editor.markupModel.allHighlighters.filter { it.getTextAttributes(null)?.effectType == EffectType.STRIKEOUT }
            pumpUntil("two hunk marks") { gutters().size == 2 }
            assertEquals(listOf("Hunk 1/2 accepted: click to reject it", "Hunk 2/2 accepted: click to reject it"), gutters().sorted())
            assertTrue(struck().isEmpty())

            // Reject the second hunk with the caret in it (the diff toolbar's action).
            val line2 = Fake.EDITED_LINES[1] - 1
            assertEquals(
                "hunk 2 rejected · 2 of 2 file(s), 1 partly",
                svc.review.setHunkAtLine(id, ni, after = true, line = line2, on = false),
            )
            pumpUntil("the rejected hunk struck through") { struck().size == 1 }
            // The whole hunk (its context lines too) is struck through, as in VS Code.
            val hunk2 = r.recs[ni]!!.ranges[1]
            assertEquals(hunk2.start, afterDoc.getLineNumber(struck()[0].startOffset))
            assertEquals(hunk2.end - 1, afterDoc.getLineNumber(struck()[0].endOffset))
            assertTrue(line2 in hunk2.start until hunk2.end)
            assertTrue(gutters().contains("Hunk 2/2 rejected: click to accept it"))

            // The Review tab agrees.
            val states = pumpUntil("the review tree") {
                val root = review.tree.model.root as CheckedTreeNode
                val file = (0 until root.childCount).map { root.getChildAt(it) as CheckedTreeNode }
                    .flatMap { s -> (0 until s.childCount).map { s.getChildAt(it) as CheckedTreeNode } }
                    .firstOrNull { (it.userObject as? ReviewItem.File)?.file == ni && it.childCount == 2 }
                file?.let { f -> (0 until 2).map { (f.getChildAt(it) as CheckedTreeNode).isChecked } }?.takeIf { it == listOf(true, false) }
            }
            assertEquals(listOf(true, false), states)
        } finally {
            own?.let { EditorFactory.getInstance().releaseEditor(it) }
        }

        // Unsaved edits in the IDE (review fix): Apply saves them first, so rw
        // sees them and the IDE gets no "file changed on disk" conflict; the
        // IDE's copy of notes.txt shows rw's result afterwards (VFS refresh).
        val fs = LocalFileSystem.getInstance()
        val readme = fs.refreshAndFindFileByPath(File(env.repo, "README.md").path)!!
        val notesVf = fs.refreshAndFindFileByPath(File(env.repo, Fake.NOTES).path)!!
        val fdm = FileDocumentManager.getInstance()
        val readmeDoc = fdm.getDocument(readme)!!
        val notesDoc = fdm.getDocument(notesVf)!!
        assertEquals(ORIG_NOTES.joinToString("\n") + "\n", notesDoc.text)
        WriteCommandAction.runWriteCommandAction(project) { readmeDoc.setText("# edited in the IDE, not saved\n") }
        assertTrue(fdm.isDocumentUnsaved(readmeDoc))

        // Apply: only the accepted hunk lands.
        svc.review.submit(id)
        assertEquals("# edited in the IDE, not saved\n", env.readRepo("README.md"))
        pumpUntil("the review to end") { svc.review.pending.isEmpty() }
        val st = pumpUntil("the task to finish", 120_000) { svc.model.state?.takeIf { !it.running && it.last != null } }
        assertTrue("task failed: " + st.last?.text, st.last?.ok == true)
        val want = ORIG_NOTES.toMutableList()
        want[Fake.EDITED_LINES[0] - 1] = Fake.edited(Fake.EDITED_LINES[0])
        assertEquals(want.joinToString("\n") + "\n", env.readRepo(Fake.NOTES))
        assertEquals(Fake.EDITED_PLAN_TEXT + "\n", env.readRepo("added.txt"))
        pumpUntil("the done notification") { notes.any { it.content.startsWith("Task done") } }
        // Answered questions do not stay on screen.
        assertTrue(notes.filter { it.content.contains("Review the changes of edit") || it.content.contains("Approve the plan") }.all { it.isExpired })
        assertTrue(svc.log.any { it.contains("applying 2 file(s), 1 of them partly") })
        pumpUntil("the IDE to see rw's change to notes.txt") { notesDoc.text == want.joinToString("\n") + "\n" }

        // rw dies: the plugin notices and says so with a next step.
        val pid = svc.rwPid!!
        ProcessHandle.of(pid).ifPresent { it.destroyForcibly() }
        pumpUntil("the crash to be noticed", 30_000) { !svc.running }
        pumpUntil("the crash notification") { notes.any { it.content.contains("rw stopped unexpectedly") && it.actions.any { a -> a.templateText == "Restart" } } }

        // Closing the project (the service is disposed) stops rw and its working agent.
        startRw()
        env.clearHang()
        svc.runTask("${Fake.HANG} until the project closes")
        val agent = pumpUntil("the hanging agent") { env.hangPid() }
        val rw = svc.rwPid!!
        svc.dispose()
        assertFalse(svc.running)
        pumpUntil("rw to exit", 20_000) { ProcessHandle.of(rw).map { !it.isAlive }.orElse(true) }
        pumpUntil("the agent to exit", 20_000) { ProcessHandle.of(agent).map { !it.isAlive }.orElse(true) }
        assertEquals("a requested stop is not a crash", 1, notes.count { it.content.contains("stopped unexpectedly") })
        env.cleanup()
    }
}
