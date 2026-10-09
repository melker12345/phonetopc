package io.github.melker12345.phonetopc.protocol

import kotlinx.coroutines.runBlocking
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.double
import kotlinx.serialization.json.int
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.long
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class SessionTest {
    private val src = FakeSensorSource()
    private val hub = BridgeHub(
        source = src,
        serverInfo = ServerInfo("phonetopc-android", "0.1.0"),
        deviceInfoProvider = { DeviceInfo("dev1", "Test Phone", "acme", "X1", "android", "34") },
        continuousQueueCapacity = 8,
        onChangeQueueCapacity = 4,
    )

    // ---- helpers ----

    private fun Session.drain(): List<JsonObject> =
        generateSequence { tryReceiveOutgoing() }.map { ProtocolJson.parseToJsonElement(it).jsonObject }.toList()

    private fun Session.send(text: String): List<JsonObject> = runBlocking {
        handleText(text)
        drain()
    }

    private fun Session.rpc(method: String, params: String? = null, id: Any = 1): JsonObject {
        val idJson = if (id is String) "\"$id\"" else id.toString()
        val p = params?.let { ",\"params\":$it" } ?: ""
        val out = send("""{"jsonrpc":"2.0","id":$idJson,"method":"$method"$p}""")
        assertEquals("exactly one response expected: $out", 1, out.size)
        return out.single()
    }

    private fun JsonObject.errorCode(): Int? = this["error"]?.jsonObject?.get("code")?.jsonPrimitive?.int
    private fun JsonObject.errorName(): String? =
        this["error"]?.jsonObject?.get("data")?.jsonObject?.get("name")?.jsonPrimitive?.content
    private fun JsonObject.result(): JsonObject = this["result"]!!.jsonObject

    private fun helloSession(): Session = hub.openSession("test").also {
        val r = it.rpc("hello", """{"protocol":"phonetopc/1","client":{"name":"t","version":"1"}}""")
        assertNotNull(r["result"])
    }

    private fun Session.subscribe(params: String): String =
        rpc("subscribe", params).result()["subscription"]!!.jsonPrimitive.content

    private fun events(frames: List<JsonObject>) = frames.filter { it["method"]?.jsonPrimitive?.content == "event" }
        .map { it["params"]!!.jsonObject }

    // ---- framing ----

    @Test fun parseErrorHasNullId() {
        val s = hub.openSession("t")
        val r = s.send("{not json").single()
        assertEquals(-32700, r.errorCode())
        assertEquals(JsonNull, r["id"])
        assertEquals("parse_error", r.errorName())
        // Unquoted words are not JSON even though kotlinx's tree parser tolerates them.
        assertEquals(-32700, s.send("nope").single().errorCode())
        assertEquals(-32700, s.send("""{"jsonrpc":"2.0","id":1,"method":hello}""").single().errorCode())
        assertEquals(-32700, s.send("").single().errorCode())
    }

    @Test fun batchArrayIsInvalidRequest() {
        val s = helloSession()
        val r = s.send("""[{"jsonrpc":"2.0","id":1,"method":"list_sensors"}]""").single()
        assertEquals(-32600, r.errorCode())
        assertEquals(JsonNull, r["id"])
    }

    @Test fun nonObjectAndMissingFieldsAreInvalidRequest() {
        val s = helloSession()
        assertEquals(-32600, s.send("42").single().errorCode())
        assertEquals(-32600, s.send("""{"jsonrpc":"2.0","id":7}""").single().errorCode())
        assertEquals(-32600, s.send("""{"jsonrpc":"1.0","id":7,"method":"list_sensors"}""").single().errorCode())
        assertEquals(-32600, s.send("""{"jsonrpc":"2.0","id":true,"method":"list_sensors"}""").single().errorCode())
        val withId = s.send("""{"id":9,"method":"list_sensors"}""").single()
        assertEquals(-32600, withId.errorCode())
        assertEquals(JsonPrimitive(9), withId["id"])
    }

    @Test fun notificationsGetNoResponse() {
        val s = helloSession()
        assertTrue(s.send("""{"jsonrpc":"2.0","method":"list_sensors"}""").isEmpty())
        // Ignored whatever the method or params, and without side effects.
        assertTrue(s.send("""{"jsonrpc":"2.0","method":"no_such_method"}""").isEmpty())
        assertTrue(s.send("""{"jsonrpc":"2.0","method":"subscribe","params":"gyro"}""").isEmpty())
        assertTrue(s.send("""{"jsonrpc":"2.0","method":"subscribe","params":{"source":"gyro"}}""").isEmpty())
        assertTrue(src.current.isEmpty())
        // Also before hello.
        assertTrue(hub.openSession("t").send("""{"jsonrpc":"2.0","method":"list_sensors"}""").isEmpty())
    }

    @Test fun paramsMustBeObject() {
        val s = helloSession()
        for (p in listOf("""["gyro"]""", "[]", "\"gyro\"", "42", "true")) {
            val r = s.rpc("subscribe", p)
            assertEquals(p, -32602, r.errorCode())
            assertEquals("invalid_params", r.errorName())
            assertEquals(JsonPrimitive(1), r["id"])
        }
        // A non-object params is -32602 even for methods that take none.
        assertEquals(-32602, s.rpc("list_sensors", "7").errorCode())
    }

    @Test fun nullParamsAreAbsent() {
        val s = helloSession()
        assertNotNull(s.rpc("list_sensors", "null")["result"])
        // subscribe with params:null is the same as no params: source is missing.
        val r = s.rpc("subscribe", "null")
        assertEquals(-32602, r.errorCode())
        assertTrue(r["error"]!!.jsonObject["message"]!!.jsonPrimitive.content.contains("source"))
        assertEquals(-32602, hub.openSession("t").rpc("hello", "null").errorCode())
    }

    @Test fun idMustBeIntegerOrString() {
        val s = helloSession()
        assertEquals(JsonPrimitive(0), s.rpc("list_sensors", id = 0)["id"])
        assertEquals(JsonPrimitive(-5), s.rpc("list_sensors", id = -5)["id"])
        for (bad in listOf("1.5", "1e3", "1.0", "null", "[1]", "{}")) {
            val r = s.send("""{"jsonrpc":"2.0","id":$bad,"method":"list_sensors"}""").single()
            assertEquals(bad, -32600, r.errorCode())
            assertEquals("invalid_request", r.errorName())
            assertEquals(JsonNull, r["id"])
        }
    }

    @Test fun standardErrorNames() {
        val s = helloSession()
        assertEquals("parse_error", s.send("{").single().errorName())
        assertEquals("invalid_request", s.send("[]").single().errorName())
        assertEquals("method_not_found", s.rpc("nope").errorName())
        assertEquals("invalid_params", s.rpc("get", "{}").errorName())
        assertEquals("internal_error", RpcException(RpcError.INTERNAL_ERROR).toErrorJson()["data"]!!.jsonObject["name"]!!.jsonPrimitive.content)
    }

    @Test fun stringIdIsEchoed() {
        val s = helloSession()
        val r = s.rpc("list_sensors", id = "abc")
        assertEquals(JsonPrimitive("abc"), r["id"])
    }

    // ---- hello ----

    @Test fun requestsBeforeHelloAreRejected() {
        val s = hub.openSession("t")
        for (m in listOf("list_sensors", "subscribe", "get", "unsubscribe", "no_such_method")) {
            val r = s.rpc(m, """{"source":"gyro"}""")
            assertEquals(m, -32003, r.errorCode())
            assertEquals("hello_required", r.errorName())
        }
    }

    @Test fun helloReturnsServerAndDevice() {
        val s = hub.openSession("t")
        val r = s.rpc("hello", """{"protocol":"phonetopc/1","client":{"name":"py","version":"0.1"},"extra":1}""").result()
        assertEquals("phonetopc/1", r["protocol"]!!.jsonPrimitive.content)
        assertEquals("phonetopc-android", r["server"]!!.jsonObject["name"]!!.jsonPrimitive.content)
        val device = r["device"]!!.jsonObject
        assertEquals("dev1", device["id"]!!.jsonPrimitive.content)
        assertEquals("android", device["platform"]!!.jsonPrimitive.content)
        assertEquals("34", device["platform_version"]!!.jsonPrimitive.content)
        assertEquals(listOf("events", "get"), r["capabilities"]!!.jsonArray.map { it.jsonPrimitive.content })
        assertEquals("py", hub.clients.value.single().clientName)
    }

    @Test fun unsupportedProtocolListsSupported() {
        val s = hub.openSession("t")
        val r = s.rpc("hello", """{"protocol":"phonetopc/9"}""")
        assertEquals(-32004, r.errorCode())
        val data = r["error"]!!.jsonObject["data"]!!.jsonObject
        assertEquals("unsupported_protocol", data["name"]!!.jsonPrimitive.content)
        assertEquals(listOf("phonetopc/1"), data["supported"]!!.jsonArray.map { it.jsonPrimitive.content })
        // Failed hello doesn't unlock the session.
        assertEquals(-32003, s.rpc("list_sensors").errorCode())
    }

    @Test fun helloWithoutProtocolIsInvalidParams() {
        val s = hub.openSession("t")
        assertEquals(-32602, s.rpc("hello", "{}").errorCode())
        assertEquals(-32602, s.rpc("hello").errorCode())
    }

    @Test fun unknownMethodAfterHello() {
        val s = helloSession()
        val r = s.rpc("command")
        assertEquals(-32601, r.errorCode())
        assertEquals("method_not_found", r.errorName())
    }

    // ---- list_sensors ----

    @Test fun listSensorsHasCatalogAndRatesOnlyForContinuous() {
        src.unavailable += SensorCatalog.LIGHT
        val s = helloSession()
        val sensors = s.rpc("list_sensors").result()["sensors"]!!.jsonArray.map { it.jsonObject }
        assertEquals(SensorCatalog.all.map { it.source }, sensors.map { it["source"]!!.jsonPrimitive.content })
        val gyro = sensors.first { it["source"]!!.jsonPrimitive.content == "gyro" }
        assertEquals("continuous", gyro["mode"]!!.jsonPrimitive.content)
        assertEquals("vec3", gyro["shape"]!!.jsonPrimitive.content)
        assertEquals(200.0, gyro["max_rate_hz"]!!.jsonPrimitive.double, 0.0)
        val prox = sensors.first { it["source"]!!.jsonPrimitive.content == "proximity" }
        assertEquals("on_change", prox["mode"]!!.jsonPrimitive.content)
        assertFalse("min_rate_hz" in prox)
        assertFalse("max_rate_hz" in prox)
        assertFalse("default_rate_hz" in prox)
        val light = sensors.first { it["source"]!!.jsonPrimitive.content == "light" }
        assertFalse(light["available"]!!.jsonPrimitive.content.toBoolean())
        val quat = sensors.first { it["source"]!!.jsonPrimitive.content == "game_rotation_vector" }
        assertEquals("", quat["unit"]!!.jsonPrimitive.content)
    }

    // ---- subscribe ----

    @Test fun subscribeErrors() {
        val s = helloSession()
        assertEquals(-32001, s.rpc("subscribe", """{"source":"thermometer"}""").errorCode())
        src.unavailable += SensorCatalog.PROXIMITY
        assertEquals(-32002, s.rpc("subscribe", """{"source":"proximity"}""").errorCode())
        assertEquals(-32602, s.rpc("subscribe", """{"source":"gyro","type":"motion"}""").errorCode())
        assertEquals(-32602, s.rpc("subscribe", """{"source":"gyro","rate_hz":0}""").errorCode())
        assertEquals(-32602, s.rpc("subscribe", """{"source":"gyro","rate_hz":"fast"}""").errorCode())
        assertEquals(-32602, s.rpc("subscribe", """{"source":"gyro","min_delta":-1}""").errorCode())
        assertEquals(-32602, s.rpc("subscribe", "{}").errorCode())
        assertEquals(-32602, s.rpc("subscribe", """{"source":5}""").errorCode())
        assertEquals(-32602, s.rpc("subscribe").errorCode())
        assertTrue(src.current.isEmpty())
    }

    @Test fun subscribeDefaultsAndClampsRate() {
        val s = helloSession()
        val def = s.rpc("subscribe", """{"source":"gyro"}""").result()
        assertEquals(50.0, def["rate_hz"]!!.jsonPrimitive.double, 0.0)
        assertEquals(listOf("rotation"), def["types"]!!.jsonArray.map { it.jsonPrimitive.content })
        assertEquals("gyro", def["source"]!!.jsonPrimitive.content)
        assertEquals("continuous", def["mode"]!!.jsonPrimitive.content)
        assertTrue(def["subscription"]!!.jsonPrimitive.content.startsWith("s-"))
        assertEquals(200.0, s.rpc("subscribe", """{"source":"gyro","rate_hz":5000}""").result()["rate_hz"]!!.jsonPrimitive.double, 0.0)
        assertEquals(1.0, s.rpc("subscribe", """{"source":"gyro","rate_hz":0.1}""").result()["rate_hz"]!!.jsonPrimitive.double, 0.0)
        assertEquals(100.0, s.rpc("subscribe", """{"source":"gyro","rate_hz":100,"batch_ms":10,"x":1}""").result()["rate_hz"]!!.jsonPrimitive.double, 0.0)
    }

    @Test fun onChangeSubscriptionHasNoRate() {
        val s = helloSession()
        val r = s.rpc("subscribe", """{"source":"battery","rate_hz":10}""").result()
        assertFalse("rate_hz" in r)
        assertEquals("on_change", r["mode"]!!.jsonPrimitive.content)
        assertEquals(listOf("subscription", "source", "types", "mode"), r.keys.toList())
        assertEquals(Demand(null), src.current["battery"])
    }

    @Test fun eachSubscriptionGetsOwnIdAndSeq() {
        val s = helloSession()
        val a = s.subscribe("""{"source":"accelerometer","rate_hz":200}""")
        val b = s.subscribe("""{"source":"accelerometer","rate_hz":200}""")
        assertTrue(a != b)
        src.emitVec("accelerometer", "motion", 1_000_000_000, 0f, 0f, 9.8f)
        src.emitVec("accelerometer", "motion", 1_005_000_000, 0f, 0f, 9.8f)
        val evs = events(s.drain())
        assertEquals(4, evs.size)
        for (sub in listOf(a, b)) {
            val mine = evs.filter { it["subscription"]!!.jsonPrimitive.content == sub }
            assertEquals(listOf(0L, 1L), mine.map { it["seq"]!!.jsonPrimitive.long })
        }
    }

    @Test fun eventEnvelope() {
        val s = helloSession()
        val sub = s.subscribe("""{"source":"gyro","rate_hz":100}""")
        src.emit(Reading("gyro", "rotation", Values.vec(0.01f, -0.32f, 0.05f), "rad/s", 1760000000123, 81234567890123))
        val frame = s.drain().single()
        assertEquals("2.0", frame["jsonrpc"]!!.jsonPrimitive.content)
        assertFalse("id" in frame)
        val e = frame["params"]!!.jsonObject
        assertEquals(sub, e["subscription"]!!.jsonPrimitive.content)
        assertEquals(0L, e["seq"]!!.jsonPrimitive.long)
        assertEquals("gyro", e["source"]!!.jsonPrimitive.content)
        assertEquals("rotation", e["type"]!!.jsonPrimitive.content)
        assertEquals("[0.01,-0.32,0.05]", e["value"].toString())
        assertEquals("rad/s", e["unit"]!!.jsonPrimitive.content)
        assertEquals(1760000000123, e["ts"]!!.jsonPrimitive.long)
        assertEquals(81234567890123, e["ts_mono_ns"]!!.jsonPrimitive.long)
        assertEquals("dev1", e["device"]!!.jsonPrimitive.content)
    }

    @Test fun typeFilter() {
        val s = helloSession()
        s.subscribe("""{"source":"gyro","type":"rotation"}""")
        src.emitVec("gyro", "something_else", 1, 1f, 1f, 1f)
        assertTrue(s.drain().isEmpty())
        src.emitVec("gyro", "rotation", 2, 1f, 1f, 1f)
        assertEquals(1, s.drain().size)
    }

    @Test fun minDeltaNumber() {
        val s = helloSession()
        s.subscribe("""{"source":"light","min_delta":10}""")
        val values = listOf(100f, 105f, 109.9f, 111f, 115f, 90f)
        values.forEachIndexed { i, v -> src.emitNumber("light", "illuminance", i.toLong(), v) }
        val sent = events(s.drain()).map { it["value"]!!.jsonPrimitive.double }
        assertEquals(listOf(100.0, 111.0, 90.0), sent)
    }

    @Test fun minDeltaVectorIsEuclidean() {
        val s = helloSession()
        s.subscribe("""{"source":"gyro","rate_hz":200,"min_delta":0.5}""")
        var t = 0L
        fun e(x: Float, y: Float, z: Float) { t += 5_000_000; src.emitVec("gyro", "rotation", t, x, y, z) }
        e(0f, 0f, 0f) // sent
        e(0.3f, 0.3f, 0f) // dist 0.424 -> suppressed
        e(0.3f, 0.3f, 0.3f) // dist from (0,0,0) = 0.52 -> sent
        e(0.6f, 0.3f, 0.3f) // 0.3 -> suppressed
        val sent = events(s.drain())
        assertEquals(2, sent.size)
        assertEquals(listOf(0L, 1L), sent.map { it["seq"]!!.jsonPrimitive.long })
    }

    @Test fun minDeltaIgnoredForObjects() {
        val s = helloSession()
        s.subscribe("""{"source":"battery","min_delta":50}""")
        src.emit(Reading("battery", "state", Values.battery(80, false), "%", 1, 1))
        src.emit(Reading("battery", "state", Values.battery(81, false), "%", 2, 2))
        assertEquals(2, events(s.drain()).size)
    }

    @Test fun downsamplesToSubscriptionRate() {
        val s = helloSession()
        val fast = s.subscribe("""{"source":"gyro","rate_hz":200}""")
        val slow = s.subscribe("""{"source":"gyro","rate_hz":50}""")
        assertEquals(Demand(200.0), src.current["gyro"])
        // 1 s of 200 Hz samples with +-0.5 ms jitter, drained as we go (queue capacity is 8 here).
        val evs = (0 until 200).flatMap { i ->
            val jitter = if (i % 2 == 0) 500_000L else -500_000L
            src.emitVec("gyro", "rotation", 1_000_000_000L + i * 5_000_000L + jitter, 0f, 0f, i.toFloat())
            events(s.drain())
        }
        val fastCount = evs.count { it["subscription"]!!.jsonPrimitive.content == fast }
        val slowCount = evs.count { it["subscription"]!!.jsonPrimitive.content == slow }
        assertEquals(200, fastCount)
        assertTrue("slow got $slowCount", slowCount in 49..51)
    }

    @Test fun downsamplerReanchorsAfterGap() {
        val d = Downsampler(10.0) // 100 ms
        assertTrue(d.isDue(0)); d.emitted(0)
        assertFalse(d.isDue(50_000_000))
        assertTrue(d.isDue(100_000_000)); d.emitted(100_000_000)
        // 5 s gap: emit once, not a burst.
        assertTrue(d.isDue(5_000_000_000)); d.emitted(5_000_000_000)
        assertFalse(d.isDue(5_010_000_000))
        assertTrue(d.isDue(5_100_000_000))
    }

    // ---- demand ----

    @Test fun demandIsMaxAcrossSessionsAndReleased() {
        val s1 = helloSession()
        val s2 = helloSession()
        val a = s1.subscribe("""{"source":"gyro","rate_hz":20}""")
        assertEquals(Demand(20.0), src.current["gyro"])
        s2.subscribe("""{"source":"gyro","rate_hz":120}""")
        assertEquals(Demand(120.0), src.current["gyro"])
        s2.close()
        assertEquals(Demand(20.0), src.current["gyro"])
        s1.rpc("unsubscribe", """{"subscription":"$a"}""")
        assertNull(src.current["gyro"])
        assertEquals(emptyMap<String, Demand>(), hub.demand.value)
    }

    @Test fun disablingSourceStopsDemandAndEvents() {
        val s = helloSession()
        s.subscribe("""{"source":"gyro"}""")
        src.unavailable += "gyro"
        hub.refreshAvailability()
        assertNull(src.current["gyro"])
        src.emitVec("gyro", "rotation", 1, 0f, 0f, 0f)
        assertTrue(s.drain().isEmpty())
        src.unavailable -= "gyro"
        hub.refreshAvailability()
        assertEquals(Demand(50.0), src.current["gyro"])
    }

    @Test fun pauseStopsSourcesAndEvents() {
        val s = helloSession()
        s.subscribe("""{"source":"gyro"}""")
        hub.setPaused(true)
        assertTrue(src.current.isEmpty())
        src.emitVec("gyro", "rotation", 1, 0f, 0f, 0f)
        assertTrue(s.drain().isEmpty())
        hub.setPaused(false)
        assertEquals(Demand(50.0), src.current["gyro"])
    }

    // ---- unsubscribe ----

    @Test fun unsubscribe() {
        val s = helloSession()
        val sub = s.subscribe("""{"source":"gyro"}""")
        val r = s.rpc("unsubscribe", """{"subscription":"$sub"}""")
        assertEquals(JsonObject(emptyMap()), r.result())
        src.emitVec("gyro", "rotation", 1, 0f, 0f, 0f)
        assertTrue(s.drain().isEmpty())
        val again = s.rpc("unsubscribe", """{"subscription":"$sub"}""")
        assertEquals(-32005, again.errorCode())
        assertEquals("unknown_subscription", again.errorName())
    }

    @Test fun cannotUnsubscribeOtherSessionsSubscription() {
        val s1 = helloSession()
        val s2 = helloSession()
        val sub = s1.subscribe("""{"source":"gyro"}""")
        assertEquals(-32005, s2.rpc("unsubscribe", """{"subscription":"$sub"}""").errorCode())
        assertEquals(Demand(50.0), src.current["gyro"])
    }

    @Test fun eventsQueuedBeforeUnsubscribeComeFirst() {
        val s = helloSession()
        val sub = s.subscribe("""{"source":"gyro"}""")
        src.emitVec("gyro", "rotation", 1, 0f, 0f, 0f)
        val frames = s.send("""{"jsonrpc":"2.0","id":"u","method":"unsubscribe","params":{"subscription":"$sub"}}""")
        assertEquals(2, frames.size)
        assertEquals("event", frames[0]["method"]!!.jsonPrimitive.content)
        assertEquals(JsonPrimitive("u"), frames[1]["id"])
    }

    // ---- get ----

    @Test fun getReturnsLatestOrNoValue() {
        val s = helloSession()
        val none = s.rpc("get", """{"source":"battery"}""")
        assertEquals(-32006, none.errorCode())
        assertEquals("no_value", none.errorName())
        src.latest["battery"] = Reading("battery", "state", Values.battery(82, true), "%", 1760000000123, 81234567890123)
        val r = s.rpc("get", """{"source":"battery"}""").result()
        assertEquals("battery", r["source"]!!.jsonPrimitive.content)
        assertEquals("state", r["type"]!!.jsonPrimitive.content)
        assertEquals(82, r["value"]!!.jsonObject["level"]!!.jsonPrimitive.int)
        assertEquals("dev1", r["device"]!!.jsonPrimitive.content)
        assertFalse("subscription" in r)
        assertFalse("seq" in r)
    }

    @Test fun getErrors() {
        val s = helloSession()
        assertEquals(-32001, s.rpc("get", """{"source":"nope"}""").errorCode())
        assertEquals(-32602, s.rpc("get", "{}").errorCode())
        assertEquals(-32602, s.rpc("get", """{"source":"battery","type":"nope"}""").errorCode())
        src.unavailable += "battery"
        assertEquals(-32002, s.rpc("get", """{"source":"battery"}""").errorCode())
    }

    // ---- backpressure ----

    @Test fun continuousEventsAreDroppedWhenQueueFull() {
        val s = helloSession()
        s.subscribe("""{"source":"gyro","rate_hz":200}""")
        for (i in 0 until 20) src.emitVec("gyro", "rotation", i * 5_000_000L, 0f, 0f, i.toFloat())
        val evs = events(s.drain())
        assertEquals(8, evs.size) // capacity
        assertEquals((0L until 8L).toList(), evs.map { it["seq"]!!.jsonPrimitive.long })
        assertEquals(12L, s.droppedEvents.get())
        // After draining, delivery resumes; the gap in seq shows the drop.
        src.emitVec("gyro", "rotation", 100 * 5_000_000L, 0f, 0f, 0f)
        assertEquals(20L, events(s.drain()).single()["seq"]!!.jsonPrimitive.long)
        assertFalse(s.closeRequest.isCompleted)
    }

    @Test fun onChangeOverflowRequestsClose1008() {
        val s = helloSession()
        s.subscribe("""{"source":"light"}""")
        for (i in 0 until 4) src.emitNumber("light", "illuminance", i.toLong(), i.toFloat())
        assertFalse(s.closeRequest.isCompleted)
        src.emitNumber("light", "illuminance", 10, 10f)
        assertTrue(s.closeRequest.isCompleted)
        @OptIn(kotlinx.coroutines.ExperimentalCoroutinesApi::class)
        assertEquals(CloseCodes.POLICY_VIOLATION, s.closeRequest.getCompleted().code)
    }

    @Test fun responsesAreNeverDropped() {
        val s = helloSession()
        s.subscribe("""{"source":"gyro","rate_hz":200}""")
        for (i in 0 until 20) src.emitVec("gyro", "rotation", i * 5_000_000L, 0f, 0f, 0f)
        val frames = s.send("""{"jsonrpc":"2.0","id":99,"method":"list_sensors"}""")
        assertEquals(JsonPrimitive(99), frames.last()["id"])
    }

    // ---- lifecycle ----

    @Test fun closeDropsSessionAndSubscriptions() {
        val s = helloSession()
        s.subscribe("""{"source":"gyro"}""")
        assertEquals(1, hub.clients.value.single().subscriptions.size)
        s.close()
        assertTrue(hub.clients.value.isEmpty())
        assertTrue(src.current.isEmpty())
        assertNull(s.tryReceiveOutgoing())
    }

    @Test fun distance() {
        assertEquals(5.0, Subscription.distance(Values.vec(0f, 0f, 0f), Values.vec(3f, 4f, 0f)), 1e-6)
        assertEquals(2.5, Subscription.distance(Values.number(1.0), Values.number(3.5)), 1e-9)
        assertEquals(Double.POSITIVE_INFINITY, Subscription.distance(Values.bool(true), Values.bool(true)), 0.0)
        assertTrue(Subscription.distance(JsonArray(emptyList()), Values.vec(1f, 2f, 3f)).isInfinite())
    }
}
