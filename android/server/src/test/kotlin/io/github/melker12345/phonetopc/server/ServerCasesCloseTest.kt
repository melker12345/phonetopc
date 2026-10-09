package io.github.melker12345.phonetopc.server

import io.github.melker12345.phonetopc.protocol.BridgeHub
import io.github.melker12345.phonetopc.protocol.Demand
import io.github.melker12345.phonetopc.protocol.DeviceInfo
import io.github.melker12345.phonetopc.protocol.Reading
import io.github.melker12345.phonetopc.protocol.ReadingSink
import io.github.melker12345.phonetopc.protocol.SensorCatalog
import io.github.melker12345.phonetopc.protocol.SensorInfo
import io.github.melker12345.phonetopc.protocol.SensorSource
import io.github.melker12345.phonetopc.protocol.ServerInfo
import io.ktor.client.HttpClient
import io.ktor.client.engine.cio.CIO
import io.ktor.client.plugins.websocket.WebSockets
import io.ktor.client.plugins.websocket.webSocket
import io.ktor.websocket.Frame
import io.ktor.websocket.readText
import kotlinx.coroutines.channels.ClosedReceiveChannelException
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
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
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.junit.runners.Parameterized
import java.io.File
import java.util.Base64
import kotlin.time.Duration.Companion.seconds

/**
 * Runs the cases of spec/conformance/server-cases.json that expect a WebSocket close (binary
 * frames) through the real transport. Everything else is covered against the Session by
 * :protocol's ServerCasesTest.
 */
@RunWith(Parameterized::class)
class ServerCasesCloseTest(@Suppress("unused") private val name: String, private val case: JsonObject) {
    companion object {
        @JvmStatic
        @Parameterized.Parameters(name = "{0}")
        fun cases(): List<Array<Any>> {
            val dir = File(System.getProperty("phonetopc.conformanceDir") ?: "../../spec/conformance")
            val cases = Json.parseToJsonElement(File(dir, "server-cases.json").readText()).jsonArray
                .map { it.jsonObject }
                .filter { "close" in it["expect"]!!.jsonObject }
            check(cases.isNotEmpty())
            return cases.map { arrayOf(it["name"]!!.jsonPrimitive.content, it) }
        }

        private const val HELLO =
            """{"jsonrpc":"2.0","id":"conformance-hello","method":"hello","params":{"protocol":"phonetopc/1","client":{"name":"conformance","version":"1"}}}"""
    }

    private class Source : SensorSource {
        override fun describe() = SensorCatalog.all.map { SensorInfo.of(it, it.source != "proximity", 1.0, 200.0, 50.0) }
        override fun setDemand(source: String, demand: Demand?) = Unit
        override suspend fun read(source: String): Reading? = null
        override fun currentReading(source: String): Reading? = null
        override fun setSink(sink: ReadingSink?) = Unit
    }

    private val hub = BridgeHub(Source(), ServerInfo("phonetopc-android", "test"), { DeviceInfo("dev1", "Phone", platform = "android") })
    private val server = PhonetopcServer(hub, port = 0, host = "127.0.0.1")
    private val client = HttpClient(CIO) { install(WebSockets) }
    private var port = 0

    @Before fun setUp() = runBlocking {
        server.start()
        port = server.boundPort()
    }

    @After fun tearDown() {
        client.close()
        server.stop()
    }

    @Test fun run() = runBlocking {
        assertNull("setup steps are not supported here", case["setup"])
        val send = case["send"]!!.jsonObject
        assertEquals("only binary frames are expected to close", setOf("binary"), send.keys)
        val bytes = Base64.getDecoder().decode(send["binary"]!!.jsonPrimitive.content)
        val wantCode = case["expect"]!!.jsonObject["close"]!!.jsonPrimitive.int
        withTimeout(10.seconds) {
            client.webSocket(host = "127.0.0.1", port = port, path = WS_PATH) {
                if (case["hello"]!!.jsonPrimitive.boolean) {
                    send(Frame.Text(HELLO))
                    val r = Json.parseToJsonElement((incoming.receive() as Frame.Text).readText()).jsonObject
                    assertTrue("hello failed: $r", "result" in r)
                }
                send(Frame.Binary(true, bytes))
                // No response may come before the close.
                try {
                    while (true) {
                        val frame = incoming.receive()
                        if (frame is Frame.Text) {
                            val m = Json.parseToJsonElement(frame.readText()).jsonObject
                            assertTrue("unexpected response before close: $m", "id" !in m)
                        }
                    }
                } catch (_: ClosedReceiveChannelException) {
                }
                assertEquals(wantCode.toShort(), closeReason.await()?.code)
            }
        }
    }
}
