package io.github.melker12345.phonetopc.ui.home

import android.content.ClipData
import androidx.compose.animation.AnimatedContent
import androidx.compose.animation.animateColorAsState
import androidx.compose.animation.animateContentSize
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.Check
import androidx.compose.material.icons.automirrored.rounded.KeyboardArrowRight
import androidx.compose.material.icons.rounded.Computer
import androidx.compose.material.icons.rounded.Dashboard
import androidx.compose.material.icons.rounded.ContentCopy
import androidx.compose.material.icons.rounded.HourglassTop
import androidx.compose.material.icons.rounded.Lock
import androidx.compose.material.icons.rounded.LockOpen
import androidx.compose.material.icons.rounded.Pause
import androidx.compose.material.icons.rounded.PlayArrow
import androidx.compose.material.icons.rounded.PowerSettingsNew
import androidx.compose.material.icons.rounded.Stop
import androidx.compose.material.icons.rounded.Usb
import androidx.compose.material.icons.rounded.WifiOff
import androidx.compose.material.icons.rounded.WifiTethering
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.FilledTonalIconButton
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LargeTopAppBar
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedCard
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBarDefaults
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.input.nestedscroll.nestedScroll
import androidx.compose.ui.platform.ClipEntry
import androidx.compose.ui.platform.LocalClipboard
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.pluralStringResource
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.tooling.preview.Preview
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import io.github.melker12345.phonetopc.R
import io.github.melker12345.phonetopc.output.displayName
import io.github.melker12345.phonetopc.panels.PanelActivity
import io.github.melker12345.phonetopc.panels.PanelKey
import io.github.melker12345.phonetopc.panels.key
import io.github.melker12345.phonetopc.protocol.ClientSnapshot
import io.github.melker12345.phonetopc.protocol.Demand
import io.github.melker12345.phonetopc.protocol.PcInfo
import io.github.melker12345.phonetopc.protocol.SubscriptionSnapshot
import io.github.melker12345.phonetopc.server.DEFAULT_PORT
import io.github.melker12345.phonetopc.server.WS_PATH
import io.github.melker12345.phonetopc.service.BridgeStatus
import io.github.melker12345.phonetopc.service.RunState
import io.github.melker12345.phonetopc.ui.MainViewModel
import io.github.melker12345.phonetopc.ui.components.IconBadge
import io.github.melker12345.phonetopc.ui.components.SectionHeader
import io.github.melker12345.phonetopc.ui.formatNumber
import io.github.melker12345.phonetopc.ui.sensorUi
import io.github.melker12345.phonetopc.ui.theme.PhonetopcTheme
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

@Composable
fun HomeScreen(vm: MainViewModel, onPair: () -> Unit) {
    val settings by vm.settings.collectAsStateWithLifecycle()
    val status by vm.status.collectAsStateWithLifecycle()
    val addresses by vm.addresses.collectAsStateWithLifecycle()
    val paired by vm.pairedPcs.collectAsStateWithLifecycle()
    val panels by vm.panels.collectAsStateWithLifecycle()
    val context = LocalContext.current
    HomeContent(
        devMode = settings?.devMode ?: false,
        configuredPort = settings?.port ?: DEFAULT_PORT,
        status = status,
        addresses = addresses,
        hasPairedPcs = paired?.isNotEmpty() ?: true,
        onStart = vm::startServer,
        onStop = vm::stopServer,
        onPause = vm::setPaused,
        onPair = onPair,
        panels = panels.map { OpenPanel(it.key, it.title, it.pc.displayName(context), it.attached) },
        onOpenPanel = { context.startActivity(PanelActivity.intent(context, it)) },
    )
}

private data class OpenPanel(val key: PanelKey, val title: String, val pcName: String, val attached: Boolean)

private enum class HomeState { STOPPED, STARTING, RUNNING, PAUSED }

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun HomeContent(
    devMode: Boolean,
    configuredPort: Int,
    status: BridgeStatus,
    addresses: List<String>,
    hasPairedPcs: Boolean,
    onStart: () -> Unit,
    onStop: () -> Unit,
    onPause: (Boolean) -> Unit,
    onPair: () -> Unit,
    panels: List<OpenPanel> = emptyList(),
    onOpenPanel: (PanelKey) -> Unit = {},
) {
    // While running, the mode the server actually runs in; otherwise the one it will start in.
    val secure = status.secure ?: !devMode
    val state = when (status.state) {
        RunState.STOPPED -> HomeState.STOPPED
        RunState.STARTING -> HomeState.STARTING
        RunState.RUNNING -> HomeState.RUNNING
        RunState.PAUSED -> HomeState.PAUSED
    }
    val scrollBehavior = TopAppBarDefaults.exitUntilCollapsedScrollBehavior()
    Scaffold(
        modifier = Modifier.nestedScroll(scrollBehavior.nestedScrollConnection),
        topBar = {
            LargeTopAppBar(
                title = { Text(stringResource(R.string.app_name)) },
                scrollBehavior = scrollBehavior,
            )
        },
    ) { padding ->
        LazyColumn(
            contentPadding = PaddingValues(
                start = 16.dp, end = 16.dp,
                top = padding.calculateTopPadding(),
                bottom = padding.calculateBottomPadding() + 24.dp,
            ),
            verticalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            item(key = "status") {
                StatusCard(state, status, secure, hasPairedPcs, onStart, onStop, onPause, onPair)
            }
            // Panels come first: a PC showing one is waiting for the user (PANELS.md §4.3).
            items(panels, key = { "panel/${it.key.pcKey}/${it.key.panel}" }) { panel ->
                PanelCard(panel, onOpen = { onOpenPanel(panel.key) })
            }
            item(key = "address") {
                AddressCard(addresses, status.port ?: configuredPort, secure, dimmed = state != HomeState.RUNNING)
            }
            if (status.active) {
                item(key = "clients-header") {
                    SectionHeader(stringResource(R.string.home_clients_header), Modifier.padding(top = 8.dp))
                }
                if (status.clients.isEmpty()) {
                    item(key = "clients-empty") {
                        Text(
                            stringResource(if (secure) R.string.home_clients_empty_secure else R.string.home_clients_empty),
                            style = MaterialTheme.typography.bodyMedium,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                            modifier = Modifier.padding(horizontal = 4.dp),
                        )
                    }
                } else {
                    items(status.clients, key = { it.sessionId }) { ClientCard(it) }
                }
            }
        }
    }
}

@Composable
private fun StatusCard(
    state: HomeState,
    status: BridgeStatus,
    secure: Boolean,
    hasPairedPcs: Boolean,
    onStart: () -> Unit,
    onStop: () -> Unit,
    onPause: (Boolean) -> Unit,
    onPair: () -> Unit,
) {
    val scheme = MaterialTheme.colorScheme
    val (container, content) = when (state) {
        HomeState.RUNNING -> scheme.primaryContainer to scheme.onPrimaryContainer
        HomeState.PAUSED -> scheme.tertiaryContainer to scheme.onTertiaryContainer
        HomeState.STARTING -> scheme.secondaryContainer to scheme.onSecondaryContainer
        HomeState.STOPPED -> scheme.surfaceContainerHigh to scheme.onSurface
    }
    val animatedContainer by animateColorAsState(container, label = "statusContainer")
    val animatedContent by animateColorAsState(content, label = "statusContent")

    Card(
        modifier = Modifier.fillMaxWidth().animateContentSize(),
        shape = MaterialTheme.shapes.extraLarge,
        colors = CardDefaults.cardColors(containerColor = animatedContainer, contentColor = animatedContent),
    ) {
        Column(Modifier.padding(20.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                AnimatedContent(state, label = "statusIcon") { s ->
                    IconBadge(
                        icon = s.icon(),
                        size = 52.dp,
                        container = animatedContent.copy(alpha = 0.12f),
                        content = animatedContent,
                    )
                }
                Spacer(Modifier.width(16.dp))
                Column(Modifier.weight(1f)) {
                    Text(stringResource(state.title()), style = MaterialTheme.typography.headlineSmall)
                    Spacer(Modifier.height(2.dp))
                    Text(
                        subtitle(state, status),
                        style = MaterialTheme.typography.bodyMedium,
                        color = animatedContent.copy(alpha = 0.8f),
                    )
                }
            }
            Spacer(Modifier.height(16.dp))
            ModeChip(secure)
            if (secure && !hasPairedPcs) {
                Spacer(Modifier.height(12.dp))
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Text(
                        stringResource(R.string.home_no_paired),
                        style = MaterialTheme.typography.bodyMedium,
                        modifier = Modifier.weight(1f),
                    )
                    TextButton(onClick = onPair, colors = ButtonDefaults.textButtonColors(contentColor = animatedContent)) {
                        Text(stringResource(R.string.home_pair_action))
                    }
                }
            }
            status.error?.takeIf { state == HomeState.STOPPED }?.let { error ->
                Spacer(Modifier.height(12.dp))
                Text(error, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.error)
            }
            Spacer(Modifier.height(20.dp))
            Row(horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
                val big = Modifier.weight(1f).height(56.dp)
                when (state) {
                    HomeState.STOPPED -> Button(onClick = onStart, modifier = big) {
                        Icon(Icons.Rounded.PlayArrow, null, Modifier.size(ButtonDefaults.IconSize))
                        Spacer(Modifier.width(ButtonDefaults.IconSpacing))
                        Text(stringResource(R.string.home_start), style = MaterialTheme.typography.titleMedium)
                    }
                    HomeState.STARTING, HomeState.RUNNING, HomeState.PAUSED -> {
                        FilledTonalButton(
                            onClick = onStop,
                            modifier = big,
                            colors = ButtonDefaults.filledTonalButtonColors(
                                containerColor = animatedContent.copy(alpha = 0.12f),
                                contentColor = animatedContent,
                            ),
                        ) {
                            Icon(Icons.Rounded.Stop, null, Modifier.size(ButtonDefaults.IconSize))
                            Spacer(Modifier.width(ButtonDefaults.IconSpacing))
                            Text(stringResource(R.string.home_stop), style = MaterialTheme.typography.titleMedium)
                        }
                        val paused = state == HomeState.PAUSED
                        FilledTonalIconButton(
                            onClick = { onPause(!paused) },
                            enabled = state != HomeState.STARTING,
                            modifier = Modifier.size(56.dp),
                        ) {
                            Icon(
                                if (paused) Icons.Rounded.PlayArrow else Icons.Rounded.Pause,
                                contentDescription = stringResource(if (paused) R.string.action_resume else R.string.action_pause),
                            )
                        }
                    }
                }
            }
        }
    }
}

/** Secure (wss, paired PCs only) or dev mode (plain ws, anyone), always visible on Home. */
@Composable
private fun ModeChip(secure: Boolean) {
    val scheme = MaterialTheme.colorScheme
    Surface(
        shape = CircleShape,
        color = if (secure) scheme.surface.copy(alpha = 0.6f) else scheme.errorContainer,
        contentColor = if (secure) scheme.onSurface else scheme.onErrorContainer,
    ) {
        Row(Modifier.padding(horizontal = 12.dp, vertical = 6.dp), verticalAlignment = Alignment.CenterVertically) {
            Icon(if (secure) Icons.Rounded.Lock else Icons.Rounded.LockOpen, null, Modifier.size(16.dp))
            Spacer(Modifier.width(6.dp))
            Text(
                stringResource(if (secure) R.string.home_mode_secure else R.string.home_mode_dev),
                style = MaterialTheme.typography.labelLarge,
            )
        }
    }
}

private fun HomeState.icon(): ImageVector = when (this) {
    HomeState.STOPPED -> Icons.Rounded.PowerSettingsNew
    HomeState.STARTING -> Icons.Rounded.HourglassTop
    HomeState.RUNNING -> Icons.Rounded.WifiTethering
    HomeState.PAUSED -> Icons.Rounded.Pause
}

private fun HomeState.title(): Int = when (this) {
    HomeState.STOPPED -> R.string.home_state_stopped
    HomeState.STARTING -> R.string.home_state_starting
    HomeState.RUNNING -> R.string.home_state_running
    HomeState.PAUSED -> R.string.home_state_paused
}

@Composable
private fun subtitle(state: HomeState, status: BridgeStatus): String = when (state) {
    HomeState.STOPPED -> stringResource(R.string.home_sub_stopped)
    HomeState.STARTING -> stringResource(R.string.home_sub_starting)
    HomeState.PAUSED -> stringResource(R.string.home_sub_paused)
    HomeState.RUNNING -> {
        val n = status.clients.size
        val clients = if (n == 0) {
            stringResource(R.string.home_sub_waiting)
        } else {
            pluralStringResource(R.plurals.home_clients_connected, n, n)
        }
        val streams = status.streaming.entries.sortedBy { it.key }.map { (src, demand) -> streamLabel(src, demand) }
        (listOf(clients) + streams).joinToString(" · ")
    }
}

@Composable
private fun streamLabel(source: String, demand: Demand): String {
    val name = stringResource(sensorUi(source).label)
    val rate = demand.rateHz ?: return name
    return stringResource(R.string.home_streaming_at, name, formatHz(rate))
}

private fun formatHz(rate: Double): String = formatNumber(rate, decimals = 1)

@Composable
private fun AddressCard(addresses: List<String>, port: Int, secure: Boolean, dimmed: Boolean) {
    val scheme = if (secure) "wss" else "ws"
    val clipboard = LocalClipboard.current
    val scope = rememberCoroutineScope()
    var copied by remember { mutableStateOf(false) }
    LaunchedEffect(copied) {
        if (copied) {
            delay(1_500)
            copied = false
        }
    }
    val primaryText = if (dimmed) MaterialTheme.colorScheme.onSurfaceVariant else MaterialTheme.colorScheme.onSurface

    Card(
        modifier = Modifier.fillMaxWidth(),
        shape = MaterialTheme.shapes.extraLarge,
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerLow),
    ) {
        Column(Modifier.padding(20.dp)) {
            Text(
                stringResource(R.string.home_connect_from_pc),
                style = MaterialTheme.typography.labelLarge,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Spacer(Modifier.height(8.dp))
            val main = addresses.firstOrNull()
            if (main == null) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Icon(Icons.Rounded.WifiOff, null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
                    Spacer(Modifier.width(12.dp))
                    Text(
                        stringResource(R.string.home_no_network),
                        style = MaterialTheme.typography.titleMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            } else {
                val hostPort = "$main:$port"
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Text(
                        hostPort,
                        style = MaterialTheme.typography.headlineMedium,
                        fontFamily = FontFamily.Monospace,
                        fontWeight = FontWeight.SemiBold,
                        color = primaryText,
                        modifier = Modifier.weight(1f),
                    )
                    IconButton(onClick = {
                        scope.launch {
                            clipboard.setClipEntry(ClipEntry(ClipData.newPlainText("phonetopc", "$scheme://$hostPort$WS_PATH")))
                            copied = true
                        }
                    }) {
                        Icon(
                            if (copied) Icons.Rounded.Check else Icons.Rounded.ContentCopy,
                            contentDescription = stringResource(R.string.home_copy_address),
                        )
                    }
                }
                Text(
                    "$scheme://$hostPort$WS_PATH",
                    style = MaterialTheme.typography.bodySmall,
                    fontFamily = FontFamily.Monospace,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                if (addresses.size > 1) {
                    Spacer(Modifier.height(4.dp))
                    Text(
                        stringResource(R.string.home_also_reachable, addresses.drop(1).joinToString(", ")),
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
            HorizontalDivider(Modifier.padding(vertical = 16.dp), color = MaterialTheme.colorScheme.outlineVariant)
            Row(verticalAlignment = Alignment.Top) {
                Icon(Icons.Rounded.Usb, null, tint = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(top = 2.dp))
                Spacer(Modifier.width(12.dp))
                Column {
                    Text(stringResource(R.string.home_usb_title), style = MaterialTheme.typography.titleSmall)
                    Text(
                        stringResource(R.string.home_usb_body, port),
                        style = MaterialTheme.typography.bodySmall,
                        fontFamily = FontFamily.Monospace,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
        }
    }
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun PanelCard(panel: OpenPanel, onOpen: () -> Unit) {
    Card(
        onClick = onOpen,
        modifier = Modifier.fillMaxWidth(),
        shape = MaterialTheme.shapes.extraLarge,
        colors = CardDefaults.cardColors(
            containerColor = MaterialTheme.colorScheme.secondaryContainer,
            contentColor = MaterialTheme.colorScheme.onSecondaryContainer,
        ),
    ) {
        Row(Modifier.padding(16.dp), verticalAlignment = Alignment.CenterVertically) {
            IconBadge(
                Icons.Rounded.Dashboard,
                container = MaterialTheme.colorScheme.onSecondaryContainer.copy(alpha = 0.12f),
                content = MaterialTheme.colorScheme.onSecondaryContainer,
            )
            Spacer(Modifier.width(16.dp))
            Column(Modifier.weight(1f)) {
                Text(panel.title, style = MaterialTheme.typography.titleMedium)
                Text(
                    stringResource(
                        R.string.home_panel_from,
                        panel.pcName,
                        stringResource(if (panel.attached) R.string.panel_connected else R.string.panel_disconnected),
                    ),
                    style = MaterialTheme.typography.bodySmall,
                )
            }
            Spacer(Modifier.width(8.dp))
            Text(stringResource(R.string.home_panel_open), style = MaterialTheme.typography.labelLarge)
            Icon(Icons.AutoMirrored.Rounded.KeyboardArrowRight, contentDescription = null)
        }
    }
}

@Composable
private fun ClientCard(client: ClientSnapshot) {
    OutlinedCard(Modifier.fillMaxWidth(), shape = MaterialTheme.shapes.large) {
        Column(Modifier.padding(16.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                IconBadge(Icons.Rounded.Computer)
                Spacer(Modifier.width(16.dp))
                Column(Modifier.weight(1f)) {
                    val clientName = client.clientName
                    val clientVersion = client.clientVersion
                    val program = when {
                        clientName.isNullOrBlank() -> stringResource(R.string.home_client_unnamed)
                        clientVersion.isNullOrBlank() -> clientName
                        else -> "$clientName $clientVersion"
                    }
                    val pcName = client.pc?.name
                    val name = when {
                        !client.helloDone -> stringResource(R.string.home_client_connecting)
                        pcName != null -> "$pcName · $program"
                        else -> program
                    }
                    Text(name, style = MaterialTheme.typography.titleMedium)
                    Text(
                        client.remote,
                        style = MaterialTheme.typography.bodySmall,
                        fontFamily = FontFamily.Monospace,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
            if (client.subscriptions.isNotEmpty()) {
                Spacer(Modifier.height(12.dp))
                FlowRow(
                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    client.subscriptions.forEach { SubscriptionPill(it) }
                }
            } else if (client.helloDone) {
                Spacer(Modifier.height(8.dp))
                Text(
                    stringResource(R.string.home_client_no_subs),
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
    }
}

@Composable
private fun SubscriptionPill(sub: SubscriptionSnapshot) {
    val ui = sensorUi(sub.source)
    Surface(
        shape = CircleShape,
        color = MaterialTheme.colorScheme.secondaryContainer,
        contentColor = MaterialTheme.colorScheme.onSecondaryContainer,
    ) {
        Row(Modifier.padding(horizontal = 12.dp, vertical = 6.dp), verticalAlignment = Alignment.CenterVertically) {
            Icon(ui.icon, null, Modifier.size(16.dp))
            Spacer(Modifier.width(6.dp))
            val label = stringResource(ui.label)
            Text(
                sub.rateHz?.let { "$label · ${formatHz(it)} Hz" } ?: label,
                style = MaterialTheme.typography.labelLarge,
            )
        }
    }
}

// ---- previews ----

private val previewClients = listOf(
    ClientSnapshot(
        1, "192.168.1.40:51234", "air-mouse", "0.1.0", true, pc = PcInfo("9d1c", "arch-desktop"), subscriptions =
        listOf(
            SubscriptionSnapshot("s-0001", "game_rotation_vector", null, 100.0),
            SubscriptionSnapshot("s-0002", "ui", null, null),
        ),
    ),
    ClientSnapshot(2, "127.0.0.1:40122", "phonetopc tail", null, true, emptyList()),
)

@Preview(showBackground = true)
@Composable
private fun HomeRunningPreview() = PhonetopcTheme(dynamicColor = false) {
    HomeContent(
        devMode = false, configuredPort = DEFAULT_PORT,
        status = BridgeStatus(
            state = RunState.RUNNING, port = DEFAULT_PORT, secure = true, clients = previewClients,
            streaming = mapOf("game_rotation_vector" to Demand(100.0), "ui" to Demand(null)),
        ),
        addresses = listOf("192.168.1.23"), hasPairedPcs = true,
        onStart = {}, onStop = {}, onPause = {}, onPair = {},
        panels = listOf(OpenPanel(PanelKey("pc/1", "pad"), "Drawing pad", "omarchy", attached = true)),
    )
}

@Preview(showBackground = true)
@Composable
private fun HomeSecureUnpairedPreview() = PhonetopcTheme(darkTheme = true, dynamicColor = false) {
    HomeContent(
        devMode = false, configuredPort = DEFAULT_PORT, status = BridgeStatus(),
        addresses = emptyList(), hasPairedPcs = false, onStart = {}, onStop = {}, onPause = {}, onPair = {},
    )
}
