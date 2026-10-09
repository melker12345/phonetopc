package io.github.melker12345.phonetopc.protocol

import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

/** JSON-RPC and phonetopc error codes (spec §7). */
enum class RpcError(val code: Int, val errorName: String, val defaultMessage: String) {
    PARSE_ERROR(-32700, "parse_error", "Parse error"),
    INVALID_REQUEST(-32600, "invalid_request", "Invalid request"),
    METHOD_NOT_FOUND(-32601, "method_not_found", "Method not found"),
    INVALID_PARAMS(-32602, "invalid_params", "Invalid params"),
    INTERNAL_ERROR(-32603, "internal_error", "Internal error"),
    UNKNOWN_SOURCE(-32001, "unknown_source", "Unknown source"),
    SOURCE_UNAVAILABLE(-32002, "source_unavailable", "Source unavailable"),
    HELLO_REQUIRED(-32003, "hello_required", "hello must be the first request"),
    UNSUPPORTED_PROTOCOL(-32004, "unsupported_protocol", "Unsupported protocol"),
    UNKNOWN_SUBSCRIPTION(-32005, "unknown_subscription", "Unknown subscription"),
    NO_VALUE(-32006, "no_value", "No value yet"),
    UNAUTHORIZED(-32010, "unauthorized", "Unauthorized"),
}

class RpcException(
    val error: RpcError,
    message: String = error.defaultMessage,
    val extraData: JsonObject? = null,
) : Exception(message) {
    fun toErrorJson(): JsonObject = buildJsonObject {
        put("code", error.code)
        put("message", message ?: error.defaultMessage)
        put("data", buildJsonObject {
            put("name", error.errorName)
            extraData?.forEach { (k, v) -> put(k, v) }
        })
    }
}
