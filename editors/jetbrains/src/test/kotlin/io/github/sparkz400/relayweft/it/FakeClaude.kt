package io.github.sparkz400.relayweft.it

import io.github.sparkz400.relayweft.core.Json
import java.io.File

// What the scripted agent and the integration tests agree on (the same as
// editors/vscode/src/test/integration/fakeClaude.consts.ts).
object Fake {
    /** Tasks with this word make the worker hang until it is killed. */
    const val HANG = "RWTEST-HANG"

    /** The file the worker edits (two hunks far apart) and the lines it changes. */
    const val NOTES = "notes.txt"
    const val NOTES_LINES = 40
    val EDITED_LINES = listOf(3, 35)
    fun edited(n: Int) = "line $n changed by the agent"

    /** The text of the plan's edit step; the tests edit the plan to change it. */
    const val PLAN_TEXT = "hello from the plan"
    const val EDITED_PLAN_TEXT = "hello from the edited plan"
}

/**
 * A scripted stand-in for the Claude Code CLI (`claude -p --output-format
 * stream-json`), so the integration tests drive a real, non-demo rw (git,
 * worktrees, change review) without spending any quota. A port of
 * editors/vscode/src/test/integration/fakeClaude.ts. rw starts it through a
 * shim (claude.cmd or claude) as
 *
 *   java -cp ... FakeClaude <state dir> <claude args...>
 *
 * with the prompt on stdin. It writes one line per call to <state dir>/calls.log.
 */
object FakeClaude {
    @JvmStatic
    fun main(argv: Array<String>) {
        val stateDir = File(argv[0])
        val args = argv.drop(1)
        val prompt = System.`in`.readBytes().toString(Charsets.UTF_8)
        val cwd = File(System.getProperty("user.dir"))
        val sid = "fake-${ProcessHandle.current().pid()}"
        val resumeAt = args.indexOf("--resume")
        val resumed = if (resumeAt >= 0 && resumeAt + 1 < args.size) args[resumeAt + 1] else ""
        // Read-only agents (explorer, planner, reviewers) never write.
        val readOnly = args.contains("dontAsk")

        fun out(v: Any?) {
            println(Json.write(v))
            System.out.flush()
        }

        fun note(what: String) {
            File(stateDir, "calls.log").appendText(
                Json.write(mapOf("what" to what, "pid" to ProcessHandle.current().pid(), "sid" to sid, "resumed" to resumed, "cwd" to cwd.path, "prompt" to prompt)) + "\n",
            )
        }

        fun result(text: String) = out(
            mapOf(
                "type" to "result", "subtype" to "success", "is_error" to false, "result" to text, "session_id" to sid,
                "usage" to mapOf("input_tokens" to 120, "output_tokens" to 12),
            ),
        )

        fun write(rel: String, content: String) {
            val p = File(cwd, rel)
            p.parentFile.mkdirs()
            p.writeText(content)
            // Real CLIs report absolute paths.
            out(mapOf("type" to "assistant", "message" to mapOf("content" to listOf(mapOf("type" to "tool_use", "name" to "Write", "input" to mapOf("file_path" to p.path))))))
        }

        out(mapOf("type" to "system", "subtype" to "init", "session_id" to sid, "model" to "fake"))
        out(mapOf("type" to "assistant", "message" to mapOf("content" to listOf(mapOf("type" to "text", "text" to "Reading the task")))))

        fun has(m: String) = prompt.contains(m)
        when {
            resumed.isNotEmpty() -> {
                note("followup")
                result("followed up")
            }
            has("[RW:PLAN-REVIEW]") || has("[RW:FINAL-REVIEW]") || has("[RW:ERROR-REVIEW]") -> {
                note("review")
                result("{\"approve\": true, \"advice\": \"ok\", \"issues\": []}")
            }
            has("[RW:JUDGE]") -> {
                note("judge")
                result("A")
            }
            has("[RW:PLAN]") -> {
                note("plan")
                val plan = mapOf(
                    "summary" to "Look around, then edit the notes and add a file.",
                    "subtasks" to listOf(
                        mapOf("id" to "look", "title" to "look around", "kind" to "explore", "prompt" to "List the top-level files.", "files" to emptyList<String>()),
                        mapOf(
                            "id" to "edit", "title" to "edit the notes", "kind" to "edit",
                            "prompt" to "Change ${Fake.NOTES} and write <<added.txt>> containing <<${Fake.PLAN_TEXT}>>",
                            "files" to listOf(Fake.NOTES, "added.txt"), "depends_on" to listOf("look"),
                        ),
                    ),
                )
                result("```json\n" + Json.write(plan) + "\n```")
            }
            has(Fake.HANG) -> {
                note("hang")
                File(stateDir, "hang.pid").writeText(ProcessHandle.current().pid().toString())
                // Wait to be killed (rw cancels the task or stops).
                Thread.sleep(30 * 60 * 1000L)
            }
            else -> {
                val m = Regex("write <<([^>]+)>> containing <<([^>]+)>>").find(prompt)
                if (m != null && !readOnly) {
                    note("edit " + m.groupValues[2])
                    val notes = File(cwd, Fake.NOTES)
                    if (notes.exists()) {
                        // Keep the file's line ends, as an editing agent does (CRLF
                        // in a core.autocrlf=true checkout on Windows).
                        val text = notes.readText()
                        val eol = if (text.contains("\r\n")) "\r\n" else "\n"
                        val lines = text.split(eol).toMutableList()
                        for (n in Fake.EDITED_LINES) lines[n - 1] = Fake.edited(n)
                        write(Fake.NOTES, lines.joinToString(eol))
                    }
                    write(m.groupValues[1], m.groupValues[2] + "\n")
                    result("Edited the notes and wrote " + m.groupValues[1])
                } else {
                    note("explore")
                    result("The repository has notes.txt and README.md.")
                }
            }
        }
    }
}
