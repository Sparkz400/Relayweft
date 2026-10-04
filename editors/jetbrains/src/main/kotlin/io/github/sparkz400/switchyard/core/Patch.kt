package io.github.sparkz400.switchyard.core

// Unified diffs as sy web sends them (one file's `git diff` output, the
// hunks numbered like the server's SplitHunks), and the before/after texts
// the IDE's diff viewer shows. A port of editors/vscode/src/patch.ts with
// the same tests, plus the conversion to the IDE's LF-only documents.

data class HunkLine(val op: Char, val text: String)

class Hunk(
    val header: String,
    val oldStart: Int,
    var oldLines: Int,
    val newStart: Int,
    var newLines: Int,
    val lines: MutableList<HunkLine> = ArrayList(),
) {
    /** "\ No newline at end of file" after the last old / new line. */
    var oldNoEol = false
    var newNoEol = false

    fun withLines(lines: List<HunkLine>): Hunk =
        Hunk(header, oldStart, oldLines, newStart, newLines, lines.toMutableList()).also {
            it.oldNoEol = oldNoEol
            it.newNoEol = newNoEol
        }
}

class ParsedPatch(val hunks: List<Hunk>, /** sy cut the patch short (huge file): the hunks are incomplete. */ val truncated: Boolean)

const val TRUNCATED_MARK = "... (diff truncated) ..."
private val HUNK_RE = Regex("""^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@""")

class Lines(val lines: List<String>, val eol: Boolean)

/** Splits text into lines (without "\n"; a "\r" stays) and whether it ends with a newline. */
fun splitLines(text: String): Lines {
    if (text.isEmpty()) return Lines(emptyList(), false)
    val lines = text.split("\n").toMutableList()
    val eol = lines.last() == ""
    if (eol) lines.removeAt(lines.size - 1)
    return Lines(lines, eol)
}

fun joinLines(lines: List<String>, eol: Boolean): String {
    if (lines.isEmpty()) return ""
    return lines.joinToString("\n") + if (eol) "\n" else ""
}

/** Parses one file's unified diff. Lines before the first @@ are the header. */
fun parsePatch(patch: String): ParsedPatch {
    val hunks = ArrayList<Hunk>()
    var truncated = false
    var cur: Hunk? = null
    var last: HunkLine? = null
    val lines = patch.split("\n").toMutableList()
    if (lines.isNotEmpty() && lines.last() == "") lines.removeAt(lines.size - 1)
    for (l in lines) {
        if (l == TRUNCATED_MARK) {
            truncated = true
            break
        }
        if (l.startsWith("@@ ")) {
            // Every "@@ " line starts a hunk, like the server numbers them.
            val m = HUNK_RE.find(l)
            val h = Hunk(
                header = l,
                oldStart = m?.groupValues?.get(1)?.toIntOrNull() ?: 0,
                oldLines = if (m == null) 0 else m.groupValues[2].ifEmpty { "1" }.toIntOrNull() ?: 0,
                newStart = m?.groupValues?.get(3)?.toIntOrNull() ?: 0,
                newLines = if (m == null) 0 else m.groupValues[4].ifEmpty { "1" }.toIntOrNull() ?: 0,
            )
            hunks.add(h)
            cur = h
            last = null
            continue
        }
        val c = cur ?: continue // header
        if (l.startsWith("\\")) {
            last?.let {
                if (it.op != '+') c.oldNoEol = true
                if (it.op != '-') c.newNoEol = true
            }
            continue
        }
        if (l.isEmpty()) {
            // Some tools strip the space of an empty context line.
            last = HunkLine(' ', "")
            c.lines.add(last)
            continue
        }
        val op = l[0]
        if (op == ' ' || op == '-' || op == '+') {
            last = HunkLine(op, l.substring(1))
            c.lines.add(last)
        }
    }
    // The line counts in the headers are not trusted (where a hunk goes is
    // found by its content): keep them consistent with the lines read.
    for (h in hunks) {
        h.oldLines = h.lines.count { it.op != '+' }
        h.newLines = h.lines.count { it.op != '-' }
    }
    return ParsedPatch(hunks, truncated)
}

/** A 0-based, end-exclusive line range. */
data class LineRange(val start: Int, val end: Int)

class Reconstructed(
    val before: String,
    val after: String,
    /** Where each hunk is in the "after" text (by hunk index). */
    val ranges: List<LineRange>,
    /** Where each hunk is in the "before" text. */
    val beforeRanges: List<LineRange>,
    /**
     * true: before is the whole file and after the whole result. false: the
     * texts hold only the hunks, with markers for the lines not shown (the
     * file on disk is not the agent's base, or the patch is truncated).
     */
    val whole: Boolean,
)

private fun oldSide(h: Hunk): List<String> = h.lines.filter { it.op != '+' }.map { it.text }

private fun matchesAt(lines: List<String>, at: Int, want: List<String>): Boolean {
    if (at < 0 || at + want.size > lines.size) return false
    for (i in want.indices) if (lines[at + i] != want[i]) return false
    return true
}

class Applied(val lines: List<String>, val eol: Boolean, val ranges: List<LineRange>, val beforeRanges: List<LineRange>)

/**
 * Applies the hunks to before. It returns null when a hunk's old lines
 * are not found (the file changed since the agent's base). A hunk may have
 * moved (lines added or removed above it): it is looked for near its
 * position first, then anywhere after the previous hunk.
 */
fun applyHunks(before: Lines, hunks: List<Hunk>): Applied? {
    val out = ArrayList<String>()
    val ranges = ArrayList<LineRange>()
    val beforeRanges = ArrayList<LineRange>()
    var pos = 0 // next unread line of before
    var shift = 0 // where hunks were found relative to their header
    var eol = before.eol
    val bl = before.lines
    for (h in hunks) {
        val want = oldSide(h)
        val nominal = (if (h.oldLines == 0) h.oldStart else h.oldStart - 1) + shift
        var at = -1
        if (nominal >= pos && matchesAt(bl, nominal, want)) {
            at = nominal
        } else {
            var d = 1
            while (d <= bl.size && at < 0) {
                for (c in intArrayOf(nominal - d, nominal + d)) {
                    if (c >= pos && matchesAt(bl, c, want)) {
                        at = c
                        break
                    }
                }
                if (nominal - d < pos && nominal + d > bl.size) break
                d++
            }
        }
        if (at < 0) return null
        shift = at - (if (h.oldLines == 0) h.oldStart else h.oldStart - 1)
        out.addAll(bl.subList(pos, at))
        val start = out.size
        for (l in h.lines) if (l.op != '-') out.add(l.text)
        ranges.add(LineRange(start, out.size))
        beforeRanges.add(LineRange(at, at + want.size))
        pos = at + want.size
        if (pos >= bl.size) {
            // The hunk reaches the end of the file: it decides the final newline.
            if (h.oldNoEol == before.eol && h.oldLines > 0) return null // the base's final newline differs from the disk's
            eol = !h.newNoEol
        }
    }
    out.addAll(bl.subList(minOf(pos, bl.size), bl.size))
    if (out.isEmpty()) eol = false
    return Applied(out, eol, ranges, beforeRanges)
}

/** Builds before/after texts from the hunks alone. */
fun fragments(hunks: List<Hunk>, truncated: Boolean): Reconstructed {
    val b = ArrayList<String>()
    val a = ArrayList<String>()
    val ranges = ArrayList<LineRange>()
    val beforeRanges = ArrayList<LineRange>()
    var next = 1 // next old line number not shown yet
    fun gap(from: Int, to: Int) {
        val m = if (to > from) "⋯ lines $from–$to not shown ⋯" else "⋯ line $from not shown ⋯"
        b.add(m)
        a.add(m)
    }
    for (h in hunks) {
        val first = if (h.oldLines == 0) h.oldStart + 1 else h.oldStart
        if (first > next) gap(next, first - 1)
        val bStart = b.size
        for (l in h.lines) if (l.op != '+') b.add(l.text)
        beforeRanges.add(LineRange(bStart, b.size))
        val start = a.size
        for (l in h.lines) if (l.op != '-') a.add(l.text)
        ranges.add(LineRange(start, a.size))
        next = first + h.oldLines
    }
    val tail = if (truncated) "⋯ the rest of the diff was too large to show; see the patch in sy web ⋯" else "⋯ rest of the file not shown ⋯"
    if (hunks.isEmpty() || truncated || next > 1) {
        b.add(tail)
        a.add(tail)
    }
    return Reconstructed(joinLines(b, true), joinLines(a, true), ranges, beforeRanges, false)
}

/**
 * Builds the before/after texts for one file of a change set. status is A
 * (added), M (modified) or D (deleted); disk is the file's current content
 * in the project (null when there is none).
 */
fun reconstruct(patch: String, status: String, disk: String?): Reconstructed {
    val p = parsePatch(patch)
    if (!p.truncated) {
        val base: Lines? = when {
            status == "A" -> Lines(emptyList(), false)
            disk != null -> splitLines(disk)
            else -> null
        }
        if (base != null) {
            applyHunks(base, p.hunks)?.let { r ->
                return Reconstructed(joinLines(base.lines, base.eol), joinLines(r.lines, r.eol), r.ranges, r.beforeRanges, true)
            }
            // core.autocrlf=true (Git for Windows' default): the file on disk
            // has CRLF, but git diffs the normalized (LF) content, so no hunk
            // matches as it is. Compare and show both sides with LF then.
            if (disk != null && disk.contains("\r\n")) {
                val lf = splitLines(disk.replace("\r\n", "\n"))
                val hunks = p.hunks.map { h -> h.withLines(h.lines.map { HunkLine(it.op, it.text.removeSuffix("\r")) }) }
                applyHunks(lf, hunks)?.let { r2 ->
                    return Reconstructed(joinLines(lf.lines, lf.eol), joinLines(r2.lines, r2.eol), r2.ranges, r2.beforeRanges, true)
                }
            }
        }
        if (status == "D" && p.hunks.size == 1 && p.hunks[0].oldStart <= 1) {
            // A deletion's patch holds the whole old file.
            val h = p.hunks[0]
            val old = oldSide(h)
            return Reconstructed(joinLines(old, !h.oldNoEol), "", listOf(LineRange(0, 0)), listOf(LineRange(0, old.size)), true)
        }
    }
    return fragments(p.hunks, p.truncated)
}

/** The index of the hunk whose range holds line (0-based), or the nearest one; -1 when there is none. */
fun hunkAt(ranges: List<LineRange>, line: Int): Int {
    var best = -1
    var dist = Int.MAX_VALUE
    ranges.forEachIndexed { i, r ->
        val d = when {
            line < r.start -> r.start - line
            line >= maxOf(r.end, r.start + 1) -> line - maxOf(r.end - 1, r.start)
            else -> 0
        }
        if (d < dist) {
            best = i
            dist = d
        }
    }
    return best
}

/**
 * The text for an IDE document, which holds "\n" line ends only. CRLF
 * becomes LF and a lone CR (rare, but possible in a file) becomes a visible
 * ␍ instead of a line break, so the hunks' line numbers stay right.
 */
fun documentText(text: String): String = text.replace("\r\n", "\n").replace('\r', '␍')
