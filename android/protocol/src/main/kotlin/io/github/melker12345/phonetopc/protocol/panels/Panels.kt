package io.github.melker12345.phonetopc.protocol.panels

import io.github.melker12345.phonetopc.protocol.Reading
import io.github.melker12345.phonetopc.protocol.RpcError
import io.github.melker12345.phonetopc.protocol.RpcException
import io.github.melker12345.phonetopc.protocol.SensorCatalog
import io.github.melker12345.phonetopc.protocol.array
import io.github.melker12345.phonetopc.protocol.bool
import io.github.melker12345.phonetopc.protocol.integer
import io.github.melker12345.phonetopc.protocol.obj
import io.github.melker12345.phonetopc.protocol.panels.Rules.choice
import io.github.melker12345.phonetopc.protocol.panels.Rules.invalid
import io.github.melker12345.phonetopc.protocol.panels.Rules.requiredId
import io.github.melker12345.phonetopc.protocol.panels.Rules.text
import io.github.melker12345.phonetopc.protocol.present
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonArray
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

enum class Orientation { ANY, PORTRAIT, LANDSCAPE }

/** Validated `panel_show` params (PANELS.md §4.3). */
data class PanelShowParams(
    val panel: String,
    val title: String,
    val layout: Widget,
    val keepAwake: Boolean,
    val orientation: Orientation,
) {
    companion object {
        private val ORIENTATIONS = mapOf("any" to Orientation.ANY, "portrait" to Orientation.PORTRAIT, "landscape" to Orientation.LANDSCAPE)

        fun parse(params: JsonObject): PanelShowParams {
            val panel = params.requiredId("panel")
            val title = params.text("title", 1, 60, newlines = false) ?: invalid("Missing required param: title")
            val layout = Layout.parse(params.present("layout") ?: invalid("Missing required param: layout"))
            return PanelShowParams(
                panel, title, layout,
                keepAwake = params.bool("keep_awake") ?: false,
                orientation = params.choice("orientation", ORIENTATIONS) ?: Orientation.ANY,
            )
        }
    }
}

/** Validated `panel_update` params (§4.5); the widget rules are checked against the panel later. */
data class PanelUpdateParams(val panel: String, val updates: List<Pair<String, JsonObject>>) {
    companion object {
        fun parse(params: JsonObject): PanelUpdateParams {
            val panel = params.requiredId("panel")
            val updates = params.array("updates") ?: invalid("Missing required param: updates")
            if (updates.size !in 1..64) invalid("updates must have 1-64 items")
            return PanelUpdateParams(panel, updates.map { u ->
                val o = u as? JsonObject ?: invalid("updates must be objects")
                val set = o.obj("set") ?: invalid("an update needs set")
                Layout.checkSet(set)
                o.requiredId("widget") to set
            })
        }
    }
}

enum class StrokePhase(val wire: String) { START("start"), MOVE("move"), END("end"), CANCEL("cancel") }

enum class StrokeTool(val wire: String) { FINGER("finger"), STYLUS("stylus"), ERASER("eraser"), MOUSE("mouse"), UNKNOWN("unknown") }

/** One touch point: canvas-relative [x], [y] in [0, 1], [pressure] in [0, 1], [t] ms since the stroke's first point. */
data class StrokePoint(val x: Float, val y: Float, val pressure: Float, val t: Long)

/** A kept stroke, for drawing a canvas again (e.g. after the screen rotated). */
data class StrokeData(val id: Long, val tool: StrokeTool, val points: List<StrokePoint>, val ended: Boolean)

/** Per canvas, what the UI needs to know beyond the layout. */
data class CanvasState(val clears: Int, val full: Boolean)

/** An immutable view of one panel for the UI. */
data class PanelSnapshot(
    val pc: PcRef,
    val id: String,
    /** Changes with every `panel_show`, so a replaced panel is a new panel to the UI. */
    val instance: Long,
    val title: String,
    val layout: Widget,
    val keepAwake: Boolean,
    val orientation: Orientation,
    val attached: Boolean,
    val visible: Boolean,
    val canvases: Map<String, CanvasState>,
)

/** Identifies a panel instance in calls from the UI; calls for a replaced or closed panel are ignored. */
data class PanelRef(val pcKey: String, val id: String, val instance: Long)

/** The app side of panels. Called without the service's lock held, from any thread. */
fun interface PanelDisplay {
    /**
     * `panel_show` created or replaced [panel]: bring it on screen per PANELS.md §4.3
     * (switch if a panel of the same PC is in front, else post a notification).
     */
    fun showRequested(panel: PanelSnapshot)
}

/** Who gets a panel's events: the owner session (§4.2). Implemented by `Session`. */
interface PanelOwner {
    /** Deliver a `panel` event to this session's matching subscriptions. Must not block. */
    fun deliverPanelEvent(reading: Reading)
}

/** A successful method result, plus what to do once it is queued (events held until then). */
class PanelReply(val result: JsonElement, val afterResponse: (() -> Unit)? = null)

/**
 * Notifications' sibling: all panels of all PCs (PANELS.md §4–§7). Owns their state, decides who
 * gets which event, keeps canvas strokes and replays them. The phone's UI reads [panels] and
 * reports the user's input through the `on…` methods.
 *
 * Panels live as long as this object (the app process); the hub only borrows it, so they
 * survive the server restarting.
 */
class PanelService(
    private val permissions: OutputPermissions,
    private val display: PanelDisplay = PanelDisplay { },
    private val clock: OutputClock = OutputClock.System,
    val maxPanelsPerPc: Int = 8,
    /** §4.1: a detached panel is removed this long after it was detached. */
    val detachedTtlMillis: Long = 10 * 60 * 1000L,
    /** §7.3: at least 100 000. */
    val maxPointsPerCanvas: Int = 200_000,
) {
    private val lock = Any()
    private val panels = LinkedHashMap<String, Panel>() // key: pc.key + "\u0000" + id, creation order
    private var nextInstance = 1L

    private val _snapshots = MutableStateFlow<List<PanelSnapshot>>(emptyList())

    /** All panels, in creation order. */
    val snapshots: StateFlow<List<PanelSnapshot>> = _snapshots.asStateFlow()

    private class Stroke(val id: Long, val tool: StrokeTool, val points: ArrayList<StrokePoint> = ArrayList(), var ended: Boolean = false)

    private class CanvasLog {
        /** The `stroke` events since the last clear, in pseq order (§7.3). */
        val events = ArrayList<Reading>()
        val strokes = LinkedHashMap<Long, Stroke>()
        var points = 0
        var full = false
        var clears = 0
        var nextStroke = 0L
    }

    private inner class Panel(
        val pc: PcRef,
        val id: String,
        val instance: Long,
        var title: String,
        var layout: Widget,
        var keepAwake: Boolean,
        var orientation: Orientation,
        var owner: PanelOwner?,
        var visible: Boolean,
    ) {
        var nextPseq = 0L
        var detachedAt: Long? = null
        val canvases = HashMap<String, CanvasLog>()

        /** Set between `panel_show`/`panel_attach` and its response being queued: events wait in [held]. */
        var holding = false
        val held = ArrayList<Reading>()

        /** The first pseq that went to [held] rather than being replayed. */
        var heldFrom = 0L

        fun canvas(widgetId: String) = canvases.getOrPut(widgetId) { CanvasLog() }

        fun snapshot() = PanelSnapshot(
            pc, id, instance, title, layout, keepAwake, orientation, owner != null, visible,
            layout.walk().filterIsInstance<Widget.Canvas>().associate { c ->
                c.id to canvases[c.id].let { CanvasState(it?.clears ?: 0, it?.full ?: false) }
            },
        )
    }

    /** A panel's map key: panel ids are per PC, and an id can't contain NUL (§8), so this is unambiguous. */
    private fun key(pcKey: String, id: String) = pcKey + "\u0000" + id

    // ---- methods (PANELS.md §4); params are already checked (step 7) ----

    fun show(owner: PanelOwner, pc: PcRef, params: PanelShowParams): PanelReply {
        val shown: PanelSnapshot
        val panel: Panel
        synchronized(lock) {
            checkAllowed(pc)
            expireLocked()
            val old = panels[key(pc.key, params.panel)]
            if (old == null && panels.values.count { it.pc.key == pc.key } >= maxPanelsPerPc) {
                throw RpcException(RpcError.TOO_MANY_PANELS, "This PC already has $maxPanelsPerPc panels; close one first")
            }
            panel = Panel(
                pc, params.panel, nextInstance++, params.title, params.layout, params.keepAwake, params.orientation,
                owner = owner,
                // A replaced panel stays where it is on screen (§4.3).
                visible = old?.visible ?: false,
            )
            panel.holding = true
            // Replacing keeps the place in creation order.
            panels[key(pc.key, params.panel)] = panel
            publishLocked()
            shown = panel.snapshot()
        }
        display.showRequested(shown)
        val result = buildJsonObject {
            put("panel", params.panel)
            put("visible", shown.visible)
        }
        return PanelReply(result) { release(panel, afterPseq = null) }
    }

    fun attach(owner: PanelOwner, pc: PcRef, panelId: String, afterPseq: Long): PanelReply = synchronized(lock) {
        val panel = findLocked(pc, panelId)
        panel.owner = owner
        panel.detachedAt = null
        panel.holding = true
        panel.heldFrom = panel.nextPseq
        publishLocked()
        val result = buildJsonObject {
            put("panel", panel.id)
            put("title", panel.title)
            put("visible", panel.visible)
            put("last_pseq", panel.nextPseq - 1)
            put("values", JsonObject(Layout.values(panel.layout)))
        }
        PanelReply(result) { release(panel, afterPseq) }
    }

    fun update(pc: PcRef, params: PanelUpdateParams) {
        synchronized(lock) {
            val panel = findLocked(pc, params.panel)
            var raw = panel.layout.raw
            for ((widget, set) in params.updates) {
                raw = Layout.merge(raw, widget, set) ?: invalid("panel ${panel.id} has no widget \"$widget\"")
            }
            panel.layout = Layout.parse(raw) // atomic: nothing changed if this throws
            publishLocked()
        }
    }

    fun close(pc: PcRef, panelId: String) {
        synchronized(lock) {
            val panel = findLocked(pc, panelId)
            panels.remove(key(pc.key, panel.id))
            publishLocked()
        }
    }

    fun list(pc: PcRef): JsonElement = synchronized(lock) {
        checkAllowed(pc)
        expireLocked()
        buildJsonObject {
            put("panels", buildJsonArray {
                panels.values.filter { it.pc.key == pc.key }.forEach { p ->
                    add(buildJsonObject {
                        put("panel", p.id)
                        put("title", p.title)
                        put("attached", p.owner != null)
                        put("visible", p.visible)
                    })
                }
            })
        }
    }

    fun clearCanvas(pc: PcRef, panelId: String, widgetId: String) {
        synchronized(lock) {
            val panel = findLocked(pc, panelId)
            if (panel.layout.find(widgetId) !is Widget.Canvas) invalid("panel ${panel.id} has no canvas \"$widgetId\"")
            val log = panel.canvas(widgetId)
            log.events.clear()
            log.strokes.clear()
            log.points = 0
            log.full = false
            log.clears++
            publishLocked()
        }
    }

    /** Permission, then expiry, then the panel itself (§9). */
    private fun findLocked(pc: PcRef, panelId: String): Panel {
        checkAllowed(pc)
        expireLocked()
        return panels[key(pc.key, panelId)] ?: throw RpcException(RpcError.UNKNOWN_PANEL, "No panel \"$panelId\" (never shown, closed, or expired)")
    }

    private fun checkAllowed(pc: PcRef) {
        if (!permissions.allowed(pc, Feature.PANELS)) notAllowed(Feature.PANELS)
    }

    /**
     * The response to `panel_show`/`panel_attach` is queued: replay the kept strokes after
     * [afterPseq] (§4.4; `null`: none), then the events of the meantime, then go live.
     */
    private fun release(panel: Panel, afterPseq: Long?) {
        synchronized(lock) {
            if (!panel.holding) return
            panel.holding = false
            val owner = panel.owner
            if (owner != null) {
                if (afterPseq != null) {
                    // Events from before the attach; later ones are in [held].
                    panel.canvases.values.flatMap { it.events }
                        .filter { pseqOf(it) > afterPseq && pseqOf(it) < panel.heldFrom }
                        .sortedBy { pseqOf(it) }
                        .forEach { r -> owner.deliverPanelEvent(r.copy(value = JsonObject(r.value as JsonObject + ("replayed" to JsonPrimitive(true))))) }
                }
                panel.held.forEach { owner.deliverPanelEvent(it) }
            }
            panel.held.clear()
        }
    }

    private fun pseqOf(r: Reading) = ((r.value as JsonObject)["pseq"] as JsonPrimitive).content.toLong()

    // ---- sessions and permissions ----

    /** The owner went away: its panels are detached (§4.1). */
    fun ownerClosed(owner: PanelOwner) {
        synchronized(lock) {
            var changed = false
            for (p in panels.values) if (p.owner === owner) {
                p.owner = null
                p.detachedAt = clock.elapsedMillis()
                p.holding = false
                p.held.clear()
                changed = true
            }
            if (changed) publishLocked()
        }
    }

    /** The user changed permissions: close the panels of PCs that may no longer show any (§1). */
    fun permissionsChanged() {
        synchronized(lock) {
            val gone = panels.values.filter { !permissions.allowed(it.pc, Feature.PANELS) }
            if (gone.isEmpty()) return
            for (p in gone) {
                emitLocked(p, "state") { put("state", "closed"); put("reason", "revoked") }
                panels.remove(key(p.pc.key, p.id))
            }
            publishLocked()
        }
    }

    /** Removes detached panels past their time. Call now and then; methods call it too. */
    fun expire() {
        synchronized(lock) { if (expireLocked()) publishLocked() }
    }

    private fun expireLocked(): Boolean {
        val now = clock.elapsedMillis()
        val gone = panels.entries.filter { (_, p) -> p.owner == null && p.detachedAt?.let { now - it >= detachedTtlMillis } == true }
        gone.forEach { panels.remove(it.key) }
        if (gone.isNotEmpty()) publishLocked()
        return gone.isNotEmpty()
    }

    // ---- the user (§6) ----

    /** The panel is (or no longer is) what the user sees. */
    fun onVisible(ref: PanelRef, visible: Boolean) = withPanel(ref) { p ->
        if (p.visible == visible) return@withPanel
        p.visible = visible
        emitLocked(p, "state") { put("state", if (visible) "visible" else "hidden") }
        publishLocked()
    }

    /** The close button in the frame. */
    fun onUserClose(ref: PanelRef) = withPanel(ref) { p ->
        emitLocked(p, "state") { put("state", "closed"); put("reason", "user") }
        panels.remove(key(p.pc.key, p.id))
        publishLocked()
    }

    fun onPress(ref: PanelRef, widgetId: String) = withInput<Widget.Button>(ref, widgetId) { p, _ ->
        emitLocked(p, "press") { put("widget", widgetId) }
    }

    fun onToggle(ref: PanelRef, widgetId: String, value: Boolean) = withInput<Widget.Toggle>(ref, widgetId) { p, w ->
        if (w.value == value) return@withInput
        setValueLocked(p, widgetId, JsonPrimitive(value))
        emitLocked(p, "change") { put("widget", widgetId); put("value", value) }
    }

    /** A slider moved; [final] on release (§6.2). The UI throttles to about 20 a second. */
    fun onSlide(ref: PanelRef, widgetId: String, value: Double, final: Boolean) = withInput<Widget.Slider>(ref, widgetId) { p, w ->
        val v = w.normalize(value)
        if (v == w.value && !final) return@withInput
        setValueLocked(p, widgetId, Rules.numberValue(v))
        emitLocked(p, "change") {
            put("widget", widgetId)
            put("value", Rules.numberValue(v))
            if (final) put("final", true)
        }
    }

    fun onSubmit(ref: PanelRef, widgetId: String, text: String) = withInput<Widget.TextInput>(ref, widgetId) { p, w ->
        if (!Rules.isDisplayText(text, 0, w.maxLength, newlines = true)) return@withInput
        emitLocked(p, "submit") { put("widget", widgetId); put("value", text) }
    }

    /**
     * A finger or stylus touched a canvas. Returns the stroke id, or `null` if the canvas takes
     * no ink (full, disabled, gone). Canvases take ink while detached too (§4.1).
     */
    fun onStrokeStart(ref: PanelRef, widgetId: String, tool: StrokeTool, widthDp: Float, heightDp: Float, first: List<StrokePoint>): Long? {
        require(first.isNotEmpty()) { "a stroke starts with a point" }
        var id: Long? = null
        withPanel(ref) { p ->
            val canvas = p.layout.find(widgetId) as? Widget.Canvas ?: return@withPanel
            if (!canvas.enabled) return@withPanel
            val log = p.canvas(widgetId)
            if (log.full) return@withPanel
            val stroke = Stroke(log.nextStroke++, tool)
            log.strokes[stroke.id] = stroke
            id = stroke.id
            appendLocked(p, widgetId, log, stroke, StrokePhase.START, first, floatArrayOf(widthDp, heightDp))
        }
        return id
    }

    /** More points of a stroke, or its end ([phase] `END`/`CANCEL`). Chunks of more than 128 points are split. */
    fun onStroke(ref: PanelRef, widgetId: String, strokeId: Long, phase: StrokePhase, points: List<StrokePoint>) {
        require(phase != StrokePhase.START)
        withPanel(ref) { p ->
            val log = p.canvases[widgetId] ?: return@withPanel
            val stroke = log.strokes[strokeId]?.takeIf { !it.ended } ?: return@withPanel // cleared meanwhile
            appendLocked(p, widgetId, log, stroke, phase, points, null)
        }
    }

    /** Strokes a canvas holds now, to draw it again. */
    fun strokes(ref: PanelRef, widgetId: String): List<StrokeData> = synchronized(lock) {
        val p = panels[key(ref.pcKey, ref.id)]?.takeIf { it.instance == ref.instance } ?: return emptyList()
        p.canvases[widgetId]?.strokes?.values?.map { StrokeData(it.id, it.tool, it.points.toList(), it.ended) }.orEmpty()
    }

    private fun appendLocked(
        p: Panel, widgetId: String, log: CanvasLog, stroke: Stroke, phase: StrokePhase, points: List<StrokePoint>, size: FloatArray?,
    ) {
        // Canvas full (§7.3): take what fits, end the stroke, and take no more ink until cleared.
        val room = (maxPointsPerCanvas - log.points).coerceAtLeast(0)
        val truncated = points.size > room
        val taken = if (truncated) points.take(room) else points
        val ends = truncated || phase == StrokePhase.END || phase == StrokePhase.CANCEL
        if (taken.isEmpty() && !ends) return
        val endPhase = if (phase == StrokePhase.CANCEL) StrokePhase.CANCEL else StrokePhase.END
        val chunks = taken.chunked(MAX_CHUNK).ifEmpty { listOf(emptyList()) }
        for ((i, chunk) in chunks.withIndex()) {
            val chunkPhase = when {
                i == 0 && phase == StrokePhase.START -> StrokePhase.START
                i == chunks.lastIndex && ends -> endPhase
                else -> StrokePhase.MOVE
            }
            emitStrokeLocked(p, widgetId, log, stroke, chunkPhase, chunk, size.takeIf { chunkPhase == StrokePhase.START })
        }
        // A start chunk can't also end the stroke.
        if (phase == StrokePhase.START && ends) emitStrokeLocked(p, widgetId, log, stroke, endPhase, emptyList(), null)
        if (ends) stroke.ended = true
        if (log.points >= maxPointsPerCanvas && !log.full) {
            log.full = true
            publishLocked()
        }
    }

    private fun emitStrokeLocked(
        p: Panel, widgetId: String, log: CanvasLog, stroke: Stroke, phase: StrokePhase, chunk: List<StrokePoint>, size: FloatArray?,
    ) {
        stroke.points += chunk
        log.points += chunk.size
        log.events += emitLocked(p, "stroke") {
            put("widget", widgetId)
            put("stroke", stroke.id)
            put("phase", phase.wire)
            put("tool", stroke.tool.wire)
            if (size != null) put("size", buildJsonArray { add(JsonPrimitive(size[0])); add(JsonPrimitive(size[1])) })
            put("points", JsonArray(chunk.map { pt ->
                buildJsonArray {
                    add(JsonPrimitive(round4(pt.x)))
                    add(JsonPrimitive(round4(pt.y)))
                    add(JsonPrimitive(round4(pt.pressure)))
                    add(JsonPrimitive(pt.t))
                }
            }))
        }
    }

    private fun round4(v: Float): Double = Math.round(v.toDouble().coerceIn(0.0, 1.0) * 10_000) / 10_000.0

    private fun setValueLocked(p: Panel, widgetId: String, value: JsonPrimitive) {
        val raw = Layout.merge(p.layout.raw, widgetId, JsonObject(mapOf("value" to value))) ?: return
        p.layout = runCatching { Layout.parse(raw) }.getOrNull() ?: return
        publishLocked()
    }

    private inline fun withPanel(ref: PanelRef, block: (Panel) -> Unit) {
        synchronized(lock) {
            val p = panels[key(ref.pcKey, ref.id)]?.takeIf { it.instance == ref.instance } ?: return
            block(p)
        }
    }

    /** Input on a widget other than a canvas: only while attached and enabled (§4.1, §5). */
    private inline fun <reified W : Widget> withInput(ref: PanelRef, widgetId: String, block: (Panel, W) -> Unit) = withPanel(ref) { p ->
        if (p.owner == null) return@withPanel
        val w = p.layout.find(widgetId) as? W ?: return@withPanel
        if (!w.enabled) return@withPanel
        block(p, w)
    }

    /** Numbers the event, then sends it to the owner, holds it, or (no owner) drops it. */
    private fun emitLocked(p: Panel, type: String, members: kotlinx.serialization.json.JsonObjectBuilder.() -> Unit): Reading {
        val value = buildJsonObject {
            put("panel", p.id)
            put("pseq", p.nextPseq++)
            members()
        }
        val reading = Reading(SensorCatalog.PANEL, type, value, "", clock.wallMillis(), clock.monoNanos())
        val owner = p.owner
        when {
            owner == null -> Unit
            p.holding -> p.held += reading
            else -> owner.deliverPanelEvent(reading)
        }
        return reading
    }

    private fun publishLocked() {
        _snapshots.value = panels.values.map { it.snapshot() }
    }

    companion object {
        const val MAX_CHUNK = 128

        /** `after_pseq` of `panel_attach` (§4.4): an integer ≥ -1, default -1. */
        fun parseAttach(params: JsonObject): Pair<String, Long> {
            val panel = params.requiredId("panel")
            val after = params.integer("after_pseq") ?: -1
            if (after < -1) invalid("after_pseq must be >= -1")
            return panel to after
        }

        fun parseCanvasClear(params: JsonObject): Pair<String, String> = params.requiredId("panel") to params.requiredId("widget")
    }
}
