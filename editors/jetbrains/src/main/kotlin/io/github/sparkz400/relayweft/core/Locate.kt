package io.github.sparkz400.relayweft.core

// Finding the rw executable and building the `rw web --client` command.
// Pure (no IDE classes) so it can be unit tested for both platforms.

class LocateEnv(
    val windows: Boolean,
    val env: Map<String, String>,
    val home: String,
    val isFile: (String) -> Boolean,
)

class Located(
    val path: String?,
    /** Where it looked, for the error message. */
    val tried: List<String>,
    /** Why the configured path was refused, if it was. */
    val problem: String? = null,
)

/** Whether p is an absolute path on the platform (a relative one would be resolved against some working folder). */
fun isAbsolutePath(p: String, windows: Boolean): Boolean =
    if (windows) Regex("""^([A-Za-z]:[\\/]|\\\\)""").containsMatchIn(p) else p.startsWith("/")

private fun join(windows: Boolean, dir: String, name: String): String {
    val sep = if (windows) '\\' else '/'
    val d = if (windows) dir.replace('/', '\\') else dir
    return if (d.endsWith(sep)) d + name else d + sep + name
}

private fun envVar(env: Map<String, String>, name: String): String? =
    env[name] ?: env.entries.firstOrNull { it.key.equals(name, ignoreCase = true) }?.value

/**
 * Resolves the rw executable: the configured path (a bare name is looked
 * up on PATH), else "rw" on PATH, else Go's, Scoop's and WinGet's install
 * folders. On Windows only .exe files count: rw is started without a
 * shell, and a .cmd/.bat shim cannot be started that way.
 */
fun locateRw(configured: String, le: LocateEnv): Located {
    val win = le.windows
    val tried = ArrayList<String>()
    fun names(base: String) = if (win && !base.lowercase().endsWith(".exe")) listOf("$base.exe") else listOf(base)
    fun check(c: String): Boolean {
        tried.add(c)
        return le.isFile(c)
    }
    fun expand(s: String) =
        if (s == "~" || s.startsWith("~/") || s.startsWith("~\\")) join(win, le.home, s.substring(1).trimStart('/', '\\')).trimEnd('/', '\\') else s

    val want = expand(configured.trim())
    if (want.isNotEmpty() && (want.contains('/') || want.contains('\\'))) {
        // A relative path would be checked against the IDE's folder but run
        // from the project's: a repository could then provide its own "rw".
        if (!isAbsolutePath(want, win)) return Located(null, tried, "the path must be absolute (it is relative: $want)")
        for (n in names(want)) if (check(n)) return Located(n, tried)
        return Located(null, tried)
    }
    val base = want.ifEmpty { "rw" }
    val pathVar = envVar(le.env, "PATH") ?: ""
    for (dir in pathVar.split(if (win) ';' else ':')) {
        val d = dir.trim().removeSurrounding("\"")
        // Relative PATH entries ("." or "bin") would resolve against the
        // project folder rw is started in: skip them.
        if (d.isEmpty() || !isAbsolutePath(d, win)) continue
        for (n in names(base)) {
            val p = join(win, d, n)
            if (check(p)) return Located(p, tried)
        }
    }
    if (want.isEmpty()) {
        val extra = ArrayList<String>()
        envVar(le.env, "GOBIN")?.takeIf { it.isNotBlank() }?.let { extra.add(it) }
        extra.add(join(win, envVar(le.env, "GOPATH")?.takeIf { it.isNotBlank() } ?: join(win, le.home, "go"), "bin"))
        if (win) {
            extra.add(join(win, envVar(le.env, "SCOOP")?.takeIf { it.isNotBlank() } ?: join(win, le.home, "scoop"), "shims"))
            envVar(le.env, "LOCALAPPDATA")?.takeIf { it.isNotBlank() }?.let { extra.add(join(win, join(win, join(win, it, "Microsoft"), "WinGet"), "Links")) }
        } else {
            extra.add("/usr/local/bin")
            extra.add("/opt/homebrew/bin")
            extra.add(join(false, join(false, le.home, ".local"), "bin"))
        }
        for (d in extra.filter { isAbsolutePath(it, win) }) {
            for (n in names("rw")) {
                val p = join(win, d, n)
                if (check(p)) return Located(p, tried)
            }
        }
    }
    return Located(null, tried)
}

/** The arguments for `rw web --client` in dir, then the user's extra ones (one per line, nothing is shell-quoted). */
fun clientArgs(dir: String, extra: List<String>): List<String> =
    listOf("web", "--client", "--dir", dir) + extra.map { it.trim() }.filter { it.isNotEmpty() }

/** Splits the extra-arguments setting: one argument per line. */
fun splitArgLines(text: String): List<String> = text.lines().map { it.trim() }.filter { it.isNotEmpty() }

/**
 * Prepares one argument for Java's ProcessBuilder on Windows so that the
 * program's C runtime (and Go's os.Args) reads it back unchanged.
 * ProcessBuilder quotes an argument with spaces itself, but passes inner
 * quotes on as they are, which splits or mangles the argument. Such an
 * argument is quoted here (backslashes doubled only before a quote or the
 * closing quote); ProcessBuilder leaves an argument that starts and ends
 * with a quote as it is. The one form it cannot pass (quotes, whitespace
 * and a trailing backslash together) is refused with an error.
 */
fun windowsArg(arg: String): String {
    if (!arg.contains('"')) return arg
    val q = windowsQuote(arg)
    if (q.endsWith("\\\"") && arg.any { it == ' ' || it == '\t' }) {
        throw IllegalArgumentException("cannot pass this argument on Windows (it has quotes, spaces and a trailing backslash): $arg")
    }
    return q
}

/** Quotes one argument for a Windows command line (the C runtime's rules). */
fun windowsQuote(arg: String): String {
    if (arg.isNotEmpty() && arg.none { it == ' ' || it == '\t' || it == '\n' || it == '\u000B' || it == '"' }) return arg
    val sb = StringBuilder("\"")
    var backslashes = 0
    for (c in arg) {
        when (c) {
            '\\' -> backslashes++
            '"' -> {
                repeat(backslashes * 2 + 1) { sb.append('\\') }
                backslashes = 0
                sb.append('"')
            }
            else -> {
                repeat(backslashes) { sb.append('\\') }
                backslashes = 0
                sb.append(c)
            }
        }
    }
    repeat(backslashes * 2) { sb.append('\\') }
    sb.append('"')
    return sb.toString()
}
