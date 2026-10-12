package io.github.melker12345.phonetopc.ui.settings

import android.Manifest
import android.content.ClipData
import android.os.Build
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.selection.toggleable
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.rounded.OpenInNew
import androidx.compose.material.icons.rounded.BatterySaver
import androidx.compose.material.icons.rounded.CheckCircle
import androidx.compose.material.icons.rounded.ContentCopy
import androidx.compose.material.icons.rounded.DeveloperMode
import androidx.compose.material.icons.rounded.Fingerprint
import androidx.compose.material.icons.rounded.Info
import androidx.compose.material.icons.rounded.Key
import androidx.compose.material.icons.rounded.Notifications
import androidx.compose.material.icons.rounded.Numbers
import androidx.compose.material.icons.rounded.PowerSettingsNew
import androidx.compose.material.icons.rounded.Warning
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LargeTopAppBar
import androidx.compose.material3.ListItem
import androidx.compose.material3.ListItemDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBarDefaults
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.input.nestedscroll.nestedScroll
import androidx.compose.ui.platform.ClipEntry
import androidx.compose.ui.platform.LocalClipboard
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringArrayResource
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.LifecycleResumeEffect
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import io.github.melker12345.phonetopc.BuildConfig
import io.github.melker12345.phonetopc.R
import io.github.melker12345.phonetopc.approval.ApprovalNotifications
import io.github.melker12345.phonetopc.data.AppSettings
import io.github.melker12345.phonetopc.data.ApprovalKey
import io.github.melker12345.phonetopc.data.SettingsRepository
import io.github.melker12345.phonetopc.protocol.PROTOCOL_VERSION
import io.github.melker12345.phonetopc.protocol.SensorCatalog
import io.github.melker12345.phonetopc.protocol.security.Pins
import io.github.melker12345.phonetopc.ui.MainViewModel
import io.github.melker12345.phonetopc.ui.PhoneKeyInfo
import io.github.melker12345.phonetopc.ui.SensorUiCatalog
import io.github.melker12345.phonetopc.ui.SystemSettings
import io.github.melker12345.phonetopc.ui.components.IconBadge
import io.github.melker12345.phonetopc.ui.components.SectionHeader
import io.github.melker12345.phonetopc.ui.findActivity
import io.github.melker12345.phonetopc.ui.rememberOnResume
import kotlinx.coroutines.launch

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SettingsScreen(vm: MainViewModel) {
    val settings by vm.settings.collectAsStateWithLifecycle()
    val deviceId by vm.deviceId.collectAsStateWithLifecycle()
    val phoneKey by vm.phoneKey.collectAsStateWithLifecycle()
    val approval by vm.approval.collectAsStateWithLifecycle()
    val s = settings ?: AppSettings()
    val scrollBehavior = TopAppBarDefaults.exitUntilCollapsedScrollBehavior()

    Scaffold(
        modifier = Modifier.nestedScroll(scrollBehavior.nestedScrollConnection),
        topBar = {
            LargeTopAppBar(title = { Text(stringResource(R.string.settings_title)) }, scrollBehavior = scrollBehavior)
        },
    ) { padding ->
        LazyColumn(
            contentPadding = PaddingValues(
                start = 16.dp, end = 16.dp,
                top = padding.calculateTopPadding(),
                bottom = padding.calculateBottomPadding() + 24.dp,
            ),
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            item { SectionHeader(stringResource(R.string.settings_section_server)) }
            item { ServerGroup(s, onDevMode = vm::setDevMode, onPort = vm::setPort, onStartOnBoot = vm::setStartOnBoot) }

            item { SectionHeader(stringResource(R.string.settings_section_sensors), Modifier.padding(top = 16.dp)) }
            item {
                Text(
                    stringResource(R.string.settings_sensors_hint),
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(start = 4.dp, end = 4.dp, bottom = 4.dp),
                )
            }
            item { SensorsGroup(s.disabledSources, vm) }

            item { SectionHeader(stringResource(R.string.settings_section_keep_alive), Modifier.padding(top = 16.dp)) }
            item { KeepAliveGroup() }
            item { VendorGuideCard(Vendor.detect()) }

            item { SectionHeader(stringResource(R.string.settings_section_approvals), Modifier.padding(top = 16.dp)) }
            item {
                Text(
                    stringResource(R.string.settings_approvals_hint),
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(start = 4.dp, end = 4.dp, bottom = 4.dp),
                )
            }
            item { ApprovalsGroup(approval, devMode = s.devMode, refresh = vm::refreshApproval) }

            item { SectionHeader(stringResource(R.string.settings_section_about), Modifier.padding(top = 16.dp)) }
            item { AboutGroup(deviceId, phoneKey) }
        }
    }
}

@Composable
private fun Group(content: @Composable ColumnScope.() -> Unit) {
    Card(
        shape = MaterialTheme.shapes.extraLarge,
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerLow),
    ) {
        Column(Modifier.padding(vertical = 8.dp), content = content)
    }
}

private val transparentItem @Composable get() = ListItemDefaults.colors(containerColor = Color.Transparent)

// ---- server ----

@Composable
private fun ServerGroup(
    s: AppSettings,
    onDevMode: (Boolean) -> Unit,
    onPort: (Int) -> Unit,
    onStartOnBoot: (Boolean) -> Unit,
) {
    var confirmDevMode by rememberSaveable { mutableStateOf(false) }
    var editPort by rememberSaveable { mutableStateOf(false) }

    Group {
        ListItem(
            colors = transparentItem,
            modifier = Modifier.toggleable(
                value = s.devMode,
                role = Role.Switch,
                onValueChange = { on -> if (on) confirmDevMode = true else onDevMode(false) },
            ),
            leadingContent = { IconBadge(Icons.Rounded.DeveloperMode, size = 36.dp) },
            headlineContent = { Text(stringResource(R.string.settings_dev_mode)) },
            supportingContent = { Text(stringResource(R.string.settings_dev_mode_summary)) },
            trailingContent = { Switch(checked = s.devMode, onCheckedChange = null) },
        )
        if (s.devMode) {
            Card(
                modifier = Modifier.padding(horizontal = 16.dp, vertical = 4.dp),
                shape = MaterialTheme.shapes.large,
                colors = CardDefaults.cardColors(
                    containerColor = MaterialTheme.colorScheme.errorContainer,
                    contentColor = MaterialTheme.colorScheme.onErrorContainer,
                ),
            ) {
                Row(Modifier.padding(16.dp)) {
                    Icon(Icons.Rounded.Warning, null, Modifier.size(20.dp))
                    Spacer(Modifier.width(12.dp))
                    Text(stringResource(R.string.settings_dev_mode_warning), style = MaterialTheme.typography.bodyMedium)
                }
            }
        }
        ListItem(
            colors = transparentItem,
            modifier = Modifier.clickable { editPort = true },
            leadingContent = { IconBadge(Icons.Rounded.Numbers, size = 36.dp) },
            headlineContent = { Text(stringResource(R.string.settings_port)) },
            supportingContent = { Text(stringResource(R.string.settings_port_summary, s.port)) },
        )
        ListItem(
            colors = transparentItem,
            modifier = Modifier.toggleable(value = s.startOnBoot, role = Role.Switch, onValueChange = onStartOnBoot),
            leadingContent = { IconBadge(Icons.Rounded.PowerSettingsNew, size = 36.dp) },
            headlineContent = { Text(stringResource(R.string.settings_start_on_boot)) },
            supportingContent = { Text(stringResource(R.string.settings_start_on_boot_summary)) },
            trailingContent = { Switch(checked = s.startOnBoot, onCheckedChange = null) },
        )
    }

    if (confirmDevMode) {
        AlertDialog(
            onDismissRequest = { confirmDevMode = false },
            icon = { Icon(Icons.Rounded.Warning, null) },
            title = { Text(stringResource(R.string.settings_dev_mode_dialog_title)) },
            text = { Text(stringResource(R.string.settings_dev_mode_dialog_body)) },
            confirmButton = {
                TextButton(
                    onClick = { onDevMode(true); confirmDevMode = false },
                    colors = ButtonDefaults.textButtonColors(contentColor = MaterialTheme.colorScheme.error),
                ) { Text(stringResource(R.string.settings_dev_mode_dialog_confirm)) }
            },
            dismissButton = {
                TextButton(onClick = { confirmDevMode = false }) { Text(stringResource(android.R.string.cancel)) }
            },
        )
    }
    if (editPort) {
        PortDialog(current = s.port, onDismiss = { editPort = false }, onSave = { onPort(it); editPort = false })
    }
}

@Composable
private fun PortDialog(current: Int, onDismiss: () -> Unit, onSave: (Int) -> Unit) {
    var text by rememberSaveable { mutableStateOf(current.toString()) }
    val port = text.toIntOrNull()
    val valid = port != null && port in SettingsRepository.MIN_PORT..SettingsRepository.MAX_PORT
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(stringResource(R.string.settings_port)) },
        text = {
            OutlinedTextField(
                value = text,
                onValueChange = { v -> text = v.filter { it.isDigit() }.take(5) },
                singleLine = true,
                isError = !valid,
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
                supportingText = {
                    Text(
                        stringResource(
                            R.string.settings_port_range,
                            SettingsRepository.MIN_PORT,
                            SettingsRepository.MAX_PORT,
                        ),
                    )
                },
            )
        },
        confirmButton = {
            TextButton(onClick = { port?.let(onSave) }, enabled = valid) { Text(stringResource(R.string.save)) }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text(stringResource(android.R.string.cancel)) } },
    )
}

// ---- sensors ----

@Composable
private fun SensorsGroup(disabled: Set<String>, vm: MainViewModel) {
    val context = LocalContext.current
    // The permission dialog and system settings pause the activity, so these pick up changes.
    val locationPermitted = rememberOnResume { vm.locationPermitted() }
    val locationOn = rememberOnResume { vm.locationEnabled() }
    val nfcOn = rememberOnResume { vm.nfcEnabled() }
    var locationDenied by rememberSaveable { mutableStateOf(false) }
    val locationLauncher = rememberLauncherForActivityResult(ActivityResultContracts.RequestMultiplePermissions()) { result ->
        locationDenied = result.values.none { it }
    }
    val askLocation = {
        locationLauncher.launch(arrayOf(Manifest.permission.ACCESS_FINE_LOCATION, Manifest.permission.ACCESS_COARSE_LOCATION))
    }

    Group {
        SensorUiCatalog.forEach { sensor ->
            val present = remember(sensor.source) { vm.hardwarePresent(sensor.source) }
            val enabled = present && sensor.source !in disabled
            val isLocation = sensor.source == SensorCatalog.LOCATION
            ListItem(
                colors = transparentItem,
                modifier = Modifier.toggleable(
                    value = enabled,
                    enabled = present,
                    role = Role.Switch,
                    onValueChange = { on ->
                        vm.setSourceEnabled(sensor.source, on)
                        if (on && isLocation && !locationPermitted) askLocation()
                    },
                ),
                leadingContent = { IconBadge(sensor.icon, size = 36.dp) },
                headlineContent = { Text(stringResource(sensor.label)) },
                supportingContent = {
                    Text(stringResource(if (present) sensor.description else R.string.settings_sensor_missing))
                },
                trailingContent = { Switch(checked = enabled, onCheckedChange = null, enabled = present) },
            )
            if (isLocation && enabled && (!locationPermitted || !locationOn)) {
                // Turned on, but Android won't let it through yet: say why and offer the fix.
                val permanentlyDenied = locationDenied &&
                    context.findActivity()?.shouldShowRequestPermissionRationale(Manifest.permission.ACCESS_FINE_LOCATION) == false
                ListItem(
                    colors = transparentItem,
                    leadingContent = { IconBadge(Icons.Rounded.Warning, size = 36.dp) },
                    headlineContent = { Text(stringResource(R.string.settings_location_unavailable)) },
                    supportingContent = {
                        Text(stringResource(if (!locationPermitted) R.string.settings_location_permission else R.string.settings_location_off))
                    },
                    trailingContent = {
                        FilledTonalButton(onClick = {
                            when {
                                !locationPermitted && permanentlyDenied -> SystemSettings.openAppDetails(context)
                                !locationPermitted -> askLocation()
                                else -> SystemSettings.openLocationSettings(context)
                            }
                        }) {
                            Text(stringResource(if (!locationPermitted) R.string.allow else R.string.settings_turn_on))
                        }
                    },
                )
            }
            if (sensor.source == SensorCatalog.NFC && enabled && !nfcOn) {
                ListItem(
                    colors = transparentItem,
                    leadingContent = { IconBadge(Icons.Rounded.Warning, size = 36.dp) },
                    headlineContent = { Text(stringResource(R.string.settings_nfc_off)) },
                    supportingContent = { Text(stringResource(R.string.settings_nfc_off_summary)) },
                    trailingContent = {
                        FilledTonalButton(onClick = { SystemSettings.openNfcSettings(context) }) {
                            Text(stringResource(R.string.settings_turn_on))
                        }
                    },
                )
            }
        }
    }
}

// ---- keep alive ----

@Composable
private fun KeepAliveGroup() {
    val context = LocalContext.current
    val exempt = rememberOnResume(SystemSettings::isIgnoringBatteryOptimizations)
    val notificationsOk = rememberOnResume(SystemSettings::notificationsGranted)
    // The permission dialog pauses the activity, so rememberOnResume picks up the result.
    val permissionLauncher = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { }

    Group {
        ListItem(
            colors = transparentItem,
            leadingContent = { IconBadge(if (exempt) Icons.Rounded.CheckCircle else Icons.Rounded.BatterySaver, size = 36.dp) },
            headlineContent = { Text(stringResource(R.string.settings_battery)) },
            supportingContent = {
                Text(stringResource(if (exempt) R.string.settings_battery_ok else R.string.settings_battery_restricted))
            },
            trailingContent = if (exempt) null else {
                {
                    FilledTonalButton(onClick = { SystemSettings.requestBatteryExemption(context) }) {
                        Text(stringResource(R.string.allow))
                    }
                }
            },
        )
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            val granted = notificationsOk
            ListItem(
                colors = transparentItem,
                leadingContent = { IconBadge(if (granted) Icons.Rounded.CheckCircle else Icons.Rounded.Notifications, size = 36.dp) },
                headlineContent = { Text(stringResource(R.string.settings_notifications)) },
                supportingContent = {
                    Text(stringResource(if (granted) R.string.settings_notifications_ok else R.string.settings_notifications_off))
                },
                trailingContent = if (granted) null else {
                    {
                        FilledTonalButton(onClick = { permissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS) }) {
                            Text(stringResource(R.string.allow))
                        }
                    }
                },
            )
        }
    }
}

@Composable
fun VendorGuideCard(vendor: Vendor, modifier: Modifier = Modifier) {
    val context = LocalContext.current
    Card(
        modifier = modifier.fillMaxWidth(),
        shape = MaterialTheme.shapes.extraLarge,
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerLow),
    ) {
        Column(Modifier.padding(20.dp)) {
            Text(stringResource(vendor.title), style = MaterialTheme.typography.titleMedium)
            Spacer(Modifier.height(12.dp))
            stringArrayResource(vendor.steps).forEachIndexed { i, step ->
                Row(Modifier.padding(vertical = 4.dp), verticalAlignment = Alignment.Top) {
                    Text(
                        "${i + 1}.",
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.primary,
                        modifier = Modifier.width(24.dp),
                    )
                    Text(step, style = MaterialTheme.typography.bodyMedium)
                }
            }
            Spacer(Modifier.height(8.dp))
            TextButton(
                onClick = { SystemSettings.openUrl(context, vendor.url) },
                contentPadding = ButtonDefaults.TextButtonWithIconContentPadding,
            ) {
                Text(stringResource(R.string.settings_vendor_more))
                Spacer(Modifier.width(ButtonDefaults.IconSpacing))
                Icon(Icons.AutoMirrored.Rounded.OpenInNew, null, Modifier.size(ButtonDefaults.IconSize))
            }
        }
    }
}

// ---- approvals ----

/** APPROVAL.md: whether approvals are possible, and the approval key's pin. */
@Composable
private fun ApprovalsGroup(status: ApprovalKey.Status?, devMode: Boolean, refresh: () -> Unit) {
    val context = LocalContext.current
    val clipboard = LocalClipboard.current
    val scope = rememberCoroutineScope()
    // Fingerprints may have been added or removed in system settings meanwhile.
    LifecycleResumeEffect(Unit) {
        refresh()
        onPauseOrDispose { }
    }
    val notificationsOk = rememberOnResume(ApprovalNotifications::canShow)
    val available = status as? ApprovalKey.Status.Available
    Group {
        ListItem(
            colors = transparentItem,
            leadingContent = {
                IconBadge(if (available != null) Icons.Rounded.CheckCircle else Icons.Rounded.Fingerprint, size = 36.dp)
            },
            headlineContent = { Text(stringResource(R.string.settings_approval_status)) },
            supportingContent = {
                val text = when (status) {
                    null -> "…"
                    is ApprovalKey.Status.Available -> stringResource(
                        if (devMode) R.string.settings_approval_secure_only else R.string.settings_approval_available,
                    )
                    is ApprovalKey.Status.Unavailable -> stringResource(
                        when (status.problem) {
                            ApprovalKey.Problem.NO_HARDWARE -> R.string.settings_approval_no_hardware
                            ApprovalKey.Problem.NONE_ENROLLED -> R.string.settings_approval_none_enrolled
                            ApprovalKey.Problem.HW_UNAVAILABLE -> R.string.settings_approval_hw_unavailable
                            ApprovalKey.Problem.SECURITY_UPDATE_REQUIRED -> R.string.settings_approval_update_required
                            ApprovalKey.Problem.OTHER -> R.string.settings_approval_other
                        },
                    )
                }
                Text(text)
            },
            trailingContent = if ((status as? ApprovalKey.Status.Unavailable)?.problem == ApprovalKey.Problem.NONE_ENROLLED) {
                {
                    FilledTonalButton(onClick = { SystemSettings.enrollBiometric(context) }) {
                        Text(stringResource(R.string.settings_approval_enroll))
                    }
                }
            } else {
                null
            },
        )
        if (available != null && !notificationsOk) {
            ListItem(
                colors = transparentItem,
                leadingContent = { IconBadge(Icons.Rounded.Notifications, size = 36.dp) },
                headlineContent = { Text(stringResource(R.string.settings_notifications)) },
                supportingContent = { Text(stringResource(R.string.settings_approval_notifications_off)) },
            )
        }
        if (available != null) {
            val pin = available.info.pin
            ListItem(
                colors = transparentItem,
                leadingContent = { IconBadge(Icons.Rounded.Key, size = 36.dp) },
                headlineContent = { Text(stringResource(R.string.settings_approval_key)) },
                supportingContent = {
                    Column {
                        Text(Pins.shortHex(pin), fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.titleMedium)
                        Text(pin, fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodySmall)
                        Text(stringResource(R.string.settings_approval_key_summary), style = MaterialTheme.typography.bodySmall)
                        if (available.strongBox) {
                            Text(stringResource(R.string.settings_approval_key_strongbox), style = MaterialTheme.typography.bodySmall)
                        }
                    }
                },
                trailingContent = {
                    IconButton(onClick = {
                        scope.launch { clipboard.setClipEntry(ClipEntry(ClipData.newPlainText("phonetopc approval key", pin))) }
                    }) {
                        Icon(Icons.Rounded.ContentCopy, contentDescription = stringResource(R.string.settings_copy_approval_pin))
                    }
                },
            )
        }
    }
}

// ---- about ----

@Composable
private fun AboutGroup(deviceId: String?, phoneKey: PhoneKeyInfo?) {
    val clipboard = LocalClipboard.current
    val scope = rememberCoroutineScope()
    Group {
        ListItem(
            colors = transparentItem,
            leadingContent = { IconBadge(Icons.Rounded.Info, size = 36.dp) },
            headlineContent = { Text(stringResource(R.string.settings_version)) },
            supportingContent = { Text("${BuildConfig.VERSION_NAME} · $PROTOCOL_VERSION (draft 3)") },
        )
        // SECURITY.md §2: the pin a PC stores for this phone, for comparing by eye.
        ListItem(
            colors = transparentItem,
            leadingContent = { IconBadge(Icons.Rounded.Key, size = 36.dp) },
            headlineContent = { Text(stringResource(R.string.settings_phone_key)) },
            supportingContent = {
                Column {
                    Text(
                        phoneKey?.let { Pins.shortHex(it.pin) } ?: "…",
                        fontFamily = FontFamily.Monospace,
                        style = MaterialTheme.typography.titleMedium,
                    )
                    phoneKey?.let {
                        Text(it.pin, fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodySmall)
                        Text(
                            stringResource(if (it.hardwareBacked) R.string.settings_phone_key_hw else R.string.settings_phone_key_sw),
                            style = MaterialTheme.typography.bodySmall,
                        )
                    }
                }
            },
            trailingContent = phoneKey?.let { key ->
                {
                    IconButton(onClick = {
                        scope.launch { clipboard.setClipEntry(ClipEntry(ClipData.newPlainText("phonetopc pin", key.pin))) }
                    }) {
                        Icon(Icons.Rounded.ContentCopy, contentDescription = stringResource(R.string.settings_copy_pin))
                    }
                }
            },
        )
        ListItem(
            colors = transparentItem,
            leadingContent = { IconBadge(Icons.Rounded.Fingerprint, size = 36.dp) },
            headlineContent = { Text(stringResource(R.string.settings_device_id)) },
            supportingContent = { Text(deviceId ?: "…", fontFamily = FontFamily.Monospace) },
        )
    }
}
