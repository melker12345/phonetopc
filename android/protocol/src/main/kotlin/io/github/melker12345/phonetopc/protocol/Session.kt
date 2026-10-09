package io.github.melker12345.phonetopc.protocol

import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.Deferred
import kotlinx.coroutines.channels.Channel
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonArray
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.doubleOrNull
import kotlinx.serialization.json.longOrNull
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicLong
import kotlin.coroutines.cancellation.CancellationException

/** WebSocket close the transport should perform on behalf of a session. */
data class CloseRequest(val code: Short, val reason: String)

object CloseCodes {
    const val UNSUPPORTED_DATA: Short = 1003
    const val POLICY_VIOLATION: Short = 1008
}

/**
 * One client connection. Transport-agnostic: the transport feeds text frames into
 * [handleText], drains [receiveOutgoing] into the socket, honours [closeRequest] and calls
 * [close] when the connection ends.
 *
 * Outgoing responses and events share one ordered queue, so a response is never overtaken
 * by an event produced after it (e.g. no event follows an `unsubscribe` response).
 * Responses are never dropped. Events are bounded per spec §5: continuous ones are dropped
 * when [BridgeHub.continuousQueueCapacity] events are pending; if
 * [BridgeHub.onChangeQueueCapacity] on_change events are pending the session asks to be
 * closed with 1008.
 */
class Session internal constructor(
    private val hub: BridgeHub,
    val sessionId: Long,
    val remote: String,
) {
    private class Out(val text: String, val kind: Int)

    private val queue = Channel<Out>(Channel.UNLIMITED)
    private val pendingContinuous = AtomicInteger()
    private val pendingOnChange = AtomicInteger()
    private val closeDeferred = CompletableDeferred<CloseRequest>()
    private val subscriptions = ConcurrentHashMap<String, Subscription>()

    @Volatile private var closed = false

    @Volatile var helloDone: Boolean = false
        private set

    @Volatile var client: ClientInfo? = null
        private set

    /** Continuous events dropped because the client could not keep up. */
    val droppedEvents = AtomicLong()

    /** Completes when the server must close the connection (e.g. on_change overflow). */
    val closeRequest: Deferred<CloseRequest> get() = closeDeferred

    val activeSubscriptions: Collection<Subscription> get() = subscriptions.values

    /** Next frame to send, suspending until there is one. `null` once the session is closed. */
    suspend fun receiveOutgoing(): String? {
        val out = queue.receiveCatching().getOrNull() ?: return null
        markSent(out)
        return out.text
    }

    /** Non-suspending variant, mainly for tests. */
    fun tryReceiveOutgoing(): String? {
        val out = queue.tryReceive().getOrNull() ?: return null
        markSent(out)
        return out.text
    }

    private fun markSent(out: Out) {
        when (out.kind) {
            KIND_CONTINUOUS -> pendingContinuous.decrementAndGet()
            KIND_ON_CHANGE -> pendingOnChange.decrementAndGet()
        }
    }

    /** Handle one inbound text frame; the response (if any) is queued for sending. */
    suspend fun handleText(text: String) {
        val response = respond(text) ?: return
        queue.trySend(Out(response, KIND_RESPONSE))
    }

    /** Ask the transport to close the connection. First request wins. */
    fun requestClose(request: CloseRequest) {
        closeDeferred.complete(request)
    }

    /** Called by the transport when the connection is gone. Drops all subscriptions. */
    fun close() {
        if (closed) return
        closed = true
        subscriptions.values.forEach { it.deactivate() }
        hub.removeSubscriptions(subscriptions.values.toList())
        subscriptions.clear()
        hub.closeSession(this)
        queue.close()
    }

    internal fun enqueueEvent(text: String, continuous: Boolean) {
        if (closed) return
        if (continuous) {
            if (pendingContinuous.incrementAndGet() > hub.continuousQueueCapacity) {
                pendingContinuous.decrementAndGet()
                droppedEvents.incrementAndGet()
                return
            }
            queue.trySend(Out(text, KIND_CONTINUOUS))
        } else {
            if (pendingOnChange.incrementAndGet() > hub.onChangeQueueCapacity) {
                pendingOnChange.decrementAndGet()
                // Spec §5: on_change events must not be dropped; close instead.
                requestClose(CloseRequest(CloseCodes.POLICY_VIOLATION, "event queue overflow"))
                return
            }
            queue.trySend(Out(text, KIND_ON_CHANGE))
        }
    }

    // ---- JSON-RPC framing ----

    internal suspend fun respond(text: String): String? {
        val element = try {
            ProtocolJson.parseToJsonElement(text).takeIf { isStrictJson(it) }
        } catch (e: CancellationException) {
            throw e
        } catch (_: Exception) {
            null
        } ?: return Rpc.error(null, RpcException(RpcError.PARSE_ERROR))
        if (element is JsonArray) {
            return Rpc.error(null, RpcException(RpcError.INVALID_REQUEST, "Batch requests are not supported"))
        }
        if (element !is JsonObject) {
            return Rpc.error(null, RpcException(RpcError.INVALID_REQUEST, "Request must be a JSON object"))
        }

        val hasId = "id" in element
        val id = element["id"]
        if (id != null && !isValidId(id)) {
            return Rpc.error(null, RpcException(RpcError.INVALID_REQUEST, "id must be a string or a number"))
        }
        val replyId: JsonElement = id ?: JsonNull

        val jsonrpc = element["jsonrpc"]
        if (jsonrpc !is JsonPrimitive || !jsonrpc.isString || jsonrpc.content != "2.0") {
            return Rpc.error(replyId, RpcException(RpcError.INVALID_REQUEST, "jsonrpc must be \"2.0\""))
        }
        val method = element["method"]
        if (method !is JsonPrimitive || !method.isString) {
            return Rpc.error(replyId, RpcException(RpcError.INVALID_REQUEST, "method must be a string"))
        }
        val params = when (val p = element["params"]) {
            null -> null
            is JsonObject -> p
            is JsonArray -> return Rpc.error(
                replyId, RpcException(RpcError.INVALID_PARAMS, "params must be an object"),
            )
            else -> return Rpc.error(
                replyId, RpcException(RpcError.INVALID_REQUEST, "params must be an object"),
            )
        }
        // A request without id is a JSON-RPC notification, which must not be answered.
        // The protocol defines no client-to-server notifications, so it is ignored.
        if (!hasId) return null

        return try {
            Rpc.result(replyId, dispatch(method.content, params ?: EMPTY))
        } catch (e: RpcException) {
            Rpc.error(replyId, e)
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            Rpc.error(replyId, RpcException(RpcError.INTERNAL_ERROR, e.message ?: "Internal error"))
        }
    }

    private fun isValidId(id: JsonElement): Boolean = when (id) {
        is JsonNull -> true // allowed by JSON-RPC 2.0 (discouraged); echoed back as null
        is JsonPrimitive -> id.isString || id.longOrNull != null || id.doubleOrNull != null
        else -> false
    }

    private suspend fun dispatch(method: String, params: JsonObject): JsonElement {
        // Spec §3: *any* request other than hello before a successful hello gets -32003,
        // including unknown methods.
        if (method != "hello" && !helloDone) throw RpcException(RpcError.HELLO_REQUIRED)
        return when (method) {
            "hello" -> hello(params)
            "list_sensors" -> ProtocolJson.encodeToJsonElement(ListSensorsResult.serializer(), ListSensorsResult(hub.describe()))
            "subscribe" -> subscribe(params)
            "unsubscribe" -> unsubscribe(params)
            "get" -> get(params)
            else -> throw RpcException(RpcError.METHOD_NOT_FOUND, "Method not found: $method")
        }
    }

    private fun hello(params: JsonObject): JsonElement {
        val protocol = params.requireString("protocol")
        if (protocol !in SUPPORTED_PROTOCOLS) {
            throw RpcException(
                RpcError.UNSUPPORTED_PROTOCOL,
                "Unsupported protocol: $protocol",
                buildJsonObject { put("supported", buildJsonArray { SUPPORTED_PROTOCOLS.forEach { add(JsonPrimitive(it)) } }) },
            )
        }
        client = (params["client"] as? JsonObject)?.let {
            ClientInfo(it.lenientString("name"), it.lenientString("version"))
        }
        helloDone = true
        hub.sessionChanged()
        val result = HelloResult(
            protocol = PROTOCOL_VERSION,
            server = hub.serverInfo,
            device = hub.deviceInfo(),
            capabilities = listOf("events", "get"),
        )
        return ProtocolJson.encodeToJsonElement(HelloResult.serializer(), result)
    }

    private fun subscribe(params: JsonObject): JsonElement {
        val sourceName = params.requireString("source")
        val spec = SensorCatalog[sourceName]
            ?: throw RpcException(RpcError.UNKNOWN_SOURCE, "Unknown source: $sourceName")
        val type = params.optString("type")
        if (type != null && type !in spec.types) {
            throw RpcException(RpcError.INVALID_PARAMS, "Unknown type \"$type\" for source $sourceName")
        }
        val requestedRate = params.optNumber("rate_hz")
        if (requestedRate != null && requestedRate <= 0.0) {
            throw RpcException(RpcError.INVALID_PARAMS, "rate_hz must be > 0")
        }
        val minDelta = params.optNumber("min_delta")
        if (minDelta != null && minDelta < 0.0) {
            throw RpcException(RpcError.INVALID_PARAMS, "min_delta must be >= 0")
        }
        val info = hub.info(sourceName)
        if (!info.available) throw RpcException(RpcError.SOURCE_UNAVAILABLE, "Source unavailable: $sourceName")

        val rate = if (spec.mode == SensorMode.CONTINUOUS) {
            effectiveRate(requestedRate, info)
        } else {
            null // rate_hz only applies to continuous sources and is ignored otherwise
        }
        val sub = Subscription(hub.newSubscriptionId(), this, spec, type, rate, minDelta)
        subscriptions[sub.id] = sub
        hub.addSubscription(sub)
        val result = SubscribeResult(sub.id, spec.source, type?.let { listOf(it) } ?: spec.types, rate)
        return ProtocolJson.encodeToJsonElement(SubscribeResult.serializer(), result)
    }

    private fun unsubscribe(params: JsonObject): JsonElement {
        val id = params.requireString("subscription")
        val sub = subscriptions.remove(id)
            ?: throw RpcException(RpcError.UNKNOWN_SUBSCRIPTION, "Unknown subscription: $id")
        sub.deactivate()
        hub.removeSubscriptions(listOf(sub))
        return EMPTY
    }

    private suspend fun get(params: JsonObject): JsonElement {
        val sourceName = params.requireString("source")
        val spec = SensorCatalog[sourceName]
            ?: throw RpcException(RpcError.UNKNOWN_SOURCE, "Unknown source: $sourceName")
        val type = params.optString("type")
        if (type != null && type !in spec.types) {
            throw RpcException(RpcError.INVALID_PARAMS, "Unknown type \"$type\" for source $sourceName")
        }
        if (!hub.info(sourceName).available) {
            throw RpcException(RpcError.SOURCE_UNAVAILABLE, "Source unavailable: $sourceName")
        }
        val reading = hub.read(sourceName)?.takeIf { type == null || it.type == type }
            ?: throw RpcException(RpcError.NO_VALUE, "No value yet for $sourceName")
        return reading.toJson(hub.deviceId)
    }

    internal fun snapshot() = ClientSnapshot(
        sessionId = sessionId,
        remote = remote,
        clientName = client?.name,
        clientVersion = client?.version,
        helloDone = helloDone,
        subscriptions = subscriptions.values
            .sortedBy { it.id }
            .map { SubscriptionSnapshot(it.id, it.spec.source, it.type, it.rateHz) },
    )

    private companion object {
        const val KIND_RESPONSE = 0
        const val KIND_CONTINUOUS = 1
        const val KIND_ON_CHANGE = 2
        val EMPTY = JsonObject(emptyMap())

        val JSON_NUMBER = Regex("-?(0|[1-9][0-9]*)(\\.[0-9]+)?([eE][+-]?[0-9]+)?")

        /**
         * kotlinx.serialization's tree parser accepts unquoted words (`nope`, `{"a":b}`) as
         * literals. Reject anything that isn't strict RFC 8259 so those get -32700.
         */
        fun isStrictJson(e: JsonElement): Boolean = when (e) {
            is JsonObject -> e.values.all { isStrictJson(it) }
            is JsonArray -> e.all { isStrictJson(it) }
            is JsonNull -> true
            is JsonPrimitive -> e.isString || e.content == "true" || e.content == "false" ||
                JSON_NUMBER.matches(e.content)
        }

        fun effectiveRate(requested: Double?, info: SensorInfo): Double {
            val min = info.minRateHz ?: Double.MIN_VALUE
            val max = maxOf(info.maxRateHz ?: Double.MAX_VALUE, min)
            val default = info.defaultRateHz ?: DEFAULT_RATE_HZ
            return (requested ?: default).coerceIn(min, max)
        }
    }
}

/** Default when a source doesn't advertise one. */
const val DEFAULT_RATE_HZ = 50.0

// ---- params helpers (unknown fields are ignored; explicit nulls count as absent) ----

private fun JsonObject.present(key: String): JsonElement? = this[key]?.takeUnless { it is JsonNull }

private fun JsonObject.requireString(key: String): String =
    optString(key) ?: throw RpcException(RpcError.INVALID_PARAMS, "Missing required param: $key")

private fun JsonObject.optString(key: String): String? {
    val v = present(key) ?: return null
    if (v !is JsonPrimitive || !v.isString) throw RpcException(RpcError.INVALID_PARAMS, "$key must be a string")
    return v.content
}

private fun JsonObject.optNumber(key: String): Double? {
    val v = present(key) ?: return null
    val d = (v as? JsonPrimitive)?.takeUnless { it.isString }?.doubleOrNull
        ?: throw RpcException(RpcError.INVALID_PARAMS, "$key must be a number")
    if (d.isNaN() || d.isInfinite()) throw RpcException(RpcError.INVALID_PARAMS, "$key must be finite")
    return d
}

private fun JsonObject.lenientString(key: String): String? =
    (this[key] as? JsonPrimitive)?.takeIf { it.isString }?.content
