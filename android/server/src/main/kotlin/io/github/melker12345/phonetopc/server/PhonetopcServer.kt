package io.github.melker12345.phonetopc.server

import io.github.melker12345.phonetopc.protocol.BridgeHub
import io.github.melker12345.phonetopc.protocol.CloseCodes
import io.ktor.server.application.Application
import io.ktor.server.application.install
import io.ktor.server.cio.CIO
import io.ktor.server.engine.EmbeddedServer
import io.ktor.server.engine.embeddedServer
import io.ktor.server.plugins.origin
import io.ktor.server.routing.routing
import io.ktor.server.websocket.DefaultWebSocketServerSession
import io.ktor.server.websocket.WebSockets
import io.ktor.server.websocket.pingPeriod
import io.ktor.server.websocket.timeout
import io.ktor.server.websocket.webSocket
import io.ktor.websocket.CloseReason
import io.ktor.websocket.Frame
import io.ktor.websocket.close
import io.ktor.websocket.readText
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.launch
import kotlin.time.Duration
import kotlin.time.Duration.Companion.seconds

const val DEFAULT_PORT = 8765
const val WS_PATH = "/v1/ws"

/**
 * WebSocket endpoint `ws://<host>:<port>/v1/ws` (spec §1) on the Ktor CIO engine.
 * One [io.github.melker12345.phonetopc.protocol.Session] per connection; any number of clients.
 */
class PhonetopcServer(
    private val hub: BridgeHub,
    val port: Int = DEFAULT_PORT,
    private val host: String = "0.0.0.0",
    private val pingPeriod: Duration = 15.seconds,
    private val pingTimeout: Duration = 30.seconds,
) {
    private var server: EmbeddedServer<*, *>? = null

    /** Binds and starts serving. Blocking; call off the main thread. Throws if the port is taken. */
    fun start() {
        check(server == null) { "already started" }
        server = embeddedServer(CIO, port = port, host = host) {
            phonetopcModule(hub, pingPeriod, pingTimeout)
        }.start(wait = false)
    }

    /** The actually bound port (useful with port 0 in tests). */
    suspend fun boundPort(): Int =
        requireNotNull(server) { "not started" }.engine.resolvedConnectors().first().port

    /** Stops the server, closing every connection. Blocking. */
    fun stop() {
        server?.stop(gracePeriodMillis = 200, timeoutMillis = 2_000)
        server = null
    }
}

fun Application.phonetopcModule(hub: BridgeHub, pingPeriod: Duration, pingTimeout: Duration) {
    install(WebSockets) {
        this.pingPeriod = pingPeriod
        this.timeout = pingTimeout
        maxFrameSize = MAX_FRAME_BYTES
        masking = false
    }
    routing {
        webSocket(WS_PATH) { serve(hub) }
    }
}

/** Requests are tiny; anything bigger is a misbehaving client. */
private const val MAX_FRAME_BYTES = 64L * 1024

private suspend fun DefaultWebSocketServerSession.serve(hub: BridgeHub) {
    val origin = call.request.origin
    val session = hub.openSession("${origin.remoteAddress}:${origin.remotePort}")
    try {
        coroutineScope {
            val writer = launch {
                while (true) {
                    val text = session.receiveOutgoing() ?: break
                    outgoing.send(Frame.Text(text))
                }
            }
            val closer = launch {
                val request = session.closeRequest.await()
                close(CloseReason(request.code, request.reason))
            }
            try {
                for (frame in incoming) {
                    when (frame) {
                        is Frame.Text -> session.handleText(frame.readText())
                        is Frame.Binary -> {
                            // Spec §1: binary frames are reserved.
                            close(CloseReason(CloseCodes.UNSUPPORTED_DATA, "binary frames are not supported"))
                            break
                        }
                        else -> Unit // ping/pong/close are handled by Ktor
                    }
                }
            } finally {
                writer.cancel()
                closer.cancel()
            }
        }
    } finally {
        session.close()
    }
}
