package io.github.sparkz400.switchyard.ide

import com.intellij.icons.AllIcons
import com.intellij.openapi.Disposable
import com.intellij.openapi.actionSystem.ActionGroup
import com.intellij.openapi.actionSystem.ActionManager
import com.intellij.openapi.actionSystem.CustomShortcutSet
import com.intellij.openapi.actionSystem.DataKey
import com.intellij.openapi.actionSystem.DataSink
import com.intellij.openapi.actionSystem.DefaultActionGroup
import com.intellij.openapi.actionSystem.KeyboardShortcut
import com.intellij.openapi.editor.colors.EditorColorsManager
import com.intellij.openapi.project.DumbAwareAction
import com.intellij.openapi.project.Project
import com.intellij.openapi.ui.SimpleToolWindowPanel
import com.intellij.ui.AnimatedIcon
import com.intellij.ui.ColoredTreeCellRenderer
import com.intellij.ui.DoubleClickListener
import com.intellij.ui.OnePixelSplitter
import com.intellij.ui.PopupHandler
import com.intellij.ui.ScrollPaneFactory
import com.intellij.ui.SimpleTextAttributes
import com.intellij.ui.components.JBLabel
import com.intellij.ui.components.JBTextArea
import com.intellij.ui.treeStructure.Tree
import com.intellij.util.ui.JBUI
import com.intellij.util.ui.tree.TreeUtil
import io.github.sparkz400.switchyard.core.AgentNode
import io.github.sparkz400.switchyard.core.AgentStatus
import io.github.sparkz400.switchyard.core.ApprovalRequest
import io.github.sparkz400.switchyard.core.TaskModel
import io.github.sparkz400.switchyard.core.oneLine
import java.awt.BorderLayout
import java.awt.event.InputEvent
import java.awt.event.KeyEvent
import java.awt.event.MouseEvent
import javax.swing.JButton
import javax.swing.JPanel
import javax.swing.JTree
import javax.swing.KeyStroke
import javax.swing.Timer
import javax.swing.ToolTipManager
import javax.swing.event.TreeExpansionEvent
import javax.swing.event.TreeExpansionListener
import javax.swing.tree.DefaultMutableTreeNode
import javax.swing.tree.DefaultTreeModel
import javax.swing.tree.TreePath

/** A row of the Agents tree. */
sealed class AgentsItem(val key: String) {
    object Status : AgentsItem("status")
    object Approvals : AgentsItem("approvals")
    class Approval(val req: ApprovalRequest) : AgentsItem("approval:" + req.id)
    object Queue : AgentsItem("queue")
    class Job(val id: Long, val label: String, val at: String) : AgentsItem("job:$id")
    class Agent(val id: String) : AgentsItem("agent:$id")
}

/**
 * The Agents tab: the status, what waits for you, the queue and the agent
 * tree (live from the event stream), the activity log, and a prompt box to
 * start a task.
 */
class AgentsPanel(private val project: Project, private val svc: SyService) : SimpleToolWindowPanel(true, true), Disposable {
    private val root = DefaultMutableTreeNode()
    private val treeModel = DefaultTreeModel(root)
    val tree = Tree(treeModel)
    private val logArea = JBTextArea()
    private val prompt = JBTextArea(3, 40)
    private val hint = JBLabel()
    private val runButton = JButton("Run")
    /** Nodes whose expansion differs from the default. */
    private val toggled = HashSet<String>()
    private var rebuilding = false
    private val refreshTimer = Timer(120) { rebuild() }.apply { isRepeats = false }

    init {
        tree.isRootVisible = false
        tree.showsRootHandles = true
        tree.cellRenderer = Renderer()
        tree.putClientProperty(AnimatedIcon.ANIMATION_IN_RENDERER_ALLOWED, true)
        ToolTipManager.sharedInstance().registerComponent(tree)
        tree.emptyText.text = "Switchyard is not running for this project."
        tree.emptyText.appendSecondaryText("Start Switchyard", SimpleTextAttributes.LINK_PLAIN_ATTRIBUTES) { svc.start() }
        tree.emptyText.appendLine("It runs `sy web --client` in the project folder.")
        tree.addTreeExpansionListener(object : TreeExpansionListener {
            override fun treeExpanded(e: TreeExpansionEvent) {
                if (!rebuilding) keyOf(e.path)?.let { setExpanded(it, true) }
            }

            override fun treeCollapsed(e: TreeExpansionEvent) {
                if (!rebuilding) keyOf(e.path)?.let { setExpanded(it, false) }
            }
        })
        object : DoubleClickListener() {
            override fun onDoubleClick(event: MouseEvent): Boolean = activate()
        }.installOn(tree)
        DumbAwareAction.create { activate() }.registerCustomShortcutSet(CustomShortcutSet(KeyStroke.getKeyStroke(KeyEvent.VK_ENTER, 0)), tree, this)
        tree.addMouseListener(object : PopupHandler() {
            override fun invokePopup(comp: java.awt.Component, x: Int, y: Int) {
                tree.getPathForLocation(x, y)?.let { tree.selectionPath = it }
                val group = contextGroup() ?: return
                ActionManager.getInstance().createActionPopupMenu("SwitchyardAgents", group).component.show(comp, x, y)
            }
        })

        logArea.isEditable = false
        logArea.font = EditorColorsManager.getInstance().globalScheme.getFont(com.intellij.openapi.editor.colors.EditorFontType.PLAIN)
        logArea.text = svc.log.joinToString("\n", postfix = if (svc.log.isEmpty()) "" else "\n")

        prompt.lineWrap = true
        prompt.wrapStyleWord = true
        prompt.emptyText.text = "Describe a task. \"@agent message\" follows up. Ctrl+Enter runs it."
        DumbAwareAction.create { submit() }.registerCustomShortcutSet(
            CustomShortcutSet(
                KeyboardShortcut(KeyStroke.getKeyStroke(KeyEvent.VK_ENTER, InputEvent.CTRL_DOWN_MASK), null),
                KeyboardShortcut(KeyStroke.getKeyStroke(KeyEvent.VK_ENTER, InputEvent.META_DOWN_MASK), null),
            ),
            prompt, this,
        )
        runButton.addActionListener { submit() }
        hint.componentStyle = com.intellij.util.ui.UIUtil.ComponentStyle.SMALL
        hint.fontColor = com.intellij.util.ui.UIUtil.FontColor.BRIGHTER

        val promptPanel = JPanel(BorderLayout(JBUI.scale(6), JBUI.scale(4)))
        promptPanel.border = JBUI.Borders.empty(6)
        promptPanel.add(ScrollPaneFactory.createScrollPane(prompt), BorderLayout.CENTER)
        promptPanel.add(runButton, BorderLayout.EAST)
        promptPanel.add(hint, BorderLayout.SOUTH)

        val split = OnePixelSplitter(true, 0.6f)
        split.firstComponent = ScrollPaneFactory.createScrollPane(tree, true)
        split.secondComponent = ScrollPaneFactory.createScrollPane(logArea, true)
        val center = JPanel(BorderLayout())
        center.add(split, BorderLayout.CENTER)
        center.add(promptPanel, BorderLayout.SOUTH)
        setContent(center)

        val group = ActionManager.getInstance().getAction("Switchyard.ToolWindowToolbar") as ActionGroup
        val tb = ActionManager.getInstance().createActionToolbar("SwitchyardToolWindow", group, true)
        tb.targetComponent = this
        toolbar = tb.component

        svc.addListener(object : SyService.Listener {
            override fun changed() = scheduleRefresh()
            override fun logged(lines: List<String>) = appendLog(lines)
        }, this)
        rebuild()
    }

    override fun dispose() {
        refreshTimer.stop()
    }

    override fun uiDataSnapshot(sink: DataSink) {
        super.uiDataSnapshot(sink)
        val item = selectedItem()
        if (item is AgentsItem.Agent) sink[AGENT_ID] = item.id
        if (item is AgentsItem.Approval) sink[APPROVAL_ID] = item.req.id
    }

    private fun scheduleRefresh() {
        if (!refreshTimer.isRunning) refreshTimer.restart()
    }

    private fun appendLog(lines: List<String>) {
        val atEnd = logArea.caretPosition >= logArea.document.length - 1
        logArea.append(lines.joinToString("\n", postfix = "\n"))
        val extra = logArea.lineCount - MAX_LOG_LINES
        if (extra > 0) logArea.replaceRange("", 0, logArea.getLineStartOffset(extra + MAX_LOG_LINES / 4))
        if (atEnd) logArea.caretPosition = logArea.document.length
    }

    private fun submit() {
        val text = prompt.text.trim()
        if (text.isEmpty()) return
        val send = { svc.runTask(text) { prompt.text = "" } }
        if (svc.running) send() else svc.start { send() }
    }

    fun selectedItem(): AgentsItem? = (tree.selectionPath?.lastPathComponent as? DefaultMutableTreeNode)?.userObject as? AgentsItem

    private fun keyOf(p: TreePath): String? = ((p.lastPathComponent as? DefaultMutableTreeNode)?.userObject as? AgentsItem)?.key

    /** Enter or double click: answer a question, follow up with an agent. */
    private fun activate(): Boolean {
        when (val it = selectedItem()) {
            is AgentsItem.Approval -> svc.answerApproval(it.req.id)
            is AgentsItem.Agent -> FollowUpAction.ask(project, svc, it.id)
            else -> return false
        }
        return true
    }

    private fun contextGroup(): ActionGroup? {
        val g = DefaultActionGroup()
        when (val it = selectedItem()) {
            is AgentsItem.Approval -> g.add(DumbAwareAction.create("Answer…") { _ -> svc.answerApproval(it.req.id) })
            is AgentsItem.Agent -> g.add(DumbAwareAction.create("Follow Up with ${it.id}…") { _ -> FollowUpAction.ask(project, svc, it.id) })
            else -> return null
        }
        return g
    }

    // --- tree ------------------------------------------------------------------------------

    private fun rebuild() {
        val selected = selectedItem()?.key
        rebuilding = true
        try {
            root.removeAllChildren()
            val st = svc.model.state
            if (svc.running || svc.starting) {
                root.add(DefaultMutableTreeNode(AgentsItem.Status))
                if (st != null && st.approvals.isNotEmpty()) {
                    val n = DefaultMutableTreeNode(AgentsItem.Approvals)
                    st.approvals.forEach { n.add(DefaultMutableTreeNode(AgentsItem.Approval(it))) }
                    root.add(n)
                }
                if (st != null && st.queue.isNotEmpty()) {
                    val n = DefaultMutableTreeNode(AgentsItem.Queue)
                    st.queue.forEach { n.add(DefaultMutableTreeNode(AgentsItem.Job(it.id, it.label, it.at))) }
                    root.add(n)
                }
                val m = svc.model
                if (m.taskText.isNotEmpty() || m.order.isNotEmpty()) root.add(agentNode(m, TaskModel.MAIN))
            }
            treeModel.reload()
            expand(root)
            if (selected != null) {
                TreeUtil.findNode(root) { (it.userObject as? AgentsItem)?.key == selected }?.let { tree.selectionPath = TreePath(it.path) }
            }
        } finally {
            rebuilding = false
        }
        updateHint()
    }

    private fun agentNode(m: TaskModel, id: String): DefaultMutableTreeNode {
        val n = DefaultMutableTreeNode(AgentsItem.Agent(id))
        for (c in m.children(id)) n.add(agentNode(m, c.id))
        return n
    }

    /** Expands every node except the ones the person collapsed (the queue starts collapsed). */
    private fun expand(n: DefaultMutableTreeNode) {
        val key = (n.userObject as? AgentsItem)?.key
        if (n !== root) {
            if (key == null || !isExpanded(key)) return
            if (n.childCount > 0) tree.expandPath(TreePath(n.path))
        }
        for (i in 0 until n.childCount) expand(n.getChildAt(i) as DefaultMutableTreeNode)
    }

    private fun isExpanded(key: String) = (key in DEFAULT_COLLAPSED) == (key in toggled)

    private fun setExpanded(key: String, on: Boolean) {
        if ((key in DEFAULT_COLLAPSED) == on) toggled.add(key) else toggled.remove(key)
    }

    private fun updateHint() {
        val st = svc.model.state
        hint.text = when {
            svc.starting -> "Starting sy…"
            !svc.running -> "Run starts sy for this project first."
            st?.running == true -> "A task is running: a new task is queued and runs unattended after it."
            else -> "\"@agent message\" follows up with a finished agent."
        }
        runButton.isEnabled = !svc.starting
    }

    private inner class Renderer : ColoredTreeCellRenderer() {
        override fun customizeCellRenderer(tree: JTree, value: Any?, selected: Boolean, expanded: Boolean, leaf: Boolean, row: Int, hasFocus: Boolean) {
            val item = (value as? DefaultMutableTreeNode)?.userObject as? AgentsItem ?: return
            val st = svc.model.state
            toolTipText = null
            when (item) {
                is AgentsItem.Status -> {
                    append(st?.project?.ifEmpty { null } ?: project.name)
                    val (desc, icon) = when {
                        svc.starting && !svc.running -> "starting…" to AnimatedIcon.Default.INSTANCE
                        !svc.connected -> (if (svc.streamError.isNotEmpty()) "disconnected: ${svc.streamError}" else "connecting…") to AllIcons.General.BalloonWarning
                        st?.running == true -> ((if (st.cancelling) "cancelling" else st.phase) + if (st.paused) " · paused" else "") to AnimatedIcon.Default.INSTANCE
                        st?.last != null -> ("idle · last task ${if (st.last.ok) "done" else "failed"}" + if (st.last.took.isNotEmpty()) " in ${st.last.took}" else "") to
                            (if (st.last.ok) AllIcons.RunConfigurations.TestPassed else AllIcons.RunConfigurations.TestFailed)
                        else -> "idle" to AllIcons.RunConfigurations.TestNotRan
                    }
                    append("  $desc", SimpleTextAttributes.GRAYED_ATTRIBUTES)
                    icon(icon)
                    toolTipText = html(
                        "Switchyard ${st?.version ?: ""}${if (st?.demo == true) " (demo)" else ""}",
                        st?.dir ?: "",
                        st?.task?.takeIf { it.isNotEmpty() }?.let { "task: " + oneLine(it, 300) } ?: "",
                        st?.last?.takeIf { !st.running }?.let { (if (it.ok) "done: " else "failed: ") + oneLine(it.text, 400) + (if (it.cost.isNotEmpty()) "\n" + it.cost else "") } ?: "",
                    )
                }
                is AgentsItem.Approvals -> {
                    append("Waiting for you (${st?.approvals?.size ?: 0})", SimpleTextAttributes.REGULAR_BOLD_ATTRIBUTES)
                    icon(AllIcons.General.BalloonWarning)
                }
                is AgentsItem.Approval -> {
                    val r = item.req
                    when (r.type) {
                        "plan" -> {
                            append("Approve plan: ${r.plan?.subtasks?.size ?: 0} subtask(s)")
                            append("  " + oneLine(r.plan?.summary?.ifEmpty { null } ?: r.task, 80), SimpleTextAttributes.GRAYED_ATTRIBUTES)
                            icon(AllIcons.Actions.Edit)
                        }
                        "changes" -> {
                            append("Review ${r.changes?.stepId ?: ""}: ${r.changes?.files?.size ?: 0} file(s)")
                            append("  " + oneLine(r.changes?.title, 80), SimpleTextAttributes.GRAYED_ATTRIBUTES)
                            icon(AllIcons.Actions.Diff)
                        }
                        else -> {
                            append("Budget reached")
                            append("  " + oneLine(r.budget?.text, 80), SimpleTextAttributes.GRAYED_ATTRIBUTES)
                            icon(AllIcons.General.BalloonWarning)
                        }
                    }
                    toolTipText = "Double-click or Enter to answer"
                }
                is AgentsItem.Queue -> {
                    append("Queue (${st?.queue?.size ?: 0})")
                    icon(AllIcons.Nodes.Folder)
                    toolTipText = "Queued tasks run unattended (no approvals) after the current one."
                }
                is AgentsItem.Job -> {
                    append(oneLine(item.label, 80))
                    append("  " + if (item.at.isNotEmpty()) "at " + item.at else "#" + item.id, SimpleTextAttributes.GRAYED_ATTRIBUTES)
                    icon(AllIcons.Vcs.History)
                }
                is AgentsItem.Agent -> agent(svc.model.nodes[item.id])
            }
        }

        private fun icon(i: javax.swing.Icon) {
            this.icon = i
        }

        private fun agent(n: AgentNode?) {
            if (n == null) {
                append("?")
                return
            }
            val m = svc.model
            append(if (n.id == TaskModel.MAIN) oneLine(m.taskText.ifEmpty { "main agent" }, 60) else n.id)
            val route = if (n.provider.isNotEmpty()) "${n.provider}:${n.model}" else ""
            val parts = listOf(if (n.id == TaskModel.MAIN) "main" else oneLine(n.title, 40), n.role, route, n.status.word).filter { it.isNotEmpty() }
            append("  " + parts.joinToString(" · "), SimpleTextAttributes.GRAYED_ATTRIBUTES)
            icon(
                when (n.status) {
                    AgentStatus.QUEUED -> AllIcons.RunConfigurations.TestNotRan
                    AgentStatus.RUNNING -> AnimatedIcon.Default.INSTANCE
                    AgentStatus.OK -> AllIcons.RunConfigurations.TestPassed
                    AgentStatus.FAILED -> AllIcons.RunConfigurations.TestFailed
                    AgentStatus.KILLED -> AllIcons.RunConfigurations.TestTerminated
                },
            )
            toolTipText = html(
                "${n.id} ${if (n.role.isNotEmpty()) "(${n.role})" else ""} $route",
                if (n.title.isNotEmpty() && n.id != TaskModel.MAIN) n.title else "",
                "status: ${n.status.word}" + if (n.tokens > 0) " · ${n.tokens} fresh tokens" else "",
                if (n.last.isNotEmpty()) (if (n.lastErr) "error: " else "") + oneLine(n.last, 400) else "",
                if (n.files.isNotEmpty()) "files: " + n.files.take(20).joinToString(", ") + if (n.files.size > 20) " …" else "" else "",
                if (n.merge.isNotEmpty()) "merge ${if (n.mergeOK) "✓" else "✗"} ${n.merge}" else "",
                if (n.id != TaskModel.MAIN) "Double-click to follow up" else "",
            )
        }
    }

    companion object {
        val AGENT_ID: DataKey<String> = DataKey.create("switchyard.agentId")
        val APPROVAL_ID: DataKey<String> = DataKey.create("switchyard.approvalId")
        private const val MAX_LOG_LINES = 5000
        private val DEFAULT_COLLAPSED = setOf("queue")

        private fun html(vararg lines: String): String =
            "<html>" + lines.filter { it.isNotBlank() }.joinToString("<br>") { Notify.escape(it) } + "</html>"
    }
}
