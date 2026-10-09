package io.github.melker12345.phonetopc.protocol

import io.github.melker12345.phonetopc.protocol.security.HelloRateLimiter
import io.github.melker12345.phonetopc.protocol.security.TokenHash
import kotlinx.coroutines.runBlocking
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.boolean
import kotlinx.serialization.json.int
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assume.assumeFalse
import org.junit.Test
import org.junit.runner.RunWith
import org.junit.runners.Parameterized
import java.io.File

/**
 * Runs spec/conformance/secure-server-cases.json (format: spec/conformance/README.md) against
 * a secure-mode [Session]. TLS, the pin and the hello timeout (`{"wait": true}`) belong to the
 * transport; :server's SecureServerCasesCloseTest runs those over real `wss://`.
 *
 * The failed-hello limit is relaxed, as the suite requires (at least 20 failures a minute).
 */
@RunWith(Parameterized::class)
class SecureServerCasesTest(@Suppress("unused") private val name: String, private val case: JsonObject) {
    companion object {
        private val suite by lazy {
            Json.parseToJsonElement(File(conformanceDir(), "secure-server-cases.json").readText()).jsonObject
        }

        @JvmStatic
        @Parameterized.Parameters(name = "{0}")
        fun cases(): List<Array<Any>> {
            val cases = suite["cases"]!!.jsonArray
            check(cases.isNotEmpty())
            return cases.map { arrayOf(it.jsonObject["name"]!!.jsonPrimitive.content, it.jsonObject) }
        }

        private const val PROBE = """{"jsonrpc":"2.0","id":"conformance-probe","method":"list_sensors"}"""

        /** The fixture's paired PCs, by token hash, as the app stores them. */
        fun fixturePcs(): Map<String, PcInfo> = suite["fixture"]!!.jsonObject["pcs"]!!.jsonArray.associate {
            val pc = it.jsonObject
            TokenHash.of(pc["token"]!!.jsonPrimitive.content) to
                PcInfo(pc["id"]!!.jsonPrimitive.content, pc["name"]!!.jsonPrimitive.content)
        }

        fun fixtureToken(): String =
            suite["fixture"]!!.jsonObject["pcs"]!!.jsonArray[0].jsonObject["token"]!!.jsonPrimitive.content
    }

    private val src = FakeSensorSource().apply {
        unavailable += "proximity"
        latest["battery"] = Reading("battery", "state", Values.battery(82, true), "%", 1_000, 1_000_000_000)
    }
    private val paired = fixturePcs()
    private val hub = BridgeHub(
        source = src,
        serverInfo = ServerInfo("phonetopc-android", "test"),
        deviceInfoProvider = { DeviceInfo("dev1", "Test Phone", "acme", "X1", "android", "34") },
        security = SecureAccess(
            authenticate = { t -> paired.entries.firstOrNull { TokenHash.matches(t, it.key) }?.value },
            limiter = HelloRateLimiter(maxFailures = 1_000),
        ),
    )
    private val session = hub.openSession("conformance", "127.0.0.1")
    private val pending = ArrayDeque<JsonObject>()

    private fun sendText(text: String) = runBlocking {
        session.handleText(text)
        while (true) {
            val out = session.tryReceiveOutgoing() ?: break
            pending += Json.parseToJsonElement(out).jsonObject
        }
    }

    private fun nextResponse(where: String): JsonObject {
        while (true) {
            val m = pending.removeFirstOrNull() ?: throw AssertionError("$where: no response")
            if ("id" in m) return m
        }
    }

    @Test fun run() {
        val send = case["send"]!!
        assumeFalse("the hello timeout is the transport's (see :server)", send is JsonObject && send["wait"] != null)
        assertNull("setup steps are not used by this suite", case["setup"])
        if (case["hello"]!!.jsonPrimitive.boolean) {
            sendText(
                """{"jsonrpc":"2.0","id":"conformance-hello","method":"hello","params":{"protocol":"phonetopc/1",""" +
                    """"client":{"name":"conformance","version":"1"},"auth":{"token":"${fixtureToken()}"}}}""",
            )
            val r = nextResponse("hello")
            assertNull("hello failed: $r", r["error"])
            assertEquals(JsonPrimitive("conformance-hello"), r["id"])
        }
        val raw = (send as? JsonObject)?.takeIf { it.size == 1 }?.get("raw")
        sendText(raw?.jsonPrimitive?.content ?: send.toString())
        val expect = case["expect"]!!.jsonObject
        when {
            "close" in expect -> throw AssertionError("close expected from a text frame")
            "no_reply" in expect -> {
                sendText(PROBE)
                val r = nextResponse("probe")
                assertEquals("want no reply, got $r", JsonPrimitive("conformance-probe"), r["id"])
                assertFalse(session.closeRequest.isCompleted)
            }
            "result" in expect -> {
                val r = nextResponse("send")
                assertNull("want a result, got $r", r["error"])
                val result = r["result"]?.jsonObject
                assertNotNull("want a result, got $r", result)
                assertEquals("id", expect["id"], r["id"])
                expect["result_has"]?.jsonObject?.forEach { (k, v) -> assertEquals("result.$k", v, result!![k]) }
            }
            "error" in expect -> {
                val r = nextResponse("send")
                val error = r["error"]?.jsonObject ?: throw AssertionError("want an error, got $r")
                assertEquals("code in $r", expect["error"]!!.jsonPrimitive.int, error["code"]!!.jsonPrimitive.int)
                val data = error["data"]!!.jsonObject
                assertEquals("data.name in $r", expect["name"], data["name"])
                assertEquals("id", expect["id"], r["id"])
                expect["data"]?.jsonObject?.forEach { (k, v) -> assertEquals("data.$k", v, data[k]) }
                val closeAfter = expect["close_after"]?.jsonPrimitive?.int
                if (closeAfter != null) {
                    assertTrue("close $closeAfter expected after $r", session.closeRequest.isCompleted)
                    assertEquals(closeAfter.toShort(), session.closeRequest.getCompleted().code)
                    // Nothing more is answered once the session is ending.
                    sendText(PROBE)
                } else {
                    assertFalse("unexpected close after $r", session.closeRequest.isCompleted)
                }
            }
            else -> throw AssertionError("unknown expect form $expect")
        }
        assertTrue("unexpected extra frames $pending", pending.none { "id" in it })
    }
}
