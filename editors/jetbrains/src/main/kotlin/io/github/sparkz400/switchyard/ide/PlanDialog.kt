package io.github.sparkz400.switchyard.ide

import com.intellij.openapi.progress.ProgressManager
import com.intellij.openapi.project.Project
import com.intellij.openapi.ui.ComboBox
import com.intellij.openapi.ui.DialogWrapper
import com.intellij.openapi.ui.Messages
import com.intellij.openapi.ui.ValidationInfo
import com.intellij.openapi.util.Disposer
import com.intellij.ui.CheckBoxList
import com.intellij.ui.CollectionListModel
import com.intellij.ui.ColoredListCellRenderer
import com.intellij.ui.DocumentAdapter
import com.intellij.ui.OnePixelSplitter
import com.intellij.ui.SimpleTextAttributes
import com.intellij.ui.ToolbarDecorator
import com.intellij.ui.components.JBLabel
import com.intellij.ui.components.JBList
import com.intellij.ui.components.JBScrollPane
import com.intellij.ui.components.JBTextArea
import com.intellij.ui.components.JBTextField
import com.intellij.util.ui.FormBuilder
import com.intellij.util.ui.JBUI
import io.github.sparkz400.switchyard.core.ApprovalRequest
import io.github.sparkz400.switchyard.core.PLAN_KINDS
import io.github.sparkz400.switchyard.core.PLAN_ROLES
import io.github.sparkz400.switchyard.core.PlanDraft
import io.github.sparkz400.switchyard.core.Subtask
import io.github.sparkz400.switchyard.core.messageOf
import io.github.sparkz400.switchyard.core.oneLine
import java.awt.BorderLayout
import java.awt.Dimension
import java.awt.event.ActionEvent
import javax.swing.AbstractAction
import javax.swing.Action
import javax.swing.JComponent
import javax.swing.JList
import javax.swing.JPanel
import javax.swing.ListSelectionModel
import javax.swing.event.DocumentEvent

/**
 * Plan approval: the plan's subtasks in a list (add, delete, move up and
 * down) and the selected one's title, kind, role, prompt, files and
 * dependencies, like the web page's plan editor. Approve sends the edited
 * plan, Reject cancels the task, Decide Later keeps it waiting.
 */
class PlanDialog(private val project: Project, private val svc: SyService, private val req: ApprovalRequest) : DialogWrapper(project, true) {
    val draft = PlanDraft(req.plan!!)
    private val listModel = CollectionListModel<Subtask>()
    private val list = JBList(listModel)
    private val idLabel = JBLabel()
    private val titleField = JBTextField()
    private val kind = ComboBox<String>()
    private val role = ComboBox<String>()
    private val repo = ComboBox<String>()
    private val prompt = JBTextArea(8, 50)
    private val files = JBTextArea(3, 50)
    private val deps = CheckBoxList<String>()
    private var loading = false
    private var selected = -1
    private var gone = false

    init {
        setTitle("Switchyard Plan: " + oneLine(req.plan!!.summary.ifEmpty { req.task }, 80))
        setOKButtonText("Approve Plan")
        setCancelButtonText("Decide Later")
        val watch = Disposer.newDisposable()
        Disposer.register(disposable, watch)
        svc.addListener(object : SyService.Listener {
            override fun changed() {
                val waiting = svc.approvals("plan").any { it.id == req.id }
                if (!waiting && !gone) {
                    gone = true
                    setErrorText("This plan is no longer waiting (it was answered elsewhere, or the task ended).")
                    isOKActionEnabled = false
                }
            }
        }, watch)
        init()
    }

    override fun createCenterPanel(): JComponent {
        list.selectionMode = ListSelectionModel.SINGLE_SELECTION
        list.accessibleContext.accessibleName = "Subtasks"
        prompt.accessibleContext.accessibleName = "Subtask prompt"
        titleField.accessibleContext.accessibleName = "Subtask title"
        list.cellRenderer = object : ColoredListCellRenderer<Subtask>() {
            override fun customizeCellRenderer(l: JList<out Subtask>, s: Subtask, index: Int, sel: Boolean, focus: Boolean) {
                append("${index + 1}. ", SimpleTextAttributes.GRAYED_ATTRIBUTES)
                append(s.id, SimpleTextAttributes.REGULAR_BOLD_ATTRIBUTES)
                append("  " + oneLine(s.title.ifEmpty { s.prompt }, 50))
                val meta = listOf(s.kind, s.role, if (s.dependsOn.isNotEmpty()) "after " + s.dependsOn.joinToString(", ") else "")
                    .filter { it.isNotEmpty() }.joinToString(" · ")
                if (meta.isNotEmpty()) append("  $meta", SimpleTextAttributes.GRAYED_SMALL_ATTRIBUTES)
            }
        }
        list.addListSelectionListener { if (!it.valueIsAdjusting) select(list.selectedIndex) }
        val left = ToolbarDecorator.createDecorator(list)
            .setAddAction { reload(draft.add()) }
            .setRemoveAction {
                val i = list.selectedIndex
                if (i >= 0) {
                    draft.delete(i)
                    reload(minOf(i, draft.size - 1))
                }
            }
            .setMoveUpAction { val i = list.selectedIndex; if (draft.move(i, i - 1)) reload(i - 1) }
            .setMoveDownAction { val i = list.selectedIndex; if (draft.move(i, i + 1)) reload(i + 1) }
            .createPanel()

        for (k in PLAN_KINDS) kind.addItem(k)
        for (r in PLAN_ROLES) role.addItem(r)
        role.renderer = object : ColoredListCellRenderer<String>() {
            override fun customizeCellRenderer(l: JList<out String>, v: String?, index: Int, sel: Boolean, focus: Boolean) {
                append(if (v.isNullOrEmpty()) "auto (router)" else v.replace('_', ' '))
            }
        }
        repo.addItem("")
        draft.repos.drop(1).forEach { repo.addItem(it) }
        repo.renderer = object : ColoredListCellRenderer<String>() {
            override fun customizeCellRenderer(l: JList<out String>, v: String?, index: Int, sel: Boolean, focus: Boolean) {
                append(if (v.isNullOrEmpty()) "the project folder" + (draft.repos.firstOrNull()?.let { " ($it)" } ?: "") else v)
            }
        }
        prompt.lineWrap = true
        prompt.wrapStyleWord = true
        val onText = object : DocumentAdapter() {
            override fun textChanged(e: DocumentEvent) = commit()
        }
        titleField.document.addDocumentListener(onText)
        prompt.document.addDocumentListener(onText)
        files.document.addDocumentListener(onText)
        kind.addActionListener { commit() }
        role.addActionListener { commit() }
        repo.addActionListener { commit() }
        deps.setCheckBoxListListener { _, _ -> commit() }

        val form = FormBuilder.createFormBuilder()
            .addLabeledComponent("Subtask:", idLabel)
            .addLabeledComponent("Title:", titleField)
            .addLabeledComponent("Kind:", kind)
            .addTooltip("explore and research are read-only; edit changes files")
            .addLabeledComponent("Role:", role)
            .apply { if (draft.repos.isNotEmpty()) addLabeledComponent("Repo:", repo) }
            .addLabeledComponent("Prompt:", JBScrollPane(prompt), true)
            .addLabeledComponent("Files (one per line):", JBScrollPane(files), true)
            .addLabeledComponent("Runs after:", JBScrollPane(deps).apply { preferredSize = Dimension(200, 90) }, true)
            .panel
        form.border = JBUI.Borders.emptyLeft(10)

        val split = OnePixelSplitter(false, 0.38f)
        split.firstComponent = left
        split.secondComponent = form
        val top = JBLabel(
            "<html><b>" + Notify.escape(oneLine(req.plan!!.summary, 300)) + "</b><br>" +
                Notify.escape("Task: " + oneLine(req.task, 300)) + "<br>Nothing has run yet. Edit, add, delete or reorder the subtasks, then approve.</html>",
        )
        top.border = JBUI.Borders.emptyBottom(8)
        val root = JPanel(BorderLayout())
        root.add(top, BorderLayout.NORTH)
        root.add(split, BorderLayout.CENTER)
        root.preferredSize = Dimension(900, 560)
        reload(0)
        return root
    }

    /** Rebuilds the list from the draft and selects index i. */
    private fun reload(i: Int) {
        loading = true
        try {
            listModel.replaceAll(draft.subtasks)
        } finally {
            loading = false
        }
        selected = -1
        if (i in 0 until draft.size) list.selectedIndex = i else select(-1)
    }

    private fun select(i: Int) {
        if (loading) return
        selected = i
        loading = true
        try {
            val s = if (i in 0 until draft.size) draft[i] else null
            for (c in listOf(titleField, kind, role, repo, prompt, files, deps)) c.isEnabled = s != null
            idLabel.text = plainText(s?.id ?: "(no subtask selected)")
            titleField.text = s?.title ?: ""
            if (s != null && (0 until kind.itemCount).none { kind.getItemAt(it) == s.kind }) kind.addItem(s.kind)
            kind.selectedItem = s?.kind ?: "edit"
            if (s != null && (0 until role.itemCount).none { role.getItemAt(it) == s.role }) role.addItem(s.role)
            role.selectedItem = s?.role ?: ""
            if (s != null && (0 until repo.itemCount).none { repo.getItemAt(it) == s.repo }) repo.addItem(s.repo)
            repo.selectedItem = s?.repo ?: ""
            prompt.text = s?.prompt ?: ""
            prompt.caretPosition = 0
            files.text = s?.files?.joinToString("\n") ?: ""
            deps.clear()
            if (s != null) {
                for (o in draft.subtasks) if (o.id != s.id) deps.addItem(o.id, plainText(o.id + "  " + oneLine(o.title, 40)), o.id in s.dependsOn)
            }
        } finally {
            loading = false
        }
    }

    /** Writes the form back into the draft. */
    private fun commit() {
        if (loading) return
        val i = selected
        if (i !in 0 until draft.size) return
        val old = draft[i]
        val checked = draft.subtasks.map { it.id }.filter { it != old.id && deps.isItemSelected(it) }
        draft.update(
            i,
            old.copy(
                title = titleField.text,
                kind = (kind.selectedItem as? String) ?: old.kind,
                role = (role.selectedItem as? String) ?: "",
                repo = (repo.selectedItem as? String) ?: "",
                prompt = prompt.text,
                files = files.text.lines().map { it.trim() }.filter { it.isNotEmpty() },
                dependsOn = checked,
            ),
        )
        loading = true
        try {
            listModel.setElementAt(draft[i], i)
        } finally {
            loading = false
        }
    }

    override fun getPreferredFocusedComponent(): JComponent = list

    override fun doValidate(): ValidationInfo? = draft.problem()?.let { ValidationInfo(it, list) }

    override fun createLeftSideActions(): Array<Action> = arrayOf(object : AbstractAction("Reject Plan…") {
        override fun actionPerformed(e: ActionEvent?) {
            val ok = Messages.showOkCancelDialog(
                contentPanel, "Reject the plan? The task is cancelled.", "Switchyard", "Reject", "Cancel", Messages.getWarningIcon(),
            )
            if (ok == Messages.OK && send(false)) close(REJECTED)
        }
    })

    override fun doOKAction() {
        if (!okAction.isEnabled) return
        if (send(true)) super.doOKAction()
    }

    /** Sends the answer with a modal progress; on failure the dialog stays open and says why. */
    private fun send(ok: Boolean): Boolean {
        val api = svc.api
        if (api == null) {
            setErrorText("Switchyard is not running any more.")
            return false
        }
        val body = mapOf("ok" to ok, "plan" to draft.toPlan().toJson())
        return try {
            val res = ProgressManager.getInstance().runProcessWithProgressSynchronously<Any?, Exception>(
                { api.call("POST", "/api/approvals/${SyService.enc(req.id)}/plan", body) },
                if (ok) "Approving the plan" else "Rejecting the plan", false, project,
            )
            svc.said(messageOf(res))
            true
        } catch (e: Exception) {
            setErrorText("sy did not take the answer: ${e.message}")
            false
        }
    }

    companion object {
        const val REJECTED = NEXT_USER_EXIT_CODE

        /** Shows the plan dialog for a waiting plan (EDT). */
        fun ask(project: Project, svc: SyService, req: ApprovalRequest) {
            if (req.plan == null) return
            PlanDialog(project, svc, req).show()
        }
    }
}

private fun FormBuilder.addTooltip(text: String): FormBuilder = addComponentToRightColumn(
    JBLabel(text).apply {
        componentStyle = com.intellij.util.ui.UIUtil.ComponentStyle.SMALL
        fontColor = com.intellij.util.ui.UIUtil.FontColor.BRIGHTER
    },
)
