package io.github.sparkz400.switchyard.ide

import com.intellij.ide.BrowserUtil
import com.intellij.ide.trustedProjects.TrustedProjects
import com.intellij.notification.NotificationType
import com.intellij.openapi.Disposable
import com.intellij.openapi.application.ApplicationManager
import com.intellij.openapi.application.ModalityState
import com.intellij.openapi.components.Service
import com.intellij.openapi.options.ShowSettingsUtil
import com.intellij.openapi.progress.ProgressIndicator
import com.intellij.openapi.progress.Task
import com.intellij.openapi.project.Project
import com.intellij.openapi.ui.Messages
import com.intellij.openapi.util.SystemInfo
import com.intellij.openapi.wm.StatusBar
import com.intellij.openapi.wm.ToolWindowManager
import io.github.sparkz400.switchyard.core.ApprovalRequest
import io.github.sparkz400.switchyard.core.Json
import io.github.sparkz400.switchyard.core.LocateEnv
import io.github.sparkz400.switchyard.core.Plan
import io.github.sparkz400.switchyard.core.SessionRow
import io.github.sparkz400.switchyard.core.SseMessage
import io.github.sparkz400.switchyard.core.StateView
import io.github.sparkz400.switchyard.core.SyApi
import io.github.sparkz400.switchyard.core.SyEvent
import io.github.sparkz400.switchyard.core.SyNotFoundException
import io.github.sparkz400.switchyard.core.SyProcess
import io.github.sparkz400.switchyard.core.TaskModel
import io.github.sparkz400.switchyard.core.asList
import io.github.sparkz400.switchyard.core.clientArgs
import io.github.sparkz400.switchyard.core.eventMillis
import io.github.sparkz400.switchyard.core.locateSy
import io.github.sparkz400.switchyard.core.logLine
import io.github.sparkz400.switchyard.core.messageOf
import io.github.sparkz400.switchyard.core.oneLine
import io.github.sparkz400.switchyard.core.parseFollowUp
import io.github.sparkz400.switchyard.core.splitArgLines
import io.github.sparkz400.switchyard.core.str
import java.io.Closeable
import java.io.File
import java.util.concurrent.CompletableFuture
import java.util.concurrent.ConcurrentLinkedQueue
import java.util.concurrent.CopyOnWriteArrayList
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean

const val INSTALL_URL = "https://github.com/sparkz400/switchyard#install"

/**
 * One `sy web --client` per project: started from the project folder,
 * logged in with the bootstrap it prints (like a browser tab), its event
 * stream folded into the agent tree and the activity log. Everything
 * stays on 127.0.0.1. sy stops when the project closes, when the IDE
 * exits, and if the IDE dies (its stdin closes).
 *
 * The model, the log and the listeners belong to the EDT; HTTP calls run
 * on pooled threads.
 */
@Service(Service.Level.PROJECT)
class SyService(val project: Project) : Disposable {
    interface Listener {
        /** The state, the tree or the connection changed. */
        fun changed() {}

        /** Lines were added to the activity log. */
        fun logged(lines: List<String>) {}
    }

    private class Session(val proc: SyProcess, val api: SyApi, val dir: String) {
        @Volatile
        var stream: Closeable? = null

        @Volatile
        var stopping = false
    }

    @Volatile
    private var session: Session? = null

    /** Guards the hand-over of a started sy against the project closing meanwhile. */
    private val lifecycle = Any()

    @Volatile
    private var disposed = false

    /** A sy that has started but is not attached yet (dispose stops it too). */
    private var pending: Session? = null

    /** The indicator of a start in progress: Stop cancels it. */
    @Volatile
    private var startIndicator: ProgressIndicator? = null

    /** Whether the project is trusted; tests replace it. Every way of running sy asks it (canRunSy). */
    var trustCheck: (Project) -> Boolean = { TrustedProjects.isProjectTrusted(it) }

    var starting = false
        private set
    val model = TaskModel()
    var connected = false
        private set
    var streamError = ""
        private set
    private val logLines = ArrayList<String>()
    private val listeners = CopyOnWriteArrayList<Listener>()
    val review = ReviewController(project, this)
    private var replaying = false
    private var lastLogged = 0L
    private val seenApprovals = HashSet<String>()

    /** The balloon of each waiting question; it goes away once the question is answered (here or elsewhere). */
    private val approvalNotes = HashMap<String, com.intellij.notification.Notification>()
    private val inbox = ConcurrentLinkedQueue<SseMessage>()
    private val draining = AtomicBoolean(false)

    val running: Boolean get() = session != null
    val api: SyApi? get() = session?.api
    val dir: String? get() = session?.dir

    /** The environment for sy instead of the IDE's (tests: scratch config folders). */
    var environment: Map<String, String>? = null

    /** The pid of the sy this project started, while it runs. */
    val syPid: Long? get() = session?.proc?.pid
    val hello get() = session?.proc?.hello
    val log: List<String> get() = logLines

    fun addListener(l: Listener, parent: Disposable) {
        listeners.add(l)
        com.intellij.openapi.util.Disposer.register(parent) { listeners.remove(l) }
    }

    private fun fire() {
        for (l in listeners) l.changed()
    }

    // --- activity log ----------------------------------------------------------------

    /** Adds lines to the activity log (EDT). */
    fun appendLog(lines: List<String>) {
        if (lines.isEmpty()) return
        logLines.addAll(lines)
        if (logLines.size > MAX_LOG) logLines.subList(0, logLines.size - MAX_LOG * 3 / 4).clear()
        for (l in listeners) l.logged(lines)
    }

    fun appendLog(line: String) = appendLog(listOf(line))

    private val logInbox = ConcurrentLinkedQueue<String>()
    private val logDraining = AtomicBoolean(false)

    /** From any thread: lines are added on the EDT in batches (sy's stderr can be chatty). */
    private fun appendLogLater(line: String) {
        logInbox.add(line)
        if (logDraining.compareAndSet(false, true)) {
            edt {
                logDraining.set(false)
                val lines = ArrayList<String>()
                while (true) lines.add(logInbox.poll() ?: break)
                appendLog(lines)
            }
        }
    }

    // --- process ---------------------------------------------------------------------

    /**
     * Whether sy may run in this project at all: only in a trusted one. sy
     * runs git (and agents) in the project, and a cloned repository's git
     * config could otherwise run its own commands. Every launch asks this
     * (start, undo); it tells the person why not.
     */
    fun canRunSy(): Boolean {
        if (project.basePath == null) {
            Notify.error(project, "This project has no folder on this machine: sy works in a project folder.")
            return false
        }
        if (!trustCheck(project)) {
            Notify.error(project, "This project is not trusted (safe mode). Trust it first: sy runs git, agents and commands in the project.")
            return false
        }
        return true
    }

    /** Whether the project is trusted (for enabling actions; no message). */
    fun trusted(): Boolean = project.basePath != null && trustCheck(project)

    /** The sy executable, or null after telling the person how to fix it. */
    fun findSy(): String? {
        val configured = SySettings.get().state.syPath
        val found = locateSy(
            configured,
            LocateEnv(SystemInfo.isWindows, System.getenv(), System.getProperty("user.home")) { File(it).isFile },
        )
        found.path?.let { return it }
        appendLog("sy was not found. Looked at:\n  " + found.tried.joinToString("\n  "))
        val what = if (found.problem != null) {
            "\"$configured\" (Settings | Tools | Switchyard): ${found.problem}."
        } else if (configured.isNotBlank()) {
            "\"$configured\" (Settings | Tools | Switchyard) is not an executable file."
        } else {
            "Could not find sy${if (SystemInfo.isWindows) ".exe" else ""} on PATH. Install Switchyard, or set the sy executable in Settings | Tools | Switchyard."
        }
        Notify.error(
            project, what,
            "Open Settings" to { ShowSettingsUtil.getInstance().showSettingsDialog(project, SySettingsConfigurable::class.java) },
            "How to Install" to { BrowserUtil.browse(INSTALL_URL) },
            "Show Details" to { showToolWindow() },
        )
        return null
    }

    /** Starts sy for the project (EDT). onReady runs on the EDT once it is connected. */
    fun start(onReady: (() -> Unit)? = null) {
        session?.let {
            Notify.info(project, "Switchyard is already running for ${project.name}.")
            onReady?.invoke()
            return
        }
        if (starting) return
        if (!canRunSy()) return
        val dir = project.basePath ?: return
        val exe = findSy() ?: return
        val args = clientArgs(dir, splitArgLines(SySettings.get().state.extraArgs))
        starting = true
        fire()
        appendLog("starting: $exe ${args.joinToString(" ") { Json.write(it) }}")
        object : Task.Backgroundable(project, "Starting Switchyard", true) {
            var s: Session? = null

            override fun run(indicator: ProgressIndicator) {
                indicator.isIndeterminate = true
                startIndicator = indicator
                val proc = SyProcess.start(
                    exe, args, File(dir), { l -> appendLogLater("sy: $l") }, env = environment,
                    cancelled = { indicator.isCanceled || disposed },
                )
                val api = SyApi(proc.hello.url) { proc.request("bootstrap").str("bootstrap") }
                try {
                    api.login(proc.hello.bootstrap)
                } catch (e: Exception) {
                    proc.stop(2000)
                    api.close()
                    throw e
                }
                val ns = Session(proc, api, dir)
                // Hand over under the lock: if the project closes now, dispose
                // sees the pending sy and stops it (onSuccess may never run).
                synchronized(lifecycle) {
                    if (disposed || indicator.isCanceled) {
                        stopSession(ns, 2000)
                        return
                    }
                    pending = ns
                }
                s = ns
            }

            override fun onSuccess() {
                val ns = s ?: return
                synchronized(lifecycle) {
                    if (pending !== ns) return // stopped meanwhile
                    pending = null
                    if (disposed || project.isDisposed) {
                        stopSession(ns, 2000)
                        return
                    }
                }
                attach(ns)
                onReady?.invoke()
            }

            override fun onCancel() {
                synchronized(lifecycle) {
                    pending?.let { stopSession(it, 2000) }
                    pending = null
                }
                appendLog("the start of sy was cancelled.")
            }

            override fun onThrowable(error: Throwable) {
                if (error is io.github.sparkz400.switchyard.core.StartCancelledException) {
                    appendLog("the start of sy was cancelled.")
                    return
                }
                val msg = error.message ?: error.javaClass.simpleName
                appendLog("could not start sy: $msg")
                if (error is SyNotFoundException) {
                    findSy()
                } else {
                    Notify.error(project, "Could not start sy: $msg", "Show Log" to { showToolWindow() })
                }
            }

            override fun onFinished() {
                startIndicator = null
                starting = false
                fire()
            }
        }.queue()
    }

    private fun attach(s: Session) {
        session = s
        val h = s.proc.hello
        appendLog("Switchyard ${h.version} for ${h.dir} on ${h.url} (pid ${h.pid})" + if (h.demo) " - demo mode" else "")
        s.proc.onExit { code ->
            edt {
                if (session !== s) return@edt
                detach()
                if (!s.stopping) {
                    appendLog("sy stopped unexpectedly (code $code).")
                    Notify.error(
                        project, "sy stopped unexpectedly (code $code).",
                        "Show Log" to { showToolWindow() },
                        "Restart" to { start() },
                    )
                }
            }
        }
        review.attach(s.dir)
        s.stream = s.api.stream(
            onMessage = { m -> enqueue(s, m) },
            onStatus = { ok, err -> edt { if (session === s) onStream(ok, err) } },
        )
        fire()
    }

    /** Stops sy (EDT): closing its stdin lets it cancel the task and stop its agents. */
    fun stop(graceMs: Long = 10_000): CompletableFuture<Unit> {
        if (session == null && starting) {
            // Stop while sy starts: cancel the start (sy is stopped as soon as it is seen).
            startIndicator?.cancel()
            synchronized(lifecycle) {
                pending?.let { stopSession(it, 2000) }
                pending = null
            }
            return CompletableFuture.completedFuture(Unit)
        }
        val s = session ?: return CompletableFuture.completedFuture(Unit)
        s.stopping = true
        detach()
        appendLog("stopping sy…")
        return stopSession(s, graceMs).thenApply { edt { appendLog("sy stopped.") } }
    }

    private fun stopSession(s: Session, graceMs: Long): CompletableFuture<Unit> {
        s.stopping = true
        try {
            s.stream?.close()
        } catch (_: Exception) {
        }
        return CompletableFuture.supplyAsync({
            s.proc.stop(graceMs)
            s.api.close()
        }, com.intellij.util.concurrency.AppExecutorUtil.getAppExecutorService())
    }

    private fun detach() {
        val s = session ?: return
        try {
            s.stream?.close()
        } catch (_: Exception) {
        }
        session = null
        inbox.clear()
        model.reset()
        model.state = null
        connected = false
        streamError = ""
        review.attach(null)
        seenApprovals.clear()
        approvalNotes.values.forEach { it.expire() }
        approvalNotes.clear()
        lastLogged = 0
        replaying = false
        fire()
    }

    /**
     * The project closes (or the IDE exits): sy's stdin is closed at once, so
     * it cancels the task and stops its agents on its own; a background
     * thread kills it if it is still running after 10 seconds. If the IDE
     * exits before that, the operating system closes the pipe the same way.
     */
    override fun dispose() {
        synchronized(lifecycle) {
            disposed = true
            pending?.let { stopSession(it, 2000) }
            pending = null
        }
        startIndicator?.cancel()
        val s = session ?: return
        session = null
        s.stopping = true
        try {
            s.stream?.close()
        } catch (_: Exception) {
        }
        s.proc.closeInput()
        val t = Thread({
            if (!s.proc.waitFor(10_000)) s.proc.kill()
            s.api.close()
        }, "Switchyard stop")
        t.isDaemon = true
        t.start()
    }

    // --- event stream -------------------------------------------------------------------

    private fun onStream(ok: Boolean, err: String?) {
        connected = ok
        streamError = err ?: ""
        if (!ok && err != null) appendLog("event stream: $err (reconnecting)")
        fire()
    }

    /** Called on the stream's thread: messages are folded on the EDT in batches. */
    private fun enqueue(s: Session, m: SseMessage) {
        inbox.add(m)
        if (draining.compareAndSet(false, true)) {
            ApplicationManager.getApplication().invokeLater({ drain(s) }, ModalityState.any())
        }
    }

    private fun drain(s: Session) {
        draining.set(false)
        if (session !== s) return
        val lines = ArrayList<String>()
        var changed = false
        while (true) {
            val m = inbox.poll() ?: break
            changed = onMessage(m, lines) || changed
        }
        appendLog(lines)
        if (changed) fire()
    }

    private fun onMessage(m: SseMessage, lines: MutableList<String>): Boolean {
        val v = try {
            Json.parse(m.data)
        } catch (_: Exception) {
            return false
        }
        when (m.event) {
            "state" -> {
                onState(StateView.from(v))
                return true
            }
            "reset" -> {
                replaying = true
                model.reset()
            }
            "synced" -> replaying = false
            "ev" -> {
                val e = SyEvent.from(v)
                val line = model.fold(e)
                val ts = eventMillis(e)
                // A reconnect replays what the log already shows: skip it.
                val fresh = !replaying || ts > lastLogged
                if (line != null && fresh) lines.add(logLine(e, line))
                if (e.kind == "task_done" && fresh && (!replaying || lastLogged > 0)) notifyTaskEnd(e)
                // sy wrote merged changes into the project: show them in the IDE now.
                if (!replaying && ((e.kind == "merge" && e.ok) || e.kind == "task_done")) refreshProject()
                if (ts > lastLogged) lastLogged = ts
            }
            "notice" -> {
                val o = v as? Map<*, *>
                val level = o?.get("level") as? String ?: ""
                val text = o?.get("text") as? String ?: ""
                if (level == "warn" || level == "error") StatusBar.Info.set("Switchyard: $text", project)
                return false
            }
            else -> return false
        }
        return true
    }

    private fun onState(st: StateView) {
        model.state = st
        review.sync(st.approvals)
        notifyApprovals(st.approvals)
    }

    private fun notifyTaskEnd(e: SyEvent) {
        if (!SySettings.get().state.notifyTaskEnd) return
        val text = oneLine(e.text, 300)
        if (e.ok) {
            Notify.info(project, "Task done: $text", "Show Log" to { showToolWindow() })
        } else {
            Notify.show(project, NotificationType.WARNING, "Task failed: $text", "Show Log" to { showToolWindow() })
        }
    }

    private fun notifyApprovals(approvals: List<ApprovalRequest>) {
        val live = approvals.map { it.id }.toSet()
        seenApprovals.retainAll(live)
        approvalNotes.keys.filter { it !in live }.forEach { approvalNotes.remove(it)?.expire() }
        val notify = SySettings.get().state.notifyApprovals
        for (a in approvals) {
            if (!seenApprovals.add(a.id) || !notify) continue
            val (msg, action) = when (a.type) {
                "plan" -> "Approve the plan (${a.plan?.subtasks?.size ?: 0} subtasks) for \"${oneLine(a.task, 80)}\"" to "Review Plan"
                "changes" -> "Review the changes of ${a.changes?.stepId ?: ""} (${a.changes?.files?.size ?: 0} file(s))" to "Review Changes"
                "budget" -> (a.budget?.text ?: "The budget is reached") to "Decide"
                else -> continue
            }
            approvalNotes[a.id] = Notify.info(project, msg, action to { answerApproval(a.id) })
        }
    }

    // --- commands ---------------------------------------------------------------------------

    fun showToolWindow() {
        ToolWindowManager.getInstance(project).getToolWindow(TOOL_WINDOW_ID)?.show()
    }

    /** Runs fn on a pooled thread with the session's API; errors become notifications. ok runs on the EDT. */
    fun <T> background(what: String, fn: (SyApi) -> T, ok: (T) -> Unit = {}) {
        val api = api
        if (api == null) {
            Notify.error(project, "Switchyard is not running: start it first (Switchyard tool window, or Tools | Switchyard | Start).")
            return
        }
        ApplicationManager.getApplication().executeOnPooledThread {
            try {
                val r = fn(api)
                ui { ok(r) }
            } catch (e: Exception) {
                val msg = e.message ?: e.javaClass.simpleName
                edt {
                    appendLog("$what failed: $msg")
                    Notify.error(project, "$what failed: $msg", "Show Log" to { showToolWindow() })
                }
            }
        }
    }

    /** Shows sy's answer: in the log and the status bar. */
    fun said(message: String) {
        appendLog(message)
        StatusBar.Info.set("Switchyard: $message", project)
    }

    /** Starts a task, or queues it while one runs; "@agent message" follows up. */
    fun runTask(text: String, onDone: (() -> Unit)? = null) {
        val t = text.trim()
        if (t.isEmpty()) return
        background("Run task", { it.call("POST", "/api/task", mapOf("text" to t)) }) {
            said(messageOf(it, "started"))
            onDone?.invoke()
        }
    }

    fun cancelTask() {
        background("Cancel", { it.call("POST", "/api/cancel") }) { said(messageOf(it, "cancelling")) }
    }

    fun followUp(agent: String, message: String) {
        val text = "@$agent ${message.trim()}"
        if (parseFollowUp(text) == null) {
            Notify.error(project, "\"$agent\" is not an agent id.")
            return
        }
        runTask(text)
    }

    fun sessions(ok: (List<SessionRow>) -> Unit) {
        background("List agents", { api -> api.call("GET", "/api/sessions").asList().map { SessionRow.from(it) } }, ok)
    }

    /** Answers a plan: ok with the (edited) plan, or reject (cancels the task). done(true) when sy took it. */
    fun answerPlan(id: String, plan: Plan?, ok: Boolean, done: (Boolean) -> Unit = {}) {
        val body = mapOf("ok" to ok, "plan" to (plan ?: Plan("", emptyList())).toJson())
        val api = api ?: run {
            Notify.error(project, "Switchyard is not running.")
            done(false)
            return
        }
        ApplicationManager.getApplication().executeOnPooledThread {
            try {
                val r = api.call("POST", "/api/approvals/${enc(id)}/plan", body)
                edt {
                    said(messageOf(r))
                    done(true)
                }
            } catch (e: Exception) {
                edt {
                    Notify.error(project, "The plan was not answered: ${e.message}")
                    done(false)
                }
            }
        }
    }

    fun answerBudget(id: String, ok: Boolean) {
        background("Budget answer", { it.call("POST", "/api/approvals/${enc(id)}/budget", mapOf("ok" to ok)) }) { said(messageOf(it)) }
    }

    fun approvals(type: String? = null): List<ApprovalRequest> =
        (model.state?.approvals ?: emptyList()).filter { type == null || it.type == type }

    /** Opens what answers a waiting question: the plan dialog, the review, or the budget choice (EDT). */
    fun answerApproval(id: String) {
        val req = approvals().firstOrNull { it.id == id }
        if (req == null) {
            Notify.info(project, "That question is no longer waiting.")
            return
        }
        when (req.type) {
            "plan" -> PlanDialog.ask(project, this, req)
            "changes" -> review.openReview(req.id)
            "budget" -> {
                val choice = Messages.showDialog(
                    project, (req.budget?.hint ?: "").ifEmpty { "Go on past the limit, or stop the task (finished work is kept)." },
                    "Switchyard: " + (req.budget?.text ?: "Budget reached"),
                    arrayOf("Go On", "Stop the Task", "Later"), 0, Messages.getQuestionIcon(),
                )
                when (choice) {
                    0 -> answerBudget(req.id, true)
                    1 -> answerBudget(req.id, false)
                }
            }
        }
    }

    fun openInBrowser() {
        val s = session ?: run {
            Notify.error(project, "Switchyard is not running: start it first.")
            return
        }
        ApplicationManager.getApplication().executeOnPooledThread {
            try {
                // The link holds a single-use bootstrap: the browser tab gets its own session.
                val link = s.proc.request("link").str("link")
                if (!link.startsWith(s.proc.hello.url + "/")) throw IllegalStateException("sy answered an unexpected link")
                ui { BrowserUtil.browse(link) }
            } catch (e: Exception) {
                edt { Notify.error(project, "Could not open the web UI: ${e.message}") }
            }
        }
    }

    /** Runs a one-shot sy command (no shell; stdin closed so it never waits). */
    private fun runSy(exe: String, args: List<String>, dir: String): Pair<Int, String> {
        if (!File(exe).isAbsolute) throw IllegalStateException("the sy path must be absolute: $exe")
        val cmd = listOf(File(exe).absolutePath) + if (SystemInfo.isWindows) args.map { io.github.sparkz400.switchyard.core.windowsArg(it) } else args
        val pb = ProcessBuilder(cmd).directory(File(dir)).redirectErrorStream(true)
        environment?.let {
            pb.environment().clear()
            pb.environment().putAll(it)
        }
        val p = pb.start()
        p.outputStream.close()
        val out = CompletableFuture.supplyAsync { p.inputStream.readBytes().toString(Charsets.UTF_8) }
        if (!p.waitFor(120, TimeUnit.SECONDS)) {
            // sy and whatever it started (git): a child holding the pipe would block the reader.
            p.descendants().forEach { it.destroyForcibly() }
            p.destroyForcibly()
            try {
                p.inputStream.close()
            } catch (_: java.io.IOException) {
            }
            throw IllegalStateException("sy ${args.firstOrNull() ?: ""} did not finish in 2 minutes")
        }
        return p.exitValue() to out.get(10, TimeUnit.SECONDS)
    }

    /** Saves the IDE's unsaved edits, so sy sees (and never overwrites) them (EDT). */
    fun saveAll() {
        com.intellij.openapi.fileEditor.FileDocumentManager.getInstance().saveAllDocuments()
    }

    /** sy changed files in the project: let the IDE see them now, not on the next focus. */
    fun refreshProject() {
        val d = session?.dir ?: project.basePath ?: return
        val root = com.intellij.openapi.vfs.LocalFileSystem.getInstance().findFileByPath(d) ?: return
        com.intellij.openapi.vfs.VfsUtil.markDirtyAndRefresh(true, true, true, root)
    }

    /** Shows what `sy undo` would change and asks before it runs `sy undo --yes`. */
    fun undoLastTask() {
        if (model.state?.running == true) {
            Notify.warn(project, "A task is running. Cancel it or wait for it to finish before undoing.")
            return
        }
        if (!canRunSy()) return
        val dir = session?.dir ?: project.basePath ?: return
        val exe = findSy() ?: return
        saveAll()
        ApplicationManager.getApplication().executeOnPooledThread {
            try {
                // Without --yes, sy undo prints what it would do and, with no
                // answer on stdin, changes nothing.
                val (code, out) = runSy(exe, listOf("undo", "--dir", dir), dir)
                val text = out.replace(Regex("\\n?Undo these changes\\? \\[y/N]\\s*"), "\n").replace(Regex("Nothing changed\\.\\s*$"), "").trim()
                ui {
                    appendLog("--- sy undo (preview) ---\n$text")
                    if (code != 0 || !Regex("^This changes \\d+ file", RegexOption.MULTILINE).containsMatchIn(text)) {
                        Notify.info(project, "Undo: " + oneLine(text, 300))
                        return@ui
                    }
                    val detail = if (text.length > 1800) text.take(1800) + "\n… (see the activity log)" else text
                    val ok = Messages.showOkCancelDialog(project, detail, "Undo the Last Switchyard Task?", "Undo", "Cancel", Messages.getWarningIcon())
                    if (ok != Messages.OK) return@ui
                    saveAll()
                    ApplicationManager.getApplication().executeOnPooledThread {
                        try {
                            val (c2, o2) = runSy(exe, listOf("undo", "--yes", "--dir", dir), dir)
                            edt {
                                appendLog("--- sy undo ---\n" + o2.trim())
                                refreshProject()
                                if (c2 == 0) {
                                    val saidLine = oneLine(o2.lines().filter { Regex("done|redo", RegexOption.IGNORE_CASE).containsMatchIn(it) }.joinToString(" "), 200)
                                    Notify.info(project, saidLine.ifEmpty { "Undone." })
                                } else {
                                    Notify.error(project, "Undo failed: " + oneLine(o2, 300), "Show Log" to { showToolWindow() })
                                }
                            }
                        } catch (e: Exception) {
                            edt { Notify.error(project, "Undo failed: ${e.message}") }
                        }
                    }
                }
            } catch (e: Exception) {
                edt { Notify.error(project, "Undo failed: ${e.message}") }
            }
        }
    }

    companion object {
        const val TOOL_WINDOW_ID = "Switchyard"
        private const val MAX_LOG = 5000

        fun get(project: Project): SyService = project.getService(SyService::class.java)

        fun enc(s: String): String = java.net.URLEncoder.encode(s, Charsets.UTF_8).replace("+", "%20")
    }
}

/** Runs fn on the EDT in any modality: for model and log updates, so the views update under open dialogs too. */
fun edt(fn: () -> Unit) {
    val app = ApplicationManager.getApplication()
    if (app.isDispatchThread) fn() else app.invokeLater(fn, ModalityState.any())
}

/** Runs fn on the EDT when no modal dialog is in the way: for code that shows dialogs or opens editors. */
fun ui(fn: () -> Unit) {
    ApplicationManager.getApplication().invokeLater(fn, ModalityState.defaultModalityState())
}
