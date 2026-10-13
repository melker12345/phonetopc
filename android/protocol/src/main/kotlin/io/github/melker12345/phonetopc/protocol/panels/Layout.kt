package io.github.melker12345.phonetopc.protocol.panels

import io.github.melker12345.phonetopc.protocol.panels.Rules.array
import io.github.melker12345.phonetopc.protocol.panels.Rules.bool
import io.github.melker12345.phonetopc.protocol.panels.Rules.choice
import io.github.melker12345.phonetopc.protocol.panels.Rules.id
import io.github.melker12345.phonetopc.protocol.panels.Rules.integer
import io.github.melker12345.phonetopc.protocol.panels.Rules.invalid
import io.github.melker12345.phonetopc.protocol.panels.Rules.number
import io.github.melker12345.phonetopc.protocol.panels.Rules.string
import io.github.melker12345.phonetopc.protocol.panels.Rules.text
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive

enum class Tone { DEFAULT, ACCENT, GOOD, WARN, BAD }
enum class TextSize { SMALL, BODY, TITLE, DISPLAY }
enum class TextAlign { START, CENTER, END }

/**
 * A parsed, valid widget of a panel layout (PANELS.md §5). [raw] is the widget as the PC sent
 * it, with updates and the user's changes merged in; the tree is re-parsed from it.
 */
sealed class Widget {
    abstract val raw: JsonObject
    abstract val id: String?

    /** Share of the space left in a row or column; `null` means natural size. */
    abstract val weight: Double?

    val enabled: Boolean get() = (raw["enabled"] as? JsonPrimitive)?.content != "false"

    data class Column(override val raw: JsonObject, override val id: String?, override val weight: Double?, val children: List<Widget>) : Widget()
    data class Row(override val raw: JsonObject, override val id: String?, override val weight: Double?, val children: List<Widget>) : Widget()
    data class Grid(override val raw: JsonObject, override val id: String?, override val weight: Double?, val cols: Int, val children: List<Widget>) : Widget()
    data class Text(
        override val raw: JsonObject, override val id: String?, override val weight: Double?,
        val text: String, val size: TextSize, val align: TextAlign, val tone: Tone,
    ) : Widget()
    data class Button(override val raw: JsonObject, override val id: String, override val weight: Double?, val label: String, val tone: Tone) : Widget()
    data class Toggle(override val raw: JsonObject, override val id: String, override val weight: Double?, val label: String, val value: Boolean) : Widget()
    data class Slider(
        override val raw: JsonObject, override val id: String, override val weight: Double?,
        val label: String, val min: Double, val max: Double, val step: Double?, val value: Double,
    ) : Widget() {
        /** [v] snapped to [step] (counted from [min]) and clamped to the range (§6.2). */
        fun normalize(v: Double): Double {
            val snapped = step?.let { s -> min + Math.round((v - min) / s) * s } ?: v
            return snapped.coerceIn(min, max)
        }
    }
    data class Progress(override val raw: JsonObject, override val id: String?, override val weight: Double?, val label: String, val value: Double?, val tone: Tone) : Widget()
    data class TextInput(
        override val raw: JsonObject, override val id: String, override val weight: Double?,
        val label: String, val placeholder: String, val value: String, val multiline: Boolean,
        val maxLength: Int, val clearOnSubmit: Boolean,
    ) : Widget()
    data class Canvas(
        override val raw: JsonObject, override val id: String, override val weight: Double?,
        val ink: Boolean, val color: Int, val width: Double, val background: Int,
    ) : Widget()

    /** This widget and all below it, depth first. */
    fun walk(): Sequence<Widget> = sequence {
        yield(this@Widget)
        when (val w = this@Widget) {
            is Column -> w.children.forEach { yieldAll(it.walk()) }
            is Row -> w.children.forEach { yieldAll(it.walk()) }
            is Grid -> w.children.forEach { yieldAll(it.walk()) }
            else -> Unit
        }
    }

    fun find(widgetId: String): Widget? = walk().firstOrNull { it.id == widgetId }
}

/** Parsing and updating layouts, all checks of PANELS.md §5 and §8. */
object Layout {
    const val MAX_WIDGETS = 256
    const val MAX_DEPTH = 8
    const val MAX_CHILDREN = 64

    private val TONES = mapOf("default" to Tone.DEFAULT, "accent" to Tone.ACCENT, "good" to Tone.GOOD, "warn" to Tone.WARN, "bad" to Tone.BAD)
    private val SIZES = mapOf("small" to TextSize.SMALL, "body" to TextSize.BODY, "title" to TextSize.TITLE, "display" to TextSize.DISPLAY)
    private val ALIGNS = mapOf("start" to TextAlign.START, "center" to TextAlign.CENTER, "end" to TextAlign.END)

    /** Members a `panel_update` can't set (§4.5). */
    val NOT_SETTABLE = setOf("type", "id", "children")

    /** Parses a whole layout; throws -32602 for anything that breaks a rule. */
    fun parse(element: JsonElement): Widget {
        val root = element as? JsonObject ?: invalid("layout must be a widget object")
        val ids = HashSet<String>()
        var count = 0
        fun parseWidget(o: JsonObject, depth: Int): Widget {
            if (depth > MAX_DEPTH) invalid("layout is more than $MAX_DEPTH levels deep")
            if (++count > MAX_WIDGETS) invalid("layout has more than $MAX_WIDGETS widgets")
            val w = widget(o) { children -> children.map { parseWidget(it, depth + 1) } }
            w.id?.let { if (!ids.add(it)) invalid("widget id \"$it\" is used twice") }
            return w
        }
        return parseWidget(root, 1)
    }

    private fun children(o: JsonObject): List<JsonObject> {
        val a = o.array("children") ?: invalid("children is required")
        if (a.size !in 1..MAX_CHILDREN) invalid("children must have 1-$MAX_CHILDREN widgets")
        return a.map { it as? JsonObject ?: invalid("children must be widget objects") }
    }

    private fun widget(o: JsonObject, parseChildren: (List<JsonObject>) -> List<Widget>): Widget {
        val type = o.string("type") ?: invalid("widget type is required")
        val id = o.id("id")
        val weight = o.number("weight")?.also { if (it <= 0) invalid("weight must be > 0") }
        o.bool("enabled")
        val tone = o.choice("tone", TONES) ?: Tone.DEFAULT
        fun needId(): String = id ?: invalid("a $type needs an id")
        return when (type) {
            "column" -> Widget.Column(o, id, weight, parseChildren(children(o)))
            "row" -> Widget.Row(o, id, weight, parseChildren(children(o)))
            "grid" -> {
                val cols = o.integer("cols") ?: invalid("grid needs cols")
                if (cols !in 1..8) invalid("cols must be 1-8")
                Widget.Grid(o, id, weight, cols.toInt(), parseChildren(children(o)))
            }
            "text" -> Widget.Text(
                o, id, weight,
                text = o.text("text", 0, 4000, newlines = true) ?: invalid("text needs text"),
                size = o.choice("size", SIZES) ?: TextSize.BODY,
                align = o.choice("align", ALIGNS) ?: TextAlign.START,
                tone = tone,
            )
            "button" -> Widget.Button(o, needId(), weight, o.text("label", 1, 100, newlines = false) ?: invalid("button needs a label"), tone)
            "toggle" -> Widget.Toggle(
                o, needId(), weight,
                label = o.text("label", 1, 100, newlines = false) ?: invalid("toggle needs a label"),
                value = o.bool("value") ?: false,
            )
            "slider" -> {
                val wid = needId()
                val label = o.text("label", 0, 100, newlines = false) ?: ""
                val min = o.number("min") ?: 0.0
                val max = o.number("max") ?: 1.0
                if (min >= max) invalid("min must be < max")
                val step = o.number("step")?.also { if (it <= 0) invalid("step must be > 0") }
                val value = (o.number("value") ?: min).coerceIn(min, max)
                Widget.Slider(o, wid, weight, label, min, max, step, value)
            }
            "progress" -> {
                val value = o.number("value")?.also { if (it < 0 || it > 1) invalid("progress value must be in [0, 1] or null") }
                Widget.Progress(o, id, weight, o.text("label", 0, 100, newlines = false) ?: "", value, tone)
            }
            "text_input" -> {
                val wid = needId()
                val maxLength = o.integer("max_length") ?: 1000
                if (maxLength !in 1..4000) invalid("max_length must be 1-4000")
                val value = o.text("value", 0, maxLength.toInt(), newlines = true) ?: ""
                Widget.TextInput(
                    o, wid, weight,
                    label = o.text("label", 0, 100, newlines = false) ?: "",
                    placeholder = o.text("placeholder", 0, 100, newlines = false) ?: "",
                    value = value,
                    multiline = o.bool("multiline") ?: false,
                    maxLength = maxLength.toInt(),
                    clearOnSubmit = o.bool("clear_on_submit") ?: true,
                )
            }
            "canvas" -> {
                val wid = needId()
                fun color(key: String, default: Int) = o.string(key)?.let { Rules.parseColor(it) ?: invalid("$key must be #rrggbb") } ?: default
                val width = o.number("width") ?: 4.0
                if (width < 0.5 || width > 64) invalid("width must be 0.5-64")
                Widget.Canvas(o, wid, weight, o.bool("ink") ?: true, color("color", BLACK), width, color("background", WHITE))
            }
            else -> invalid("unknown widget type \"$type\"")
        }
    }

    private const val BLACK = 0xFF000000.toInt()
    private const val WHITE = 0xFFFFFFFF.toInt()

    /**
     * [root]'s raw tree with [set] merged into the widget [widgetId]. Doesn't validate: the
     * caller re-parses the result. `null` if there is no such widget.
     */
    fun merge(root: JsonObject, widgetId: String, set: JsonObject): JsonObject? {
        var found = false
        fun walk(o: JsonObject): JsonObject {
            if (!found && (o["id"] as? JsonPrimitive)?.takeIf { it.isString }?.content == widgetId) {
                found = true
                return JsonObject(o + set)
            }
            val children = o["children"] as? JsonArray ?: return o
            if (found) return o
            val updated = children.map { c -> (c as? JsonObject)?.let { walk(it) } ?: c }
            return JsonObject(o + ("children" to JsonArray(updated)))
        }
        val result = walk(root)
        return if (found) result else null
    }

    /** `panel_attach`'s `values`: every toggle and slider, by id (§4.4). */
    fun values(root: Widget): Map<String, JsonPrimitive> = root.walk().mapNotNull { w ->
        when (w) {
            is Widget.Toggle -> w.id to JsonPrimitive(w.value)
            is Widget.Slider -> w.id to Rules.numberValue(w.value)
            else -> null
        }
    }.toMap()

    /** Checks a `set` object for step 7: no `type`, `id` or `children` (§4.5). */
    fun checkSet(set: JsonObject) {
        NOT_SETTABLE.firstOrNull { it in set }?.let { invalid("$it can't be set") }
    }
}
