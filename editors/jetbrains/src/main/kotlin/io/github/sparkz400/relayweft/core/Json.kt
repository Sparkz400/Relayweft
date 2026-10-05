package io.github.sparkz400.relayweft.core

// A small JSON reader and writer, so the plugin needs no JSON library
// (the IDE's bundled ones change between versions). Values are the usual
// Kotlin types: null, Boolean, Long or Double, String, List<Any?> and
// Map<String, Any?> (insertion order kept).

class JsonException(message: String) : RuntimeException(message)

object Json {
    fun parse(text: String): Any? {
        val p = Reader(text)
        p.ws()
        val v = p.value(0)
        p.ws()
        if (p.i != text.length) p.fail("unexpected text after the value")
        return v
    }

    fun write(v: Any?): String = StringBuilder().also { writeTo(it, v) }.toString()

    private fun writeTo(sb: StringBuilder, v: Any?) {
        when (v) {
            null -> sb.append("null")
            is Boolean -> sb.append(v)
            is Int, is Long, is Short, is Byte -> sb.append(v)
            is Double -> if (v.isFinite()) sb.append(if (v == Math.floor(v) && Math.abs(v) < 1e15) v.toLong().toString() else v.toString()) else sb.append("null")
            is Float -> writeTo(sb, v.toDouble())
            is Number -> sb.append(v.toString())
            is String -> quote(sb, v)
            is Map<*, *> -> {
                sb.append('{')
                var first = true
                for ((k, x) in v) {
                    if (!first) sb.append(',')
                    first = false
                    quote(sb, k.toString())
                    sb.append(':')
                    writeTo(sb, x)
                }
                sb.append('}')
            }
            is Iterable<*> -> {
                sb.append('[')
                var first = true
                for (x in v) {
                    if (!first) sb.append(',')
                    first = false
                    writeTo(sb, x)
                }
                sb.append(']')
            }
            is Array<*> -> writeTo(sb, v.asList())
            else -> quote(sb, v.toString())
        }
    }

    private fun quote(sb: StringBuilder, s: String) {
        sb.append('"')
        for (c in s) {
            when (c) {
                '"' -> sb.append("\\\"")
                '\\' -> sb.append("\\\\")
                '\n' -> sb.append("\\n")
                '\r' -> sb.append("\\r")
                '\t' -> sb.append("\\t")
                '\b' -> sb.append("\\b")
                '\u000C' -> sb.append("\\f")
                ' ', ' ' -> sb.append(String.format("\\u%04x", c.code))
                else -> if (c < ' ') sb.append(String.format("\\u%04x", c.code)) else sb.append(c)
            }
        }
        sb.append('"')
    }

    private class Reader(val s: String) {
        var i = 0

        fun fail(msg: String): Nothing = throw JsonException("JSON: $msg at offset $i")

        fun ws() {
            while (i < s.length && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r')) i++
        }

        fun value(depth: Int): Any? {
            if (depth > 200) fail("nested too deeply")
            if (i >= s.length) fail("unexpected end")
            return when (s[i]) {
                '{' -> obj(depth)
                '[' -> arr(depth)
                '"' -> str()
                't' -> lit("true", true)
                'f' -> lit("false", false)
                'n' -> lit("null", null)
                else -> num()
            }
        }

        fun lit(word: String, v: Any?): Any? {
            if (!s.startsWith(word, i)) fail("unexpected character '${s[i]}'")
            i += word.length
            return v
        }

        fun obj(depth: Int): Map<String, Any?> {
            val m = LinkedHashMap<String, Any?>()
            i++
            ws()
            if (i < s.length && s[i] == '}') {
                i++
                return m
            }
            while (true) {
                ws()
                if (i >= s.length || s[i] != '"') fail("want a key")
                val k = str()
                ws()
                if (i >= s.length || s[i] != ':') fail("want ':'")
                i++
                ws()
                m[k] = value(depth + 1)
                ws()
                if (i >= s.length) fail("unexpected end")
                when (s[i++]) {
                    ',' -> continue
                    '}' -> return m
                    else -> { i--; fail("want ',' or '}'") }
                }
            }
        }

        fun arr(depth: Int): List<Any?> {
            val l = ArrayList<Any?>()
            i++
            ws()
            if (i < s.length && s[i] == ']') {
                i++
                return l
            }
            while (true) {
                ws()
                l.add(value(depth + 1))
                ws()
                if (i >= s.length) fail("unexpected end")
                when (s[i++]) {
                    ',' -> continue
                    ']' -> return l
                    else -> { i--; fail("want ',' or ']'") }
                }
            }
        }

        fun str(): String {
            i++ // opening quote
            val sb = StringBuilder()
            while (true) {
                if (i >= s.length) fail("unterminated string")
                val c = s[i++]
                when {
                    c == '"' -> return sb.toString()
                    c == '\\' -> {
                        if (i >= s.length) fail("unterminated string")
                        when (val e = s[i++]) {
                            '"' -> sb.append('"')
                            '\\' -> sb.append('\\')
                            '/' -> sb.append('/')
                            'b' -> sb.append('\b')
                            'f' -> sb.append('\u000C')
                            'n' -> sb.append('\n')
                            'r' -> sb.append('\r')
                            't' -> sb.append('\t')
                            'u' -> {
                                if (i + 4 > s.length) fail("bad \\u escape")
                                val hex = s.substring(i, i + 4)
                                val code = hex.toIntOrNull(16) ?: fail("bad \\u escape")
                                sb.append(code.toChar())
                                i += 4
                            }
                            else -> fail("bad escape \\$e")
                        }
                    }
                    c < ' ' -> fail("control character in a string")
                    else -> sb.append(c)
                }
            }
        }

        fun num(): Any {
            val start = i
            if (i < s.length && s[i] == '-') i++
            var frac = false
            while (i < s.length) {
                val c = s[i]
                if (c in '0'..'9') {
                    i++
                } else if (c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-') {
                    frac = true
                    i++
                } else {
                    break
                }
            }
            val t = s.substring(start, i)
            if (t.isEmpty() || t == "-") fail("unexpected character")
            if (!frac) t.toLongOrNull()?.let { return it }
            return t.toDoubleOrNull() ?: fail("bad number '$t'")
        }
    }
}

// Typed reads from a decoded JSON object. Missing or mistyped fields read
// as the type's empty value, like the TypeScript client treats them.

@Suppress("UNCHECKED_CAST")
fun Any?.asObj(): Map<String, Any?> = this as? Map<String, Any?> ?: emptyMap()

fun Any?.asList(): List<Any?> = this as? List<Any?> ?: emptyList()

fun Map<String, Any?>.str(k: String): String = when (val v = this[k]) {
    is String -> v
    null -> ""
    else -> v.toString()
}

fun Map<String, Any?>.strOrNull(k: String): String? = this[k] as? String

fun Map<String, Any?>.bool(k: String): Boolean = this[k] as? Boolean ?: false

fun Map<String, Any?>.long(k: String): Long = when (val v = this[k]) {
    is Long -> v
    is Double -> v.toLong()
    is Int -> v.toLong()
    else -> 0L
}

fun Map<String, Any?>.int(k: String): Int = long(k).toInt()

fun Map<String, Any?>.double(k: String): Double = when (val v = this[k]) {
    is Number -> v.toDouble()
    else -> 0.0
}

fun Map<String, Any?>.obj(k: String): Map<String, Any?>? = this[k].let { if (it is Map<*, *>) it.asObj() else null }

fun Map<String, Any?>.strings(k: String): List<String> = this[k].asList().mapNotNull { it as? String }
