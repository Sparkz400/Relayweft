package io.github.sparkz400.switchyard.core

// A minimal server-sent events parser (the subset sy web sends: event,
// data, retry and comments), fed with decoded text chunks of any size.

data class SseMessage(val event: String, val data: String)

class SseParser(private val onMessage: (SseMessage) -> Unit) {
    private val buf = StringBuilder()
    private var event = ""
    private val data = ArrayList<String>()

    /** The last retry interval the server asked for, in milliseconds. */
    var retry: Long? = null
        private set

    fun push(chunk: CharSequence) {
        buf.append(chunk)
        while (true) {
            var i = -1
            for (k in buf.indices) {
                val c = buf[k]
                if (c == '\n' || c == '\r') {
                    i = k
                    break
                }
            }
            if (i < 0) break
            // A lone \r at the very end may be the first half of \r\n.
            if (buf[i] == '\r' && i == buf.length - 1) break
            val line = buf.substring(0, i)
            val eol = if (buf[i] == '\r' && i + 1 < buf.length && buf[i + 1] == '\n') 2 else 1
            buf.delete(0, i + eol)
            line(line)
        }
    }

    /** Drops a partly received message (the connection ended). */
    fun reset() {
        buf.setLength(0)
        event = ""
        data.clear()
    }

    private fun line(line: String) {
        if (line.isEmpty()) {
            if (data.isNotEmpty()) onMessage(SseMessage(event.ifEmpty { "message" }, data.joinToString("\n")))
            event = ""
            data.clear()
            return
        }
        if (line.startsWith(":")) return // comment (keep-alive ping)
        val c = line.indexOf(':')
        val field = if (c < 0) line else line.substring(0, c)
        var value = if (c < 0) "" else line.substring(c + 1)
        if (value.startsWith(" ")) value = value.substring(1)
        when (field) {
            "event" -> event = value
            "data" -> data.add(value)
            "retry" -> value.toLongOrNull()?.let { if (it >= 0) retry = it }
        }
    }
}
