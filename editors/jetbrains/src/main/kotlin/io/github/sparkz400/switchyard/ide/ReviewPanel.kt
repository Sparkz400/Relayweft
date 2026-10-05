package io.github.sparkz400.switchyard.ide

import com.intellij.icons.AllIcons
import com.intellij.openapi.Disposable
import com.intellij.openapi.actionSystem.ActionGroup
import com.intellij.openapi.actionSystem.ActionManager
import com.intellij.openapi.actionSystem.DataKey
import com.intellij.openapi.actionSystem.DataSink
import com.intellij.openapi.project.Project
import com.intellij.openapi.ui.SimpleToolWindowPanel
import com.intellij.ui.CheckboxTree
import com.intellij.ui.CheckboxTreeBase
import com.intellij.ui.CheckboxTreeListener
import com.intellij.ui.CheckedTreeNode
import com.intellij.ui.DoubleClickListener
import com.intellij.ui.ScrollPaneFactory
import com.intellij.ui.SimpleTextAttributes
import com.intellij.util.ui.tree.TreeUtil
import io.github.sparkz400.switchyard.core.fileOn
import io.github.sparkz400.switchyard.core.hunkCount
import io.github.sparkz400.switchyard.core.hunkOn
import io.github.sparkz400.switchyard.core.oneLine
import io.github.sparkz400.switchyard.core.selectionSummary
import java.awt.event.MouseEvent
import javax.swing.JTree
import javax.swing.Timer
import javax.swing.tree.DefaultTreeModel
import javax.swing.tree.TreePath

/** A row of the Review tree. */
sealed class ReviewItem(val id: String) {
    class Set(id: String) : ReviewItem(id)
    class File(id: String, val file: Int) : ReviewItem(id)
    class Hunk(id: String, val file: Int, val hunk: Int) : ReviewItem(id)

    val key: String
        get() = when (this) {
            is Set -> "set:$id"
            is File -> "file:$id:$file"
            is Hunk -> "hunk:$id:$file:$hunk"
        }
}

/**
 * The Review tab: each change set waiting for review, its files and their
 * hunks with checkboxes. Double-click a file or hunk to open the diff.
 */
class ReviewPanel(private val project: Project, private val svc: SyService) : SimpleToolWindowPanel(true, true), Disposable {
    private val root = CheckedTreeNode(null)
    val tree: CheckboxTree
    private var rebuilding = false
    private val refreshTimer = Timer(80) { rebuild() }.apply { isRepeats = false }
    private val collapsedFiles = HashSet<String>()
    var onCount: (Int) -> Unit = {}

    init {
        tree = CheckboxTree(Renderer(), root, CheckboxTreeBase.CheckPolicy(false, false, false, false))
        tree.isRootVisible = false
        tree.accessibleContext.accessibleName = "Switchyard review"
        tree.showsRootHandles = true
        tree.emptyText.text = "No changes are waiting for your review."
        tree.emptyText.appendLine("Turn on \"review changes\" in sy (orchestrator.review_changes)")
        tree.emptyText.appendLine("to review each agent's changes here before they land.")
        tree.addCheckboxTreeListener(object : CheckboxTreeListener {
            override fun nodeStateChanged(node: CheckedTreeNode) {
                if (rebuilding) return
                when (val it = node.userObject as? ReviewItem) {
                    is ReviewItem.Set -> svc.review.get(it.id)?.let { r -> r.files.indices.forEach { i -> svc.review.setWholeFile(it.id, i, node.isChecked) } }
                    is ReviewItem.File -> svc.review.setWholeFile(it.id, it.file, node.isChecked)
                    is ReviewItem.Hunk -> svc.review.setHunkState(it.id, it.file, it.hunk, node.isChecked)
                    null -> {}
                }
            }
        })
        object : DoubleClickListener() {
            override fun onDoubleClick(event: MouseEvent): Boolean = openSelected()
        }.installOn(tree)
        // Enter opens the diff too (Space switches the checkbox).
        com.intellij.openapi.project.DumbAwareAction.create { openSelected() }.registerCustomShortcutSet(
            com.intellij.openapi.actionSystem.CustomShortcutSet(javax.swing.KeyStroke.getKeyStroke(java.awt.event.KeyEvent.VK_ENTER, 0)), tree, this,
        )
        setContent(ScrollPaneFactory.createScrollPane(tree, true))
        val group = ActionManager.getInstance().getAction("Switchyard.ReviewToolbar") as ActionGroup
        val tb = ActionManager.getInstance().createActionToolbar("SwitchyardReview", group, true)
        tb.targetComponent = this
        toolbar = tb.component
        svc.review.addListener(this) { if (!refreshTimer.isRunning) refreshTimer.restart() }
        rebuild()
    }

    override fun dispose() {
        refreshTimer.stop()
    }

    override fun uiDataSnapshot(sink: DataSink) {
        super.uiDataSnapshot(sink)
        selectedItem()?.let { sink[REVIEW_ID] = it.id }
    }

    private fun openSelected(): Boolean {
        when (val it = selectedItem()) {
            is ReviewItem.File -> svc.review.openDiff(it.id, it.file)
            is ReviewItem.Hunk -> svc.review.openDiff(it.id, it.file, it.hunk)
            else -> return false
        }
        return true
    }

    fun selectedItem(): ReviewItem? = (tree.selectionPath?.lastPathComponent as? CheckedTreeNode)?.userObject as? ReviewItem

    private fun build(): CheckedTreeNode {
        val r0 = CheckedTreeNode(null)
        for (r in svc.review.pending) {
            val set = CheckedTreeNode(ReviewItem.Set(r.req.id))
            set.isChecked = r.files.indices.any { fileOn(r.files, r.sel, it) }
            r.files.forEachIndexed { i, f ->
                val fn = CheckedTreeNode(ReviewItem.File(r.req.id, i))
                fn.isChecked = fileOn(r.files, r.sel, i)
                for (j in 0 until hunkCount(f)) {
                    val hn = CheckedTreeNode(ReviewItem.Hunk(r.req.id, i, j))
                    hn.isChecked = hunkOn(r.files, r.sel, i, j)
                    fn.add(hn)
                }
                set.add(fn)
            }
            r0.add(set)
        }
        return r0
    }

    private fun shape(n: CheckedTreeNode): List<String> =
        TreeUtil.treeNodeTraverser(n).toList().mapNotNull { ((it as? CheckedTreeNode)?.userObject as? ReviewItem)?.key }

    internal fun rebuild() {
        val selected = selectedItem()?.key
        val fresh = build()
        rebuilding = true
        try {
            if (shape(fresh) == shape(root)) {
                // Same rows: update the checkboxes in place (a reload would drop clicks and selection).
                val olds = TreeUtil.treeNodeTraverser(root).toList().filterIsInstance<CheckedTreeNode>().filter { it.userObject is ReviewItem }
                val news = TreeUtil.treeNodeTraverser(fresh).toList().filterIsInstance<CheckedTreeNode>().filter { it.userObject is ReviewItem }
                olds.zip(news).forEach { (o, n) ->
                    o.isChecked = n.isChecked
                    (tree.model as DefaultTreeModel).nodeChanged(o)
                }
                return
            }
            // Remember what the person collapsed.
            TreeUtil.treeNodeTraverser(root).forEach { n ->
                val node = n as? CheckedTreeNode ?: return@forEach
                val it = node.userObject as? ReviewItem
                if (it is ReviewItem.File && node.childCount > 0) {
                    if (tree.isExpanded(TreePath(node.path))) collapsedFiles.remove(it.key) else collapsedFiles.add(it.key)
                }
            }
            root.removeAllChildren()
            while (fresh.childCount > 0) root.add(fresh.getChildAt(0) as CheckedTreeNode)
            (tree.model as DefaultTreeModel).reload()
            TreeUtil.treeNodeTraverser(root).forEach { n ->
                val node = n as? CheckedTreeNode ?: return@forEach
                val it = node.userObject as? ReviewItem ?: return@forEach
                if (it is ReviewItem.Set || (it is ReviewItem.File && it.key !in collapsedFiles)) tree.expandPath(TreePath(node.path))
            }
            if (selected != null) {
                TreeUtil.findNode(root) { (it.userObject as? ReviewItem)?.key == selected }?.let { tree.selectionPath = TreePath(it.path) }
            }
        } finally {
            rebuilding = false
            onCount(svc.review.pending.size)
        }
    }

    private inner class Renderer : CheckboxTree.CheckboxTreeCellRenderer() {
        override fun customizeRenderer(tree: JTree?, value: Any?, selected: Boolean, expanded: Boolean, leaf: Boolean, row: Int, hasFocus: Boolean) {
            val item = (value as? CheckedTreeNode)?.userObject as? ReviewItem ?: return
            val r = svc.review.get(item.id) ?: return
            val t = textRenderer
            when (item) {
                is ReviewItem.Set -> {
                    t.append("${r.cv.stepId}: ${oneLine(r.cv.title, 80)}", SimpleTextAttributes.REGULAR_BOLD_ATTRIBUTES)
                    t.append("  round ${r.cv.round} · ${selectionSummary(r.files, r.sel)}", SimpleTextAttributes.GRAYED_ATTRIBUTES)
                    t.icon = AllIcons.Actions.Diff
                    t.toolTipText = "<html>" + Notify.escape(r.cv.summary.ifEmpty { r.cv.title }) + "</html>"
                }
                is ReviewItem.File -> {
                    val f = r.files[item.file]
                    t.append(f.path)
                    val word = when (f.status) {
                        "A" -> "added"
                        "D" -> "deleted"
                        else -> "modified"
                    }
                    val n = hunkCount(f)
                    val hunks = if (n > 0) " · ${r.sel.hunks[item.file].size}/$n hunks" else ""
                    t.append("  $word +${f.added} −${f.deleted}$hunks${if (f.binary) " · binary" else ""}", SimpleTextAttributes.GRAYED_ATTRIBUTES)
                    t.toolTipText = "Double-click to open the diff"
                }
                is ReviewItem.Hunk -> {
                    val h = r.files[item.file].hunks.getOrNull(item.hunk) ?: ""
                    val head = h.lineSequence().firstOrNull() ?: ""
                    t.append("hunk ${item.hunk + 1}")
                    t.append("  " + (head.replace(Regex("^@@[^@]*@@\\s?"), "").ifEmpty { head }), SimpleTextAttributes.GRAYED_ATTRIBUTES)
                    t.toolTipText = "<html><pre>" + Notify.escape(h.take(3000)).replace("<br>", "\n") + "</pre></html>"
                }
            }
        }
    }

    companion object {
        val REVIEW_ID: DataKey<String> = DataKey.create("switchyard.reviewId")
    }
}
