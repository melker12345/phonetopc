package io.github.melker12345.phonetopc.protocol

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
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assume.assumeFalse
import org.junit.Test
import org.junit.runner.RunWith
import org.junit.runners.Parameterized
import java.io.File

/** The shared conformance directory (spec/conformance), passed in by Gradle. */
internal fun conformanceDir(): File =
    File(System.getProperty("phonetopc.conformanceDir") ?: "../../spec/conformance")

/**
 * Runs every case of spec/conformance/server-cases.json (format: spec/conformance/README.md)
 * against [Session]. Binary frames never reach a [Session] (the transport handles them), so
 * `binary` cases are run by the :server module's ServerCasesCloseTest instead.
 */
@RunWith(Parameterized::class)
class ServerCasesTest(@Suppress("unused") private val name: String, private val case: JsonObject) {
    companion object {
        @JvmStatic
        @Parameterized.Parameters(name = "{0}")
        fun cases(): List<Array<Any>> {
            val cases = Json.parseToJsonElement(File(conformanceDir(), "server-cases.json").readText()).jsonArray
            check(cases.isNotEmpty())
            return cases.map { arrayOf(it.jsonObject["name"]!!.jsonPrimitive.content, it.jsonObject) }
        }

        private const val HELLO =
            """{"jsonrpc":"2.0","id":"conformance-hello","method":"hello","params":{"protocol":"phonetopc/1","client":{"name":"conformance","version":"1"}}}"""
        private const val PROBE = """{"jsonrpc":"2.0","id":"conformance-probe","method":"list_sensors"}"""
    }

    // The fixture the suite assumes: proximity unavailable, battery has a value, ui has none.
    private val src = FakeSensorSource().apply {
        unavailable += "proximity"
        latest["battery"] = Reading("battery", "state", Values.battery(82, true), "%", 1_000, 1_000_000_000)
    }
    private val hub = BridgeHub(
        source = src,
        serverInfo = ServerInfo("phonetopc-android", "test"),
        deviceInfoProvider = { DeviceInfo("dev1", "Test Phone", "acme", "X1", "android", "34") },
    )
    private val session = hub.openSession("conformance")

    /** Frames the session sent that haven't been consumed yet. */
    private val pending = ArrayDeque<JsonObject>()

    private fun sendText(text: String) = runBlocking {
        session.handleText(text)
        while (true) {
            val out = session.tryReceiveOutgoing() ?: break
            pending += Json.parseToJsonElement(out).jsonObject
        }
    }

    /** The next response, skipping notifications. */
    private fun nextResponse(where: String): JsonObject {
        while (true) {
            val m = pending.removeFirstOrNull() ?: throw AssertionError("$where: no response")
            if ("id" in m) return m
        }
    }

    @Test fun run() {
        val steps = (case["setup"]?.jsonArray ?: emptyList()).map { it.jsonObject } + case
        assumeFalse(
            "binary frames are handled by the transport (see :server)",
            steps.any { (it["send"] as? JsonObject)?.let { s -> s.size == 1 && "binary" in s } == true },
        )
        if (case["hello"]!!.jsonPrimitive.boolean) {
            sendText(HELLO)
            val r = nextResponse("hello")
            assertNull("hello failed: $r", r["error"])
            assertEquals(JsonPrimitive("conformance-hello"), r["id"])
        }
        steps.forEachIndexed { i, step ->
            runStep(if (i < steps.size - 1) "setup[$i]" else "send", step)
        }
    }

    private fun runStep(where: String, step: JsonObject) {
        val send = step["send"]!!
        val raw = (send as? JsonObject)?.takeIf { it.size == 1 }?.get("raw")
        sendText(raw?.jsonPrimitive?.content ?: send.toString())
        val expect = step["expect"]!!.jsonObject
        when {
            "close" in expect -> throw AssertionError("$where: close expected from a text frame")
            "no_reply" in expect -> {
                sendText(PROBE)
                val r = nextResponse(where)
                assertEquals("$where: want no reply, got $r", JsonPrimitive("conformance-probe"), r["id"])
            }
            "result" in expect -> {
                val r = nextResponse(where)
                assertNull("$where: want a result, got $r", r["error"])
                assertNotNull("$where: want a result, got $r", r["result"])
                assertEquals("$where: id", expect["id"], r["id"])
            }
            "error" in expect -> {
                val r = nextResponse(where)
                val error = r["error"]?.jsonObject ?: throw AssertionError("$where: want an error, got $r")
                assertEquals("$where: code in $r", expect["error"]!!.jsonPrimitive.int, error["code"]!!.jsonPrimitive.int)
                val data = error["data"]!!.jsonObject
                assertEquals("$where: data.name in $r", expect["name"], data["name"])
                assertEquals("$where: id", expect["id"], r["id"])
                expect["data"]?.jsonObject?.forEach { (k, v) -> assertEquals("$where: data.$k", v, data[k]) }
            }
            else -> throw AssertionError("$where: unknown expect form $expect")
        }
        // Nothing but events may be left over.
        assertTrue("$where: unexpected extra frames $pending", pending.none { "id" in it })
    }
}
