package io.github.melker12345.phonetopc.protocol.panels

import io.github.melker12345.phonetopc.protocol.BridgeHub
import io.github.melker12345.phonetopc.protocol.DeviceInfo
import io.github.melker12345.phonetopc.protocol.FakeSensorSource
import io.github.melker12345.phonetopc.protocol.ServerInfo
import io.github.melker12345.phonetopc.protocol.Session
import kotlinx.coroutines.runBlocking
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
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

class FakeClock : OutputClock {
    var now = 1_000_000L
    override fun elapsedMillis() = now
    override fun wallMillis() = now
    override fun monoNanos() = now * 1_000_000
}

/** Ownership, detaching, replay, expiry and routing of PANELS.md through real sessions. */
class PanelSessionTest {
    private val clock = FakeClock()
    private var allowed = true
    private val permissions = OutputPermissions { _, _ -> allowed }
    private val notifier = FakeNotifier()
    private val panels = PanelService(permissions, clock = clock)
    private val notifications = NotificationService(permissions, notifier, clock)
    private val hub = BridgeHub(
        source = FakeSensorSource(),
        serverInfo = ServerInfo("phonetopc-android", "test"),
        deviceInfoProvider = { DeviceInfo("dev1", "Test Phone", "acme", "X1", "android", "34") },
        notifications = notifications,
        panels = panels,
        outputPermissions = permissions,
    )

    private inner class Client {
        val session: Session = hub.openSession("test")
        val frames = ArrayList<JsonObject>()
        private var nextId = 1

        init {
            call("hello", """{"protocol":"phonetopc/1"}""")
        }

        fun drain() {
            while (true) frames += Json.parseToJsonElement(session.tryReceiveOutgoing() ?: break).jsonObject
        }

        /** Sends a request and returns its response; events stay in [frames]. */
        fun call(method: String, params: String = "{}"): JsonObject = runBlocking {
            val id = nextId++
            session.handleText("""{"jsonrpc":"2.0","id":$id,"method":"$method","params":$params}""")
            drain()
            val r = frames.first { it["id"]?.jsonPrimitive?.content == "$id" }
            frames.remove(r)
            r
        }

        fun result(method: String, params: String = "{}"): JsonObject {
            val r = call(method, params)
            assertNull("$method failed: $r", r["error"])
            return r["result"]!!.jsonObject
        }

        /** Event values of [type] (all types if null) received so far, consumed. */
        fun events(type: String? = null): List<JsonObject> {
            drain()
            val evs = frames.filter { it["method"]?.jsonPrimitive?.content == "event" }
                .map { it["params"]!!.jsonObject }
                .filter { type == null || it["type"]!!.jsonPrimitive.content == type }
            frames.removeAll { f -> f["method"] != null && evs.any { it == f["params"] } }
            return evs.map { it["value"]!!.jsonObject }
        }
    }

    private val deck = """{"panel":"deck","title":"Deck","layout":{"type":"column","children":[
        {"type":"button","id":"mute","label":"Mute"},
        {"type":"toggle","id":"mic","label":"Mic"},
        {"type":"slider","id":"vol","min":0,"max":100,"step":5},
        {"type":"canvas","id":"ink"}]}}"""

    private fun ref(id: String = "deck") = panels.snapshots.value.first { it.id == id }.let { PanelRef(it.pc.key, it.id, it.instance) }

    private fun pt(x: Float, t: Long = 0) = StrokePoint(x, 0.5f, 1f, t)

    @Test fun eventsGoOnlyToTheOwner() {
        val a = Client().apply { result("subscribe", """{"source":"panel"}""") }
        val b = Client().apply { result("subscribe", """{"source":"panel"}""") }
        a.result("panel_show", deck)
        panels.onPress(ref(), "mute")
        assertEquals(listOf("mute"), a.events("press").map { it["widget"]!!.jsonPrimitive.content })
        assertTrue(b.events().isEmpty())
        // Another session of the same PC takes it over.
        b.result("panel_attach", """{"panel":"deck"}""")
        panels.onPress(ref(), "mute")
        assertTrue(a.events().isEmpty())
        assertEquals(1, b.events("press").size)
    }

    @Test fun pseqCountsEveryEventAndValuesFollowTheUser() {
        val a = Client().apply { result("subscribe", """{"source":"panel"}""") }
        a.result("panel_show", deck)
        panels.onVisible(ref(), true)
        panels.onToggle(ref(), "mic", true)
        panels.onSlide(ref(), "vol", 42.0, final = false)
        panels.onSlide(ref(), "vol", 42.0, final = true)
        val evs = a.events()
        assertEquals(listOf(0L, 1L, 2L, 3L), evs.map { it["pseq"]!!.jsonPrimitive.long })
        assertEquals("visible", evs[0]["state"]!!.jsonPrimitive.content)
        assertEquals(JsonPrimitive(40), evs[2]["value"]) // snapped to step 5
        assertEquals(JsonPrimitive(true), evs[3]["final"])
        val attach = a.result("panel_attach", """{"panel":"deck"}""")
        assertEquals(JsonPrimitive(3), attach["last_pseq"])
        assertEquals(JsonPrimitive(true), attach["values"]!!.jsonObject["mic"])
        assertEquals(JsonPrimitive(40), attach["values"]!!.jsonObject["vol"])
    }

    @Test fun eventsWaitForTheShowResponse() {
        val owner = object : PanelOwner {
            val got = ArrayList<String>()
            override fun deliverPanelEvent(reading: io.github.melker12345.phonetopc.protocol.Reading) {
                got += reading.type
            }
        }
        val reply = panels.show(owner, PcRef.DEV, PanelShowParams.parse(Json.parseToJsonElement(deck).jsonObject))
        panels.onVisible(ref(), true)
        assertTrue(owner.got.isEmpty())
        reply.afterResponse!!.invoke()
        assertEquals(listOf("state"), owner.got)
    }

    @Test fun detachedCanvasKeepsInkAndAttachReplaysWhatWasMissed() {
        val a = Client().apply { result("subscribe", """{"source":"panel"}""") }
        a.result("panel_show", deck)
        val s1 = panels.onStrokeStart(ref(), "ink", StrokeTool.FINGER, 300f, 400f, listOf(pt(0.1f)))!!
        panels.onStroke(ref(), "ink", s1, StrokePhase.END, listOf(pt(0.2f, 16)))
        val seen = a.events("stroke").last()["pseq"]!!.jsonPrimitive.long
        a.session.close()
        assertFalse(panels.snapshots.value.single().attached)
        // Buttons are dead while detached, the canvas isn't.
        panels.onPress(ref(), "mute")
        val s2 = panels.onStrokeStart(ref(), "ink", StrokeTool.STYLUS, 300f, 400f, listOf(pt(0.3f)))!!
        panels.onStroke(ref(), "ink", s2, StrokePhase.END, emptyList())

        val b = Client().apply { result("subscribe", """{"source":"panel"}""") }
        val attach = b.result("panel_attach", """{"panel":"deck","after_pseq":$seen}""")
        assertTrue(attach["last_pseq"]!!.jsonPrimitive.long > seen)
        val replayed = b.events("stroke")
        assertEquals(listOf(s2, s2), replayed.map { it["stroke"]!!.jsonPrimitive.long })
        assertTrue(replayed.all { it["replayed"] == JsonPrimitive(true) })
        assertEquals(listOf("start", "end"), replayed.map { it["phase"]!!.jsonPrimitive.content })
        assertEquals(JsonPrimitive("stylus"), replayed[0]["tool"])
        assertNotNull(replayed[0]["size"])

        // From scratch: everything the canvas holds.
        val c = Client().apply { result("subscribe", """{"source":"panel"}""") }
        c.result("panel_attach", """{"panel":"deck"}""")
        assertEquals(4, c.events("stroke").size)
        // A clear drops what is kept.
        c.result("canvas_clear", """{"panel":"deck","widget":"ink"}""")
        c.result("panel_attach", """{"panel":"deck"}""")
        assertTrue(c.events("stroke").isEmpty())
    }

    @Test fun detachedPanelsExpireAfterTenMinutes() {
        val a = Client()
        a.result("panel_show", deck)
        a.session.close()
        clock.now += 10 * 60 * 1000L - 1
        val b = Client()
        b.result("panel_attach", """{"panel":"deck"}""")
        b.session.close()
        clock.now += 10 * 60 * 1000L
        val c = Client()
        assertEquals(-32031, c.call("panel_attach", """{"panel":"deck"}""")["error"]!!.jsonObject["code"]!!.jsonPrimitive.int)
        assertTrue(panels.snapshots.value.isEmpty())
    }

    @Test fun attachedPanelsDontExpire() {
        val a = Client()
        a.result("panel_show", deck)
        clock.now += 60 * 60 * 1000L
        assertEquals(1, a.result("panel_list")["panels"]!!.jsonArray.size)
    }

    @Test fun revokingClosesPanelsWithAnEvent() {
        val a = Client().apply { result("subscribe", """{"source":"panel"}""") }
        a.result("panel_show", deck)
        allowed = false
        panels.permissionsChanged()
        val closed = a.events("state").single()
        assertEquals(JsonPrimitive("closed"), closed["state"])
        assertEquals(JsonPrimitive("revoked"), closed["reason"])
        assertTrue(panels.snapshots.value.isEmpty())
    }

    @Test fun userCloseSendsClosed() {
        val a = Client().apply { result("subscribe", """{"source":"panel","type":"state"}""") }
        a.result("panel_show", deck)
        panels.onUserClose(ref())
        assertEquals(JsonPrimitive("user"), a.events().single()["reason"])
        assertEquals(-32031, a.call("panel_close", """{"panel":"deck"}""")["error"]!!.jsonObject["code"]!!.jsonPrimitive.int)
    }

    @Test fun replacedPanelIgnoresInputForTheOldInstance() {
        val a = Client().apply { result("subscribe", """{"source":"panel"}""") }
        a.result("panel_show", deck)
        val old = ref()
        a.result("panel_show", deck)
        panels.onPress(old, "mute")
        assertTrue(a.events("press").isEmpty())
        panels.onPress(ref(), "mute")
        assertEquals(JsonPrimitive(0), a.events("press").single()["pseq"]) // pseq restarts
    }

    @Test fun longChunksAreSplitAndAFullCanvasEndsTheStroke() {
        val small = PanelService(permissions, clock = clock, maxPointsPerCanvas = 300)
        val got = ArrayList<JsonObject>()
        val owner = object : PanelOwner {
            override fun deliverPanelEvent(reading: io.github.melker12345.phonetopc.protocol.Reading) {
                got += reading.value.jsonObject
            }
        }
        small.show(owner, PcRef.DEV, PanelShowParams.parse(Json.parseToJsonElement(deck).jsonObject)).afterResponse!!()
        val r = small.snapshots.value.single().let { PanelRef(it.pc.key, it.id, it.instance) }
        val s = small.onStrokeStart(r, "ink", StrokeTool.FINGER, 1f, 1f, listOf(pt(0f)))!!
        small.onStroke(r, "ink", s, StrokePhase.MOVE, List(400) { pt(0.5f, it.toLong()) })
        // 300 points fit: the last chunk that fits ends the stroke.
        assertEquals(listOf(1, 128, 128, 43), got.map { it["points"]!!.jsonArray.size })
        assertEquals(listOf("start", "move", "move", "end"), got.map { it["phase"]!!.jsonPrimitive.content })
        assertTrue(small.snapshots.value.single().canvases["ink"]!!.full)
        assertNull(small.onStrokeStart(r, "ink", StrokeTool.FINGER, 1f, 1f, listOf(pt(0f))))
    }

    @Test fun notificationEventsGoToThatPcsSessions() {
        val a = Client().apply { result("subscribe", """{"source":"notification"}""") }
        val id = a.result("notify", """{"title":"Done","actions":[{"id":"ok","label":"OK"}]}""")["id"]!!.jsonPrimitive.content
        notifications.onAction(PcRef.DEV, id, "ok")
        val ev = a.events("action").single()
        assertEquals(JsonPrimitive(id), ev["notification"])
        assertEquals(JsonPrimitive("ok"), ev["action"])
        // Gone after the action: a second one is ignored.
        notifications.onTap(PcRef.DEV, id)
        assertTrue(a.events().isEmpty())
        // Another PC's notification never reaches this one.
        notifications.notify(PcRef("other", "Other PC"), NotifyParams("x", "", "x", emptyList(), false))
        notifications.onTap(PcRef("other", "Other PC"), "x")
        assertTrue(a.events().isEmpty())
    }

    @Test fun seventeenthNotificationRemovesTheOldest() {
        val a = Client()
        repeat(17) { a.result("notify", """{"title":"n$it","id":"id$it"}""") }
        assertEquals(16, notifications.shown(PcRef.DEV).size)
        assertTrue(PcRef.DEV.key to "id0" in notifier.cancelled)
    }

    @Test fun rateLimit() {
        val a = Client()
        repeat(60) { a.result("notify", """{"title":"n"}""") }
        assertEquals(-32033, a.call("notify", """{"title":"n"}""")["error"]!!.jsonObject["code"]!!.jsonPrimitive.int)
        clock.now += 60_000
        a.result("notify", """{"title":"n"}""")
    }

    @Test fun panelsBelongToThePc() {
        val other = PcRef("pc-2", "laptop")
        val owner = object : PanelOwner {
            override fun deliverPanelEvent(reading: io.github.melker12345.phonetopc.protocol.Reading) = Unit
        }
        panels.show(owner, other, PanelShowParams.parse(Json.parseToJsonElement(deck).jsonObject))
        val a = Client()
        assertTrue(a.result("panel_list")["panels"]!!.jsonArray.isEmpty())
        assertEquals(-32031, a.call("panel_attach", """{"panel":"deck"}""")["error"]!!.jsonObject["code"]!!.jsonPrimitive.int)
    }

    @Test fun listSensorsFollowsPermission() {
        val a = Client()
        fun available(src: String) = a.result("list_sensors")["sensors"]!!.jsonArray
            .first { it.jsonObject["source"]!!.jsonPrimitive.content == src }.jsonObject["available"]!!.jsonPrimitive.content
        assertEquals("true", available("panel"))
        allowed = false
        assertEquals("false", available("panel"))
        assertEquals("false", available("notification"))
    }
}
