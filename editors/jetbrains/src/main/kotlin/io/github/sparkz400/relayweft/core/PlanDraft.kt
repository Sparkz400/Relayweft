package io.github.sparkz400.relayweft.core

// A plan being edited before approval, with the same operations as the
// web page's plan editor (internal/web/static/app.js: planEditor): edit a
// subtask, delete one (dependencies on it are dropped), move one, add one.
// The server normalizes the plan again when it is approved.

val PLAN_KINDS = listOf("edit", "explore", "research")

/** Roles a subtask can be pinned to; "" lets the router decide. */
val PLAN_ROLES = listOf("", "planner", "worker", "worker_high", "explorer", "researcher")

class PlanDraft(plan: Plan) {
    val summary: String = plan.summary
    val repos: List<String> = plan.repos
    private val steps: MutableList<Subtask> = plan.subtasks.map {
        it.copy(kind = it.kind.ifEmpty { "edit" })
    }.toMutableList()

    val subtasks: List<Subtask> get() = steps.toList()
    val size: Int get() = steps.size

    operator fun get(i: Int): Subtask = steps[i]

    /** Replaces subtask i (its id stays: other subtasks depend on it by id). */
    fun update(i: Int, s: Subtask) {
        val old = steps[i]
        steps[i] = s.copy(id = old.id, dependsOn = s.dependsOn.filter { it != old.id && hasId(it) }.distinct())
    }

    fun delete(i: Int) {
        val gone = steps.removeAt(i).id
        for (k in steps.indices) {
            val st = steps[k]
            if (gone in st.dependsOn) steps[k] = st.copy(dependsOn = st.dependsOn.filter { it != gone })
        }
    }

    /** Moves subtask from to index to; false when to is out of range. */
    fun move(from: Int, to: Int): Boolean {
        if (from !in steps.indices || to !in steps.indices || from == to) return false
        val x = steps.removeAt(from)
        steps.add(to, x)
        return true
    }

    /** Adds an empty edit subtask at the end; returns its index. */
    fun add(): Int {
        var n = steps.size + 1
        while (hasId("step-$n")) n++
        steps.add(Subtask(id = "step-$n", title = "", kind = "edit", prompt = ""))
        return steps.size - 1
    }

    private fun hasId(id: String) = steps.any { it.id == id }

    /** What is wrong with the plan before it is sent, or null. */
    fun problem(): String? {
        if (steps.isEmpty()) return "No subtasks left: add one, or reject the plan to cancel the task."
        steps.forEachIndexed { i, s ->
            if (s.title.isBlank() && s.prompt.isBlank()) return "Subtask ${i + 1} (${s.id}) has no title and no prompt."
        }
        return null
    }

    fun toPlan(): Plan = Plan(summary, subtasks, repos)
}
