package io.github.sparkz400.switchyard.core

import java.nio.ByteBuffer
import java.nio.charset.CodingErrorAction
import java.nio.charset.StandardCharsets
import java.nio.file.Files
import java.nio.file.InvalidPathException
import java.nio.file.Path
import java.nio.file.Paths

/** Files larger than this are not read for a diff (the diff then shows the hunks only). */
const val MAX_DIFF_FILE_BYTES = 16L shl 20

/**
 * Resolves rel (a path from a change set, '/'-separated) inside dir. It
 * returns null for a path that leaves dir, also through a symbolic link:
 * the plugin never reads a file outside the project for a diff.
 */
fun resolveInside(dir: String, rel: String): Path? {
    return try {
        val base = Paths.get(dir).toRealPath()
        if (rel.isEmpty() || rel.startsWith("/") || rel.startsWith("\\") || Paths.get(rel).isAbsolute) return null
        val p = base.resolve(rel).normalize()
        if (!p.startsWith(base) || p == base) return null
        if (Files.exists(p)) {
            val real = p.toRealPath()
            if (!real.startsWith(base)) return null
            real
        } else {
            p
        }
    } catch (_: InvalidPathException) {
        null
    } catch (_: java.io.IOException) {
        null
    }
}

/** The file's text (UTF-8, bad bytes replaced) when it is inside dir, a regular file and not too large; else null. */
fun readProjectFile(dir: String, rel: String): String? {
    val p = resolveInside(dir, rel) ?: return null
    return try {
        if (!Files.isRegularFile(p) || Files.size(p) > MAX_DIFF_FILE_BYTES) return null
        val bytes = Files.readAllBytes(p)
        StandardCharsets.UTF_8.newDecoder()
            .onMalformedInput(CodingErrorAction.REPLACE)
            .onUnmappableCharacter(CodingErrorAction.REPLACE)
            .decode(ByteBuffer.wrap(bytes))
            .toString()
    } catch (_: java.io.IOException) {
        null
    }
}
