package io.github.melker12345.phonetopc.protocol

import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.doubleOrNull
import kotlin.math.abs
import kotlin.math.sqrt

/**
 * One `subscribe` of one session. Filters (type, rate, min_delta), numbers and forwards
 * readings to its session. All mutable state is guarded by `this`.
 */
class Subscription internal constructor(
    val id: String,
    internal val session: Session,
    val spec: SensorSpec,
    /** Restrict to one event type; null means all types of the source. */
    val type: String?,
    /** Effective rate after clamping; null for on_change sources. */
    val rateHz: Double?,
    val minDelta: Double?,
) {
    private val downsampler = rateHz?.let { Downsampler(it) }
    private var seq = 0L
    private var lastSent: JsonElement? = null
    private var active = true

    val continuous: Boolean get() = spec.mode == SensorMode.CONTINUOUS

    internal fun offer(reading: Reading, deviceId: String) {
        synchronized(this) {
            if (!active) return
            if (type != null && reading.type != type) return
            if (downsampler != null && !downsampler.isDue(reading.tsMonoNs)) return
            val last = lastSent
            if (minDelta != null && last != null && distance(last, reading.value) < minDelta) return
            downsampler?.emitted(reading.tsMonoNs)
            lastSent = reading.value
            // seq is consumed even if the session then drops the event under backpressure,
            // so drops show up as gaps (spec §5).
            val event = Rpc.notification("event", reading.toJson(deviceId, id, seq++))
            session.enqueueEvent(event, continuous)
        }
    }

    /** After this returns, no further event of this subscription is enqueued. */
    internal fun deactivate() {
        synchronized(this) { active = false }
    }

    companion object {
        /**
         * Euclidean distance for vectors, absolute difference for numbers.
         * Other shapes (battery object, bool) are not comparable and always count as changed.
         */
        fun distance(a: JsonElement, b: JsonElement): Double {
            if (a is JsonPrimitive && b is JsonPrimitive && !a.isString && !b.isString) {
                val x = a.doubleOrNull ?: return Double.POSITIVE_INFINITY
                val y = b.doubleOrNull ?: return Double.POSITIVE_INFINITY
                return abs(x - y)
            }
            if (a is JsonArray && b is JsonArray && a.size == b.size) {
                var sum = 0.0
                for (i in a.indices) {
                    val x = (a[i] as? JsonPrimitive)?.doubleOrNull ?: return Double.POSITIVE_INFINITY
                    val y = (b[i] as? JsonPrimitive)?.doubleOrNull ?: return Double.POSITIVE_INFINITY
                    sum += (x - y) * (x - y)
                }
                return sqrt(sum)
            }
            return Double.POSITIVE_INFINITY
        }
    }
}

/**
 * Thins a stream of timestamped samples down to roughly [rateHz].
 *
 * Emission slots sit on a grid of one period; a sample is due once it is within a quarter
 * period of the next slot. The tolerance absorbs timestamp jitter when the hardware runs at
 * exactly the requested rate, while still rejecting samples at twice the rate. If the stream
 * stalls for more than a period, the grid re-anchors instead of bursting to catch up.
 */
internal class Downsampler(rateHz: Double) {
    private val periodNs = (1_000_000_000.0 / rateHz).toLong().coerceAtLeast(1)
    private val toleranceNs = periodNs / 4
    private var nextDueNs: Long? = null

    fun isDue(tsNs: Long): Boolean {
        val due = nextDueNs ?: return true
        return tsNs >= due - toleranceNs
    }

    fun emitted(tsNs: Long) {
        val due = nextDueNs
        nextDueNs = if (due == null || tsNs - due > periodNs) tsNs + periodNs else due + periodNs
    }
}
