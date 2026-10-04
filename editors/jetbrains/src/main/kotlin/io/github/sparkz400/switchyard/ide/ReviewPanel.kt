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
            override fun onDoubleClick(event: MouseEvent): Boolean {
                when (val it = selectedItem()) {
                    is ReviewItem.File -> svc.review.openDiff(it.id, it.file)
                    is ReviewItem.Hunk -> svc.review.openDiff(it.id, it.file, it.hunk)
                    else -> return false
                }
                return true
            }
        }.installOn(tree)
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

    fun selectedItem(): ReviewItem? = (tree.selectionPath?.lastPathComponent as? CheckedTreeNode)?.userObject as? ReviewItem

    private fun rebuild() {
        val selected = selectedItem()?.key
        rebuilding = true
        try {
            // Remember what the person collapsed.
            TreeUtil.treeNodeTraverser(root).forEach { n ->
                val node = n as? CheckedTreeNode ?: return@forEach
                val it = node.userObject as? ReviewItem
                if (it is ReviewItem.File && node.childCount > 0) {
                    if (tree.isExpanded(TreePath(node.path))) collapsedFiles.remove(it.key) else collapsedFiles.add(it.key)
                }
            }
            root.removeAllChildren()
            val pending = svc.review.pending
            for (r in pending) {
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
                root.add(set)
            }
            (tree.model as DefaultTreeModel).reload()
            TreeUtil.treeNodeTraverser(root).forEach { n ->
                val node = n as? CheckedTreeNode ?: return@forEach
                val it = node.userObject as? ReviewItem ?: return@forEach
                if (it is ReviewItem.Set || (it is ReviewItem.File && it.key !in collapsedFiles)) tree.expandPath(TreePath(node.path))
            }
            if (selected != null) {
                TreeUtil.findNode(root) { (it.userObject as? ReviewItem)?.key == selected }?.let { tree.selectionPath = TreePath(it.path) }
            }
            onCount(pending.size)
        } finally {
            rebuilding = false
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
                    t.toolTipText = Notify.escape(r.cv.summary.ifEmpty { r.cv.title })
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
