package io.github.melker12345.phonetopc.protocol.panels

import io.github.melker12345.phonetopc.protocol.Reading
import io.github.melker12345.phonetopc.protocol.RpcError
import io.github.melker12345.phonetopc.protocol.RpcException
import io.github.melker12345.phonetopc.protocol.SensorCatalog
import io.github.melker12345.phonetopc.protocol.panels.Rules.array
import io.github.melker12345.phonetopc.protocol.panels.Rules.choice
import io.github.melker12345.phonetopc.protocol.panels.Rules.id
import io.github.melker12345.phonetopc.protocol.panels.Rules.invalid
import io.github.melker12345.phonetopc.protocol.panels.Rules.requiredId
import io.github.melker12345.phonetopc.protocol.panels.Rules.text
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

/** One button of a notification (PANELS.md §3.1). */
data class NotificationAction(val id: String, val label: String)

/** What the phone shows for a `notify` (PANELS.md §3.1). */
data class PcNotification(
    val pc: PcRef,
    val id: String,
    val title: String,
    val body: String,
    val actions: List<NotificationAction>,
    val highPriority: Boolean,
)

/** Validated `notify` params; [id] `null` means the phone picks one. */
data class NotifyParams(
    val title: String,
    val body: String,
    val id: String?,
    val actions: List<NotificationAction>,
    val highPriority: Boolean,
) {
    companion object {
        private val PRIORITIES = mapOf("default" to Priority.DEFAULT, "high" to Priority.HIGH)

        private enum class Priority { DEFAULT, HIGH }

        /** Step 7 of PROTOCOL.md §2: every rule of §3.1 and §8; throws -32602. */
        fun parse(params: JsonObject): NotifyParams {
            val title = params.text("title", 1, 100, newlines = false) ?: invalid("Missing required param: title")
            val body = params.text("body", 0, 1000, newlines = true) ?: ""
            val id = params.id("id")
            val actions = (params.array("actions") ?: emptyList()).map { a ->
                val o = a as? JsonObject ?: invalid("actions must be objects")
                NotificationAction(
                    o.requiredId("id"),
                    o.text("label", 1, 30, newlines = false) ?: invalid("an action needs a label"),
                )
            }
            if (actions.size > 3) invalid("at most 3 actions")
            if (actions.map { it.id }.toSet().size != actions.size) invalid("action ids must be unique")
            val priority = params.choice("priority", PRIORITIES) ?: Priority.DEFAULT
            return NotifyParams(title, body, id, actions, priority == Priority.HIGH)
        }
    }
}

/** Platform side: shows and removes notifications (Android NotificationManager, a fake in tests). */
interface PcNotifier {
    /** Shows [notification], replacing one with the same PC and id. */
    fun post(notification: PcNotification)

    fun cancel(pc: PcRef, id: String)
}

/**
 * `notify` and `notify_cancel` (PANELS.md §3) for all sessions: ids, the limit of
 * [maxPerPc] notifications per PC, a rate limit, and the events the user's actions produce.
 *
 * @param deliver sends a `notification` event to every session of that PC (§2).
 */
class NotificationService(
    private val permissions: OutputPermissions,
    private val notifier: PcNotifier,
    private val clock: OutputClock = OutputClock.System,
    val maxPerPc: Int = 16,
    /** §3.1: SHOULD allow at least 30 a minute. */
    val perMinute: Int = 60,
) {
    private val lock = Any()

    /** Shown notifications per PC key, oldest first. */
    private val shown = HashMap<String, LinkedHashMap<String, PcNotification>>()
    private val recent = HashMap<String, ArrayDeque<Long>>()
    private var nextId = 1L

    @Volatile private var deliver: (PcRef, Reading) -> Unit = { _, _ -> }

    /** Called by the hub that routes events to sessions. */
    fun bind(deliver: (PcRef, Reading) -> Unit) {
        this.deliver = deliver
    }

    /** Posts a notification; returns its id. Throws -32030 or -32033. */
    fun notify(pc: PcRef, params: NotifyParams): String {
        if (!permissions.allowed(pc, Feature.NOTIFY)) notAllowed(Feature.NOTIFY)
        val removed = ArrayList<String>()
        val notification = synchronized(lock) {
            val window = recent.getOrPut(pc.key) { ArrayDeque() }
            val now = clock.elapsedMillis()
            while (window.isNotEmpty() && now - window.first() >= 60_000) window.removeFirst()
            if (window.size >= perMinute) {
                throw RpcException(RpcError.RATE_LIMITED, "More than $perMinute notifications a minute; try again later")
            }
            window.addLast(now)
            val mine = shown.getOrPut(pc.key) { LinkedHashMap() }
            val id = params.id ?: generateSequence { "n-${nextId++}" }.first { it !in mine }
            mine.remove(id)
            val n = PcNotification(pc, id, params.title, params.body, params.actions, params.highPriority)
            mine[id] = n
            while (mine.size > maxPerPc) {
                val oldest = mine.keys.first()
                mine.remove(oldest)
                removed += oldest
            }
            n
        }
        removed.forEach { notifier.cancel(pc, it) }
        notifier.post(notification)
        return notification.id
    }

    /** `notify_cancel` (§3.3): fine when there is no such notification. */
    fun cancel(pc: PcRef, id: String) {
        if (!permissions.allowed(pc, Feature.NOTIFY)) notAllowed(Feature.NOTIFY)
        val had = synchronized(lock) { shown[pc.key]?.remove(id) != null }
        if (had) notifier.cancel(pc, id)
    }

    /** The user pressed an action (§3.2). Ignored if the notification isn't ours any more. */
    fun onAction(pc: PcRef, id: String, action: String) = userEnded(pc, id, "action") { put("action", action) }

    fun onTap(pc: PcRef, id: String) = userEnded(pc, id, "tap") {}

    fun onDismiss(pc: PcRef, id: String) = userEnded(pc, id, "dismiss") {}

    private fun userEnded(pc: PcRef, id: String, type: String, extra: kotlinx.serialization.json.JsonObjectBuilder.() -> Unit) {
        val had = synchronized(lock) { shown[pc.key]?.remove(id) != null }
        if (!had) return
        if (type != "dismiss") notifier.cancel(pc, id)
        val value: JsonElement = buildJsonObject {
            put("notification", id)
            extra()
        }
        deliver(pc, Reading(SensorCatalog.NOTIFICATION, type, value, "", clock.wallMillis(), clock.monoNanos()))
    }

    /** The user changed permissions: remove the notifications of PCs that may no longer post (§1). */
    fun permissionsChanged() {
        val gone = synchronized(lock) {
            val list = ArrayList<PcNotification>()
            val it = shown.values.iterator()
            while (it.hasNext()) {
                val mine = it.next()
                val pc = mine.values.firstOrNull()?.pc ?: continue
                if (!permissions.allowed(pc, Feature.NOTIFY)) {
                    list += mine.values
                    it.remove()
                }
            }
            list
        }
        gone.forEach { notifier.cancel(it.pc, it.id) }
    }

    /** What is shown now, for tests and the UI. */
    fun shown(pc: PcRef): List<PcNotification> = synchronized(lock) { shown[pc.key]?.values?.toList().orEmpty() }
}
