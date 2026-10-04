package io.github.sparkz400.switchyard.ide

import com.intellij.notification.Notification
import com.intellij.notification.NotificationAction
import com.intellij.notification.NotificationGroupManager
import com.intellij.notification.NotificationType
import com.intellij.openapi.project.Project

/** Balloons in the "Switchyard" notification group (plugin.xml). */
object Notify {
    const val GROUP = "Switchyard"

    fun show(
        project: Project?,
        type: NotificationType,
        text: String,
        vararg actions: Pair<String, () -> Unit>,
    ): Notification {
        val n = NotificationGroupManager.getInstance().getNotificationGroup(GROUP)
            .createNotification("Switchyard", escape(text), type)
        for ((label, fn) in actions) {
            n.addAction(NotificationAction.createSimpleExpiring(label) { fn() })
        }
        n.notify(project)
        return n
    }

    fun info(project: Project?, text: String, vararg actions: Pair<String, () -> Unit>) = show(project, NotificationType.INFORMATION, text, *actions)

    fun warn(project: Project?, text: String, vararg actions: Pair<String, () -> Unit>) = show(project, NotificationType.WARNING, text, *actions)

    fun error(project: Project?, text: String, vararg actions: Pair<String, () -> Unit>) = show(project, NotificationType.ERROR, text, *actions)

    /** Notification content is HTML: show sy's text as text. */
    fun escape(s: String): String =
        s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;").replace("\n", "<br>")
}
