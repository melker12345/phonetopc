package io.github.melker12345.phonetopc.protocol.panels

import io.github.melker12345.phonetopc.protocol.RpcError
import io.github.melker12345.phonetopc.protocol.RpcException
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.doubleOrNull

/** The value rules of PANELS.md §8, and typed access to params that throws -32602. */
object Rules {
    private val ID = Regex("^[A-Za-z0-9_.-]{1,64}$")
    private val COLOR = Regex("^#[0-9A-Fa-f]{6}$")
    private val JSON_INTEGER = Regex("-?(0|[1-9][0-9]*)")

    /** U+061C, U+200E, U+200F, U+202A–U+202E, U+2066–U+2069. */
    private fun isBidiControl(cp: Int) =
        cp == 0x061C || cp == 0x200E || cp == 0x200F || cp in 0x202A..0x202E || cp in 0x2066..0x2069

    fun isId(s: String) = ID.matches(s)

    /**
     * Display text: [min]–[max] code points, no control characters (except a newline when
     * [newlines]), no bidirectional controls.
     */
    fun isDisplayText(s: String, min: Int, max: Int, newlines: Boolean): Boolean {
        val count = s.codePointCount(0, s.length)
        if (count < min || count > max) return false
        var i = 0
        while (i < s.length) {
            val cp = s.codePointAt(i)
            if (Character.getType(cp) == Character.CONTROL.toInt() && !(newlines && cp == '\n'.code)) return false
            if (isBidiControl(cp)) return false
            i += Character.charCount(cp)
        }
        return true
    }

    /** `#rrggbb` as an opaque ARGB int, or `null` if [s] isn't a color. */
    fun parseColor(s: String): Int? = if (COLOR.matches(s)) (0xFF000000.toInt() or s.substring(1).toInt(16)) else null

    /** A number for the wire: whole numbers without a fraction (`40`, not `40.0`). */
    fun numberValue(d: Double): JsonPrimitive =
        if (d == Math.rint(d) && Math.abs(d) < 9.007199254740991E15) JsonPrimitive(d.toLong()) else JsonPrimitive(d)

    fun invalid(message: String): Nothing = throw RpcException(RpcError.INVALID_PARAMS, message)

    /** A member that is present and not `null` (explicit nulls count as absent). */
    fun JsonObject.present(key: String): JsonElement? = this[key]?.takeUnless { it is JsonNull }

    fun JsonObject.string(key: String): String? {
        val v = present(key) ?: return null
        if (v !is JsonPrimitive || !v.isString) invalid("$key must be a string")
        return v.content
    }

    fun JsonObject.requiredString(key: String): String = string(key) ?: invalid("Missing required param: $key")

    fun JsonObject.id(key: String): String? = string(key)?.also { if (!isId(it)) invalid("$key must match ^[A-Za-z0-9_.-]{1,64}$") }

    fun JsonObject.requiredId(key: String): String = id(key) ?: invalid("Missing required param: $key")

    fun JsonObject.text(key: String, min: Int, max: Int, newlines: Boolean): String? = string(key)?.also {
        if (!isDisplayText(it, min, max, newlines)) {
            invalid("$key must be $min-$max code points" + (if (newlines) "" else " without newlines") + ", without control or bidi characters")
        }
    }

    fun JsonObject.bool(key: String): Boolean? {
        val v = present(key) ?: return null
        if (v !is JsonPrimitive || v.isString || (v.content != "true" && v.content != "false")) invalid("$key must be a boolean")
        return v.content == "true"
    }

    fun JsonObject.number(key: String): Double? {
        val v = present(key) ?: return null
        val d = (v as? JsonPrimitive)?.takeUnless { it.isString }?.doubleOrNull ?: invalid("$key must be a number")
        if (d.isNaN() || d.isInfinite()) invalid("$key must be finite")
        return d
    }

    fun JsonObject.integer(key: String): Long? {
        val v = present(key) ?: return null
        if (v !is JsonPrimitive || v.isString || !JSON_INTEGER.matches(v.content)) invalid("$key must be an integer")
        return v.content.toLongOrNull() ?: invalid("$key is out of range")
    }

    fun JsonObject.obj(key: String): JsonObject? {
        val v = present(key) ?: return null
        return v as? JsonObject ?: invalid("$key must be an object")
    }

    fun JsonObject.array(key: String): JsonArray? {
        val v = present(key) ?: return null
        return v as? JsonArray ?: invalid("$key must be an array")
    }

    fun <T : Enum<T>> JsonObject.choice(key: String, values: Map<String, T>): T? =
        string(key)?.let { values[it] ?: invalid("$key must be one of ${values.keys.joinToString()}") }
}
