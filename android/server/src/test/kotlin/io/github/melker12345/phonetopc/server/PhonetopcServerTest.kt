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
import io.github.melker12345.phonetopc.protocol.Values
import io.ktor.client.HttpClient
import io.ktor.client.engine.cio.CIO
import io.ktor.client.plugins.websocket.DefaultClientWebSocketSession
import io.ktor.client.plugins.websocket.WebSockets
import io.ktor.client.plugins.websocket.webSocket
import io.ktor.websocket.Frame
import io.ktor.websocket.readText
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.long
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import java.util.concurrent.ConcurrentHashMap
import kotlin.time.Duration.Companion.seconds

class PhonetopcServerTest {
    private class Source : SensorSource {
        @Volatile var target: ReadingSink? = null
        val demand = ConcurrentHashMap<String, Demand>()
        override fun describe() = SensorCatalog.all.map { SensorInfo.of(it, true, 1.0, 200.0, 50.0) }
        override fun setDemand(source: String, demand: Demand?) {
            if (demand == null) this.demand.remove(source) else this.demand[source] = demand
        }
        override suspend fun read(source: String): Reading? = null
        override fun setSink(sink: ReadingSink?) { target = sink }
    }

    private val source = Source()
    private val hub = BridgeHub(source, ServerInfo("phonetopc-android", "test"), { DeviceInfo("dev1", "Phone", platform = "android") })
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

    private suspend fun DefaultClientWebSocketSession.next(): JsonObject {
        val frame = incoming.receive() as Frame.Text
        return Json.parseToJsonElement(frame.readText()).jsonObject
    }

    private suspend fun DefaultClientWebSocketSession.call(id: Int, method: String, params: String = "{}"): JsonObject {
        send(Frame.Text("""{"jsonrpc":"2.0","id":$id,"method":"$method","params":$params}"""))
        return next()
    }

    private fun ws(block: suspend DefaultClientWebSocketSession.() -> Unit) = runBlocking {
        withTimeout(10.seconds) {
            client.webSocket(host = "127.0.0.1", port = port, path = WS_PATH) { block() }
        }
    }

    @Test fun helloSubscribeAndReceiveEvents() = ws {
        val hello = call(1, "hello", """{"protocol":"phonetopc/1"}""")
        assertEquals("dev1", hello["result"]!!.jsonObject["device"]!!.jsonObject["id"]!!.jsonPrimitive.content)
        val sub = call(2, "subscribe", """{"source":"gyro","rate_hz":100}""")
        val id = sub["result"]!!.jsonObject["subscription"]!!.jsonPrimitive.content
        assertEquals(Demand(100.0), source.demand["gyro"])
        for (i in 0 until 3) {
            source.target!!.onReading(Reading("gyro", "rotation", Values.vec(0f, 0f, i.toFloat()), "rad/s", 1, i * 10_000_000L))
        }
        for (i in 0 until 3) {
            val ev = next()
            assertEquals("event", ev["method"]!!.jsonPrimitive.content)
            assertEquals(id, ev["params"]!!.jsonObject["subscription"]!!.jsonPrimitive.content)
            assertEquals(i.toLong(), ev["params"]!!.jsonObject["seq"]!!.jsonPrimitive.long)
        }
    }

    @Test fun binaryFrameClosesWith1003() = ws {
        send(Frame.Binary(true, byteArrayOf(1, 2, 3)))
        val reason = closeReason.await()
        assertEquals(1003.toShort(), reason?.code)
    }

    @Test fun parseErrorKeepsConnectionOpen() = ws {
        send(Frame.Text("nope"))
        assertEquals(-32700, next()["error"]!!.jsonObject["code"]!!.jsonPrimitive.content.toInt())
        val hello = call(1, "hello", """{"protocol":"phonetopc/1"}""")
        assertTrue("result" in hello)
    }

    @Test fun concurrentClientsAndCleanupOnDisconnect() = runBlocking {
        withTimeout(10.seconds) {
            client.webSocket(host = "127.0.0.1", port = port, path = WS_PATH) {
                call(1, "hello", """{"protocol":"phonetopc/1"}""")
                call(2, "subscribe", """{"source":"accelerometer","rate_hz":10}""")
                client.webSocket(host = "127.0.0.1", port = port, path = WS_PATH) {
                    call(1, "hello", """{"protocol":"phonetopc/1"}""")
                    call(2, "subscribe", """{"source":"accelerometer","rate_hz":150}""")
                    assertEquals(2, hub.clients.value.size)
                    assertEquals(Demand(150.0), source.demand["accelerometer"])
                }
                // Second client gone: demand falls back once the server notices.
                var tries = 0
                while (source.demand["accelerometer"] != Demand(10.0) && tries++ < 100) kotlinx.coroutines.delay(20)
                assertEquals(Demand(10.0), source.demand["accelerometer"])
                assertEquals(1, hub.clients.value.size)
            }
        }
    }
}
