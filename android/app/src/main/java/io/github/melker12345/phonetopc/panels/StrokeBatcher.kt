package io.github.melker12345.phonetopc.panels

import io.github.melker12345.phonetopc.protocol.panels.StrokePhase
import io.github.melker12345.phonetopc.protocol.panels.StrokePoint
import io.github.melker12345.phonetopc.protocol.panels.StrokeTool

/**
 * Turns a canvas's touch input into stroke chunks (PANELS.md §7.2): the touch-down point goes
 * out at once as `start`, further points are batched to about one chunk per display frame per
 * finger (no sooner than [minIntervalMs] after the previous chunk), and `end` or `cancel` goes
 * out as soon as the touch ends. Coordinates are already canvas-relative; times are the
 * input's uptime milliseconds. Not thread-safe: use it from the UI thread.
 */
class StrokeBatcher(private val sink: Sink, private val minIntervalMs: Long = 16) {
    interface Sink {
        /** A new stroke with its first point; returns its id, or `null` if the canvas takes no ink. */
        fun start(tool: StrokeTool, first: StrokePoint): Long?

        fun send(strokeId: Long, phase: StrokePhase, points: List<StrokePoint>)
    }

    private class Active(val strokeId: Long, val downTime: Long, var lastSent: Long) {
        val pending = ArrayList<StrokePoint>()
    }

    /** By pointer id. */
    private val active = HashMap<Int, Active>()

    /** A finger touched down; returns the stroke id, or `null` if the canvas refused it. */
    fun down(pointerId: Int, tool: StrokeTool, x: Float, y: Float, pressure: Float, time: Long): Long? {
        active.remove(pointerId)?.let { end(it, StrokePhase.CANCEL) } // a lost up; shouldn't happen
        val id = sink.start(tool, point(x, y, pressure, 0)) ?: return null
        active[pointerId] = Active(id, time, time)
        return id
    }

    /** A point of a moving finger; returns its stroke id, or `null` if the finger draws no stroke. */
    fun move(pointerId: Int, x: Float, y: Float, pressure: Float, time: Long): Long? {
        val a = active[pointerId] ?: return null
        a.pending += point(x, y, pressure, (time - a.downTime).coerceAtLeast(a.pending.lastOrNull()?.t ?: 0))
        return a.strokeId
    }

    /** The finger lifted at ([x], [y]); sends what is pending and `end`. */
    fun up(pointerId: Int, x: Float, y: Float, pressure: Float, time: Long): Long? {
        val id = move(pointerId, x, y, pressure, time) ?: return null
        end(active.remove(pointerId)!!, StrokePhase.END)
        return id
    }

    /** The system took the touch away (e.g. palm rejection). */
    fun cancel(pointerId: Int) {
        active.remove(pointerId)?.let { end(it, StrokePhase.CANCEL) }
    }

    fun cancelAll() {
        val all = active.values.toList()
        active.clear()
        all.forEach { end(it, StrokePhase.CANCEL) }
    }

    /**
     * Sends the pending points of every stroke whose last chunk is at least [minIntervalMs] old.
     * Returns how many ms until the next flush is due, or `null` if nothing is pending.
     */
    fun flush(now: Long): Long? {
        var next: Long? = null
        for (a in active.values) {
            if (a.pending.isEmpty()) continue
            val due = a.lastSent + minIntervalMs
            if (now >= due) {
                sink.send(a.strokeId, StrokePhase.MOVE, a.pending.toList())
                a.pending.clear()
                a.lastSent = now
            } else {
                next = minOf(next ?: Long.MAX_VALUE, due - now)
            }
        }
        return next
    }

    val drawing: Boolean get() = active.isNotEmpty()

    private fun end(a: Active, phase: StrokePhase) {
        sink.send(a.strokeId, phase, a.pending.toList())
        a.pending.clear()
    }

    private fun point(x: Float, y: Float, pressure: Float, t: Long) =
        StrokePoint(x.coerceIn(0f, 1f), y.coerceIn(0f, 1f), pressure.coerceIn(0f, 1f), t)
}
