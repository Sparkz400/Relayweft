package io.github.sparkz400.switchyard.ui

import com.intellij.driver.client.Driver
import com.intellij.driver.sdk.invokeAction
import com.intellij.driver.sdk.ui.Finder
import com.intellij.driver.sdk.ui.components.UiComponent
import com.intellij.driver.sdk.ui.components.common.IdeaFrameUI
import com.intellij.driver.sdk.ui.components.common.ideFrame
import com.intellij.driver.sdk.ui.components.elements.dialog
import com.intellij.driver.sdk.waitForIndicators
import com.intellij.ide.starter.driver.engine.runIdeWithDriver
import com.intellij.ide.starter.ide.IdeProductProvider
import com.intellij.ide.starter.models.TestCase
import com.intellij.ide.starter.plugins.PluginConfigurator
import com.intellij.ide.starter.project.LocalProjectInfo
import com.intellij.ide.starter.runner.Starter
import io.github.sparkz400.switchyard.it.Fake
import io.github.sparkz400.switchyard.it.ItEnv
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Assumptions
import org.junit.jupiter.api.Test
import java.awt.Rectangle
import java.awt.Robot
import java.awt.event.KeyEvent
import java.io.File
import java.nio.file.Path
import javax.imageio.ImageIO
import kotlin.time.Duration.Companion.minutes

/**
 * The plugin in a real IDE (IntelliJ IDEA Community, the version it is
 * compiled against, as a separate process with its own window), driven
 * through its UI with the Starter and Driver frameworks, against a real sy
 * with the scripted agent: open the tool window, Start, type a task and
 * Run, answer the plan in the plan dialog, reject one hunk in the Review
 * tab, look at the diff, apply, and Stop. Screenshots of the IDE window go
 * to build/reports/uiTest.
 *
 *   SY_UI_TEST=1 xvfb-run -a ./gradlew uiTest   (CI: the jetbrains workflow's ui job)
 */
class SwitchyardUiTest {
    private val task = "Please edit the notes file in this repository and add a new file next to it with a greeting"
    private val shots = File("build/reports/uiTest").apply { mkdirs() }

    private fun shot(frame: UiComponent, name: String) {
        try {
            val p = frame.component.getLocationOnScreen()
            val img = Robot().createScreenCapture(Rectangle(p.x, p.y, frame.component.width, frame.component.height))
            ImageIO.write(img, "png", File(shots, "$name.png"))
        } catch (e: Exception) {
            println("no screenshot $name: $e")
        }
    }

    private fun <T : Any> until(what: String, ms: Long = 60_000, fn: () -> T?): T {
        val end = System.currentTimeMillis() + ms
        var last: Throwable? = null
        while (true) {
            try {
                val v = fn()
                if (v != null && v != false) return v
            } catch (e: Throwable) {
                last = e
            }
            if (System.currentTimeMillis() > end) throw AssertionError("timed out waiting for $what" + (last?.let { ": $it" } ?: ""))
            Thread.sleep(300)
        }
    }

    private fun Finder.byName(name: String): UiComponent = x("//div[@accessiblename='$name']")

    private fun UiComponent.texts(): List<String> = getAllTexts { true }.map { it.text }

    @Test
    fun theFlowInARealIde() {
        // The driver moves the mouse and types with java.awt.Robot into
        // whatever window has the focus: run it on a display of its own
        // (CI: Xvfb), never by accident on someone's desktop.
        Assumptions.assumeTrue(System.getenv("SY_UI_TEST") == "1", "set SY_UI_TEST=1 to run the UI test (on a display of its own, e.g. xvfb-run)")
        val env = ItEnv("ui")
        val pluginPath = System.getProperty("path.to.build.plugin") ?: error("run through ./gradlew uiTest")
        Starter.newContext("switchyard-ui", TestCase(IdeProductProvider.IC, LocalProjectInfo(env.repo.toPath())).useRelease("2025.2.6.3")).apply {
            PluginConfigurator(this).installPluginFromPath(Path.of(pluginPath))
            // The sy to use, as a person would set it in Settings | Tools | Switchyard.
            val options = paths.configDir.resolve("options").toFile().apply { mkdirs() }
            File(options, "switchyard.xml").writeText(
                "<application>\n  <component name=\"SwitchyardSettings\">\n" +
                    "    <option name=\"syPath\" value=\"${env.sy.replace("&", "&amp;").replace("\"", "&quot;")}\" />\n" +
                    "  </component>\n</application>\n",
            )
            applyVMOptionsPatch {
                addSystemProperty("idea.trust.all.projects", true)
                addSystemProperty("ide.show.tips.on.startup.default.value", false)
                // No embedded browser: on CI its "suspended" balloon covers the prompt box.
                addSystemProperty("ide.browser.jcef.enabled", false)
                // sy's config and state in the scratch folders (the IDE passes its environment on to sy).
                for (k in listOf("APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME")) env.env[k]?.let { withEnv(k, it) }
            }
        }.runIdeWithDriver().useDriverAndCloseIde {
            waitForIndicators(5.minutes)
            flow(this, env)
        }
        until("no sy process left after the IDE closed", 20_000) { env.syProcesses().isEmpty() }
        env.cleanup()
    }

    private fun flow(driver: Driver, env: ItEnv) {
        driver.invokeAction("ActivateSwitchyardToolWindow")
        driver.ideFrame {
            val frame: IdeaFrameUI = this
            val agents = byName("Switchyard agents")
            until("the tool window") { agents.present() }
            shot(frame, "1-tool-window")

            // Start sy from the tool window's toolbar.
            byName("Start Switchyard").click()
            until("sy to run") { agents.texts().any { it.contains("idle") } }

            // Type a task in the prompt box and run it.
            val prompt = byName("Switchyard task")
            prompt.component.requestFocus()
            prompt.keyboard { typeText(task, 5) }
            // Ctrl+Enter runs it (balloons in the corner can cover the Run button on a small display).
            prompt.keyboard { hotKey(KeyEvent.VK_CONTROL, KeyEvent.VK_ENTER) }
            until("the plan in the tree", 120_000) { agents.texts().firstOrNull { it.contains("Approve plan: 2 subtask") } }
            shot(frame, "2-plan-waiting")

            // The plan dialog: edit the second subtask's prompt, approve.
            agents.getAllTexts { it.text.contains("Approve plan: 2 subtask") }.first().doubleClick()
            dialog({ contains(byTitle("Switchyard Plan")) }) {
                val list = byName("Subtasks")
                list.getAllTexts { it.text.contains("edit the notes") }.first().click()
                val p = byName("Subtask prompt")
                p.click()
                p.keyboard {
                    hotKey(KeyEvent.VK_CONTROL, KeyEvent.VK_A)
                    typeText("Change ${Fake.NOTES} and write <<added.txt>> containing <<${Fake.EDITED_PLAN_TEXT}>>", 5)
                }
                shot(this, "3-plan-dialog")
                x("//div[@class='JButton' and @visible_text='Approve Plan']").click()
            }

            // The change review: reject the second hunk of notes.txt in the Review tab.
            until("the review in the tree", 120_000) { agents.texts().firstOrNull { it.contains("Review edit: 2 file") } }
            // Select the row and press Enter (a double click works too, but
            // is timing-sensitive on a slow CI display).
            agents.getAllTexts { it.text.contains("Review edit: 2 file") }.first().click()
            agents.keyboard { enter() }
            val review = byName("Switchyard review")
            until("the review tab") { review.present() && review.texts().any { it.contains("hunk 2") } }
            review.getAllTexts { it.text == "hunk 2" }.first().click()
            review.keyboard { space() }
            until("the hunk unticked") { review.texts().any { it.contains("1 of 2 file(s)") || it.contains("1 partly") } }
            // The diff of notes.txt, with the rejected hunk struck through.
            review.getAllTexts { it.text == "hunk 2" }.first().click()
            review.keyboard { enter() }
            until("the diff of notes.txt") { x("//div[@class='IdeFrameImpl']").hasSubtext("${Fake.NOTES} — edit review") }
            Thread.sleep(2_000)
            shot(frame, "4-diff-review")

            // Apply from the Review tab's toolbar.
            // The bar above the diff: "Apply selected changes".
            x("//div[@visible_text='Apply selected changes']").click()
            until("the task to finish", 120_000) { agents.texts().any { it.contains("last task done") } }
            shot(frame, "5-done")

            val want = ItEnv.ORIG_NOTES.toMutableList()
            want[Fake.EDITED_LINES[0] - 1] = Fake.edited(Fake.EDITED_LINES[0])
            assertEquals(want.joinToString("\n") + "\n", env.readRepo(Fake.NOTES), "only the accepted hunk is in notes.txt")
            assertEquals(Fake.EDITED_PLAN_TEXT + "\n", env.readRepo("added.txt"), "the plan edited in the dialog reached the agent")

            // Stop from the toolbar: sy exits.
            byName("Stop Switchyard").click()
            until("sy to stop", 20_000) { env.syProcesses().isEmpty() }
            assertTrue(agents.texts().none { it.contains("idle") })
        }
    }
}
