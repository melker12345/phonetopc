package io.github.melker12345.phonetopc.protocol

import io.github.melker12345.phonetopc.protocol.approval.ApprovalKeyInfo
import io.github.melker12345.phonetopc.protocol.approval.ApprovalOutcome
import io.github.melker12345.phonetopc.protocol.approval.ApprovalParams
import io.github.melker12345.phonetopc.protocol.approval.ApprovalService
import io.github.melker12345.phonetopc.protocol.approval.ApprovalWhy
import io.github.melker12345.phonetopc.protocol.approval.DecryptOutcome
import io.github.melker12345.phonetopc.protocol.approval.DecryptParams
import io.github.melker12345.phonetopc.protocol.panels.NotifyParams
import io.github.melker12345.phonetopc.protocol.panels.PanelOwner
import io.github.melker12345.phonetopc.protocol.panels.PanelService
import io.github.melker12345.phonetopc.protocol.panels.PanelShowParams
import io.github.melker12345.phonetopc.protocol.panels.PanelUpdateParams
import io.github.melker12345.phonetopc.protocol.panels.PcRef
import io.github.melker12345.phonetopc.protocol.panels.Rules
import io.github.melker12345.phonetopc.protocol.security.TokenHash
import kotlinx.coroutines.Job
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
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicLong
import kotlin.coroutines.cancellation.CancellationException

/** WebSocket close the transport should perform on behalf of a session. */
data class CloseRequest(val code: Short, val reason: String)

object CloseCodes {
    const val UNSUPPORTED_DATA: Short = 1003
    const val POLICY_VIOLATION: Short = 1008

    /** SECURITY.md §5: an address over the failed-hello limit. */
    const val TRY_AGAIN_LATER: Short = 1013
}

/**
 * One client connection. Transport-agnostic: the transport feeds text frames into
 * [handleText], drains [receiveOutgoing] into the socket, honours [closeRequest] and calls
 * [close] when the connection ends.
 *
 * Outgoing responses and events share one ordered queue, so a response is never overtaken
 * by an event produced after it (e.g. no event follows an `unsubscribe` response). A new
 * subscription only starts producing events after its `subscribe` response is queued.
 * Responses are never dropped, but a client that sends requests without reading the answers
 * is closed with 1008 once [BridgeHub.responseQueueCapacity] are pending, and so is one that
 * sends more than [BridgeHub.framesBeforeHello] frames without a successful `hello`. Events
 * are bounded per spec §5: continuous ones are dropped when
 * [BridgeHub.continuousQueueCapacity] events are pending; if [BridgeHub.onChangeQueueCapacity]
 * on_change events are pending the session asks to be closed with 1008.
 */
class Session internal constructor(
    private val hub: BridgeHub,
    val sessionId: Long,
    val remote: String,
    /** Source address, for rate-limiting failed hellos in secure mode. */
    val remoteHost: String = remote,
) : PanelOwner {
    private class Out(val text: String, val kind: Int, val close: CloseRequest? = null)

    private val queue = Channel<Out>(Channel.UNLIMITED)
    private val pendingResponses = AtomicInteger()
    private val pendingContinuous = AtomicInteger()
    private val pendingOnChange = AtomicInteger()
    private val closeDeferred = CompletableDeferred<CloseRequest>()
    private val subscriptions = ConcurrentHashMap<String, Subscription>()

    @Volatile private var closed = false

    /** Frames received before a successful hello. Only touched by [handleText]. */
    private var framesBeforeHello = 0

    @Volatile var helloDone: Boolean = false
        private set

    @Volatile var client: ClientInfo? = null
        private set

    /** Secure mode: the paired PC this session authenticated as. */
    @Volatile var pc: PcInfo? = null
        private set

    /**
     * Secure mode: [io.github.melker12345.phonetopc.protocol.security.TokenHash] of the token
     * this session authenticated with, so re-pairing can close exactly the sessions that used
     * the old token (SECURITY.md §4.4).
     */
    @Volatile var tokenHash: String? = null
        private set

    /** The approval key this session's `hello` announced (APPROVAL.md §1), if any. */
    @Volatile var approvalKey: ApprovalKeyInfo? = null
        private set

    /** The decryption key this session's `hello` announced (DECRYPT.md §1), if any. */
    @Volatile var decryptKey: ApprovalKeyInfo? = null
        private set

    /** Pending `request_approval`s and `decrypt`s; cancelled when the session closes (APPROVAL.md §4). */
    private val approvals: MutableSet<Job> = ConcurrentHashMap.newKeySet()

    /**
     * Set when the session must end (failed auth, revoked, a close was requested): later frames
     * are ignored and nothing more is answered.
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
            KIND_RESPONSE -> pendingResponses.decrementAndGet()
            KIND_CONTINUOUS -> pendingContinuous.decrementAndGet()
            KIND_ON_CHANGE -> pendingOnChange.decrementAndGet()
        }
    }

    suspend fun handleText(text: String) {
        if (ending) return
        if (!helloDone && ++framesBeforeHello > hub.framesBeforeHello) {
            closeAfterQueued(CloseRequest(CloseCodes.POLICY_VIOLATION, "hello required"))
            return
        }
        val reply = respond(text) ?: return
        reply.text?.let { enqueueResponse(it) }
        reply.closeAfter?.let { closeAfterQueued(it) }
        // PANELS.md §4.4: held panel events and the stroke replay go out after the response.
        reply.afterResponse?.invoke()
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

    /** Ask the transport to close the connection. First request wins; later frames are ignored. */
    fun requestClose(request: CloseRequest) {
        ending = true
        closeDeferred.complete(request)
    }

    /**
     * Responses are not dropped, so a client that doesn't read them can't be allowed to pile
     * them up without bound: close it instead.
     */
    private fun enqueueResponse(text: String) {
        if (pendingResponses.incrementAndGet() > hub.responseQueueCapacity) {
            pendingResponses.decrementAndGet()
            requestClose(CloseRequest(CloseCodes.POLICY_VIOLATION, "response queue overflow"))
            return
        }
        queue.trySend(Out(text, KIND_RESPONSE))
    }

    /** Called by the transport when the connection is gone. Drops all subscriptions. */
    fun close() {
        if (closed) return
        closed = true
        approvals.forEach { it.cancel() }
        approvals.clear()
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

    /**
     * A response to queue, plus the subscription it created, which may start once it's queued.
     * [text] is `null` when the session closes without answering (rate limited).
     */
    internal class Reply(
        val text: String?,
        val newSubscription: Subscription? = null,
        val closeAfter: CloseRequest? = null,
        val afterResponse: (() -> Unit)? = null,
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
        // request_approval and decrypt exist only in secure mode (APPROVAL.md §1, DECRYPT.md §1).
        if (!methodExists(method.content)) {
            return error(replyId, RpcException(RpcError.METHOD_NOT_FOUND, "Method not found: ${method.content}"))
        }
        // Spec §2: explicit null params count as absent; any other non-object is -32602.
        val params = when (val p = element["params"]) {
            null, JsonNull -> EMPTY
            is JsonObject -> p
            else -> return error(replyId, RpcException(RpcError.INVALID_PARAMS, "params must be an object"))
        }

        val isHello = method.content == "hello"
        val rehello = isHello && helloDone
        return try {
            val handled = dispatch(method.content, params, replyId)
            val result = handled.result ?: return null // answered later (request_approval)
            Reply(Rpc.result(replyId, result), handled.newSubscription, afterResponse = handled.afterResponse)
        } catch (e: RateLimited) {
            // SECURITY.md §5: a limited address is closed with 1013, even with a valid token.
            Reply(null, closeAfter = CloseRequest(CloseCodes.TRY_AGAIN_LATER, "too many failed hellos; try again later"))
        } catch (e: RpcException) {
            // SECURITY.md §5: -32010 closes with 1008, and so does any failed hello after a
            // successful one.
            val close = when {
                e.error == RpcError.UNAUTHORIZED -> CloseRequest(CloseCodes.POLICY_VIOLATION, "unauthorized")
                rehello && hub.security != null -> CloseRequest(CloseCodes.POLICY_VIOLATION, "hello failed")
                else -> null
            }
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

    /** [result] `null`: the response is sent later. */
    private class Handled(
        val result: JsonElement?,
        val newSubscription: Subscription? = null,
        val afterResponse: (() -> Unit)? = null,
    )

    /**
     * request_approval and decrypt exist only in secure mode (APPROVAL.md §1, DECRYPT.md §1); notifications and panels
     * only when the phone offers them (PANELS.md §1).
     */
    private fun methodExists(method: String): Boolean = when (method) {
        in METHODS -> true
        APPROVAL_METHOD, DECRYPT_METHOD -> hub.security != null
        in NOTIFY_METHODS -> hub.notifications != null
        in PANEL_METHODS -> hub.panels != null
        else -> false
    }

    /** Who this session's notifications and panels belong to (PANELS.md §1). */
    private val pcRef: PcRef get() = PcRef.of(pc)

    /** Deliver a PC-scoped event to this session's subscriptions of its source (PANELS.md §2). */
    internal fun deliverDirect(reading: Reading) {
        if (closed) return
        val id = hub.deviceId
        for (sub in subscriptions.values) if (sub.spec.source == reading.source) sub.offer(reading, id)
    }

    override fun deliverPanelEvent(reading: Reading) = deliverDirect(reading)

    private suspend fun dispatch(method: String, params: JsonObject, replyId: JsonElement): Handled {
        return when (method) {
            "hello" -> Handled(hello(params))
            "list_sensors" -> Handled(
                ProtocolJson.encodeToJsonElement(ListSensorsResult.serializer(), ListSensorsResult(hub.describe(pcRef))),
            )
            "subscribe" -> subscribe(params)
            "unsubscribe" -> Handled(unsubscribe(params))
            "get" -> Handled(get(params))
            APPROVAL_METHOD -> requestApproval(params, replyId)
            DECRYPT_METHOD -> decrypt(params, replyId)
            "notify" -> {
                val parsed = NotifyParams.parse(params)
                Handled(buildJsonObject { put("id", JsonPrimitive(hub.notifications!!.notify(pcRef, parsed))) })
            }
            "notify_cancel" -> {
                val id = with(Rules) { params.requiredId("id") }
                hub.notifications!!.cancel(pcRef, id)
                Handled(EMPTY)
            }
            "panel_show" -> {
                val parsed = PanelShowParams.parse(params)
                hub.panels!!.show(this, pcRef, parsed).let { Handled(it.result, afterResponse = it.afterResponse) }
            }
            "panel_attach" -> {
                val (panel, after) = PanelService.parseAttach(params)
                hub.panels!!.attach(this, pcRef, panel, after).let { Handled(it.result, afterResponse = it.afterResponse) }
            }
            "panel_update" -> {
                val parsed = PanelUpdateParams.parse(params)
                hub.panels!!.update(pcRef, parsed)
                Handled(EMPTY)
            }
            "panel_close" -> {
                val panel = with(Rules) { params.requiredId("panel") }
                hub.panels!!.close(pcRef, panel)
                Handled(EMPTY)
            }
            "panel_list" -> Handled(hub.panels!!.list(pcRef))
            "canvas_clear" -> {
                val (panel, widget) = PanelService.parseCanvasClear(params)
                hub.panels!!.clearCanvas(pcRef, panel, widget)
                Handled(EMPTY)
            }
            else -> throw RpcException(RpcError.METHOD_NOT_FOUND, "Method not found: $method")
        }
    }

    /**
     * Order of checks (SECURITY.md §5): rate limit (close 1013, no answer), params shape
     * (-32602), then in secure mode `auth` (-32010), then the protocol version (-32004). An
     * unauthenticated client learns nothing but -32010.
     */
    private fun hello(params: JsonObject): JsonElement {
        val security = hub.security
        if (security != null && security.limiter.isBlocked(remoteHost)) throw RateLimited()
        val protocol = params.requiredString("protocol")
        val auth = security?.let { authenticate(it, params) }
        if (protocol !in SUPPORTED_PROTOCOLS) {
            throw RpcException(
                RpcError.UNSUPPORTED_PROTOCOL,
                "Unsupported protocol: $protocol",
                buildJsonObject { put("supported", buildJsonArray { SUPPORTED_PROTOCOLS.forEach { add(JsonPrimitive(it)) } }) },
            )
        }
        val pcInfo = auth?.first
        client = (params["client"] as? JsonObject)?.let {
            ClientInfo(it.lenientString("name"), it.lenientString("version"))
        }
        if (auth != null) {
            pc = auth.first
            tokenHash = auth.second
            // A revocation between authenticate() and here saw no pc on this session, so
            // disconnectSessions() missed it. Revokers remove the token before they look for
            // sessions, so checking again after publishing pc catches it.
            if (params.authToken()?.let(security!!.authenticate) != auth.first) {
                pc = null
                tokenHash = null
                throw RpcException(RpcError.UNAUTHORIZED, "Unknown or revoked token")
            }
        }
        // APPROVAL.md §1: secure mode only, and only while the phone can do approvals.
        val approval = if (auth != null) hub.approvals?.keyInfo() else null
        val decrypt = if (auth != null) hub.approvals?.decryptKeyInfo() else null
        approvalKey = approval
        decryptKey = decrypt
        helloDone = true
        hub.sessionChanged()
        pcInfo?.let { hub.security?.onAuthenticated?.invoke(it) }
        val result = HelloResult(
            protocol = PROTOCOL_VERSION,
            server = hub.serverInfo,
            device = hub.deviceInfo(),
            capabilities = buildList {
                add("events")
                add("get")
                if (approval != null) add("approval")
                if (decrypt != null) add("decrypt")
                if (hub.notifications != null) add("notify")
                if (hub.panels != null) add("panels")
            },
            pc = pcInfo,
            approval = approval,
            decrypt = decrypt,
        )
        return ProtocolJson.encodeToJsonElement(HelloResult.serializer(), result)
    }

    /**
     * SECURITY.md §5. A missing, malformed, unknown or revoked token is -32010, and the caller
     * closes with 1008. Returns the PC and the hash of the token it used.
     */
    private fun JsonObject.authToken(): String? =
        ((this["auth"] as? JsonObject)?.get("token") as? JsonPrimitive)?.takeIf { it.isString }?.content

    private fun authenticate(security: SecureAccess, params: JsonObject): Pair<PcInfo, String> {
        val token = params.authToken()
        val found = token?.takeIf { it.isNotEmpty() }?.let(security.authenticate)
        if (token == null || found == null) {
            security.limiter.recordFailure(remoteHost)
            throw RpcException(
                RpcError.UNAUTHORIZED,
                if (token == null) "hello needs auth.token; pair this PC first" else "Unknown or revoked token",
            )
        }
        return found to TokenHash.of(token)
    }

    /** The address is over the failed-hello limit: close with 1013 without answering. */
    private class RateLimited : Exception()

    /**
     * APPROVAL.md §4. Params are checked first (-32602); everything else is a result. The
     * answer comes when the user acted or the request timed out; other requests and events
     * carry on meanwhile.
     */
    /** DECRYPT.md §4, run like [requestApproval]. */
    private fun decrypt(params: JsonObject, replyId: JsonElement): Handled {
        val parsed = DecryptParams.parse(params)
        val service = hub.approvals
        val pcInfo = pc
        if (service == null || pcInfo == null) {
            return Handled(DecryptOutcome.Refused(ApprovalWhy.UNAVAILABLE).toJson())
        }
        val start = service.startDecrypt(pcInfo, decryptKey, parsed) { outcome ->
            if (!closed && !ending) enqueueResponse(Rpc.result(replyId, outcome.toJson()))
        }
        return started(start)
    }

    private fun requestApproval(params: JsonObject, replyId: JsonElement): Handled {
        val parsed = ApprovalParams.parse(params)
        val service = hub.approvals
        val pcInfo = pc
        if (service == null || pcInfo == null) {
            return Handled(ApprovalOutcome.Refused(ApprovalWhy.UNAVAILABLE).toJson())
        }
        val start = service.start(hub.deviceId, pcInfo, approvalKey, parsed) { outcome ->
            if (!closed && !ending) enqueueResponse(Rpc.result(replyId, outcome.toJson()))
        }
        return started(start)
    }

    private fun started(start: ApprovalService.Start): Handled {
        return when (start) {
            is ApprovalService.Start.Answered -> Handled(start.outcome.toJson())
            is ApprovalService.Start.DecryptAnswered -> Handled(start.outcome.toJson())
            is ApprovalService.Start.Pending -> {
                val job = start.job
                approvals += job
                job.invokeOnCompletion { approvals.remove(job) }
                // Closed while starting: close() may have missed it.
                if (closed) job.cancel()
                Handled(null)
            }
        }
    }

    private fun subscribe(params: JsonObject): Handled {
        // Spec §2 step 7: params that can be checked without the catalog, for any source.
        val sourceName = params.requiredString("source")
        val type = params.string("type")
        val requestedRate = params.number("rate_hz")
        if (requestedRate != null && requestedRate <= 0.0) {
            throw RpcException(RpcError.INVALID_PARAMS, "rate_hz must be > 0")
        }
        val minDelta = params.number("min_delta")
        if (minDelta != null && minDelta < 0.0) {
            throw RpcException(RpcError.INVALID_PARAMS, "min_delta must be >= 0")
        }
        val spec = lookup(sourceName, type)
        val info = hub.info(sourceName, pcRef)

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
        val id = params.requiredString("subscription")
        val sub = subscriptions.remove(id)
            ?: throw RpcException(RpcError.UNKNOWN_SUBSCRIPTION, "Unknown subscription: $id")
        sub.deactivate()
        hub.removeSubscriptions(listOf(sub))
        return EMPTY
    }

    private suspend fun get(params: JsonObject): JsonElement {
        val sourceName = params.requiredString("source")
        val type = params.string("type")
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
        if (!hub.info(sourceName, pcRef).available) {
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
        const val APPROVAL_METHOD = "request_approval"
        const val DECRYPT_METHOD = "decrypt"
        val NOTIFY_METHODS = setOf("notify", "notify_cancel")
        val PANEL_METHODS = setOf("panel_show", "panel_attach", "panel_update", "panel_close", "panel_list", "canvas_clear")

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
