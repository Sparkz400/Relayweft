package io.github.sparkz400.switchyard.ide

import com.intellij.openapi.actionSystem.ActionUpdateThread
import com.intellij.openapi.actionSystem.AnActionEvent
import com.intellij.openapi.options.ShowSettingsUtil
import com.intellij.openapi.project.DumbAwareAction
import com.intellij.openapi.project.Project
import com.intellij.openapi.ui.Messages
import com.intellij.openapi.ui.popup.JBPopupFactory
import com.intellij.openapi.wm.StatusBar
import io.github.sparkz400.switchyard.core.oneLine

// The plugin's actions (registered in plugin.xml): in the tool window's
// toolbars, the Tools | Switchyard menu, the diff viewer's toolbar and
// Find Action. They read the service's state on the EDT.

abstract class SyAction : DumbAwareAction() {
    override fun getActionUpdateThread(): ActionUpdateThread = ActionUpdateThread.EDT

    protected fun svc(e: AnActionEvent): SyService? = e.project?.let { SyService.get(it) }

    override fun update(e: AnActionEvent) {
        val s = svc(e)
        e.presentation.isEnabled = s != null && enabled(s, e)
    }

    open fun enabled(s: SyService, e: AnActionEvent): Boolean = s.running
}

class StartAction : SyAction() {
    override fun enabled(s: SyService, e: AnActionEvent) = !s.running && !s.starting
    override fun actionPerformed(e: AnActionEvent) {
        val s = svc(e) ?: return
        s.showToolWindow()
        s.start()
    }
}

class StopAction : SyAction() {
    override fun actionPerformed(e: AnActionEvent) {
        svc(e)?.stop()
    }
}

class RunTaskAction : SyAction() {
    override fun enabled(s: SyService, e: AnActionEvent) = !s.starting
    override fun actionPerformed(e: AnActionEvent) {
        val s = svc(e) ?: return
        val project = e.project ?: return
        val busy = s.model.state?.running == true
        val text = Messages.showMultilineInputDialog(
            project,
            if (busy) "A task is running: this one is queued and runs unattended after it. \"@agent message\" follows up."
            else "Describe the task. \"@agent message\" follows up with a finished agent.",
            "Switchyard: Run a Task", "", null, null,
        )
        if (text.isNullOrBlank()) return
        if (s.running) s.runTask(text) else s.start { s.runTask(text) }
    }
}

class CancelTaskAction : SyAction() {
    override fun enabled(s: SyService, e: AnActionEvent) = s.running && s.model.state?.running == true
    override fun actionPerformed(e: AnActionEvent) {
        svc(e)?.cancelTask()
    }
}

class ApprovePlanAction : SyAction() {
    override fun enabled(s: SyService, e: AnActionEvent) = s.running && s.approvals("plan").isNotEmpty()
    override fun actionPerformed(e: AnActionEvent) {
        val s = svc(e) ?: return
        val project = e.project ?: return
        val plans = s.approvals("plan")
        val chosen = e.getData(AgentsPanel.APPROVAL_ID)?.let { id -> plans.firstOrNull { it.id == id } }
        when {
            chosen != null -> PlanDialog.ask(project, s, chosen)
            plans.isEmpty() -> Notify.info(project, "No plan is waiting for approval.")
            plans.size == 1 -> PlanDialog.ask(project, s, plans[0])
            else -> JBPopupFactory.getInstance()
                .createPopupChooserBuilder(plans.map { oneLine(it.plan?.summary?.ifEmpty { null } ?: it.task, 100) })
                .setTitle("Which Plan?")
                .setItemChosenCallback { label -> plans.firstOrNull { oneLine(it.plan?.summary?.ifEmpty { null } ?: it.task, 100) == label }?.let { PlanDialog.ask(project, s, it) } }
                .createPopup().showCenteredInCurrentWindow(project)
        }
    }
}

class FollowUpAction : SyAction() {
    override fun actionPerformed(e: AnActionEvent) {
        val s = svc(e) ?: return
        ask(e.project ?: return, s, e.getData(AgentsPanel.AGENT_ID))
    }

    companion object {
        /** Asks for the message (and the agent, when none is given) and sends "@agent message". */
        fun ask(project: Project, svc: SyService, agent: String?) {
            if (agent != null) {
                message(project, svc, agent)
                return
            }
            svc.sessions { rows ->
                val labels = listOf("@ (the newest agent)") + rows.map { r ->
                    val what = listOf(if (r.running) "running: delivered when its turn ends" else r.role, if (r.provider.isNotEmpty()) "${r.provider}:${r.model}" else "")
                        .filter { it.isNotEmpty() }.joinToString(" · ")
                    "@${r.agent}  $what  ${oneLine(r.title, 80)}".trim()
                }
                JBPopupFactory.getInstance().createPopupChooserBuilder(labels)
                    .setTitle("Follow Up with Which Agent?")
                    .setItemChosenCallback { label ->
                        val i = labels.indexOf(label)
                        message(project, svc, if (i <= 0) "" else rows[i - 1].agent)
                    }
                    .createPopup().showCenteredInCurrentWindow(project)
            }
        }

        private fun message(project: Project, svc: SyService, agent: String) {
            val msg = Messages.showMultilineInputDialog(
                project, "Your message to the agent. It keeps its session and context.",
                "Switchyard: Follow Up with " + if (agent.isEmpty()) "the Newest Agent" else "@$agent", "", null, null,
            )
            if (!msg.isNullOrBlank()) svc.followUp(agent, msg)
        }
    }
}

class UndoLastTaskAction : SyAction() {
    override fun enabled(s: SyService, e: AnActionEvent) = s.model.state?.running != true
    override fun actionPerformed(e: AnActionEvent) {
        svc(e)?.undoLastTask()
    }
}

class OpenInBrowserAction : SyAction() {
    override fun actionPerformed(e: AnActionEvent) {
        svc(e)?.openInBrowser()
    }
}

class OpenSettingsAction : SyAction() {
    override fun enabled(s: SyService, e: AnActionEvent) = true
    override fun actionPerformed(e: AnActionEvent) {
        ShowSettingsUtil.getInstance().showSettingsDialog(e.project, SySettingsConfigurable::class.java)
    }
}

// --- review -------------------------------------------------------------------------------

abstract class ReviewAction : SyAction() {
    override fun enabled(s: SyService, e: AnActionEvent) = s.review.pending.isNotEmpty()

    /** The review the action is about: the open diff's, the Review tab's selection, or null (pick). */
    protected fun reviewId(s: SyService, e: AnActionEvent): String? = s.review.targetOf(e)?.id ?: e.getData(ReviewPanel.REVIEW_ID)
}

class ReviewOpenAction : ReviewAction() {
    override fun actionPerformed(e: AnActionEvent) {
        val s = svc(e) ?: return
        s.review.openReview(reviewId(s, e))
    }
}

/** Accepts or rejects the hunk at the caret of a review diff. */
abstract class HunkAtCaretAction(private val on: Boolean) : ReviewAction() {
    override fun enabled(s: SyService, e: AnActionEvent) = s.review.targetOf(e)?.let { s.review.get(it.id) != null } == true

    override fun actionPerformed(e: AnActionEvent) {
        val s = svc(e) ?: return
        val project = e.project ?: return
        val (t, after, line) = s.review.caretTarget(e) ?: run {
            Notify.info(project, "Put the caret in a review diff first (double-click a file in the Review tab).")
            return
        }
        s.review.setHunkAtLine(t.id, t.file, after, line, on)?.let { StatusBar.Info.set("Switchyard: $it", project) }
    }
}

class AcceptHunkAction : HunkAtCaretAction(true)

class RejectHunkAction : HunkAtCaretAction(false)

abstract class WholeFileAction(private val on: Boolean) : ReviewAction() {
    override fun actionPerformed(e: AnActionEvent) {
        val s = svc(e) ?: return
        val t = s.review.targetOf(e)
        if (t != null) {
            s.review.setWholeFile(t.id, t.file, on)
            return
        }
        // The Review tab: the selected file.
        val project = e.project ?: return
        Notify.info(project, "Open a file's diff (or tick its checkbox in the Review tab) to accept or reject it.")
    }
}

class AcceptFileAction : WholeFileAction(true)

class RejectFileAction : WholeFileAction(false)

class ApplySelectedAction : ReviewAction() {
    override fun actionPerformed(e: AnActionEvent) {
        val s = svc(e) ?: return
        s.review.submit(reviewId(s, e))
    }
}

class RejectAllAction : ReviewAction() {
    override fun actionPerformed(e: AnActionEvent) {
        val s = svc(e) ?: return
        s.review.rejectAll(reviewId(s, e))
    }
}

class FeedbackAction : ReviewAction() {
    override fun actionPerformed(e: AnActionEvent) {
        val s = svc(e) ?: return
        s.review.feedback(reviewId(s, e))
    }
}
