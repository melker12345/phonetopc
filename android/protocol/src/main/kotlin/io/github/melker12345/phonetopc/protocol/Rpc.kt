package io.github.melker12345.phonetopc.protocol

import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

/** Builders for outgoing JSON-RPC 2.0 frames. */
object Rpc {
    fun result(id: JsonElement, result: JsonElement): String = buildJsonObject {
        put("jsonrpc", "2.0")
        put("id", id)
        put("result", result)
    }.toString()

    fun error(id: JsonElement?, error: RpcException): String = buildJsonObject {
        put("jsonrpc", "2.0")
        put("id", id ?: JsonNull)
        put("error", error.toErrorJson())
    }.toString()

    fun notification(method: String, params: JsonObject): String = buildJsonObject {
        put("jsonrpc", "2.0")
        put("method", method)
        put("params", params)
    }.toString()
}
