package io.github.melker12345.phonetopc.panels

import android.content.Context
import android.content.Intent
import android.content.pm.ActivityInfo
import android.os.Bundle
import android.view.WindowManager
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.Close
import androidx.compose.material.icons.rounded.ViewCarousel
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.lifecycleScope
import androidx.lifecycle.repeatOnLifecycle
import io.github.melker12345.phonetopc.R
import io.github.melker12345.phonetopc.appGraph
import io.github.melker12345.phonetopc.output.displayName
import io.github.melker12345.phonetopc.protocol.panels.Orientation
import io.github.melker12345.phonetopc.protocol.panels.PanelRef
import io.github.melker12345.phonetopc.protocol.panels.PanelService
import io.github.melker12345.phonetopc.protocol.panels.PanelSnapshot
import io.github.melker12345.phonetopc.ui.theme.PhonetopcTheme
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.launch

val PanelSnapshot.ref: PanelRef get() = PanelRef(pc.key, id, instance)

/**
 * The panel screen (PANELS.md §4–§7): one panel at a time, full screen, below a frame the PC
 * can't hide that names the PC as the phone knows it, the panel's title and whether the PC is
 * connected, with a close button and, when there are several panels, a switcher.
 */
class PanelActivity : ComponentActivity() {
    private val panels: PanelService get() = appGraph.panels

    /** What the user picked; the panel actually shown is [current], which falls back if it's gone. */
    private val selected = MutableStateFlow<PanelKey?>(null)
    private val resumed = MutableStateFlow(false)

    /** The panel the service was last told is visible. */
    private var visibleRef: PanelRef? = null

    override fun onCreate(savedInstanceState: Bundle?) {
        enableEdgeToEdge()
        super.onCreate(savedInstanceState)
        selected.value = keyOf(intent) ?: savedInstanceState?.let { keyOf(it) }
        setContent {
            PhonetopcTheme {
                val all by panels.snapshots.collectAsStateWithLifecycle()
                val want by selected.collectAsStateWithLifecycle()
                val shown = current(all, want)
                if (shown != null) {
                    PanelFrame(
                        shown, all, panels,
                        onSelect = { selected.value = it.key },
                        onClose = { panels.onUserClose(shown.ref) },
                    )
                }
            }
        }
        lifecycleScope.launch {
            repeatOnLifecycle(Lifecycle.State.CREATED) {
                combine(panels.snapshots, selected, resumed) { all, want, front -> Triple(all, current(all, want), front) }
                    .collect { (all, shown, front) ->
                        if (shown == null) {
                            report(null)
                            if (all.isEmpty()) finish()
                            return@collect
                        }
                        apply(shown)
                        PanelScreen.frontPcKey = if (front) shown.pc.key else null
                        report(if (front) shown.ref else null)
                    }
            }
        }
        lifecycleScope.launch {
            repeatOnLifecycle(Lifecycle.State.RESUMED) {
                PanelScreen.switchTo.collect { selected.value = it }
            }
        }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        keyOf(intent)?.let { selected.value = it }
    }

    override fun onResume() {
        super.onResume()
        resumed.value = true
    }

    override fun onPause() {
        // Synchronously: the panel is hidden from the moment the user leaves (§6.1).
        resumed.value = false
        PanelScreen.frontPcKey = null
        report(null)
        super.onPause()
    }

    override fun onSaveInstanceState(outState: Bundle) {
        super.onSaveInstanceState(outState)
        selected.value?.let { outState.putString(EXTRA_PC, it.pcKey); outState.putString(EXTRA_PANEL, it.panel) }
    }

    /**
     * The picked panel, or if it's gone (closed, expired), another of the same PC, or any.
     * A replaced panel keeps its key, so it's redrawn in place.
     */
    private fun current(all: List<PanelSnapshot>, want: PanelKey?): PanelSnapshot? =
        all.firstOrNull { it.key == want } ?: all.firstOrNull { it.pc.key == want?.pcKey } ?: all.firstOrNull()

    /** Tells the service which panel the user sees now (§6.1): `visible` and `hidden` events. */
    private fun report(ref: PanelRef?) {
        if (ref == visibleRef) return
        visibleRef?.let { panels.onVisible(it, false) }
        visibleRef = ref
        ref?.let { panels.onVisible(it, true) }
    }

    /** keep_awake while attached, and orientation (§4.3). */
    private fun apply(shown: PanelSnapshot) {
        if (shown.keepAwake && shown.attached) {
            window.addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
        } else {
            window.clearFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
        }
        val orientation = when (shown.orientation) {
            Orientation.ANY -> ActivityInfo.SCREEN_ORIENTATION_UNSPECIFIED
            Orientation.PORTRAIT -> ActivityInfo.SCREEN_ORIENTATION_USER_PORTRAIT
            Orientation.LANDSCAPE -> ActivityInfo.SCREEN_ORIENTATION_USER_LANDSCAPE
        }
        if (requestedOrientation != orientation) requestedOrientation = orientation
    }

    companion object {
        private const val EXTRA_PC = "pc_key"
        private const val EXTRA_PANEL = "panel"

        /** On the first panel when [key] is `null`. */
        fun intent(context: Context, key: PanelKey?): Intent = Intent(context, PanelActivity::class.java).apply {
            if (key != null) putExtra(EXTRA_PC, key.pcKey).putExtra(EXTRA_PANEL, key.panel)
        }

        private fun keyOf(intent: Intent?): PanelKey? {
            val pc = intent?.getStringExtra(EXTRA_PC) ?: return null
            val panel = intent.getStringExtra(EXTRA_PANEL) ?: return null
            return PanelKey(pc, panel)
        }

        private fun keyOf(state: Bundle): PanelKey? {
            val pc = state.getString(EXTRA_PC) ?: return null
            val panel = state.getString(EXTRA_PANEL) ?: return null
            return PanelKey(pc, panel)
        }
    }
}

@Composable
private fun PanelFrame(
    shown: PanelSnapshot,
    all: List<PanelSnapshot>,
    panels: PanelService,
    onSelect: (PanelSnapshot) -> Unit,
    onClose: () -> Unit,
) {
    val context = LocalContext.current
    Surface(Modifier.fillMaxSize(), color = MaterialTheme.colorScheme.surface) {
        Column(Modifier.fillMaxSize().safeDrawingPadding()) {
            // The frame (PANELS.md §1): always there, never drawn by the PC.
            Surface(color = MaterialTheme.colorScheme.surfaceContainerHigh, modifier = Modifier.fillMaxWidth()) {
                Row(Modifier.padding(start = 16.dp, end = 4.dp, top = 6.dp, bottom = 6.dp), verticalAlignment = Alignment.CenterVertically) {
                    Column(Modifier.weight(1f)) {
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            Text(
                                shown.pc.displayName(context),
                                style = MaterialTheme.typography.labelLarge,
                                fontWeight = FontWeight.SemiBold,
                                color = if (shown.pc.isDevMode) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurface,
                                maxLines = 1, overflow = TextOverflow.Ellipsis,
                                modifier = Modifier.weight(1f, fill = false),
                            )
                            Spacer(Modifier.width(8.dp))
                            ConnectionDot(shown.attached)
                            Spacer(Modifier.width(4.dp))
                            Text(
                                stringResource(if (shown.attached) R.string.panel_connected else R.string.panel_disconnected),
                                style = MaterialTheme.typography.labelMedium,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                            )
                        }
                        Text(shown.title, style = MaterialTheme.typography.titleMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
                    }
                    if (all.size > 1) Switcher(shown, all, onSelect)
                    IconButton(onClick = onClose) {
                        Icon(Icons.Rounded.Close, contentDescription = stringResource(R.string.panel_close))
                    }
                }
            }
            Box(Modifier.fillMaxWidth().weight(1f).padding(12.dp)) {
                PanelContent(shown, panels, Modifier.fillMaxSize())
            }
        }
    }
}

@Composable
private fun ConnectionDot(attached: Boolean) {
    Surface(
        Modifier.size(8.dp), shape = CircleShape,
        color = if (attached) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.outline,
    ) {}
}

@Composable
private fun Switcher(shown: PanelSnapshot, all: List<PanelSnapshot>, onSelect: (PanelSnapshot) -> Unit) {
    val context = LocalContext.current
    var open by remember { mutableStateOf(false) }
    Box {
        IconButton(onClick = { open = true }) {
            Icon(Icons.Rounded.ViewCarousel, contentDescription = stringResource(R.string.panel_switch))
        }
        DropdownMenu(expanded = open, onDismissRequest = { open = false }) {
            for (p in all) {
                DropdownMenuItem(
                    text = {
                        Column {
                            Text(p.title, fontWeight = if (p.key == shown.key) FontWeight.Bold else null)
                            Text(p.pc.displayName(context), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                        }
                    },
                    onClick = {
                        open = false
                        onSelect(p)
                    },
                )
            }
        }
    }
}
