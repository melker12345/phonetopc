package io.github.melker12345.phonetopc.server

import io.github.melker12345.phonetopc.pairing.NewPairing
import io.github.melker12345.phonetopc.pairing.PairingClient
import io.github.melker12345.phonetopc.pairing.PairingFailure
import io.github.melker12345.phonetopc.pairing.PairingQr
import io.github.melker12345.phonetopc.pairing.PairingState
import io.github.melker12345.phonetopc.pairing.PhoneInfo
import io.github.melker12345.phonetopc.pairing.WebSocketPairingConnector
import io.github.melker12345.phonetopc.protocol.security.Pem
import io.github.melker12345.phonetopc.protocol.security.Pins
import io.ktor.server.application.install
import io.ktor.server.cio.CIO
import io.ktor.server.engine.EmbeddedServer
import io.ktor.server.engine.embeddedServer
import io.ktor.server.routing.routing
import io.ktor.server.websocket.WebSockets
import io.ktor.server.websocket.webSocket
import io.ktor.websocket.Frame
import io.ktor.websocket.readText
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Test
import java.io.File
import kotlin.time.Duration.Companion.seconds

/**
 * The real pairing transport (pinned wss:// client + JSON-RPC channel) against a scripted PC
 * pairing listener, served with the PC test certificate from spec/conformance/vectors.
 */
class PairingOverTlsTest {
    private val vectors = File(System.getProperty("phonetopc.conformanceDir") ?: "../../spec/conformance", "vectors")
    private val pcIdentity = TlsIdentity(
        Pem.ecPrivateKey(File(vectors, "pc-key.pem").readText()),
        Pins.parsePemCertificate(File(vectors, "pc-cert.pem").readText()),
    )
    private val secret = "AAECAwQFBgcICQoLDA0ODw"
    private val received = mutableListOf<String>()
    private var backend: EmbeddedServer<*, *>? = null
    private var frontend: TlsFrontend? = null

    private fun startListener(): Int {
        val b = embeddedServer(CIO, port = 0, host = "127.0.0.1") {
            install(WebSockets)
            routing {
                webSocket("/v1/pair") {
                    for (frame in incoming) {
                        val msg = Json.parseToJsonElement((frame as Frame.Text).readText()).jsonObject
                        val id = msg["id"].toString()
                        val method = msg["method"]!!.jsonPrimitive.content
                        synchronized(received) { received += method }
                        val params = msg["params"]!!.jsonObject
                        val reply = when (method) {
                            "pair_qr" -> if (params["secret"]!!.jsonPrimitive.content == secret) {
                                """{"jsonrpc":"2.0","id":$id,"result":{}}"""
                            } else {
                                """{"jsonrpc":"2.0","id":$id,"error":{"code":-32020,"message":"bad secret"}}"""
                            }
                            "pair_finish" -> """{"jsonrpc":"2.0","id":$id,"result":{"pc":{"id":"9d1c0a","name":"arch-desktop"}}}"""
                            else -> """{"jsonrpc":"2.0","id":$id,"error":{"code":-32022,"message":"out of order"}}"""
                        }
                        // An unrelated notification first: the client must skip it.
                        outgoing.send(Frame.Text("""{"jsonrpc":"2.0","method":"noise","params":{}}"""))
                        outgoing.send(Frame.Text(reply))
                    }
                }
            }
        }.start(wait = false)
        backend = b
        val backendPort = runBlocking { b.engine.resolvedConnectors().first().port }
        val f = TlsFrontend(pcIdentity, "127.0.0.1", 0, backendPort)
        f.start()
        frontend = f
        return f.boundPort
    }

    @After fun tearDown() {
        frontend?.stop()
        backend?.stop(0, 500)
    }

    private fun qr(port: Int, pin: String = pcIdentity.pin, s: String = secret) = PairingQr.parse(
        "phonetopc://pair?v=1&id=9d1c0a&n=arch-desktop&h=127.0.0.2,127.0.0.1&p=$port&pin=$pin&s=$s",
    )

    private fun pair(qr: PairingQr): Pair<PairingState, List<NewPairing>> = runBlocking {
        val stored = mutableListOf<NewPairing>()
        val client = PairingClient(
            WebSocketPairingConnector(connectTimeoutMillis = 2_000),
            phone = { PhoneInfo("635f3a01db1859bc", "Phone", "sha256/tQITurCV6l1Rq9Rk0X_kPrHlWaaKPnkI6CGl03GEaBc", 8765, emptyList()) },
            store = { stored += it },
        )
        withTimeout(30.seconds) { client.pairWithQr(qr) }
        client.state.value to stored
    }

    @Test fun qrPairingOverRealTls() {
        val port = startListener()
        assertEquals("sha256/89Nebgssmdcpo6zU3xFLFXCRqV4YpVYbvJbyq99cMGA", pcIdentity.pin)
        val (state, stored) = pair(qr(port))
        assertEquals(PairingState.Paired("9d1c0a", "arch-desktop"), state)
        assertEquals(pcIdentity.pin, stored.single().pin)
        assertEquals(listOf("pair_qr", "pair_finish"), synchronized(received) { received.toList() })
    }

    @Test fun qrWithAnotherPinIsRefusedBeforeAnyMessage() {
        val port = startListener()
        val (state, stored) = pair(qr(port, pin = "sha256/tQITurCV6l1Rq9Rk0X_kPrHlWaaKPnkI6CGl03GEaBc"))
        assertEquals(PairingFailure.PinMismatch, (state as PairingState.Failed).failure)
        assertEquals(emptyList<NewPairing>(), stored)
        assertEquals(emptyList<String>(), received)
    }

    @Test fun badSecretIsReported() {
        val port = startListener()
        val (state, _) = pair(qr(port, s = "BBECAwQFBgcICQoLDA0ODw"))
        assertEquals(PairingFailure.BadSecret, (state as PairingState.Failed).failure)
    }
}
