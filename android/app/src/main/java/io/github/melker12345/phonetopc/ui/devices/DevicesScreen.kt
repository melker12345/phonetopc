package io.github.melker12345.phonetopc.ui.devices

import android.Manifest
import android.text.format.DateUtils
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.selection.toggleable
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.Add
import androidx.compose.material.icons.rounded.Code
import androidx.compose.material.icons.rounded.Computer
import androidx.compose.material.icons.rounded.Devices
import androidx.compose.material.icons.rounded.LinkOff
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedCard
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.pluralStringResource
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.tooling.preview.Preview
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import io.github.melker12345.phonetopc.R
import io.github.melker12345.phonetopc.data.PairedPc
import io.github.melker12345.phonetopc.panels.PanelActivity
import io.github.melker12345.phonetopc.protocol.panels.Feature
import io.github.melker12345.phonetopc.protocol.security.Pins
import io.github.melker12345.phonetopc.ui.MainViewModel
import io.github.melker12345.phonetopc.ui.SystemSettings
import io.github.melker12345.phonetopc.ui.components.EmptyState
import io.github.melker12345.phonetopc.ui.components.IconBadge
import io.github.melker12345.phonetopc.ui.theme.PhonetopcTheme
import java.text.DateFormat
import java.util.Date

@Composable
fun DevicesScreen(vm: MainViewModel, onPair: () -> Unit) {
    val pcs by vm.pairedPcs.collectAsStateWithLifecycle()
    val status by vm.status.collectAsStateWithLifecycle()
    val settings by vm.settings.collectAsStateWithLifecycle()
    val panels by vm.panels.collectAsStateWithLifecycle()
    val connected = status.clients.mapNotNull { it.pc?.id }.toSet()
    val context = LocalContext.current
    // Android 13+: notifications and panel requests need POST_NOTIFICATIONS; ask when either is turned on.
    val permission = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) {}
    DevicesContent(
        pcs, connected, onPair, onRevoke = vm::revoke,
        devOutput = settings?.takeIf { it.devMode }?.devOutput,
        openPanels = panels.size,
        onOpenPanels = { context.startActivity(PanelActivity.intent(context, null)) },
        onSetAllowed = { pcId, feature, allowed ->
            // notificationsGranted is true below Android 13, where there is nothing to ask.
            if (allowed && !SystemSettings.notificationsGranted(context)) {
                permission.launch(Manifest.permission.POST_NOTIFICATIONS)
            }
            vm.setOutputAllowed(pcId, feature, allowed)
        },
    )
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun DevicesContent(
    pcs: List<PairedPc>?,
    connected: Set<String>,
    onPair: () -> Unit,
    onRevoke: (String) -> Unit,
    /** What the dev-mode PC may do, or `null` when developer mode is off (no such PC then). */
    devOutput: Set<Feature>? = null,
    openPanels: Int = 0,
    onOpenPanels: () -> Unit = {},
    /** A PC's id, `null` for the dev-mode PC. */
    onSetAllowed: (String?, Feature, Boolean) -> Unit = { _, _, _ -> },
) {
    var revoking by rememberSaveable { mutableStateOf<String?>(null) }

    Scaffold(topBar = { TopAppBar(title = { Text(stringResource(R.string.devices_title)) }) }) { padding ->
        when {
            pcs == null -> Box(Modifier.fillMaxSize().padding(padding))
            pcs.isEmpty() && devOutput == null && openPanels == 0 -> Box(Modifier.fillMaxSize().padding(padding), contentAlignment = Alignment.Center) {
                EmptyState(
                    icon = Icons.Rounded.Devices,
                    title = stringResource(R.string.devices_empty_title),
                    body = stringResource(R.string.devices_empty_body),
                    action = { OutlinedButton(onClick = onPair) { Text(stringResource(R.string.devices_pair_action)) } },
                )
            }
            else -> LazyColumn(
                contentPadding = PaddingValues(
                    start = 16.dp, end = 16.dp,
                    top = padding.calculateTopPadding() + 8.dp,
                    bottom = padding.calculateBottomPadding() + 24.dp,
                ),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                if (openPanels > 0) {
                    item(key = "panels") {
                        FilledTonalButton(onClick = onOpenPanels, modifier = Modifier.fillMaxWidth()) {
                            Text(pluralStringResource(R.plurals.devices_open_panels, openPanels, openPanels))
                        }
                    }
                }
                if (devOutput != null) {
                    item(key = "dev") { DevPcCard(devOutput, onSetAllowed = { f, on -> onSetAllowed(null, f, on) }) }
                }
                items(pcs.sortedByDescending { it.lastConnected ?: it.pairedAt }, key = { it.id }) { pc ->
                    PcCard(pc, pc.id in connected, onRevoke = { revoking = pc.id }, onSetAllowed = { f, on -> onSetAllowed(pc.id, f, on) })
                }
                item {
                    OutlinedButton(onClick = onPair, modifier = Modifier.padding(top = 4.dp)) {
                        Icon(Icons.Rounded.Add, null, Modifier.size(ButtonDefaults.IconSize))
                        Spacer(Modifier.width(ButtonDefaults.IconSpacing))
                        Text(stringResource(R.string.devices_pair_another))
                    }
                }
            }
        }
    }

    val target = pcs?.firstOrNull { it.id == revoking }
    if (target != null) {
        AlertDialog(
            onDismissRequest = { revoking = null },
            icon = { Icon(Icons.Rounded.LinkOff, null) },
            title = { Text(stringResource(R.string.devices_revoke_title, target.name)) },
            text = { Text(stringResource(R.string.devices_revoke_body)) },
            confirmButton = {
                TextButton(
                    onClick = { onRevoke(target.id); revoking = null },
                    colors = ButtonDefaults.textButtonColors(contentColor = MaterialTheme.colorScheme.error),
                ) { Text(stringResource(R.string.devices_revoke)) }
            },
            dismissButton = { TextButton(onClick = { revoking = null }) { Text(stringResource(R.string.cancel)) } },
        )
    }
}

@Composable
private fun PcCard(pc: PairedPc, connectedNow: Boolean, onRevoke: () -> Unit, onSetAllowed: (Feature, Boolean) -> Unit) {
    OutlinedCard(Modifier.fillMaxWidth(), shape = MaterialTheme.shapes.large) {
        Row(Modifier.padding(start = 16.dp, top = 16.dp, bottom = 16.dp, end = 4.dp), verticalAlignment = Alignment.Top) {
            IconBadge(
                Icons.Rounded.Computer,
                container = if (connectedNow) MaterialTheme.colorScheme.primaryContainer else MaterialTheme.colorScheme.secondaryContainer,
                content = if (connectedNow) MaterialTheme.colorScheme.onPrimaryContainer else MaterialTheme.colorScheme.onSecondaryContainer,
            )
            Spacer(Modifier.width(16.dp))
            Column(Modifier.weight(1f)) {
                Text(pc.name, style = MaterialTheme.typography.titleMedium)
                if (connectedNow) {
                    Spacer(Modifier.height(4.dp))
                    ConnectedPill()
                    Spacer(Modifier.height(4.dp))
                } else {
                    Text(
                        pc.lastConnected?.let {
                            stringResource(
                                R.string.devices_last_connected,
                                DateUtils.getRelativeTimeSpanString(it, System.currentTimeMillis(), DateUtils.MINUTE_IN_MILLIS).toString(),
                            )
                        } ?: stringResource(R.string.devices_never_connected),
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                Text(
                    stringResource(R.string.devices_paired_on, DateFormat.getDateInstance(DateFormat.MEDIUM).format(Date(pc.pairedAt))),
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Text(
                    stringResource(R.string.devices_pin, Pins.shortHex(pc.pin)),
                    style = MaterialTheme.typography.bodySmall,
                    fontFamily = FontFamily.Monospace,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            IconButton(onClick = onRevoke) {
                Icon(Icons.Rounded.LinkOff, contentDescription = stringResource(R.string.devices_revoke))
            }
        }
        OutputSwitches(notify = pc.allowNotify, panels = pc.allowPanels, onSetAllowed)
    }
}

/** Developer mode: every client counts as one unverified PC with its own two switches (PANELS.md §1). */
@Composable
private fun DevPcCard(allowed: Set<Feature>, onSetAllowed: (Feature, Boolean) -> Unit) {
    OutlinedCard(Modifier.fillMaxWidth(), shape = MaterialTheme.shapes.large) {
        Row(Modifier.padding(16.dp), verticalAlignment = Alignment.Top) {
            IconBadge(
                Icons.Rounded.Code,
                container = MaterialTheme.colorScheme.errorContainer,
                content = MaterialTheme.colorScheme.onErrorContainer,
            )
            Spacer(Modifier.width(16.dp))
            Column(Modifier.weight(1f)) {
                Text(stringResource(R.string.panel_dev_pc_name), style = MaterialTheme.typography.titleMedium)
                Text(
                    stringResource(R.string.devices_dev_pc_body),
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
        OutputSwitches(notify = Feature.NOTIFY in allowed, panels = Feature.PANELS in allowed, onSetAllowed)
    }
}

/** The per-PC switches of PANELS.md §1, both off until the user turns them on. */
@Composable
private fun OutputSwitches(notify: Boolean, panels: Boolean, onSetAllowed: (Feature, Boolean) -> Unit) {
    Column(Modifier.padding(start = 16.dp, end = 16.dp, bottom = 8.dp)) {
        HorizontalDivider()
        OutputSwitch(stringResource(R.string.devices_allow_notify), stringResource(R.string.devices_allow_notify_summary), notify) {
            onSetAllowed(Feature.NOTIFY, it)
        }
        OutputSwitch(stringResource(R.string.devices_allow_panels), stringResource(R.string.devices_allow_panels_summary), panels) {
            onSetAllowed(Feature.PANELS, it)
        }
    }
}

@Composable
private fun OutputSwitch(title: String, summary: String, checked: Boolean, onChange: (Boolean) -> Unit) {
    Row(
        Modifier.fillMaxWidth().toggleable(checked, role = Role.Switch, onValueChange = onChange).padding(vertical = 8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(Modifier.weight(1f)) {
            Text(title, style = MaterialTheme.typography.bodyLarge)
            Text(summary, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Spacer(Modifier.width(12.dp))
        Switch(checked = checked, onCheckedChange = null)
    }
}

@Composable
private fun ConnectedPill() {
    Surface(
        shape = CircleShape,
        color = MaterialTheme.colorScheme.primaryContainer,
        contentColor = MaterialTheme.colorScheme.onPrimaryContainer,
    ) {
        Row(Modifier.padding(horizontal = 10.dp, vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
            Surface(Modifier.size(8.dp), shape = CircleShape, color = MaterialTheme.colorScheme.primary) {}
            Spacer(Modifier.width(6.dp))
            Text(stringResource(R.string.devices_connected_now), style = MaterialTheme.typography.labelMedium)
        }
    }
}

@Preview(showBackground = true)
@Composable
private fun DevicesPreview() = PhonetopcTheme(dynamicColor = false) {
    val now = System.currentTimeMillis()
    DevicesContent(
        pcs = listOf(
            PairedPc("a", "arch-desktop", "sha256/89Nebgssmdcpo6zU3xFLFXCRqV4YpVYbvJbyq99cMGA", "h", now - 86_400_000, now - 60_000, allowPanels = true),
            PairedPc("b", "work-laptop", "sha256/tQITurCV6l1Rq9Rk0X_kPrHlWaaKPnkI6CGl03GEaBc", "h", now - 7 * 86_400_000),
        ),
        connected = setOf("a"),
        onPair = {},
        onRevoke = {},
        devOutput = setOf(Feature.NOTIFY),
        openPanels = 2,
    )
}

@Preview(showBackground = true)
@Composable
private fun DevicesEmptyPreview() = PhonetopcTheme(dynamicColor = false) {
    DevicesContent(pcs = emptyList(), connected = emptySet(), onPair = {}, onRevoke = {})
}
