package io.github.melker12345.phonetopc.pairing

import io.github.melker12345.phonetopc.protocol.ProtocolJson
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.launch
import kotlinx.coroutines.runInterruptible
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeout
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.intOrNull
import kotlinx.serialization.json.longOrNull
import kotlinx.serialization.json.put
import java.io.Closeable
import java.io.IOException
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicLong
import kotlin.time.Duration

class PairingRpcException(val code: Int, message: String) : Exception(message)

/** One open connection to a PC's pairing listener (`/v1/pair`). */
interface PairingChannel : Closeable {
    /** Pin of the certificate the PC presented (`pc_pin_seen`). */
    val pcPin: String

    /**
     * Sends a request and waits up to [timeout] for its result.
     * @throws PairingRpcException for an error response
     * @throws IOException if the connection is lost
     * @throws kotlinx.coroutines.TimeoutCancellationException on timeout
     */
    suspend fun call(method: String, params: JsonObject, timeout: Duration): JsonObject
}

/** Opens a [PairingChannel]; the real one is [WebSocketPairingConnector], tests use a fake PC. */
fun interface PairingConnector {
    /**
     * @param expectedPin the PC pin from the QR code, or `null` for way B, which learns it here.
     * @throws PinMismatchException if [expectedPin] is set and the PC shows another one
     */
    suspend fun connect(host: String, port: Int, expectedPin: String?): PairingChannel
}

const val PAIR_PATH = "/v1/pair"

class WebSocketPairingConnector(private val connectTimeoutMillis: Int = 5_000) : PairingConnector {
    override suspend fun connect(host: String, port: Int, expectedPin: String?): PairingChannel {
        val ws = withContext(Dispatchers.IO) {
            runInterruptible { PinnedWebSocket.connect(host, port, PAIR_PATH, expectedPin, connectTimeoutMillis) }
        }
        return WebSocketPairingChannel(ws)
    }
}

/** JSON-RPC 2.0 over a [PinnedWebSocket]: one reader thread, responses matched by id. */
class WebSocketPairingChannel(private val ws: PinnedWebSocket) : PairingChannel {
    override val pcPin: String get() = ws.pinSeen

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val pending = ConcurrentHashMap<Long, CompletableDeferred<JsonObject>>()
    private val nextId = AtomicLong(1)
    private val closedWith = CompletableDeferred<IOException>()

    init {
        scope.launch {
            val reason = try {
                while (true) {
                    val text = ws.receive() ?: break
                    dispatch(text)
                }
                IOException("the PC closed the connection")
            } catch (e: IOException) {
                e
            } catch (e: Exception) {
                // Anything else from a misbehaving PC must fail the pairing, not kill the app.
                IOException("bad data from the PC", e)
            }
            closedWith.complete(reason)
            pending.values.forEach { it.completeExceptionally(reason) }
        }
    }

    private fun dispatch(text: String) {
        val msg = runCatching { ProtocolJson.parseToJsonElement(text) as? JsonObject }.getOrNull() ?: return
        val id = (msg["id"] as? JsonPrimitive)?.longOrNull ?: return // notifications or junk: ignore
        val waiter = pending.remove(id) ?: return
        val error = msg["error"] as? JsonObject
        if (error != null) {
            val code = (error["code"] as? JsonPrimitive)?.intOrNull ?: 0
            val message = (error["message"] as? JsonPrimitive)?.content ?: "error $code"
            waiter.completeExceptionally(PairingRpcException(code, message))
        } else {
            waiter.complete(msg["result"] as? JsonObject ?: JsonObject(emptyMap()))
        }
    }

    override suspend fun call(method: String, params: JsonObject, timeout: Duration): JsonObject {
        val id = nextId.getAndIncrement()
        val waiter = CompletableDeferred<JsonObject>()
        pending[id] = waiter
        closedWith.takeIf { it.isCompleted }?.let { throw it.getCompleted() }
        try {
            val frame = buildJsonObject {
                put("jsonrpc", "2.0")
                put("id", id)
                put("method", method)
                put("params", params)
            }.toString()
            withContext(Dispatchers.IO) { ws.send(frame) }
            return withTimeout(timeout) { waiter.await() }
        } finally {
            pending.remove(id)
        }
    }

    override fun close() {
        ws.close()
        scope.cancel()
    }
}
