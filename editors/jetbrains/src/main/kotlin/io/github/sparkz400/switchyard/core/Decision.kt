package io.github.sparkz400.switchyard.core

// What the person picked in a change review, and the request body the
// server expects (POST /api/approvals/{id}/changes), with the same rules
// as the web page and the VS Code extension: a file is applied when it is
// included and, if it can be split, at least one of its hunks is; a split
// file lists its hunks only when some are left out.

class Selection(files: List<FileView>) {
    /** Per file (same order as the change set): included at all. */
    val include: BooleanArray = BooleanArray(files.size) { true }

    /** Per file: the selected hunk indexes (splittable files only). */
    val hunks: List<MutableSet<Int>> = files.map { f -> f.hunks.indices.toMutableSet() }
}

/** The number of hunks that can be picked one by one (0 for a file that cannot be split). */
fun hunkCount(f: FileView): Int = if (f.splittable) f.hunks.size else 0

/** Whether file i is applied (wholly or partly). */
fun fileOn(files: List<FileView>, sel: Selection, i: Int): Boolean = sel.include[i] && (!files[i].splittable || sel.hunks[i].isNotEmpty())

/** Whether hunk j of file i is applied. Hunks of a file that cannot be split follow the file. */
fun hunkOn(files: List<FileView>, sel: Selection, i: Int, j: Int): Boolean {
    if (!files[i].splittable) return sel.include[i]
    return sel.include[i] && j in sel.hunks[i]
}

fun setFile(files: List<FileView>, sel: Selection, i: Int, on: Boolean) {
    sel.include[i] = on
    if (files[i].splittable) {
        sel.hunks[i].clear()
        if (on) sel.hunks[i].addAll(files[i].hunks.indices)
    }
}

/** Sets one hunk. It returns false when the file cannot be split, in which case the whole file is set instead. */
fun setHunk(files: List<FileView>, sel: Selection, i: Int, j: Int, on: Boolean): Boolean {
    if (!files[i].splittable) {
        sel.include[i] = on
        return false
    }
    if (on) sel.hunks[i].add(j) else sel.hunks[i].remove(j)
    sel.include[i] = sel.hunks[i].isNotEmpty()
    return true
}

data class ChangesBody(val apply: List<String>, val hunks: Map<String, List<Int>>) {
    fun toJson(): Map<String, Any?> = mapOf("apply" to apply, "hunks" to hunks)
}

fun decisionBody(files: List<FileView>, sel: Selection): ChangesBody {
    val apply = ArrayList<String>()
    val hunks = LinkedHashMap<String, List<Int>>()
    files.forEachIndexed { i, f ->
        if (!fileOn(files, sel, i)) return@forEachIndexed
        apply.add(f.path)
        if (f.splittable && sel.hunks[i].size < f.hunks.size) hunks[f.path] = sel.hunks[i].sorted()
    }
    return ChangesBody(apply, hunks)
}

/** A one-line summary, e.g. "2 of 3 file(s), 1 partly". */
fun selectionSummary(files: List<FileView>, sel: Selection): String {
    val b = decisionBody(files, sel)
    val partly = b.hunks.size
    return "${b.apply.size} of ${files.size} file(s)" + if (partly > 0) ", $partly partly" else ""
}
