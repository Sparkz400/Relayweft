package io.github.sparkz400.switchyard.core

import java.time.Instant
import java.time.ZoneId
import java.time.format.DateTimeFormatter

// The agent tree and the activity log, folded from sy web's event stream
// the way the web page does it (internal/web/static/app.js: fold) and the
// VS Code extension does (editors/vscode/src/model.ts).

enum class AgentStatus { QUEUED, RUNNING, OK, FAILED, KILLED;

    val word: String get() = name.lowercase()
}

class AgentNode(val id: String) {
    var parent: String = if (id == TaskModel.MAIN) "" else TaskModel.MAIN
    var role = ""
    var title = ""
    var provider = ""
    var model = ""
    var status = AgentStatus.QUEUED
    var last = ""
    var lastErr = false
    val files = ArrayList<String>()
    var merge = ""
    var mergeOK = false
    var tokens = 0L
    var started = 0L
    var ended = 0L
}

fun oneLine(s: String?, n: Int = 0): String {
    val t = (s ?: "").split(Regex("\\s+")).filter { it.isNotEmpty() }.joinToString(" ")
    return if (n > 0 && t.length > n) t.substring(0, maxOf(0, n - 1)) + "…" else t
}

private fun parseTs(ts: String): Long {
    if (ts.isEmpty()) return 0L
    return try {
        java.time.OffsetDateTime.parse(ts).toInstant().toEpochMilli()
    } catch (_: Exception) {
        try {
            Instant.parse(ts).toEpochMilli()
        } catch (_: Exception) {
            0L
        }
    }
}

class TaskModel {
    companion object {
        const val MAIN = "main"
        const val REVIEWER = "reviewer"
        const val JUDGE = "judge"
    }

    val nodes = LinkedHashMap<String, AgentNode>()

    /** Agent ids in the order they appeared (main, reviewer and judge excluded). */
    val order = ArrayList<String>()
    var state: StateView? = null
    var taskText = ""
        private set

    init {
        reset()
    }

    fun reset() {
        nodes.clear()
        nodes[MAIN] = AgentNode(MAIN).apply { role = "planner"; title = "main agent" }
        order.clear()
        taskText = ""
    }

    fun node(id: String): AgentNode = nodes.getOrPut(id) {
        AgentNode(id).also {
            it.role = when (id) {
                REVIEWER -> "reviewer"
                JUDGE -> "judge"
                else -> ""
            }
            if (id != MAIN && id != REVIEWER && id != JUDGE) order.add(id)
        }
    }

    /** The children of id, in order (reviewer and judge last). */
    fun children(id: String): List<AgentNode> {
        val out = ArrayList<AgentNode>()
        for (k in order) {
            val n = nodes[k] ?: continue
            if (parentOf(n) == id) out.add(n)
        }
        if (id == MAIN) {
            for (k in listOf(REVIEWER, JUDGE)) nodes[k]?.let { out.add(it) }
        }
        return out
    }

    private fun parentOf(n: AgentNode): String =
        // Nest under the parent the orchestrator named when it is in the tree.
        if (n.parent.isNotEmpty() && n.parent != n.id && nodes.containsKey(n.parent)) n.parent else MAIN

    /** Applies one event. It returns the activity log line for it, if any. */
    fun fold(e: SyEvent): String? {
        when (e.kind) {
            "task_start" -> {
                reset()
                taskText = e.text
                nodes[MAIN]!!.title = e.text
                return "task: " + e.text
            }
            "task_done" -> {
                val ts = parseTs(e.ts).takeIf { it > 0 } ?: System.currentTimeMillis()
                for (n in nodes.values) {
                    if (n.status == AgentStatus.QUEUED || n.status == AgentStatus.RUNNING) {
                        if (n.id == MAIN && e.ok) continue
                        n.status = AgentStatus.KILLED
                        if (n.ended == 0L) n.ended = ts
                    }
                }
                val main = nodes[MAIN]!!
                if (e.ok) {
                    main.status = AgentStatus.OK
                } else if (main.status != AgentStatus.KILLED) {
                    main.status = AgentStatus.FAILED
                }
                main.ended = ts
                return (if (e.ok) "done: " else "failed: ") + e.text
            }
            "phase" -> return "phase: " + e.text
            "log" -> return (if (e.agentId.isNotEmpty()) e.agentId + ": " else "") + e.text
            "provider" -> return "${e.provider}: ${e.text}"
            "quota" -> return null
            "route" -> {
                val d = e.decision ?: return null
                val label = d.provider + ":" + d.model + if (d.effort.isNotEmpty()) "@" + d.effort else ""
                return "${e.agentId} → $label  [${d.rule} ${String.format(java.util.Locale.ROOT, "%.2f", d.confidence)}] ${d.reason}" +
                    if (d.fallback) " (limit fallback)" else ""
            }
            "checkpoint" -> {
                node(REVIEWER)
                return "reviewer: ${if (e.ok) "approved" else "changes requested"} · ${oneLine(e.text)}"
            }
            "merge" -> {
                if (e.agentId.isEmpty()) return null
                val n = node(e.agentId)
                n.merge = e.text
                n.mergeOK = e.ok
                return "${e.agentId}: merge ${if (e.ok) "✓" else "✗"} ${e.text}"
            }
        }
        if (e.agentId.isEmpty()) return null
        return nodeEvent(node(e.agentId), e)
    }

    private fun nodeEvent(n: AgentNode, e: SyEvent): String? {
        val ts = parseTs(e.ts).takeIf { it > 0 } ?: System.currentTimeMillis()
        if (e.parentId.isNotEmpty() && n.id != MAIN) n.parent = e.parentId
        when (e.kind) {
            "queued" -> {
                if (e.text.isNotEmpty() && n.id != MAIN) n.title = e.text
                if (e.role.isNotEmpty() && n.role.isEmpty()) n.role = e.role
                if (e.provider.isNotEmpty()) {
                    n.provider = e.provider
                    n.model = e.model
                    if (e.role.isNotEmpty()) n.role = e.role
                    n.status = AgentStatus.QUEUED
                }
                return null
            }
            "started" -> {
                n.status = AgentStatus.RUNNING
                n.started = ts
                n.ended = 0
                if (e.provider.isNotEmpty()) n.provider = e.provider
                if (e.model.isNotEmpty()) n.model = e.model
                if (e.role.isNotEmpty()) n.role = e.role
            }
            "thinking", "tool", "message" -> if (e.text.isNotEmpty()) {
                n.last = e.text
                n.lastErr = false
            }
            "edit" -> {
                if (e.text.isNotEmpty() && !n.files.contains(e.text)) n.files.add(e.text)
                n.last = "edit " + e.text
                n.lastErr = false
            }
            "usage" -> {
                val t = e.tokens
                if (t != null) n.tokens += t.input - t.cached + t.output
                return null
            }
            "error" -> {
                n.last = e.text
                n.lastErr = true
            }
            "limit" -> {
                n.last = "usage limit: " + e.text
                n.lastErr = true
            }
            "done" -> {
                n.ended = ts
                n.status = if (e.ok) AgentStatus.OK else if (e.text.contains("killed")) AgentStatus.KILLED else AgentStatus.FAILED
                if (e.text.isNotEmpty()) {
                    n.last = e.text
                    n.lastErr = !e.ok
                }
            }
        }
        var text = e.text
        if (e.kind == "done" && text.isBlank()) text = n.status.word
        if ((e.kind == "thinking" || e.kind == "message") && text.isBlank()) return null
        val prov = if (e.provider.isNotEmpty()) " (${e.provider})" else ""
        return "${n.id}$prov ${e.kind}: $text"
    }
}

private val TIME = DateTimeFormatter.ofPattern("HH:mm:ss")

/** Formats a log line with the event's local time. */
fun logLine(e: SyEvent, text: String, zone: ZoneId = ZoneId.systemDefault()): String {
    val ms = parseTs(e.ts).takeIf { it > 0 } ?: System.currentTimeMillis()
    return "[" + TIME.format(Instant.ofEpochMilli(ms).atZone(zone)) + "] " + text
}

/** The event's time in milliseconds (0 when it has none). */
fun eventMillis(e: SyEvent): Long = parseTs(e.ts)

data class FollowUp(val agent: String, val message: String)

private val AGENT_ID = Regex("^[a-z0-9_-]+$")

/**
 * Parses "@agent message" or "@ message" (the newest agent) like the
 * server does; null when text is not a follow-up.
 */
fun parseFollowUp(text: String): FollowUp? {
    if (!text.startsWith("@")) return null
    val rest = text.substring(1)
    if (rest.isEmpty() || rest[0].isWhitespace()) return FollowUp("", rest.trim())
    val i = rest.indexOfFirst { it == ' ' || it == '\t' || it == '\n' }
    val agent = if (i < 0) rest else rest.substring(0, i)
    if (!AGENT_ID.matches(agent)) return null
    val msg = if (i < 0) "" else rest.substring(i).trim()
    return FollowUp(if (agent == "last") "" else agent, msg)
}
