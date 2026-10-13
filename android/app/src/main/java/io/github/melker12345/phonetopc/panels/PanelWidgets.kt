package io.github.melker12345.phonetopc.panels

import android.os.SystemClock
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.toggleable
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.rounded.Send
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonColors
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.LocalContentColor
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Slider
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableFloatStateOf
import androidx.compose.runtime.mutableLongStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.style.TextAlign as UiTextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import io.github.melker12345.phonetopc.R
import io.github.melker12345.phonetopc.output.displayName
import io.github.melker12345.phonetopc.protocol.panels.PanelRef
import io.github.melker12345.phonetopc.protocol.panels.PanelService
import io.github.melker12345.phonetopc.protocol.panels.PanelSnapshot
import io.github.melker12345.phonetopc.protocol.panels.TextAlign
import io.github.melker12345.phonetopc.protocol.panels.TextSize
import io.github.melker12345.phonetopc.protocol.panels.Tone
import io.github.melker12345.phonetopc.protocol.panels.Widget
import kotlin.math.roundToInt

/** What every widget needs besides itself. */
private class PanelContext(val snapshot: PanelSnapshot, val panels: PanelService, val pcName: String) {
    val ref: PanelRef = snapshot.ref

    /** Detached (§4.1): widgets are greyed and disabled, canvases keep taking ink. */
    val attached: Boolean get() = snapshot.attached
}

/** The panel's layout, filling [modifier] (PANELS.md §5). */
@Composable
fun PanelContent(snapshot: PanelSnapshot, panels: PanelService, modifier: Modifier) {
    val ctx = PanelContext(snapshot, panels, snapshot.pc.displayName(LocalContext.current))
    Box(modifier) { RenderWidget(ctx, snapshot.layout, Modifier.fillMaxSize(), inner = Modifier.fillMaxSize()) }
}

/** Whether a widget wants all the space it can get: a canvas, or a container holding one. */
private fun fills(w: Widget): Boolean = when (w) {
    is Widget.Canvas -> true
    is Widget.Column -> w.children.any(::fills)
    is Widget.Row -> w.children.any(::fills)
    is Widget.Grid -> w.children.any(::fills)
    else -> false
}

/** §5: `weight`, or 1 for a widget that fills (a canvas without a weight counts as 1). */
private fun weightOf(w: Widget): Float? = w.weight?.toFloat() ?: if (fills(w)) 1f else null

/**
 * [modifier] places the widget in its parent; [inner] sizes a leaf inside that place: filling a
 * grid cell, the width of a column, or its natural size in a row. [sized] says whether the parent
 * gave the widget its height (the root, a grid cell, a weighted child of a column, or a child of
 * such a row) rather than asking for its natural height.
 */
@Composable
private fun RenderWidget(ctx: PanelContext, w: Widget, modifier: Modifier, inner: Modifier, sized: Boolean = true) {
    when (w) {
        is Widget.Column -> {
            val weighted = w.children.any { weightOf(it) != null }
            // Without weights a column has natural heights and may be taller than the screen.
            val m = if (weighted) modifier else modifier.verticalScroll(rememberScrollState())
            Column(m, verticalArrangement = Arrangement.spacedBy(8.dp)) {
                w.children.forEach { child ->
                    val weight = weightOf(child)
                    val place = weight?.let { Modifier.weight(it).fillMaxWidth() } ?: Modifier.fillMaxWidth()
                    RenderWidget(ctx, child, place, Modifier.fillMaxWidth(), sized = weight != null)
                }
            }
        }
        is Widget.Row -> Row(modifier, horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
            w.children.forEach { child ->
                val weight = weightOf(child)
                // Filling the height of a row that has none of its own would make the row take
                // all the height a column offers it, leaving nothing for the column's weighted
                // children, a canvas for example.
                val place = weight?.let { if (sized) Modifier.weight(it).fillMaxHeight() else Modifier.weight(it) } ?: Modifier
                RenderWidget(ctx, child, place, if (weight != null) Modifier.fillMaxWidth() else Modifier, sized = sized && weight != null)
            }
        }
        is Widget.Grid -> Column(modifier, verticalArrangement = Arrangement.spacedBy(8.dp)) {
            // Cells are all the same width. Given a height (the root, a weight), the rows share it
            // and widgets fill their cell; otherwise rows are as tall as their tallest widget, and
            // widgets fill only the cell's width, so a row of buttons stays a row of buttons.
            w.children.chunked(w.cols).forEach { row ->
                Row(
                    if (sized) Modifier.weight(1f).fillMaxWidth() else Modifier.fillMaxWidth(),
                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    row.forEach { child ->
                        val cell = if (sized) Modifier.weight(1f).fillMaxHeight() else Modifier.weight(1f)
                        Box(cell, contentAlignment = Alignment.Center) {
                            val fill = if (sized) Modifier.fillMaxSize() else Modifier.fillMaxWidth()
                            RenderWidget(ctx, child, fill, fill, sized = sized)
                        }
                    }
                    repeat(w.cols - row.size) { Spacer(Modifier.weight(1f)) }
                }
            }
        }
        is Widget.Canvas -> PanelCanvas(ctx.ref, w, ctx.snapshot.canvases[w.id], ctx.panels, modifier)
        else -> Box(if (ctx.attached) modifier else modifier.alpha(0.38f), contentAlignment = Alignment.Center) {
            Leaf(ctx, w, inner)
        }
    }
}

@Composable
private fun Leaf(ctx: PanelContext, w: Widget, modifier: Modifier) {
    val enabled = w.enabled && ctx.attached
    when (w) {
        is Widget.Text -> Text(
            w.text,
            style = textStyle(w.size),
            color = toneColor(w.tone) ?: LocalContentColor.current,
            textAlign = when (w.align) {
                TextAlign.START -> UiTextAlign.Start
                TextAlign.CENTER -> UiTextAlign.Center
                TextAlign.END -> UiTextAlign.End
            },
            modifier = modifier,
        )
        is Widget.Button -> {
            val m = modifier.heightIn(min = 48.dp)
            val onClick = { ctx.panels.onPress(ctx.ref, w.id) }
            // Material's 24 dp each side leaves a narrow grid cell almost no room for the label.
            val padding = PaddingValues(horizontal = 8.dp, vertical = 8.dp)
            val label: @Composable () -> Unit = { Text(w.label, textAlign = UiTextAlign.Center, maxLines = 3, overflow = TextOverflow.Ellipsis) }
            if (w.tone == Tone.DEFAULT) {
                FilledTonalButton(onClick, m, enabled = enabled, shape = MaterialTheme.shapes.large, contentPadding = padding) { label() }
            } else {
                Button(onClick, m, enabled = enabled, shape = MaterialTheme.shapes.large, colors = buttonColors(w.tone), contentPadding = padding) { label() }
            }
        }
        is Widget.Toggle -> Row(
            modifier
                .toggleable(w.value, enabled = enabled, role = Role.Switch) { ctx.panels.onToggle(ctx.ref, w.id, it) }
                .padding(vertical = 8.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text(w.label, Modifier.weight(1f, fill = false), style = MaterialTheme.typography.bodyLarge)
            Spacer(Modifier.width(12.dp))
            Switch(checked = w.value, onCheckedChange = null, enabled = enabled)
        }
        is Widget.Slider -> SliderWidget(ctx, w, enabled, modifier.widthIn(min = 160.dp))
        is Widget.Progress -> Column(modifier.widthIn(min = 120.dp)) {
            if (w.label.isNotEmpty()) Text(w.label, style = MaterialTheme.typography.bodyMedium)
            Spacer(Modifier.height(6.dp))
            val color = toneColor(w.tone) ?: MaterialTheme.colorScheme.primary
            val value = w.value
            if (value == null) {
                LinearProgressIndicator(Modifier.fillMaxWidth(), color = color)
            } else {
                LinearProgressIndicator(progress = { value.toFloat() }, modifier = Modifier.fillMaxWidth(), color = color)
            }
        }
        is Widget.TextInput -> TextInputWidget(ctx, w, enabled, modifier.widthIn(min = 200.dp))
        else -> Unit // containers and the canvas are handled by RenderWidget
    }
}

/** §6.2: the slider moves on screen at once and sends at most ~20 changes a second, then a final one. */
@Composable
private fun SliderWidget(ctx: PanelContext, w: Widget.Slider, enabled: Boolean, modifier: Modifier) {
    var local by remember(ctx.snapshot.instance, w.id) { mutableFloatStateOf(w.value.toFloat()) }
    var dragging by remember { mutableStateOf(false) }
    var lastSent by remember { mutableLongStateOf(0L) }
    // An update from the PC moves the slider, unless the user is dragging it.
    LaunchedEffect(w.value) { if (!dragging) local = w.value.toFloat() }
    val steps = w.step?.let { s -> ((w.max - w.min) / s).roundToInt().takeIf { it in 1..200 }?.minus(1) } ?: 0
    Column(modifier, verticalArrangement = Arrangement.Center) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            if (w.label.isNotEmpty()) Text(w.label, Modifier.weight(1f), style = MaterialTheme.typography.bodyLarge)
            else Spacer(Modifier.weight(1f))
            Text(formatNumber(w.normalize(local.toDouble())), style = MaterialTheme.typography.labelLarge)
        }
        Slider(
            value = local,
            onValueChange = { v ->
                dragging = true
                local = v
                val now = SystemClock.uptimeMillis()
                if (now - lastSent >= SLIDER_INTERVAL_MS) {
                    lastSent = now
                    ctx.panels.onSlide(ctx.ref, w.id, v.toDouble(), final = false)
                }
            },
            onValueChangeFinished = {
                dragging = false
                ctx.panels.onSlide(ctx.ref, w.id, local.toDouble(), final = true)
            },
            valueRange = w.min.toFloat()..w.max.toFloat(),
            steps = steps,
            enabled = enabled,
        )
    }
}

/** §5: what is typed goes to the PC only on send, and the field says so. No password field. */
@Composable
private fun TextInputWidget(ctx: PanelContext, w: Widget.TextInput, enabled: Boolean, modifier: Modifier) {
    var text by rememberSaveable(ctx.snapshot.instance, w.id) { mutableStateOf(w.value) }
    LaunchedEffect(w.value) { text = w.value }
    val submit = {
        ctx.panels.onSubmit(ctx.ref, w.id, text)
        if (w.clearOnSubmit) text = ""
    }
    OutlinedTextField(
        value = text,
        onValueChange = { if (it.codePointCount(0, it.length) <= w.maxLength) text = if (w.multiline) it else it.replace("\n", "") },
        modifier = modifier,
        enabled = enabled,
        label = w.label.takeIf { it.isNotEmpty() }?.let { { Text(it) } },
        placeholder = w.placeholder.takeIf { it.isNotEmpty() }?.let { { Text(it) } },
        supportingText = { Text(stringResource(R.string.panel_sent_to, ctx.pcName)) },
        singleLine = !w.multiline,
        minLines = if (w.multiline) 3 else 1,
        keyboardOptions = KeyboardOptions(imeAction = if (w.multiline) ImeAction.Default else ImeAction.Send),
        keyboardActions = KeyboardActions(onSend = { submit() }),
        trailingIcon = {
            IconButton(onClick = submit, enabled = enabled) {
                Icon(Icons.AutoMirrored.Rounded.Send, contentDescription = stringResource(R.string.panel_send))
            }
        },
    )
}

@Composable
private fun textStyle(size: TextSize): TextStyle = when (size) {
    TextSize.SMALL -> MaterialTheme.typography.bodySmall
    TextSize.BODY -> MaterialTheme.typography.bodyLarge
    TextSize.TITLE -> MaterialTheme.typography.headlineSmall
    TextSize.DISPLAY -> MaterialTheme.typography.displayMedium
}

/** The theme's color for a tone; `null` for the default. */
@Composable
private fun toneColor(tone: Tone): Color? {
    val dark = isSystemInDarkTheme()
    return when (tone) {
        Tone.DEFAULT -> null
        Tone.ACCENT -> MaterialTheme.colorScheme.primary
        Tone.GOOD -> if (dark) Color(0xFF81C995) else Color(0xFF1E7B34)
        Tone.WARN -> if (dark) Color(0xFFFDD663) else Color(0xFF9A6700)
        Tone.BAD -> MaterialTheme.colorScheme.error
    }
}

@Composable
private fun buttonColors(tone: Tone): ButtonColors = when (tone) {
    Tone.BAD -> ButtonDefaults.buttonColors(MaterialTheme.colorScheme.error, MaterialTheme.colorScheme.onError)
    Tone.GOOD, Tone.WARN -> {
        val container = toneColor(tone)!!
        ButtonDefaults.buttonColors(container, if (isSystemInDarkTheme()) Color.Black else Color.White)
    }
    else -> ButtonDefaults.buttonColors()
}

private fun formatNumber(v: Double): String =
    if (v == Math.rint(v) && kotlin.math.abs(v) < 1e15) v.toLong().toString() else "%.2f".format(v)

private const val SLIDER_INTERVAL_MS = 50L
