package io.github.melker12345.phonetopc.server

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
import io.github.melker12345.phonetopc.protocol.security.HelloRateLimiter
import io.github.melker12345.phonetopc.protocol.security.Pem
import io.github.melker12345.phonetopc.protocol.security.Pins
import io.github.melker12345.phonetopc.protocol.security.TokenHash
import kotlinx.coroutines.runBlocking
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.boolean
import kotlinx.serialization.json.int
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.junit.runners.Parameterized
import java.io.File
import kotlin.time.Duration.Companion.seconds

/**
 * The cases of spec/conformance/secure-server-cases.json that end in a WebSocket close
 * (`close`, `close_after`), run over real pinned `wss://` against the fixture key. The rest is
 * covered against the Session by :protocol's SecureServerCasesTest.
 */
@RunWith(Parameterized::class)
class SecureServerCasesCloseTest(@Suppress("unused") private val name: String, private val case: JsonObject) {
    companion object {
        private val dir = File(System.getProperty("phonetopc.conformanceDir") ?: "../../spec/conformance")
        private val suite by lazy { Json.parseToJsonElement(File(dir, "secure-server-cases.json").readText()).jsonObject }
        private val fixture get() = suite["fixture"]!!.jsonObject

        @JvmStatic
        @Parameterized.Parameters(name = "{0}")
        fun cases(): List<Array<Any>> {
            val cases = suite["cases"]!!.jsonArray.map { it.jsonObject }.filter {
                val expect = it["expect"]!!.jsonObject
                "close" in expect || "close_after" in expect
            }
            check(cases.isNotEmpty())
            return cases.map { arrayOf(it["name"]!!.jsonPrimitive.content, it) }
        }
    }

    private class Source : SensorSource {
        override fun describe() = SensorCatalog.all.map { SensorInfo.of(it, it.source != "proximity", 1.0, 200.0, 50.0) }
        override fun setDemand(source: String, demand: Demand?) = Unit
        override suspend fun read(source: String): Reading? = null
        override fun setSink(sink: ReadingSink?) = Unit
    }

    private val pin = fixture["pin"]!!.jsonPrimitive.content
    private val pcs = fixture["pcs"]!!.jsonArray.associate {
        val pc = it.jsonObject
        TokenHash.of(pc["token"]!!.jsonPrimitive.content) to PcInfo(pc["id"]!!.jsonPrimitive.content, pc["name"]!!.jsonPrimitive.content)
    }
    private val token = fixture["pcs"]!!.jsonArray[0].jsonObject["token"]!!.jsonPrimitive.content
    private val identity = TlsIdentity(
        Pem.ecPrivateKey(File(dir, fixture["key"]!!.jsonPrimitive.content).readText()),
        Pins.parsePemCertificate(File(dir, "vectors/phone-cert.pem").readText()),
    )
    private val hub = BridgeHub(
        Source(), ServerInfo("phonetopc-android", "test"), { DeviceInfo("dev1", "Phone", platform = "android") },
        security = SecureAccess(
            authenticate = { t -> pcs.entries.firstOrNull { TokenHash.matches(t, it.key) }?.value },
            limiter = HelloRateLimiter(maxFailures = 1_000),
        ),
    )

    // A short hello timeout keeps no_hello_timeout fast; the case only needs *some* timeout.
    private val server = PhonetopcServer(hub, port = 0, host = "127.0.0.1", tls = identity, helloTimeout = 1.seconds)

    @After fun tearDown() = server.stop()

    @Test fun run() {
        server.start()
        val port = runBlocking { server.boundPort() }
        PinnedWebSocket.connect("127.0.0.1", port, WS_PATH, pin, readTimeoutMillis = 15_000).use { ws ->
            assertEquals(pin, ws.pinSeen)
            if (case["hello"]!!.jsonPrimitive.boolean) {
                ws.send(
                    """{"jsonrpc":"2.0","id":"conformance-hello","method":"hello","params":{"protocol":"phonetopc/1",""" +
                        """"client":{"name":"conformance","version":"1"},"auth":{"token":"$token"}}}""",
                )
                assertTrue("result" in Json.parseToJsonElement(ws.receive()!!).jsonObject)
            }
            val send = case["send"]!!
            if (!(send is JsonObject && send["wait"] != null)) ws.send(send.toString())
            val expect = case["expect"]!!.jsonObject
            val closeAfter = expect["close_after"]?.jsonPrimitive?.int
            if (closeAfter != null) {
                val r = Json.parseToJsonElement(ws.receive()!!).jsonObject
                assertEquals(expect["error"]!!.jsonPrimitive.int, r["error"]!!.jsonObject["code"]!!.jsonPrimitive.int)
                assertNull("nothing may follow the error", ws.receive())
                assertEquals(closeAfter, ws.closeCode)
            } else {
                assertNull("no response may come before the close", ws.receive())
                assertEquals(expect["close"]!!.jsonPrimitive.int, ws.closeCode)
            }
        }
    }
}
