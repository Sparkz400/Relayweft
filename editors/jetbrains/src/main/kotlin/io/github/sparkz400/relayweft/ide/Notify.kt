package io.github.sparkz400.relayweft.ide

import com.intellij.notification.Notification
import com.intellij.notification.NotificationAction
import com.intellij.notification.NotificationGroupManager
import com.intellij.notification.NotificationType
import com.intellij.openapi.project.Project

/** Balloons in the "Relayweft" notification group (plugin.xml). */
object Notify {
    const val GROUP = "Relayweft"

    fun show(
        project: Project?,
        type: NotificationType,
        text: String,
        vararg actions: Pair<String, () -> Unit>,
    ): Notification {
        val n = NotificationGroupManager.getInstance().getNotificationGroup(GROUP)
            .createNotification("Relayweft", escape(text), type)
        for ((label, fn) in actions) {
            n.addAction(NotificationAction.createSimpleExpiring(label) { fn() })
        }
        n.notify(project)
        return n
    }

    fun info(project: Project?, text: String, vararg actions: Pair<String, () -> Unit>) = show(project, NotificationType.INFORMATION, text, *actions)

    fun warn(project: Project?, text: String, vararg actions: Pair<String, () -> Unit>) = show(project, NotificationType.WARNING, text, *actions)

    fun error(project: Project?, text: String, vararg actions: Pair<String, () -> Unit>) = show(project, NotificationType.ERROR, text, *actions)

    /** A plain label text Swing would render as HTML (it starts with <html>) gets a space in front. */
    fun plain(s: String): String = if (javax.swing.plaf.basic.BasicHTML.isHTMLString(s)) " $s" else s

    /** Notification content is HTML: show rw's text as text. */
    fun escape(s: String): String =
        s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;").replace("\n", "<br>")
}

/** Text for a Swing label or title that must not be read as HTML. */
fun plainText(s: String): String = Notify.plain(s)
