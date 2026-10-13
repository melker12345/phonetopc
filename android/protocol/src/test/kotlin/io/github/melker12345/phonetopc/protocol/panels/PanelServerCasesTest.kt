package io.github.melker12345.phonetopc.protocol.panels

import io.github.melker12345.phonetopc.protocol.BridgeHub
import io.github.melker12345.phonetopc.protocol.DeviceInfo
import io.github.melker12345.phonetopc.protocol.FakeSensorSource
import io.github.melker12345.phonetopc.protocol.Reading
import io.github.melker12345.phonetopc.protocol.ServerInfo
import io.github.melker12345.phonetopc.protocol.Values
import io.github.melker12345.phonetopc.protocol.conformanceDir
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
import org.junit.Test
import org.junit.runner.RunWith
import org.junit.runners.Parameterized
import java.io.File

/** A [PcNotifier] that only records. */
class FakeNotifier : PcNotifier {
    val posted = mutableListOf<PcNotification>()
    val cancelled = mutableListOf<Pair<String, String>>()
    override fun post(notification: PcNotification) {
        posted += notification
    }

    override fun cancel(pc: PcRef, id: String) {
        cancelled += pc.key to id
    }
}

/**
 * Runs every case of spec/conformance/panel-server-cases.json (format: spec/conformance/README.md,
 * "Panel server cases") against a fresh dev-mode hub per case, with notifications and panels
 * allowed unless the case says `"permissions": "off"`.
 */
@RunWith(Parameterized::class)
class PanelServerCasesTest(@Suppress("unused") private val name: String, private val case: JsonObject) {
    companion object {
        @JvmStatic
        @Parameterized.Parameters(name = "{0}")
        fun cases(): List<Array<Any>> {
            val doc = Json.parseToJsonElement(File(conformanceDir(), "panel-server-cases.json").readText()).jsonObject
            val cases = doc["cases"]!!.jsonArray
            check(cases.isNotEmpty())
            return cases.map { arrayOf(it.jsonObject["name"]!!.jsonPrimitive.content, it.jsonObject) }
        }

        private const val HELLO =
            """{"jsonrpc":"2.0","id":"conformance-hello","method":"hello","params":{"protocol":"phonetopc/1","client":{"name":"conformance","version":"1"}}}"""
        private const val PROBE = """{"jsonrpc":"2.0","id":"conformance-probe","method":"list_sensors"}"""
    }

    private val allowed = case["permissions"]?.jsonPrimitive?.content != "off"
    private val permissions = OutputPermissions { _, _ -> allowed }
    private val src = FakeSensorSource().apply {
        unavailable += "proximity"
        latest["battery"] = Reading("battery", "state", Values.battery(82, true), "%", 1_000, 1_000_000_000)
    }
    private val hub = BridgeHub(
        source = src,
        serverInfo = ServerInfo("phonetopc-android", "test"),
        deviceInfoProvider = { DeviceInfo("dev1", "Test Phone", "acme", "X1", "android", "34") },
        notifications = NotificationService(permissions, FakeNotifier()),
        panels = PanelService(permissions),
        outputPermissions = permissions,
    )
    private val session = hub.openSession("conformance")
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
        val steps = (case["setup"]?.jsonArray ?: emptyList()).map { it.jsonObject } + case
        if (case["hello"]!!.jsonPrimitive.boolean) {
            sendText(HELLO)
            val r = nextResponse("hello")
            assertNull("hello failed: $r", r["error"])
        }
        steps.forEachIndexed { i, step -> runStep(if (i < steps.size - 1) "setup[$i]" else "send", step) }
    }

    private fun runStep(where: String, step: JsonObject) {
        sendText(step["send"]!!.toString())
        val expect = step["expect"]!!.jsonObject
        when {
            "no_reply" in expect -> {
                sendText(PROBE)
                assertEquals("$where: want no reply", JsonPrimitive("conformance-probe"), nextResponse(where)["id"])
            }
            "result" in expect -> {
                val r = nextResponse(where)
                assertNull("$where: want a result, got $r", r["error"])
                val result = r["result"]
                assertNotNull("$where: want a result, got $r", result)
                assertEquals("$where: id", expect["id"], r["id"])
                expect["result_has"]?.jsonObject?.forEach { (k, v) ->
                    assertEquals("$where: result.$k in $r", v, result!!.jsonObject[k])
                }
                expect["result_capabilities"]?.jsonArray?.forEach { c ->
                    assertTrue("$where: capability $c in $r", c in result!!.jsonObject["capabilities"]!!.jsonArray)
                }
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
        assertTrue("$where: unexpected extra frames $pending", pending.none { "id" in it })
    }
}
