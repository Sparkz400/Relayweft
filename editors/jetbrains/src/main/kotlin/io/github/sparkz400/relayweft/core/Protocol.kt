package io.github.sparkz400.relayweft.core

// The parts of rw web's JSON API the plugin uses (internal/web), decoded
// by hand from Json values. Field names follow the server's JSON tags.

/** The version of the `rw web --client` hello line this plugin speaks (web.ClientProtocol). */
const val PROTOCOL = 1

/** The line `rw web --client` prints first (web.ClientHello). */
data class ClientHello(
    val protocol: Int,
    val version: String,
    val url: String,
    val addr: String,
    val bootstrap: String,
    val dir: String,
    val pid: Long,
    val demo: Boolean,
)

private val LOOPBACK_URL = Regex("""^http://(127\.0\.0\.1|localhost|\[::1]):\d{1,5}$""")
private val BOOTSTRAP = Regex("^[0-9a-f]{64}$")

/** Parses the hello line; it throws with a readable reason. */
fun parseHello(line: String): ClientHello {
    val v = try {
        Json.parse(line)
    } catch (e: JsonException) {
        throw IllegalStateException(
            "rw printed something that is not the client hello: ${line.take(200)} (is this rw older than `rw web --client`?)",
        )
    }
    val m = v as? Map<*, *> ?: throw IllegalStateException("rw printed an unexpected first line (not a web-client hello)")
    val h = m.asObj()
    if (h.str("relayweft") != "web-client") {
        throw IllegalStateException("rw printed an unexpected first line (not a web-client hello)")
    }
    val protocol = (h["protocol"] as? Long)?.toInt()
    if (protocol != PROTOCOL) {
        throw IllegalStateException("rw speaks client protocol ${h["protocol"]}, this plugin speaks $PROTOCOL: update one of them")
    }
    val url = h.str("url")
    if (!LOOPBACK_URL.matches(url)) throw IllegalStateException("rw announced a non-loopback address: $url")
    val bootstrap = h.str("bootstrap")
    if (!BOOTSTRAP.matches(bootstrap)) throw IllegalStateException("rw announced no valid bootstrap")
    return ClientHello(protocol, h.str("version"), url, h.str("addr"), bootstrap, h.str("dir"), h.long("pid"), h.bool("demo"))
}

data class TokenUsage(val input: Long, val cached: Long, val output: Long)

data class RouteDecision(
    val stepId: String,
    val role: String,
    val provider: String,
    val model: String,
    val effort: String,
    val rule: String,
    val reason: String,
    val confidence: Double,
    val fallback: Boolean,
)

/** event.Event as JSON; kind is the event kind's name. */
data class RwEvent(
    val kind: String,
    val agentId: String = "",
    val parentId: String = "",
    val provider: String = "",
    val model: String = "",
    val role: String = "",
    val text: String = "",
    val tokens: TokenUsage? = null,
    val ts: String = "",
    val ok: Boolean = false,
    val decision: RouteDecision? = null,
) {
    companion object {
        fun from(v: Any?): RwEvent {
            val m = v.asObj()
            val t = m.obj("tokens")
            val d = m.obj("decision")
            return RwEvent(
                kind = m.str("kind"),
                agentId = m.str("agent_id"),
                parentId = m.str("parent_id"),
                provider = m.str("provider"),
                model = m.str("model"),
                role = m.str("role"),
                text = m.str("text"),
                tokens = t?.let { TokenUsage(it.long("input"), it.long("cached"), it.long("output")) },
                ts = m.str("ts"),
                ok = m.bool("ok"),
                decision = d?.let {
                    RouteDecision(
                        it.str("step_id"), it.str("role"), it.str("provider"), it.str("model"), it.str("effort"),
                        it.str("rule"), it.str("reason"), it.double("confidence"), it.bool("fallback"),
                    )
                },
            )
        }
    }
}

data class Subtask(
    val id: String,
    val title: String,
    val kind: String,
    val prompt: String,
    val files: List<String> = emptyList(),
    val dependsOn: List<String> = emptyList(),
    val role: String = "",
    val repo: String = "",
) {
    fun toJson(): Map<String, Any?> {
        val m = linkedMapOf<String, Any?>(
            "id" to id, "title" to title, "kind" to kind, "prompt" to prompt, "files" to files, "depends_on" to dependsOn,
        )
        if (role.isNotEmpty()) m["role"] = role
        if (repo.isNotEmpty()) m["repo"] = repo
        return m
    }

    companion object {
        fun from(v: Any?): Subtask {
            val m = v.asObj()
            return Subtask(
                m.str("id"), m.str("title"), m.str("kind"), m.str("prompt"), m.strings("files"), m.strings("depends_on"),
                m.str("role"), m.str("repo"),
            )
        }
    }
}

data class Plan(val summary: String, val subtasks: List<Subtask>, val repos: List<String> = emptyList()) {
    fun toJson(): Map<String, Any?> {
        val m = linkedMapOf<String, Any?>("summary" to summary, "subtasks" to subtasks.map { it.toJson() })
        if (repos.isNotEmpty()) m["repos"] = repos
        return m
    }

    companion object {
        fun from(v: Any?): Plan {
            val m = v.asObj()
            return Plan(m.str("summary"), m["subtasks"].asList().map { Subtask.from(it) }, m.strings("repos"))
        }
    }
}

data class FileView(
    val path: String,
    /** A (added), M (modified) or D (deleted). */
    val status: String,
    val added: Int,
    val deleted: Int,
    val binary: Boolean,
    val patch: String,
    val splittable: Boolean,
    val header: String,
    val hunks: List<String>,
) {
    companion object {
        fun from(v: Any?): FileView {
            val m = v.asObj()
            return FileView(
                m.str("path"), m.str("status"), m.int("added"), m.int("deleted"), m.bool("binary"), m.str("patch"),
                m.bool("splittable"), m.str("header"), m.strings("hunks"),
            )
        }
    }
}

data class ChangeView(val stepId: String, val title: String, val summary: String, val round: Int, val files: List<FileView>) {
    companion object {
        fun from(v: Any?): ChangeView {
            val m = v.asObj()
            return ChangeView(m.str("step_id"), m.str("title"), m.str("summary"), m.int("round"), m["files"].asList().map { FileView.from(it) })
        }
    }
}

data class BudgetView(val text: String, val hint: String)

data class ApprovalRequest(
    val id: String,
    /** plan, changes or budget. */
    val type: String,
    val task: String,
    val plan: Plan?,
    val changes: ChangeView?,
    val budget: BudgetView?,
    val created: String,
) {
    companion object {
        fun from(v: Any?): ApprovalRequest {
            val m = v.asObj()
            return ApprovalRequest(
                m.str("id"), m.str("type"), m.str("task"),
                m.obj("plan")?.let { Plan.from(it) },
                m.obj("changes")?.let { ChangeView.from(it) },
                m.obj("budget")?.let { BudgetView(it.str("text"), it.str("hint")) },
                m.str("created"),
            )
        }
    }
}

data class ResultView(val ok: Boolean, val text: String, val cost: String, val took: String)

data class QueuedJob(val id: Long, val label: String, val kind: String, val at: String)

data class StateView(
    val version: String,
    val dir: String,
    val project: String,
    val demo: Boolean,
    val running: Boolean,
    val cancelling: Boolean,
    val paused: Boolean,
    val phase: String,
    val task: String,
    val queue: List<QueuedJob>,
    val approvals: List<ApprovalRequest>,
    val runningAgents: List<String>,
    val last: ResultView?,
) {
    companion object {
        fun from(v: Any?): StateView {
            val m = v.asObj()
            return StateView(
                m.str("version"), m.str("dir"), m.str("project"), m.bool("demo"), m.bool("running"), m.bool("cancelling"),
                m.bool("paused"), m.str("phase"), m.str("task"),
                m["queue"].asList().map { val j = it.asObj(); QueuedJob(j.long("id"), j.str("label"), j.str("kind"), j.str("at")) },
                m["approvals"].asList().map { ApprovalRequest.from(it) },
                m.strings("running_agents"),
                m.obj("last")?.let { ResultView(it.bool("ok"), it.str("text"), it.str("cost"), it.str("took")) },
            )
        }
    }
}

data class SessionRow(val agent: String, val role: String, val provider: String, val model: String, val title: String, val running: Boolean) {
    companion object {
        fun from(v: Any?): SessionRow {
            val m = v.asObj()
            return SessionRow(m.str("agent"), m.str("role"), m.str("provider"), m.str("model"), m.str("title"), m.bool("running"))
        }
    }
}

/** The "message" of an API answer ({"message": "..."}), or a fallback. */
fun messageOf(v: Any?, fallback: String = "done"): String = v.asObj().str("message").ifEmpty { fallback }
