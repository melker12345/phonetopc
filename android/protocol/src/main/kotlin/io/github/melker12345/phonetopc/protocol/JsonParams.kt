package io.github.melker12345.phonetopc.protocol

import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.doubleOrNull

// Typed access to request params, shared by the core methods (Session), panels (Rules, PANELS.md
// §8) and approvals (APPROVAL.md §4). Unknown members are ignored and an explicit `null` counts as
// absent (spec §2); a member of the wrong type is -32602 with "<key> must be a …", wording the
// conformance cases and the PC see, so keep it stable.

private val JSON_INTEGER = Regex("-?(0|[1-9][0-9]*)")

internal fun invalidParams(message: String): Nothing = throw RpcException(RpcError.INVALID_PARAMS, message)

/** A member that is present and not `null` (explicit nulls count as absent). */
internal fun JsonObject.present(key: String): JsonElement? = this[key]?.takeUnless { it is JsonNull }

internal fun JsonObject.string(key: String): String? {
    val v = present(key) ?: return null
    if (v !is JsonPrimitive || !v.isString) invalidParams("$key must be a string")
    return v.content
}

internal fun JsonObject.requiredString(key: String): String = string(key) ?: invalidParams("Missing required param: $key")

internal fun JsonObject.bool(key: String): Boolean? {
    val v = present(key) ?: return null
    if (v !is JsonPrimitive || v.isString || (v.content != "true" && v.content != "false")) invalidParams("$key must be a boolean")
    return v.content == "true"
}

internal fun JsonObject.number(key: String): Double? {
    val v = present(key) ?: return null
    val d = (v as? JsonPrimitive)?.takeUnless { it.isString }?.doubleOrNull ?: invalidParams("$key must be a number")
    if (d.isNaN() || d.isInfinite()) invalidParams("$key must be finite")
    return d
}

/** A JSON integer spelled as one (`30000`, not `3e4` or `30000.0`). */
internal fun JsonObject.integer(key: String): Long? {
    val v = present(key) ?: return null
    if (v !is JsonPrimitive || v.isString || !JSON_INTEGER.matches(v.content)) invalidParams("$key must be an integer")
    return v.content.toLongOrNull() ?: invalidParams("$key is out of range")
}

internal fun JsonObject.obj(key: String): JsonObject? {
    val v = present(key) ?: return null
    return v as? JsonObject ?: invalidParams("$key must be an object")
}

internal fun JsonObject.array(key: String): JsonArray? {
    val v = present(key) ?: return null
    return v as? JsonArray ?: invalidParams("$key must be an array")
}

// Lenient reads, for what the other side reports rather than asks for (hello's client info, a
// PC's pairing results): a member of the wrong type is treated as absent instead of an error.
// Public because :pairing reads the PC's answers with them.

fun JsonObject.lenientString(key: String): String? = (this[key] as? JsonPrimitive)?.takeIf { it.isString }?.content

fun JsonObject.lenientObj(key: String): JsonObject? = this[key] as? JsonObject
