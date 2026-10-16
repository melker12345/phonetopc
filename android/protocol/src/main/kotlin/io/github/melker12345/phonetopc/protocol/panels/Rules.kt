package io.github.melker12345.phonetopc.protocol.panels

import io.github.melker12345.phonetopc.protocol.invalidParams
import io.github.melker12345.phonetopc.protocol.isHidden
import io.github.melker12345.phonetopc.protocol.string
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive

/** The value rules of PANELS.md §8; generic typed params access is in protocol/JsonParams.kt. */
object Rules {
    private val ID = Regex("^[A-Za-z0-9_.-]{1,64}$")
    private val COLOR = Regex("^#[0-9A-Fa-f]{6}$")

    fun isId(s: String) = ID.matches(s)

    /**
     * Display text: [min]–[max] code points, no control characters (except a newline when
     * [newlines]), nothing [isHidden].
     */
    fun isDisplayText(s: String, min: Int, max: Int, newlines: Boolean): Boolean {
        val count = s.codePointCount(0, s.length)
        if (count < min || count > max) return false
        var i = 0
        while (i < s.length) {
            val cp = s.codePointAt(i)
            if (Character.getType(cp) == Character.CONTROL.toInt() && !(newlines && cp == '\n'.code)) return false
            if (isHidden(cp)) return false
            i += Character.charCount(cp)
        }
        return true
    }

    /** `#rrggbb` as an opaque ARGB int, or `null` if [s] isn't a color. */
    fun parseColor(s: String): Int? = if (COLOR.matches(s)) (0xFF000000.toInt() or s.substring(1).toInt(16)) else null

    /** A number for the wire: whole numbers without a fraction (`40`, not `40.0`). */
    fun numberValue(d: Double): JsonPrimitive =
        if (d == Math.rint(d) && Math.abs(d) < 9.007199254740991E15) JsonPrimitive(d.toLong()) else JsonPrimitive(d)

    fun invalid(message: String): Nothing = invalidParams(message)

    fun JsonObject.id(key: String): String? = string(key)?.also { if (!isId(it)) invalid("$key must match ^[A-Za-z0-9_.-]{1,64}$") }

    fun JsonObject.requiredId(key: String): String = id(key) ?: invalid("Missing required param: $key")

    fun JsonObject.text(key: String, min: Int, max: Int, newlines: Boolean): String? = string(key)?.also {
        if (!isDisplayText(it, min, max, newlines)) {
            invalid("$key must be $min-$max code points" + (if (newlines) "" else " without newlines") + ", without control or bidi characters")
        }
    }

    fun <T : Enum<T>> JsonObject.choice(key: String, values: Map<String, T>): T? =
        string(key)?.let { values[it] ?: invalid("$key must be one of ${values.keys.joinToString()}") }
}
