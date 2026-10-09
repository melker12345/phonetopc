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
 *
 * A subscription is created *pending*: it is already routed (so the hardware can start and
 * nothing is missed) but holds back events until [start], which the session calls after it
 * queued the `subscribe` response. That guarantees the response goes out first (spec §4.3).
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
    private enum class State { PENDING, LIVE, INACTIVE }

    private val downsampler = rateHz?.let { Downsampler(it) }
    private var seq = 0L
    private var lastSent: Reading? = null
    private var state = State.PENDING

    /** on_change readings that arrived while pending; only the newest for stateful sources. */
    private val held = ArrayList<Reading>()

    val continuous: Boolean get() = spec.mode == SensorMode.CONTINUOUS

    internal fun offer(reading: Reading, deviceId: String) {
        synchronized(this) {
            when (state) {
                State.INACTIVE -> Unit
                // Continuous samples from before the response are simply not part of the stream.
                State.PENDING -> if (!continuous) {
                    if (spec.stateful) held.clear()
                    held += reading
                }
                State.LIVE -> send(reading, deviceId)
            }
        }
    }

    /**
     * Go live once the `subscribe` response is queued. For a stateful source the first event
     * (seq 0) is the current value: the newer of [current] (the source's cached value) and any
     * reading that arrived while pending. Event-like sources only deliver what was held.
     */
    internal fun start(current: Reading?, deviceId: String) {
        synchronized(this) {
            if (state != State.PENDING) return
            state = State.LIVE
            val initial = if (spec.stateful) {
                listOfNotNull(current, held.lastOrNull()).maxByOrNull { it.tsMonoNs }?.let { listOf(it) }.orEmpty()
            } else {
                held.toList()
            }
            held.clear()
            for (reading in initial) send(reading, deviceId)
        }
    }

    private fun send(reading: Reading, deviceId: String) {
        if (type != null && reading.type != type) return
        val last = lastSent
        // The initial value can reach a stateful subscription twice (from the cache and from
        // the listener that just started), so drop readings that are older than the last one
        // sent or don't change the state.
        if (spec.stateful && last != null && (reading.tsMonoNs < last.tsMonoNs || reading.value == last.value)) return
        if (downsampler != null && !downsampler.isDue(reading.tsMonoNs)) return
        if (minDelta != null && last != null && distance(last.value, reading.value) < minDelta) return
        downsampler?.emitted(reading.tsMonoNs)
        lastSent = reading
        // seq counts events that passed the filters above. It is consumed even if the session
        // then drops the event under backpressure, so drops show up as gaps (spec §5).
        val event = Rpc.notification("event", reading.toJson(deviceId, id, seq++))
        session.enqueueEvent(event, continuous)
    }

    /** After this returns, no further event of this subscription is enqueued. */
    internal fun deactivate() {
        synchronized(this) {
            state = State.INACTIVE
            held.clear()
        }
    }

    companion object {
        /**
         * Euclidean distance for vectors (vec3, quat), absolute difference for numbers.
         * Other shapes (battery object, bool) are not comparable and always count as changed,
         * so min_delta doesn't apply to them (spec §4.3).
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
