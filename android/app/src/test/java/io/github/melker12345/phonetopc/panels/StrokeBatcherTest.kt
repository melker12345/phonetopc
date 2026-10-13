package io.github.melker12345.phonetopc.panels

import io.github.melker12345.phonetopc.protocol.panels.StrokePhase
import io.github.melker12345.phonetopc.protocol.panels.StrokePoint
import io.github.melker12345.phonetopc.protocol.panels.StrokeTool
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class StrokeBatcherTest {
    private val sent = ArrayList<Triple<Long, StrokePhase, List<StrokePoint>>>()
    private var nextId = 0L
    private var refuse = false
    private val sink = object : StrokeBatcher.Sink {
        override fun start(tool: StrokeTool, first: StrokePoint): Long? {
            if (refuse) return null
            sent += Triple(nextId, StrokePhase.START, listOf(first))
            return nextId++
        }

        override fun send(strokeId: Long, phase: StrokePhase, points: List<StrokePoint>) {
            sent += Triple(strokeId, phase, points)
        }
    }
    private val batcher = StrokeBatcher(sink, minIntervalMs = 16)

    @Test fun startGoesOutAtOnceWithTheDownPoint() {
        batcher.down(0, StrokeTool.FINGER, 0.1f, 0.2f, 0.5f, time = 1000)
        assertEquals(1, sent.size)
        assertEquals(StrokePhase.START, sent[0].second)
        assertEquals(StrokePoint(0.1f, 0.2f, 0.5f, 0), sent[0].third.single())
    }

    @Test fun movesAreBatchedPerFrame() {
        batcher.down(0, StrokeTool.FINGER, 0f, 0f, 1f, 1000)
        batcher.move(0, 0.1f, 0f, 1f, 1004)
        batcher.move(0, 0.2f, 0f, 1f, 1008)
        assertEquals(12L, batcher.flush(1004)) // too soon after start: due at 1016
        assertEquals(1, sent.size)
        assertNull(batcher.flush(1016))
        assertEquals(StrokePhase.MOVE, sent[1].second)
        assertEquals(listOf(4L, 8L), sent[1].third.map { it.t })
        assertNull(batcher.flush(1100)) // nothing pending, nothing sent
        assertEquals(2, sent.size)
    }

    @Test fun upSendsPendingPointsWithEndAtOnce() {
        batcher.down(0, StrokeTool.STYLUS, 0f, 0f, 1f, 1000)
        batcher.move(0, 0.1f, 0f, 1f, 1005)
        batcher.up(0, 0.2f, 0f, 1f, 1009)
        assertEquals(StrokePhase.END, sent.last().second)
        assertEquals(listOf(5L, 9L), sent.last().third.map { it.t })
        assertEquals(false, batcher.drawing)
    }

    @Test fun pointersAreSeparateStrokes() {
        batcher.down(0, StrokeTool.FINGER, 0f, 0f, 1f, 1000)
        batcher.down(1, StrokeTool.FINGER, 1f, 1f, 1f, 1002)
        batcher.up(1, 0.9f, 0.9f, 1f, 1003)
        batcher.cancel(0)
        assertEquals(listOf(0L to StrokePhase.START, 1L to StrokePhase.START, 1L to StrokePhase.END, 0L to StrokePhase.CANCEL), sent.map { it.first to it.second })
    }

    @Test fun refusedStrokeSendsNothing() {
        refuse = true
        assertNull(batcher.down(0, StrokeTool.FINGER, 0f, 0f, 1f, 1000))
        assertNull(batcher.move(0, 0.5f, 0.5f, 1f, 1010))
        assertNull(batcher.up(0, 0.5f, 0.5f, 1f, 1020))
        assertEquals(0, sent.size)
    }

    @Test fun clampsToTheCanvasAndKeepsTimeMonotonic() {
        batcher.down(0, StrokeTool.FINGER, -0.5f, 1.5f, 2f, 1000)
        batcher.move(0, 0.5f, 0.5f, 1f, 990) // an older historical time must not go backwards
        batcher.up(0, 0.5f, 0.5f, 1f, 1020)
        assertEquals(StrokePoint(0f, 1f, 1f, 0), sent[0].third.single())
        assertEquals(listOf(0L, 20L), sent.last().third.map { it.t })
    }
}
