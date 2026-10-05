package io.github.sparkz400.relayweft.ide

import com.intellij.openapi.application.ApplicationManager
import com.intellij.openapi.components.PersistentStateComponent
import com.intellij.openapi.components.RoamingType
import com.intellij.openapi.components.Service
import com.intellij.openapi.components.State
import com.intellij.openapi.components.Storage
import com.intellij.openapi.fileChooser.FileChooserDescriptorFactory
import com.intellij.openapi.options.Configurable
import com.intellij.openapi.ui.TextFieldWithBrowseButton
import com.intellij.ui.components.JBCheckBox
import com.intellij.ui.components.JBLabel
import com.intellij.ui.components.JBScrollPane
import com.intellij.ui.components.JBTextArea
import com.intellij.util.ui.FormBuilder
import com.intellij.util.ui.UIUtil
import javax.swing.JComponent

/**
 * The plugin's settings. They live in the IDE's own configuration
 * (relayweft.xml, not synced between machines), never in a project: the
 * rw path and its arguments choose what program runs, and a cloned
 * repository must not be able to choose that.
 */
@Service(Service.Level.APP)
@State(name = "RelayweftSettings", storages = [Storage(value = "relayweft.xml", roamingType = RoamingType.DISABLED)])
class RwSettings : PersistentStateComponent<RwSettings.Data> {
    class Data {
        /** Path to rw (rw.exe on Windows); empty: search PATH and the usual install folders. */
        var rwPath: String = ""

        /** Extra arguments for `rw web --client`, one per line, not shell-quoted. */
        var extraArgs: String = ""
        var notifyApprovals: Boolean = true
        var notifyTaskEnd: Boolean = true
    }

    private var data = Data()

    override fun getState(): Data = data

    override fun loadState(state: Data) {
        data = state
    }

    companion object {
        fun get(): RwSettings = ApplicationManager.getApplication().getService(RwSettings::class.java)
    }
}

class RwSettingsConfigurable : Configurable {
    private var path: TextFieldWithBrowseButton? = null
    private var args: JBTextArea? = null
    private var notifyApprovals: JBCheckBox? = null
    private var notifyTaskEnd: JBCheckBox? = null

    override fun getDisplayName(): String = "Relayweft"

    override fun createComponent(): JComponent {
        val p = TextFieldWithBrowseButton()
        p.addBrowseFolderListener(null, FileChooserDescriptorFactory.createSingleFileNoJarsDescriptor().withTitle("The rw Executable"))
        val a = JBTextArea(4, 40)
        val na = JBCheckBox("Notify when a plan, a change set, a budget or a merge conflict question waits for me")
        val nt = JBCheckBox("Notify when a task is done or failed")
        path = p
        args = a
        notifyApprovals = na
        notifyTaskEnd = nt
        reset()
        return FormBuilder.createFormBuilder()
            .addLabeledComponent("rw executable:", p)
            .addComponentToRightColumn(hint("Empty: look for rw on PATH, then in Go's, Scoop's and WinGet's folders. On Windows it must be rw.exe (rw starts without a shell)."))
            .addLabeledComponent("Extra arguments:", JBScrollPane(a), true)
            .addComponentToRightColumn(hint("For `rw web --client`, one per line, nothing is shell-quoted. Examples: --threads, 2 (two lines), or --demo to try it with fake agents."))
            .addComponent(na)
            .addComponent(nt)
            .addComponentFillVertically(javax.swing.JPanel(), 0)
            .panel
    }

    private fun hint(text: String) = JBLabel(text).apply {
        componentStyle = UIUtil.ComponentStyle.SMALL
        fontColor = UIUtil.FontColor.BRIGHTER
    }

    override fun isModified(): Boolean {
        val d = RwSettings.get().state
        return path?.text != d.rwPath || args?.text != d.extraArgs ||
            notifyApprovals?.isSelected != d.notifyApprovals || notifyTaskEnd?.isSelected != d.notifyTaskEnd
    }

    override fun apply() {
        val d = RwSettings.get().state
        d.rwPath = path?.text?.trim() ?: ""
        d.extraArgs = args?.text ?: ""
        d.notifyApprovals = notifyApprovals?.isSelected ?: true
        d.notifyTaskEnd = notifyTaskEnd?.isSelected ?: true
    }

    override fun reset() {
        val d = RwSettings.get().state
        path?.text = d.rwPath
        args?.text = d.extraArgs
        notifyApprovals?.isSelected = d.notifyApprovals
        notifyTaskEnd?.isSelected = d.notifyTaskEnd
    }

    override fun disposeUIResources() {
        path = null
        args = null
        notifyApprovals = null
        notifyTaskEnd = null
    }
}
