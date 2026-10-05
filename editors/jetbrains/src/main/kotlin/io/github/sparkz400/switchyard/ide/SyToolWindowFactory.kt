package io.github.sparkz400.switchyard.ide

import com.intellij.openapi.project.DumbAware
import com.intellij.openapi.project.Project
import com.intellij.openapi.wm.ToolWindow
import com.intellij.openapi.wm.ToolWindowFactory
import com.intellij.openapi.wm.ToolWindowManager
import com.intellij.ui.content.ContentFactory

/** The Switchyard tool window: an Agents tab and a Review tab. */
class SyToolWindowFactory : ToolWindowFactory, DumbAware {
    override fun createToolWindowContent(project: Project, toolWindow: ToolWindow) {
        val svc = SyService.get(project)
        val cf = ContentFactory.getInstance()
        val agents = AgentsPanel(project, svc)
        val agentsContent = cf.createContent(agents, "Agents", false)
        agentsContent.setDisposer(agents)
        toolWindow.contentManager.addContent(agentsContent)
        val review = ReviewPanel(project, svc)
        val reviewContent = cf.createContent(review, "Review", false)
        reviewContent.setDisposer(review)
        reviewContent.putUserData(REVIEW_TAB, true)
        review.onCount = { n -> reviewContent.displayName = if (n > 0) "Review ($n)" else "Review" }
        review.onCount(svc.review.pending.size)
        toolWindow.contentManager.addContent(reviewContent)
    }

    companion object {
        private val REVIEW_TAB = com.intellij.openapi.util.Key.create<Boolean>("switchyard.reviewTab")

        fun selectReviewTab(project: Project) {
            val tw = ToolWindowManager.getInstance(project).getToolWindow(SyService.TOOL_WINDOW_ID) ?: return
            tw.contentManager.contents.firstOrNull { it.getUserData(REVIEW_TAB) == true }?.let { tw.contentManager.setSelectedContent(it) }
        }
    }
}
