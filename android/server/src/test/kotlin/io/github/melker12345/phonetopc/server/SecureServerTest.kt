package io.github.melker12345.phonetopc.server

import io.github.melker12345.phonetopc.pairing.PinMismatchException
import io.github.melker12345.phonetopc.pairing.PinnedWebSocket
import io.github.melker12345.phonetopc.protocol.BridgeHub
import io.github.melker12345.phonetopc.protocol.Demand
import io.github.melker12345.phonetopc.protocol.DeviceInfo
import io.github.melker12345.phonetopc.protocol.PcInfo
import io.github.melker12345.phonetopc.protocol.Reading
import io.github.melker12345.phonetopc.protocol.ReadingSink
import io.github.melker12345.phonetopc.protocol.SecureAccess
import io.github.melker12345.phonetopc.protocol.SensorCatalog
import io.github.melker12345.phonetopc.protocol.SensorInfo
import io.github.melker12345.phonetopc.protocol.SensorSource
import io.github.melker12345.phonetopc.protocol.ServerInfo
import io.github.melker12345.phonetopc.protocol.security.Pem
import io.github.melker12345.phonetopc.protocol.security.Pins
import io.github.melker12345.phonetopc.protocol.security.Secrets
import io.github.melker12345.phonetopc.protocol.security.TokenHash
import io.ktor.client.HttpClient
import io.ktor.client.engine.cio.CIO
import io.ktor.client.plugins.websocket.WebSockets
import io.ktor.client.plugins.websocket.webSocket
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.int
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test
import java.io.File
import java.net.Socket
import java.security.cert.X509Certificate
import javax.net.ssl.SSLContext
import javax.net.ssl.SSLSocket
import javax.net.ssl.X509TrustManager
import kotlin.time.Duration.Companion.milliseconds
import kotlin.time.Duration.Companion.seconds

/** Secure mode end to end: TLS frontend, pin, token auth, hello timeout and revocation. */
class SecureServerTest {
    private val vectors = File(System.getProperty("phonetopc.conformanceDir") ?: "../../spec/conformance", "vectors")
    private val identity = TlsIdentity(
        Pem.ecPrivateKey(File(vectors, "phone-key.pem").readText()),
        Pins.parsePemCertificate(File(vectors, "phone-cert.pem").readText()),
    )
    private val phonePin = "sha256/tQITurCV6l1Rq9Rk0X_kPrHlWaaKPnkI6CGl03GEaBc"
    private val pcPin = "sha256/89Nebgssmdcpo6zU3xFLFXCRqV4YpVYbvJbyq99cMGA"

    private val token = Secrets.newToken()
    private val pc = PcInfo("9d1c0a", "arch-desktop")

    private class Source : SensorSource {
        override fun describe() = SensorCatalog.all.map { SensorInfo.of(it, true, 1.0, 200.0, 50.0) }
        override fun setDemand(source: String, demand: Demand?) {}
        override suspend fun read(source: String): Reading? = null
        override fun setSink(sink: ReadingSink?) {}
    }

    private val hub = BridgeHub(
        Source(), ServerInfo("phonetopc-android", "test"), { DeviceInfo("dev1", "Phone", platform = "android") },
        security = SecureAccess(authenticate = { t -> pc.takeIf { TokenHash.matches(t, TokenHash.of(token)) } }),
    )
    private var server: PhonetopcServer? = null

    private fun start(helloTimeoutMs: Long = 10_000): Int = runBlocking {
        val s = PhonetopcServer(hub, port = 0, host = "127.0.0.1", tls = identity, helloTimeout = helloTimeoutMs.milliseconds)
        s.start()
        server = s
        s.boundPort()
    }

    @After fun tearDown() {
        server?.stop()
    }

    private fun connect(port: Int, protocols: List<String> = PinnedWebSocket.PROTOCOLS, pin: String? = phonePin) =
        PinnedWebSocket.connect("127.0.0.1", port, WS_PATH, pin, readTimeoutMillis = 10_000, protocols = protocols)

    private fun PinnedWebSocket.call(id: Int, method: String, params: String): JsonObject {
        send("""{"jsonrpc":"2.0","id":$id,"method":"$method","params":$params}""")
        return Json.parseToJsonElement(receive()!!).jsonObject
    }

    private fun hello(t: String?) =
        """{"protocol":"phonetopc/1","client":{"name":"test"}${t?.let { ""","auth":{"token":"$it"}""" } ?: ""}}"""

    @Test fun pinnedHelloOverTls13And12() {
        val port = start()
        for (versions in listOf(listOf("TLSv1.3"), listOf("TLSv1.2"))) {
            connect(port, versions).use { ws ->
                assertEquals(phonePin, ws.pinSeen)
                val r = ws.call(1, "hello", hello(token))["result"]!!.jsonObject
                assertEquals("9d1c0a", r["pc"]!!.jsonObject["id"]!!.jsonPrimitive.content)
                assertTrue("sensors" in ws.call(2, "list_sensors", "{}")["result"]!!.jsonObject)
            }
        }
        // The server saw the real client address, not the loopback relay's.
        assertTrue(hub.clients.value.all { it.remote.startsWith("127.0.0.1:") })
    }

    @Test fun wrongPinFailsTheHandshake() {
        val port = start()
        try {
            connect(port, pin = pcPin).close()
            fail("connected despite a pin mismatch")
        } catch (e: PinMismatchException) {
            assertEquals(phonePin, e.seen)
        }
    }

    @Test fun badTokenGetsUnauthorizedThenClose1008() {
        val port = start()
        connect(port).use { ws ->
            val r = ws.call(1, "hello", hello(Secrets.newToken()))
            assertEquals(-32010, r["error"]!!.jsonObject["code"]!!.jsonPrimitive.int)
            assertNull(ws.receive())
            assertEquals(1008, ws.closeCode)
        }
    }

    @Test fun rateLimitedAddressIsClosed1013EvenWithAValidToken() {
        val port = start()
        repeat(5) {
            connect(port).use { ws ->
                ws.call(1, "hello", hello(Secrets.newToken()))
                assertNull(ws.receive())
                assertEquals(1008, ws.closeCode)
            }
        }
        connect(port).use { ws ->
            ws.send("""{"jsonrpc":"2.0","id":1,"method":"hello","params":${hello(token)}}""")
            assertNull("no answer when rate limited", ws.receive())
            assertEquals(1013, ws.closeCode)
        }
    }

    @Test fun helloTimeoutCloses1008() {
        val port = start(helloTimeoutMs = 300)
        connect(port).use { ws ->
            assertNull(ws.receive())
            assertEquals(1008, ws.closeCode)
        }
    }

    @Test fun revokingClosesTheSession() {
        val port = start()
        connect(port).use { ws ->
            ws.call(1, "hello", hello(token))
            assertEquals(1, hub.disconnectPcs { it.id == "9d1c0a" })
            assertNull(ws.receive())
            assertEquals(1008, ws.closeCode)
        }
    }

    @Test fun plaintextClientsCantConnect() {
        val port = start()
        val client = HttpClient(CIO) { install(WebSockets) }
        var opened = false
        try {
            runBlocking {
                withTimeout(10.seconds) {
                    client.webSocket(host = "127.0.0.1", port = port, path = WS_PATH) { opened = true }
                }
            }
        } catch (_: Exception) {
            // Expected: the TLS frontend drops a plaintext request.
        } finally {
            client.close()
        }
        assertTrue("plaintext ws:// was accepted", !opened)
    }

    /** A TLS connection that trusts anything, for clients that speak WebSocket by hand ([Flood]). */
    private fun tlsSocket(port: Int): Socket {
        val trustAll = object : X509TrustManager {
            override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?) {}
            override fun checkServerTrusted(chain: Array<out X509Certificate>?, authType: String?) {}
            override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()
        }
        val context = SSLContext.getInstance("TLS").apply { init(null, arrayOf(trustAll), null) }
        return (context.socketFactory.createSocket("127.0.0.1", port) as SSLSocket).apply { startHandshake() }
    }

    @Test fun clientThatOnlySendsIsDroppedBeforeHello() {
        val port = start()
        Flood(tlsSocket(port), port, first = null, request = Flood.LIST_SENSORS).use {
            assertTrue("not dropped", it.awaitDropped(10_000))
        }
    }

    @Test fun clientThatOnlySendsIsDroppedAfterHello() {
        val port = start()
        val hello = """{"jsonrpc":"2.0","id":1,"method":"hello","params":${hello(token)}}"""
        Flood(tlsSocket(port), port, first = hello, request = Flood.LIST_SENSORS).use {
            assertTrue("not dropped", it.awaitDropped(10_000))
        }
        // The relay slot is free again.
        connect(port).use { ws -> assertTrue("result" in ws.call(1, "hello", hello(token))) }
    }
}
