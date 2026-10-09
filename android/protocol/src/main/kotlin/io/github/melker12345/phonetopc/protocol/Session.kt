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
 * by an event produced after it (e.g. no event follows an `unsubscribe` response). A new
 * subscription only starts producing events after its `subscribe` response is queued.
 * Responses are never dropped. Events are bounded per spec §5: continuous ones are dropped
 * when [BridgeHub.continuousQueueCapacity] events are pending; if
 * [BridgeHub.onChangeQueueCapacity] on_change events are pending the session asks to be
 * closed with 1008.
 */
class Session internal constructor(
    private val hub: BridgeHub,
    val sessionId: Long,
    val remote: String,
    /** Source address, for rate-limiting failed hellos in secure mode. */
    val remoteHost: String = remote,
) {
    private class Out(val text: String, val kind: Int, val close: CloseRequest? = null)

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

    /** Secure mode: the paired PC this session authenticated as. */
    @Volatile var pc: PcInfo? = null
        private set

    /**
     * Set when the session must end (failed auth, revoked): later frames are ignored and the
     * close is queued behind the responses already queued.
     */
    @Volatile private var ending = false

    /** Continuous events dropped because the client could not keep up. */
    val droppedEvents = AtomicLong()

    /** Completes when the server must close the connection (e.g. on_change overflow). */
    val closeRequest: Deferred<CloseRequest> get() = closeDeferred

    val activeSubscriptions: Collection<Subscription> get() = subscriptions.values

    /** Next frame to send, suspending until there is one. `null` once the session is closed. */
    suspend fun receiveOutgoing(): String? {
        val out = queue.receiveCatching().getOrNull() ?: return null
        return deliver(out)
    }

    /** Non-suspending variant, mainly for tests. */
    fun tryReceiveOutgoing(): String? {
        val out = queue.tryReceive().getOrNull() ?: return null
        return deliver(out)
    }

    /** A close marker means everything before it has been handed out: now ask for the close. */
    private fun deliver(out: Out): String? {
        out.close?.let {
            requestClose(it)
            return null
        }
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
        if (ending) return
        val reply = respond(text) ?: return
        queue.trySend(Out(reply.text, KIND_RESPONSE))
        reply.closeAfter?.let { closeAfterQueued(it) }
        // Spec §4.3: a new subscription's events (including its initial value) may only be
        // queued once its subscribe response is.
        reply.newSubscription?.let { hub.startSubscription(it) }
    }

    /**
     * Close once everything queued so far has been sent, so e.g. the -32010 error reaches the
     * client before the 1008 close (SECURITY.md §5).
     */
    private fun closeAfterQueued(request: CloseRequest) {
        ending = true
        if (queue.trySend(Out("", KIND_RESPONSE, request)).isFailure) requestClose(request)
    }

    /** The PC was revoked: close now with 1008 (SECURITY.md §5). */
    internal fun revoke() {
        ending = true
        requestClose(CloseRequest(CloseCodes.POLICY_VIOLATION, "pairing revoked"))
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

    /** A response to queue, plus the subscription it created, which may start once it's queued. */
    internal class Reply(
        val text: String,
        val newSubscription: Subscription? = null,
        val closeAfter: CloseRequest? = null,
    )

    /**
     * Checks a frame in the order of spec §2: strict JSON, object, id present (else ignored),
     * id/jsonrpc/method valid, hello done, method known, params valid, then the method itself.
     */
    internal suspend fun respond(text: String): Reply? {
        fun error(id: JsonElement?, e: RpcException) = Reply(Rpc.error(id, e))

        val element = try {
            ProtocolJson.parseToJsonElement(text).takeIf { isStrictJson(it) }
        } catch (e: CancellationException) {
            throw e
        } catch (_: Exception) {
            null
        } ?: return error(null, RpcException(RpcError.PARSE_ERROR))
        if (element is JsonArray) {
            return error(null, RpcException(RpcError.INVALID_REQUEST, "Batch requests are not supported"))
        }
        if (element !is JsonObject) {
            return error(null, RpcException(RpcError.INVALID_REQUEST, "Request must be a JSON object"))
        }

        // Spec §2: an object without id is a notification, even if otherwise malformed. The
        // protocol defines none from the client, so it is ignored without a reply.
        val replyId = element["id"] ?: return null
        if (!isValidId(replyId)) {
            return error(null, RpcException(RpcError.INVALID_REQUEST, "id must be a string or an integer within ±(2^53-1)"))
        }

        val jsonrpc = element["jsonrpc"]
        if (jsonrpc !is JsonPrimitive || !jsonrpc.isString || jsonrpc.content != "2.0") {
            return error(replyId, RpcException(RpcError.INVALID_REQUEST, "jsonrpc must be \"2.0\""))
        }
        val method = element["method"]
        if (method !is JsonPrimitive || !method.isString) {
            return error(replyId, RpcException(RpcError.INVALID_REQUEST, "method must be a string"))
        }
        // Spec §3: *any* request other than hello before a successful hello gets -32003,
        // including unknown methods and requests with invalid params.
        if (method.content != "hello" && !helloDone) {
            return error(replyId, RpcException(RpcError.HELLO_REQUIRED))
        }
        if (method.content !in METHODS) {
            return error(replyId, RpcException(RpcError.METHOD_NOT_FOUND, "Method not found: ${method.content}"))
        }
        // Spec §2: explicit null params count as absent; any other non-object is -32602.
        val params = when (val p = element["params"]) {
            null, JsonNull -> EMPTY
            is JsonObject -> p
            else -> return error(replyId, RpcException(RpcError.INVALID_PARAMS, "params must be an object"))
        }

        return try {
            val handled = dispatch(method.content, params)
            Reply(Rpc.result(replyId, handled.result), handled.newSubscription)
        } catch (e: RpcException) {
            val close = if (e.error == RpcError.UNAUTHORIZED) CloseRequest(CloseCodes.POLICY_VIOLATION, "unauthorized") else null
            Reply(Rpc.error(replyId, e), closeAfter = close)
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            error(replyId, RpcException(RpcError.INTERNAL_ERROR, e.message ?: "Internal error"))
        }
    }

    /**
     * Spec §2: a string or an integer within ±(2^53-1). Fractions, exponents, out-of-range
     * integers and `null` are rejected.
     */
    private fun isValidId(id: JsonElement): Boolean = when (id) {
        is JsonNull -> false
        is JsonPrimitive -> id.isString || (
            JSON_INTEGER.matches(id.content) &&
                id.content.toLongOrNull()?.let { it in -MAX_SAFE_ID..MAX_SAFE_ID } == true
            )
        else -> false
    }

    private class Handled(val result: JsonElement, val newSubscription: Subscription? = null)

    private suspend fun dispatch(method: String, params: JsonObject): Handled {
        return when (method) {
            "hello" -> Handled(hello(params))
            "list_sensors" -> Handled(
                ProtocolJson.encodeToJsonElement(ListSensorsResult.serializer(), ListSensorsResult(hub.describe())),
            )
            "subscribe" -> subscribe(params)
            "unsubscribe" -> Handled(unsubscribe(params))
            "get" -> Handled(get(params))
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
        val pcInfo = hub.security?.let { authenticate(it, params) }
        client = (params["client"] as? JsonObject)?.let {
            ClientInfo(it.lenientString("name"), it.lenientString("version"))
        }
        if (pcInfo != null) pc = pcInfo
        helloDone = true
        hub.sessionChanged()
        pcInfo?.let { hub.security?.onAuthenticated?.invoke(it) }
        val result = HelloResult(
            protocol = PROTOCOL_VERSION,
            server = hub.serverInfo,
            device = hub.deviceInfo(),
            capabilities = listOf("events", "get"),
            pc = pcInfo,
        )
        return ProtocolJson.encodeToJsonElement(HelloResult.serializer(), result)
    }

    /**
     * SECURITY.md §5. A missing, malformed, unknown or revoked token is -32010, and the caller
     * closes with 1008. Addresses with too many recent failures are refused without a lookup.
     */
    private fun authenticate(security: SecureAccess, params: JsonObject): PcInfo {
        if (security.limiter.isBlocked(remoteHost)) {
            throw RpcException(RpcError.UNAUTHORIZED, "Too many failed attempts; try again later")
        }
        val token = ((params["auth"] as? JsonObject)?.get("token") as? JsonPrimitive)
            ?.takeIf { it.isString }?.content
        val found = token?.takeIf { it.isNotEmpty() }?.let(security.authenticate)
        if (found == null) {
            security.limiter.recordFailure(remoteHost)
            throw RpcException(
                RpcError.UNAUTHORIZED,
                if (token == null) "hello needs auth.token; pair this PC first" else "Unknown or revoked token",
            )
        }
        return found
    }

    private fun subscribe(params: JsonObject): Handled {
        // Spec §2 step 7: params that can be checked without the catalog, for any source.
        val sourceName = params.requireString("source")
        val type = params.optString("type")
        val requestedRate = params.optNumber("rate_hz")
        if (requestedRate != null && requestedRate <= 0.0) {
            throw RpcException(RpcError.INVALID_PARAMS, "rate_hz must be > 0")
        }
        val minDelta = params.optNumber("min_delta")
        if (minDelta != null && minDelta < 0.0) {
            throw RpcException(RpcError.INVALID_PARAMS, "min_delta must be >= 0")
        }
        val spec = lookup(sourceName, type)
        val info = hub.info(sourceName)

        val rate = if (spec.mode == SensorMode.CONTINUOUS) {
            effectiveRate(requestedRate, info)
        } else {
            null // rate_hz only applies to continuous sources and is ignored otherwise
        }
        // The subscription is routed (and the source started) now but stays pending, holding
        // back events, until handleText has queued this response.
        val sub = Subscription(hub.newSubscriptionId(), this, spec, type, rate, minDelta)
        subscriptions[sub.id] = sub
        hub.addSubscription(sub)
        val result = SubscribeResult(sub.id, spec.source, type?.let { listOf(it) } ?: spec.types, spec.mode, rate)
        return Handled(ProtocolJson.encodeToJsonElement(SubscribeResult.serializer(), result), sub)
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
        val type = params.optString("type")
        lookup(sourceName, type)
        val reading = hub.read(sourceName)?.takeIf { type == null || it.type == type }
            ?: throw RpcException(RpcError.NO_VALUE, "No value yet for $sourceName")
        return reading.toJson(hub.deviceId)
    }

    /**
     * Spec §2 step 8 for a source and optional type: unknown source, then a type the source
     * doesn't have, then availability.
     */
    private fun lookup(sourceName: String, type: String?): SensorSpec {
        val spec = SensorCatalog[sourceName]
            ?: throw RpcException(RpcError.UNKNOWN_SOURCE, "Unknown source: $sourceName")
        if (type != null && type !in spec.types) {
            throw RpcException(RpcError.INVALID_PARAMS, "Unknown type \"$type\" for source $sourceName")
        }
        if (!hub.info(sourceName).available) {
            throw RpcException(RpcError.SOURCE_UNAVAILABLE, "Source unavailable: $sourceName")
        }
        return spec
    }

    internal fun snapshot() = ClientSnapshot(
        sessionId = sessionId,
        remote = remote,
        clientName = client?.name,
        clientVersion = client?.version,
        helloDone = helloDone,
        pc = pc,
        subscriptions = subscriptions.values
            .sortedBy { it.id }
            .map { SubscriptionSnapshot(it.id, it.spec.source, it.type, it.rateHz) },
    )

    private companion object {
        const val KIND_RESPONSE = 0
        const val KIND_CONTINUOUS = 1
        const val KIND_ON_CHANGE = 2
        val EMPTY = JsonObject(emptyMap())
        val METHODS = setOf("hello", "list_sensors", "subscribe", "unsubscribe", "get")

        /** Largest magnitude of an integer id (2^53-1), so every JSON library keeps it exact. */
        const val MAX_SAFE_ID = (1L shl 53) - 1

        val JSON_NUMBER = Regex("-?(0|[1-9][0-9]*)(\\.[0-9]+)?([eE][+-]?[0-9]+)?")
        val JSON_INTEGER = Regex("-?(0|[1-9][0-9]*)")

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
