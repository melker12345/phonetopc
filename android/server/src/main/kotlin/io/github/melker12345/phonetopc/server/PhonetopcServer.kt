package io.github.melker12345.phonetopc.server

import io.github.melker12345.phonetopc.protocol.BridgeHub
import io.github.melker12345.phonetopc.protocol.CloseCodes
import io.github.melker12345.phonetopc.protocol.CloseRequest
import io.ktor.server.application.Application
import io.ktor.server.application.install
import io.ktor.server.cio.CIO
import io.ktor.server.engine.EmbeddedServer
import io.ktor.server.engine.embeddedServer
import io.ktor.server.routing.routing
import io.ktor.server.websocket.DefaultWebSocketServerSession
import io.ktor.server.websocket.WebSockets
import io.ktor.server.websocket.pingPeriod
import io.ktor.server.websocket.timeout
import io.ktor.server.websocket.webSocket
import io.ktor.websocket.ChannelOverflow
import io.ktor.websocket.CloseReason
import io.ktor.websocket.Frame
import io.ktor.websocket.close
import io.ktor.websocket.readText
import kotlinx.coroutines.awaitCancellation
import kotlinx.coroutines.cancel
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeoutOrNull
import kotlin.time.Duration
import kotlin.time.Duration.Companion.seconds

const val DEFAULT_PORT = 8765
const val WS_PATH = "/v1/ws"

/**
 * WebSocket endpoint `/v1/ws` (spec §1) on the Ktor CIO engine. One
 * [io.github.melker12345.phonetopc.protocol.Session] per connection; any number of clients.
 *
 * - **Dev mode** ([tls] `null`): `ws://<host>:<port>`, no authentication (draft 1).
 * - **Secure mode**: `wss://<host>:<port>` (SECURITY.md §3). CIO can't serve TLS, so it listens
 *   on a loopback port behind a [TlsFrontend] on the public one. Give the [BridgeHub] a
 *   [io.github.melker12345.phonetopc.protocol.SecureAccess] so `hello` needs a token.
 *   Connections without a successful `hello` within [helloTimeout] are closed with 1008.
 */
class PhonetopcServer(
    private val hub: BridgeHub,
    val port: Int = DEFAULT_PORT,
    private val host: String = "0.0.0.0",
    private val pingPeriod: Duration = 15.seconds,
    private val pingTimeout: Duration = 30.seconds,
    private val tls: TlsIdentity? = null,
    private val helloTimeout: Duration = 10.seconds,
) {
    private var server: EmbeddedServer<*, *>? = null
    private var frontend: TlsFrontend? = null

    val secure: Boolean get() = tls != null

    /** Binds and starts serving. Blocking; call off the main thread. Throws if the port is taken. */
    fun start() {
        check(server == null) { "already started" }
        if (tls == null) {
            server = embeddedServer(CIO, port = port, host = host) {
                phonetopcModule(hub, pingPeriod, pingTimeout)
            }.start(wait = false)
            return
        }
        var front: TlsFrontend? = null
        val backend = embeddedServer(CIO, port = 0, host = LOOPBACK) {
            phonetopcModule(
                hub, pingPeriod, pingTimeout, helloTimeout,
                object : Relays {
                    override fun peerFor(loopbackPort: Int) = front?.peerFor(loopbackPort)
                    override fun authenticated(loopbackPort: Int) { front?.authenticated(loopbackPort) }
                    override fun drop(loopbackPort: Int) { front?.drop(loopbackPort) }
                },
            )
        }.start(wait = false)
        try {
            val backendPort = runBlocking { backend.engine.resolvedConnectors().first().port }
            front = TlsFrontend(
                tls, host, port, backendPort,
                backendHost = LOOPBACK,
                handshakeTimeoutMillis = helloTimeout.inWholeMilliseconds.toInt(),
            )
            front.start()
        } catch (e: Exception) {
            backend.stop(0, 500)
            throw e
        }
        server = backend
        frontend = front
    }

    /** The actually bound public port (useful with port 0 in tests). */
    suspend fun boundPort(): Int {
        frontend?.let { return it.boundPort }
        return requireNotNull(server) { "not started" }.engine.resolvedConnectors().first().port
    }

    /** Stops the server, closing every connection. Blocking. */
    fun stop() {
        frontend?.stop()
        frontend = null
        server?.stop(gracePeriodMillis = 200, timeoutMillis = 2_000)
        server = null
    }

    private companion object {
        const val LOOPBACK = "127.0.0.1"
    }
}

/**
 * @param helloTimeout secure mode: close connections that haven't completed `hello` in time.
 * @param relays secure mode: the [TlsFrontend] in front. Loopback connections it doesn't know
 *   are refused.
 */
fun Application.phonetopcModule(
    hub: BridgeHub,
    pingPeriod: Duration,
    pingTimeout: Duration,
    helloTimeout: Duration? = null,
    relays: Relays? = null,
) {
    install(WebSockets) {
        this.pingPeriod = pingPeriod
        this.timeout = pingTimeout
        maxFrameSize = MAX_FRAME_BYTES
        masking = false
        // Ktor's queues are unbounded by default, so a client that doesn't read would make
        // frames pile up there. Bounded, the session's own limits apply (see Session).
        channels {
            incoming = bounded(FRAME_QUEUE, ChannelOverflow.SUSPEND)
            outgoing = bounded(FRAME_QUEUE, ChannelOverflow.SUSPEND)
        }
    }
    routing {
        webSocket(WS_PATH) { serve(hub, helloTimeout, relays) }
    }
}

/** Requests are tiny; anything bigger is a misbehaving client. */
private const val MAX_FRAME_BYTES = 64L * 1024

/** Frames Ktor buffers each way per connection. */
private const val FRAME_QUEUE = 16

/** How long a closing client gets to take the close frame and answer it. */
private val CLOSE_GRACE = 1.seconds

private suspend fun DefaultWebSocketServerSession.serve(
    hub: BridgeHub,
    helloTimeout: Duration?,
    relays: Relays?,
) {
    val local = call.request.local
    val peer = if (relays == null) {
        Peer(local.remoteAddress, local.remotePort)
    } else {
        relays.peerFor(local.remotePort) ?: run {
            // A loopback connection that didn't come through the TLS frontend.
            close(CloseReason(CloseCodes.POLICY_VIOLATION, "use wss://"))
            return
        }
    }
    val session = hub.openSession(peer.toString(), peer.host)
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
                // A client that doesn't read can't take the close frame, let alone answer it,
                // and would hold its unsent frames forever: give it a moment, then drop it.
                // If it does answer, the connection ends and this is cancelled first. Without
                // the TLS frontend (dev mode) Ktor may keep such a socket open after the
                // session has ended, but no longer queues anything for it.
                withTimeoutOrNull(CLOSE_GRACE) {
                    close(CloseReason(request.code, request.reason))
                    awaitCancellation()
                }
                relays?.drop(local.remotePort)
                this@serve.cancel()
            }
            // SECURITY.md §5: drop connections that don't authenticate promptly. The session
            // ignores everything it receives from then on.
            val helloTimer = helloTimeout?.let { timeout ->
                launch {
                    delay(timeout)
                    if (!session.helloDone) session.requestClose(CloseRequest(CloseCodes.POLICY_VIOLATION, "hello timeout"))
                }
            }
            var authenticated = false
            try {
                for (frame in incoming) {
                    when (frame) {
                        is Frame.Text -> {
                            session.handleText(frame.readText())
                            // Paired PCs don't count against their address's limit.
                            if (!authenticated && session.pc != null) {
                                authenticated = true
                                relays?.authenticated(local.remotePort)
                            }
                        }
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
                helloTimer?.cancel()
            }
        }
    } finally {
        session.close()
    }
}
