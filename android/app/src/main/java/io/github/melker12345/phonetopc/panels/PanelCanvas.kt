package io.github.melker12345.phonetopc.panels

import android.os.Build
import android.os.Handler
import android.os.Looper
import android.os.SystemClock
import android.view.MotionEvent
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.ExperimentalComposeUiApi
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clipToBounds
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.StrokeJoin
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.input.pointer.pointerInteropFilter
import androidx.compose.ui.layout.onSizeChanged
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.IntSize
import androidx.compose.ui.unit.dp
import io.github.melker12345.phonetopc.R
import io.github.melker12345.phonetopc.protocol.panels.CanvasState
import io.github.melker12345.phonetopc.protocol.panels.PanelRef
import io.github.melker12345.phonetopc.protocol.panels.PanelService
import io.github.melker12345.phonetopc.protocol.panels.StrokePhase
import io.github.melker12345.phonetopc.protocol.panels.StrokePoint
import io.github.melker12345.phonetopc.protocol.panels.StrokeTool
import io.github.melker12345.phonetopc.protocol.panels.Widget

/** A stroke as drawn on screen: canvas-relative points, and the ink it was drawn with. */
private class InkStroke(val color: Int, val widthDp: Float) {
    val points = ArrayList<Offset>()
}

/** What a canvas shows: kept strokes from the service, then the user's own as they're drawn. */
private class InkModel(initial: List<InkStroke>) {
    val strokes = LinkedHashMap<Long, InkStroke>().apply { initial.forEachIndexed { i, s -> put(-1L - i, s) } }
}

/**
 * A canvas widget (PANELS.md §7): draws ink locally at once and streams the strokes to the PC in
 * per-frame chunks, one stroke per finger or stylus, with `ink: false` drawing nothing (a
 * touchpad). It takes ink while the panel is detached too; only `enabled: false` or a full
 * canvas stops it.
 */
@OptIn(ExperimentalComposeUiApi::class)
@Composable
fun PanelCanvas(ref: PanelRef, w: Widget.Canvas, state: CanvasState?, panels: PanelService, modifier: Modifier) {
    val widget by rememberUpdatedState(w)
    val density = LocalDensity.current.density
    var size by remember { mutableStateOf(IntSize.Zero) }
    var redraw by remember { mutableIntStateOf(0) }
    val clears = state?.clears ?: 0

    // Rebuilt when the panel is replaced or the PC clears the canvas: then only kept strokes show.
    val model = remember(ref, w.id, clears) {
        InkModel(
            panels.strokes(ref, w.id).map { s ->
                InkStroke(w.color, w.width.toFloat()).apply { s.points.forEach { points += Offset(it.x, it.y) } }
            },
        )
    }
    val handler = remember { Handler(Looper.getMainLooper()) }
    val batcher = remember(model) {
        StrokeBatcher(object : StrokeBatcher.Sink {
            override fun start(tool: StrokeTool, first: StrokePoint): Long? =
                panels.onStrokeStart(ref, widget.id, tool, size.width / density, size.height / density, listOf(first))

            override fun send(strokeId: Long, phase: StrokePhase, points: List<StrokePoint>) =
                panels.onStroke(ref, widget.id, strokeId, phase, points)
        })
    }
    val flush = remember(batcher) {
        object : Runnable {
            override fun run() {
                batcher.flush(SystemClock.uptimeMillis())?.let { handler.postDelayed(this, it) }
            }
        }
    }
    DisposableEffect(batcher) {
        onDispose {
            handler.removeCallbacks(flush)
            batcher.cancelAll()
        }
    }

    fun handle(ev: MotionEvent): Boolean {
        if (!widget.enabled || size.width == 0 || size.height == 0) return false
        val wPx = size.width.toFloat()
        val hPx = size.height.toFloat()
        fun ink(id: Long?, x: Float, y: Float) {
            if (id == null || !widget.ink) return
            model.strokes.getOrPut(id) { InkStroke(widget.color, widget.width.toFloat()) }.points += Offset(x / wPx, y / hPx)
        }
        when (ev.actionMasked) {
            MotionEvent.ACTION_DOWN, MotionEvent.ACTION_POINTER_DOWN -> {
                val i = ev.actionIndex
                val id = batcher.down(ev.getPointerId(i), toolOf(ev.getToolType(i)), ev.getX(i) / wPx, ev.getY(i) / hPx, ev.getPressure(i), ev.eventTime)
                ink(id, ev.getX(i), ev.getY(i))
            }
            MotionEvent.ACTION_MOVE -> {
                // Android batches the points since the last frame into the event's history.
                for (h in 0 until ev.historySize) {
                    for (p in 0 until ev.pointerCount) {
                        val x = ev.getHistoricalX(p, h)
                        val y = ev.getHistoricalY(p, h)
                        ink(batcher.move(ev.getPointerId(p), x / wPx, y / hPx, ev.getHistoricalPressure(p, h), ev.getHistoricalEventTime(h)), x, y)
                    }
                }
                for (p in 0 until ev.pointerCount) {
                    ink(batcher.move(ev.getPointerId(p), ev.getX(p) / wPx, ev.getY(p) / hPx, ev.getPressure(p), ev.eventTime), ev.getX(p), ev.getY(p))
                }
            }
            MotionEvent.ACTION_UP, MotionEvent.ACTION_POINTER_UP -> {
                val i = ev.actionIndex
                val canceled = Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU && ev.flags and MotionEvent.FLAG_CANCELED != 0
                if (canceled) {
                    batcher.cancel(ev.getPointerId(i))
                } else {
                    ink(batcher.up(ev.getPointerId(i), ev.getX(i) / wPx, ev.getY(i) / hPx, ev.getPressure(i), ev.eventTime), ev.getX(i), ev.getY(i))
                }
            }
            MotionEvent.ACTION_CANCEL -> batcher.cancelAll()
        }
        handler.removeCallbacks(flush)
        batcher.flush(ev.eventTime)?.let { handler.postDelayed(flush, it) }
        redraw++
        return true
    }

    Box(modifier.clipToBounds()) {
        Canvas(
            Modifier.fillMaxSize()
                .onSizeChanged { size = it }
                .pointerInteropFilter { handle(it) },
        ) {
            redraw // read, so new ink redraws
            drawRect(Color(widget.background))
            for (s in model.strokes.values) {
                val color = Color(s.color)
                val widthPx = s.widthDp * density
                if (s.points.size == 1) {
                    drawCircle(color, widthPx / 2, Offset(s.points[0].x * this.size.width, s.points[0].y * this.size.height))
                    continue
                }
                val path = Path()
                s.points.forEachIndexed { i, p ->
                    val x = p.x * this.size.width
                    val y = p.y * this.size.height
                    if (i == 0) path.moveTo(x, y) else path.lineTo(x, y)
                }
                drawPath(path, color, style = Stroke(widthPx, cap = StrokeCap.Round, join = StrokeJoin.Round))
            }
        }
        if (state?.full == true) {
            Surface(
                Modifier.align(Alignment.TopCenter).padding(8.dp),
                shape = RoundedCornerShape(50),
                color = MaterialTheme.colorScheme.inverseSurface,
            ) {
                Text(stringResource(R.string.panel_canvas_full), Modifier.padding(horizontal = 12.dp, vertical = 4.dp), style = MaterialTheme.typography.labelMedium)
            }
        }
    }
}

private fun toolOf(type: Int): StrokeTool = when (type) {
    MotionEvent.TOOL_TYPE_FINGER -> StrokeTool.FINGER
    MotionEvent.TOOL_TYPE_STYLUS -> StrokeTool.STYLUS
    MotionEvent.TOOL_TYPE_ERASER -> StrokeTool.ERASER
    MotionEvent.TOOL_TYPE_MOUSE -> StrokeTool.MOUSE
    else -> StrokeTool.UNKNOWN
}
