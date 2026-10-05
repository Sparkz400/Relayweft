package io.github.sparkz400.switchyard.ide

import com.intellij.diff.DiffContentFactory
import com.intellij.diff.chains.SimpleDiffRequestChain
import com.intellij.diff.contents.DocumentContent
import com.intellij.diff.editor.ChainDiffVirtualFile
import com.intellij.diff.editor.DiffEditorTabFilesManager
import com.intellij.diff.requests.DiffRequest
import com.intellij.diff.requests.MessageDiffRequest
import com.intellij.diff.requests.SimpleDiffRequest
import com.intellij.diff.util.DiffNotificationProvider
import com.intellij.diff.util.DiffUserDataKeys
import com.intellij.diff.util.Side
import com.intellij.icons.AllIcons
import com.intellij.openapi.Disposable
import com.intellij.openapi.actionSystem.ActionManager
import com.intellij.openapi.actionSystem.AnAction
import com.intellij.openapi.actionSystem.AnActionEvent
import com.intellij.openapi.application.ApplicationManager
import com.intellij.openapi.editor.Document
import com.intellij.openapi.editor.Editor
import com.intellij.openapi.editor.EditorFactory
import com.intellij.openapi.editor.event.EditorFactoryEvent
import com.intellij.openapi.editor.event.EditorFactoryListener
import com.intellij.openapi.editor.markup.EffectType
import com.intellij.openapi.editor.markup.GutterIconRenderer
import com.intellij.openapi.editor.markup.HighlighterLayer
import com.intellij.openapi.editor.markup.HighlighterTargetArea
import com.intellij.openapi.editor.markup.MarkupModel
import com.intellij.openapi.editor.markup.RangeHighlighter
import com.intellij.openapi.editor.markup.TextAttributes
import com.intellij.openapi.fileEditor.FileEditorManager
import com.intellij.openapi.fileTypes.FileTypeManager
import com.intellij.openapi.project.Project
import com.intellij.openapi.ui.Messages
import com.intellij.openapi.ui.popup.JBPopupFactory
import com.intellij.openapi.util.Disposer
import com.intellij.openapi.util.Key
import com.intellij.openapi.vfs.VirtualFile
import com.intellij.ui.EditorNotificationPanel
import com.intellij.ui.JBColor
import io.github.sparkz400.switchyard.core.ApprovalRequest
import io.github.sparkz400.switchyard.core.ChangeView
import io.github.sparkz400.switchyard.core.FileView
import io.github.sparkz400.switchyard.core.Reconstructed
import io.github.sparkz400.switchyard.core.Selection
import io.github.sparkz400.switchyard.core.decisionBody
import io.github.sparkz400.switchyard.core.documentText
import io.github.sparkz400.switchyard.core.fileOn
import io.github.sparkz400.switchyard.core.hunkAt
import io.github.sparkz400.switchyard.core.hunkCount
import io.github.sparkz400.switchyard.core.hunkOn
import io.github.sparkz400.switchyard.core.messageOf
import io.github.sparkz400.switchyard.core.readProjectFile
import io.github.sparkz400.switchyard.core.reconstruct
import io.github.sparkz400.switchyard.core.selectionSummary
import io.github.sparkz400.switchyard.core.setFile
import io.github.sparkz400.switchyard.core.setHunk
import java.awt.Font
import java.util.concurrent.CopyOnWriteArrayList
import javax.swing.Icon
import javax.swing.JComponent

/** Which review file a diff request shows. */
data class ReviewTarget(val id: String, val file: Int)

/**
 * Change review in the IDE's diff viewer. For each file of a pending change
 * set, read-only "before" and "after" documents are built from the patch
 * sy sends, applied to the file in the project (or the hunks alone when the
 * file there differs from the agent's base). Hunks are accepted or rejected
 * with the gutter icons, the diff toolbar's actions (at the caret), or the
 * checkboxes in the Review tab; rejected hunks are struck through. The
 * decision goes to POST /api/approvals/{id}/changes.
 */
class ReviewController(private val project: Project, private val svc: SyService) {
    class Review(val req: ApprovalRequest, val cv: ChangeView) {
        val sel = Selection(cv.files)
        val recs = HashMap<Int, Reconstructed>()
        val docs = HashMap<Int, Pair<DocumentContent, DocumentContent>>()
        var diffFile: VirtualFile? = null
        val banners = ArrayList<Pair<Int, EditorNotificationPanel>>()
        val files: List<FileView> get() = cv.files
    }

    private data class DocKey(val id: String, val file: Int, val after: Boolean)

    private val reviews = LinkedHashMap<String, Review>()
    private val docKeys = HashMap<Document, DocKey>()
    private var dir: String? = null
    private val listeners = CopyOnWriteArrayList<() -> Unit>()

    init {
        EditorFactory.getInstance().addEditorFactoryListener(object : EditorFactoryListener {
            override fun editorCreated(event: EditorFactoryEvent) {
                if (docKeys.containsKey(event.editor.document)) decorate(event.editor)
            }
        }, svc)
    }

    fun addListener(parent: Disposable, fn: () -> Unit) {
        listeners.add(fn)
        Disposer.register(parent) { listeners.remove(fn) }
    }

    /** The project folder of the running sy (null: detached, every review ends). */
    fun attach(dir: String?) {
        this.dir = dir
        if (dir == null) sync(emptyList())
    }

    val pending: List<Review> get() = reviews.values.toList()

    fun get(id: String): Review? = reviews[id]

    /** Takes the server's pending approvals; returns the change sets that are new. */
    fun sync(approvals: List<ApprovalRequest>): List<ApprovalRequest> {
        val fresh = ArrayList<ApprovalRequest>()
        val live = HashSet<String>()
        for (a in approvals) {
            val cv = a.changes
            if (a.type != "changes" || cv == null) continue
            live.add(a.id)
            if (!reviews.containsKey(a.id)) {
                reviews[a.id] = Review(a, cv)
                fresh.add(a)
            }
        }
        val gone = reviews.keys.filter { it !in live }
        for (id in gone) end(id)
        if (fresh.isNotEmpty() || gone.isNotEmpty()) changed()
        return fresh
    }

    /** A review is over (answered here or elsewhere, or the task ended): its diff closes. */
    private fun end(id: String) {
        val r = reviews.remove(id) ?: return
        docKeys.entries.removeIf { it.value.id == id }
        r.banners.clear()
        r.diffFile?.let { f -> ui { if (!project.isDisposed) FileEditorManager.getInstance(project).closeFile(f) } }
        r.diffFile = null
    }

    private fun changed() {
        for ((_, r) in reviews) {
            for ((i, p) in r.banners) updateBanner(r, i, p)
        }
        for (doc in docKeys.keys.toList()) {
            for (ed in EditorFactory.getInstance().getEditors(doc)) decorate(ed)
        }
        for (l in listeners) l()
    }

    // --- documents and the diff viewer ----------------------------------------------------

    /** Opens the change set's diff (all its files, at file / hunk). */
    fun openDiff(id: String, file: Int = 0, hunk: Int? = null) {
        val r = reviews[id] ?: return
        // The "before" side is read from disk: write the IDE's unsaved edits first.
        svc.saveAll()
        val f = r.files.getOrNull(file) ?: return
        val d = dir
        val todo = r.files.indices.filter { !r.files[it].binary && !r.recs.containsKey(it) }
        ApplicationManager.getApplication().executeOnPooledThread {
            val computed = todo.associateWith { i ->
                val fv = r.files[i]
                val disk = if (fv.status == "A" || d == null) null else readProjectFile(d, fv.path)
                reconstruct(fv.patch, fv.status, disk)
            }
            ui {
                if (reviews[id] !== r || project.isDisposed) return@ui
                for ((i, rec) in computed) r.recs.putIfAbsent(i, rec)
                show(r, file, hunk)
                if (!f.binary && r.recs[file]?.whole == false) {
                    com.intellij.openapi.wm.StatusBar.Info.set(
                        "Switchyard: the file in your project is not the agent's base (or the patch is truncated): showing the hunks only", project,
                    )
                }
            }
        }
    }

    private fun contents(r: Review, i: Int): Pair<DocumentContent, DocumentContent>? {
        r.docs[i]?.let { return it }
        val rec = r.recs[i] ?: return null
        val f = r.files[i]
        val type = FileTypeManager.getInstance().getFileTypeByFileName(f.path.substringAfterLast('/'))
        val factory = DiffContentFactory.getInstance()
        val before = factory.create(project, documentText(rec.before), type)
        val after = factory.create(project, documentText(rec.after), type)
        docKeys[before.document] = DocKey(r.req.id, i, false)
        docKeys[after.document] = DocKey(r.req.id, i, true)
        val p = before to after
        r.docs[i] = p
        return p
    }

    private fun request(r: Review, i: Int): DiffRequest {
        val f = r.files[i]
        // Paths and step ids come from the agent: never let Swing read them as HTML.
        val title = plainText("${f.path} — ${r.cv.stepId} review")
        val target = ReviewTarget(r.req.id, i)
        val c = if (f.binary) null else contents(r, i)
        val req: DiffRequest = if (c == null) {
            MessageDiffRequest(title, plainText("${f.path} is a binary file: accept or reject it as a whole in the Review tab."))
        } else {
            val whole = r.recs[i]?.whole != false
            SimpleDiffRequest(
                title + if (whole) "" else " (hunks only)", c.first, c.second,
                "Before: the file as the agent found it", "After: with the agent's changes",
            )
        }
        req.putUserData(TARGET, target)
        val am = ActionManager.getInstance()
        req.putUserData(DiffUserDataKeys.CONTEXT_ACTIONS, CONTEXT_ACTIONS.mapNotNull { am.getAction(it) })
        req.putUserData(DiffUserDataKeys.NOTIFICATION_PROVIDERS, listOf(DiffNotificationProvider { banner(target) }))
        return req
    }

    private fun show(r: Review, file: Int, hunk: Int?) {
        r.banners.clear()
        val requests = r.files.indices.map { request(r, it) }
        r.recs[file]?.let { rec ->
            val rg = if (hunk != null) rec.ranges.getOrNull(hunk) else rec.ranges.firstOrNull()
            if (rg != null) requests[file].putUserData(DiffUserDataKeys.SCROLL_TO_LINE, com.intellij.openapi.util.Pair.create(Side.RIGHT, rg.start))
        }
        r.diffFile?.let { FileEditorManager.getInstance(project).closeFile(it) }
        // A headless IDE (the plugin's tests) has no editor tabs to show it in;
        // the documents and their marks exist all the same.
        if (ApplicationManager.getApplication().isHeadlessEnvironment) return
        val vf = ChainDiffVirtualFile(SimpleDiffRequestChain(requests, file), "${r.cv.stepId} review")
        r.diffFile = vf
        DiffEditorTabFilesManager.getInstance(project).showDiffFile(vf, true)
    }

    private fun banner(t: ReviewTarget): JComponent? {
        val r = reviews[t.id] ?: return null
        val p = EditorNotificationPanel(EditorNotificationPanel.Status.Info)
        p.createActionLabel("Accept file") { setWholeFile(t.id, t.file, true) }
        p.createActionLabel("Reject file") { setWholeFile(t.id, t.file, false) }
        p.createActionLabel("Apply selected changes") { submit(t.id) }
        p.createActionLabel("Send back with feedback…") { feedback(t.id) }
        // The viewer makes a new bar each time it is rebuilt: drop the ones no longer shown.
        r.banners.removeIf { (i, old) -> i == t.file && !old.isDisplayable }
        r.banners.add(t.file to p)
        updateBanner(r, t.file, p)
        return p
    }

    private fun updateBanner(r: Review, i: Int, p: EditorNotificationPanel) {
        val f = r.files[i]
        val n = hunkCount(f)
        val on = fileOn(r.files, r.sel, i)
        val what = when {
            !on -> "file rejected"
            n > 0 -> "${r.sel.hunks[i].size} of $n hunks accepted (click the gutter icons to switch)"
            else -> "file accepted (it is accepted or rejected as a whole)"
        }
        // Starts with fixed text: a label whose text starts with <html> renders it as HTML.
        p.text = "Review of ${f.path}: $what. Apply sends ${selectionSummary(r.files, r.sel)}."
    }

    // --- hunk marks in the diff editors -------------------------------------------------------

    private fun decorate(editor: Editor) {
        val mm = editor.markupModel
        editor.getUserData(MARKS)?.forEach { if (it.isValid) mm.removeHighlighter(it) }
        editor.putUserData(MARKS, null)
        val key = docKeys[editor.document] ?: return
        val r = reviews[key.id] ?: return
        val rec = r.recs[key.file] ?: return
        val files = r.files
        val f = files[key.file]
        val doc = editor.document
        val marks = ArrayList<RangeHighlighter>()
        val on = fileOn(files, r.sel, key.file)
        if (key.after && !on && doc.textLength > 0) marks.add(strike(mm, doc, 0, doc.lineCount))
        val n = hunkCount(f)
        val ranges = if (key.after) rec.ranges else rec.beforeRanges
        ranges.forEachIndexed { j, rg ->
            if (j >= n) return@forEachIndexed
            val hOn = hunkOn(files, r.sel, key.file, j)
            if (key.after && on && !hOn && rg.end > rg.start) marks.add(strike(mm, doc, rg.start, rg.end))
            val line = rg.start.coerceIn(0, maxOf(0, doc.lineCount - 1))
            val off = if (doc.textLength == 0) 0 else doc.getLineStartOffset(line)
            val h = mm.addRangeHighlighter(off, off, HighlighterLayer.LAST, null, HighlighterTargetArea.LINES_IN_RANGE)
            h.gutterIconRenderer = HunkGutter(hOn, j, n) { toggleHunk(key.id, key.file, j) }
            marks.add(h)
        }
        editor.putUserData(MARKS, marks)
    }

    private fun strike(mm: MarkupModel, doc: Document, startLine: Int, endLine: Int): RangeHighlighter {
        val last = maxOf(0, doc.lineCount - 1)
        val s = doc.getLineStartOffset(startLine.coerceIn(0, last))
        val e = doc.getLineEndOffset((endLine - 1).coerceIn(0, last))
        return mm.addRangeHighlighter(s, maxOf(s, e), HighlighterLayer.SELECTION - 1, REJECTED, HighlighterTargetArea.LINES_IN_RANGE)
    }

    private class HunkGutter(val on: Boolean, val j: Int, val n: Int, val toggle: () -> Unit) : GutterIconRenderer() {
        override fun getIcon(): Icon = if (on) AllIcons.Actions.Checked else AllIcons.Actions.Cancel
        override fun getTooltipText(): String =
            if (on) "Hunk ${j + 1}/$n accepted: click to reject it" else "Hunk ${j + 1}/$n rejected: click to accept it"
        override fun isNavigateAction(): Boolean = true
        override fun getClickAction(): AnAction = object : AnAction() {
            override fun actionPerformed(e: AnActionEvent) = toggle()
        }
        override fun getAlignment(): Alignment = Alignment.LEFT
        override fun equals(other: Any?): Boolean = other is HunkGutter && other.on == on && other.j == j && other.n == n
        override fun hashCode(): Int = (if (on) 1 else 0) + 31 * j + 961 * n
    }

    // --- selection -------------------------------------------------------------------------

    fun toggleHunk(id: String, file: Int, hunk: Int) {
        val r = reviews[id] ?: return
        setHunk(r.files, r.sel, file, hunk, !hunkOn(r.files, r.sel, file, hunk))
        changed()
    }

    fun setHunkState(id: String, file: Int, hunk: Int, on: Boolean) {
        val r = reviews[id] ?: return
        setHunk(r.files, r.sel, file, hunk, on)
        changed()
    }

    fun setWholeFile(id: String, file: Int, on: Boolean) {
        val r = reviews[id] ?: return
        setFile(r.files, r.sel, file, on)
        changed()
    }

    /** Accepts or rejects the hunk at line (0-based) of the before or after side; a file that cannot be split is set as a whole. */
    fun setHunkAtLine(id: String, file: Int, after: Boolean, line: Int, on: Boolean): String? {
        val r = reviews[id] ?: return null
        val f = r.files.getOrNull(file) ?: return null
        if (!f.splittable) {
            setFile(r.files, r.sel, file, on)
            changed()
            return "${f.path} cannot be split into hunks: the whole file is ${if (on) "accepted" else "rejected"}"
        }
        val rec = r.recs[file] ?: return null
        val j = hunkAt(if (after) rec.ranges else rec.beforeRanges, line)
        if (j < 0) return null
        setHunk(r.files, r.sel, file, j, on)
        changed()
        return "hunk ${j + 1} ${if (on) "accepted" else "rejected"} · ${selectionSummary(r.files, r.sel)}"
    }

    /** For actions in a review diff: the target and the side and line of the caret. */
    fun caretTarget(e: AnActionEvent): Triple<ReviewTarget, Boolean, Int>? {
        val t = e.getData(com.intellij.diff.tools.util.DiffDataKeys.DIFF_REQUEST)?.getUserData(TARGET) ?: return null
        val editor = e.getData(com.intellij.diff.tools.util.DiffDataKeys.CURRENT_EDITOR)
            ?: e.getData(com.intellij.openapi.actionSystem.CommonDataKeys.EDITOR)
        val key = editor?.let { docKeys[it.document] }
        if (editor == null || key == null || key.id != t.id) return Triple(t, true, 0)
        return Triple(t, key.after, editor.caretModel.logicalPosition.line)
    }

    fun targetOf(e: AnActionEvent): ReviewTarget? =
        e.getData(com.intellij.diff.tools.util.DiffDataKeys.DIFF_REQUEST)?.getUserData(TARGET)

    // --- answers ---------------------------------------------------------------------------

    /** Resolves the review a command is about: the given one, the only one, or a pick. */
    fun pick(id: String?, then: (String) -> Unit) {
        if (id != null && reviews.containsKey(id)) {
            then(id)
            return
        }
        val all = pending
        when (all.size) {
            0 -> Notify.info(project, "No changes are waiting for review.")
            1 -> then(all[0].req.id)
            else -> JBPopupFactory.getInstance()
                .createPopupChooserBuilder(all.map { "${it.cv.stepId}: ${it.cv.title} (${it.files.size} file(s))" })
                .setTitle("Which Change Set?")
                .setItemChosenCallback { label -> all.firstOrNull { "${it.cv.stepId}: ${it.cv.title} (${it.files.size} file(s))" == label }?.let { then(it.req.id) } }
                .createPopup()
                .showCenteredInCurrentWindow(project)
        }
    }

    /** Opens a change set: the Review tab and the diff of its first file. */
    fun openReview(id: String?) {
        pick(id) { rid ->
            svc.showToolWindow()
            SyToolWindowFactory.selectReviewTab(project)
            openDiff(rid, 0)
        }
    }

    fun submit(id: String?) = pick(id) { rid ->
        val r = reviews[rid] ?: return@pick
        val body = decisionBody(r.files, r.sel)
        if (body.apply.isEmpty()) {
            val ok = Messages.showOkCancelDialog(
                project, "Nothing is selected: reject all changes of ${r.cv.stepId}? They are kept on a branch.",
                "Switchyard Review", "Reject All", "Cancel", Messages.getWarningIcon(),
            )
            if (ok != Messages.OK) return@pick
        }
        // sy writes the accepted hunks: the IDE's unsaved edits go to disk first,
        // so sy sees them and the IDE gets no "file changed on disk" conflict.
        svc.saveAll()
        post(rid, body.toJson())
    }

    fun rejectAll(id: String?) = pick(id) { rid ->
        val r = reviews[rid] ?: return@pick
        val ok = Messages.showOkCancelDialog(
            project, "Reject every change of ${r.cv.stepId}? They are kept on a branch.",
            "Switchyard Review", "Reject All", "Cancel", Messages.getWarningIcon(),
        )
        if (ok == Messages.OK) post(rid, mapOf("apply" to emptyList<String>(), "hunks" to emptyMap<String, Any>()))
    }

    fun feedback(id: String?) = pick(id) { rid ->
        val r = reviews[rid] ?: return@pick
        val text = Messages.showMultilineInputDialog(
            project, "What should the agent change? Its current changes stay in its worktree and come back for review.",
            "Send ${r.cv.stepId} Back to Its Agent", "", null, null,
        )
        if (!text.isNullOrBlank()) post(rid, mapOf("feedback" to text.trim()))
    }

    private fun post(id: String, body: Map<String, Any?>) {
        svc.background("Review answer", { it.call("POST", "/api/approvals/${SyService.enc(id)}/changes", body) }) { res ->
            svc.said(messageOf(res))
            svc.refreshProject()
            end(id)
            changed()
        }
    }

    companion object {
        val TARGET: Key<ReviewTarget> = Key.create("switchyard.review.target")
        private val MARKS: Key<List<RangeHighlighter>> = Key.create("switchyard.review.marks")
        val CONTEXT_ACTIONS = listOf("Switchyard.Review.AcceptHunk", "Switchyard.Review.RejectHunk", "Switchyard.Review.Apply", "Switchyard.Review.Feedback")
        private val REJECTED = TextAttributes(JBColor.GRAY, null, JBColor.GRAY, EffectType.STRIKEOUT, Font.PLAIN)
    }
}
