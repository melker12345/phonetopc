package io.github.melker12345.phonetopc.ui.pair

import android.Manifest
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.provider.Settings
import androidx.activity.compose.BackHandler
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.animation.AnimatedContent
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.aspectRatio
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.rounded.ArrowBack
import androidx.compose.material.icons.automirrored.rounded.KeyboardArrowRight
import androidx.compose.material.icons.rounded.CheckCircle
import androidx.compose.material.icons.rounded.Computer
import androidx.compose.material.icons.rounded.ErrorOutline
import androidx.compose.material.icons.rounded.Lan
import androidx.compose.material.icons.rounded.PhotoCamera
import androidx.compose.material.icons.rounded.QrCodeScanner
import androidx.compose.material.icons.rounded.Warning
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.ListItem
import androidx.compose.material3.ListItemDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedCard
import androidx.compose.material3.Scaffold
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
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalContext
import io.github.melker12345.phonetopc.ui.findActivity
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.tooling.preview.Preview
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.core.content.ContextCompat
import androidx.core.net.toUri
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import io.github.melker12345.phonetopc.R
import io.github.melker12345.phonetopc.data.DeviceIdentity
import io.github.melker12345.phonetopc.pairing.NetworkPc
import io.github.melker12345.phonetopc.pairing.PairingFailure
import io.github.melker12345.phonetopc.pairing.PairingQr
import io.github.melker12345.phonetopc.pairing.PairingState
import io.github.melker12345.phonetopc.protocol.security.Sas
import io.github.melker12345.phonetopc.ui.components.IconBadge
import io.github.melker12345.phonetopc.ui.theme.PhonetopcTheme

/**
 * Pairing (SECURITY.md §4): pick a way in, then follow the state machine in
 * [io.github.melker12345.phonetopc.pairing.PairingClient].
 */
@Composable
fun PairScreen(devMode: Boolean, onOpenDevices: () -> Unit, vm: PairViewModel = viewModel()) {
    val step by vm.step.collectAsStateWithLifecycle()
    val state by vm.state.collectAsStateWithLifecycle()
    val found by vm.found.collectAsStateWithLifecycle()
    val browseFailed by vm.browseFailed.collectAsStateWithLifecycle()
    val scanProblem by vm.scanProblem.collectAsStateWithLifecycle()

    BackHandler(enabled = step != PairStep.CHOOSE) { vm.back() }

    PairScaffold(showBack = step != PairStep.CHOOSE, onBack = vm::back) {
        AnimatedContent(step, label = "pairStep") { s ->
            when (s) {
                PairStep.CHOOSE -> ChooseContent(onScan = vm::scan, onBrowse = vm::browse)
                PairStep.SCAN -> ScanContent(scanProblem, onScanned = vm::onScanned)
                PairStep.BROWSE -> BrowseContent(found, browseFailed, onPick = vm::pick, onUseQr = vm::scan)
                PairStep.RUN -> RunContent(
                    state = state,
                    devMode = devMode,
                    onAnswer = vm::answerCode,
                    onCancel = vm::back,
                    onRetry = vm::retry,
                    onDone = vm::back,
                    onOpenDevices = { vm.back(); onOpenDevices() },
                )
            }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun PairScaffold(showBack: Boolean, onBack: () -> Unit, content: @Composable () -> Unit) {
    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text(stringResource(R.string.pair_title)) },
                navigationIcon = {
                    if (showBack) {
                        IconButton(onClick = onBack) {
                            Icon(Icons.AutoMirrored.Rounded.ArrowBack, stringResource(R.string.back))
                        }
                    }
                },
            )
        },
    ) { padding ->
        Box(Modifier.fillMaxSize().padding(padding)) { content() }
    }
}

// ---- choose ----

@Composable
private fun ChooseContent(onScan: () -> Unit, onBrowse: () -> Unit) {
    Column(
        Modifier
            .fillMaxSize()
            .verticalScroll(rememberScrollState())
            .padding(horizontal = 16.dp, vertical = 8.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        Text(
            stringResource(R.string.pair_intro),
            style = MaterialTheme.typography.bodyLarge,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.padding(horizontal = 4.dp, vertical = 4.dp),
        )
        BigOption(Icons.Rounded.QrCodeScanner, stringResource(R.string.pair_qr_title), stringResource(R.string.pair_qr_body), onScan)
        BigOption(Icons.Rounded.Lan, stringResource(R.string.pair_network_title), stringResource(R.string.pair_network_body), onBrowse)
    }
}

@Composable
private fun BigOption(icon: ImageVector, title: String, body: String, onClick: () -> Unit) {
    OutlinedCard(onClick = onClick, shape = MaterialTheme.shapes.extraLarge, modifier = Modifier.fillMaxWidth()) {
        Row(Modifier.padding(20.dp), verticalAlignment = Alignment.CenterVertically) {
            IconBadge(icon, size = 56.dp)
            Spacer(Modifier.width(16.dp))
            Column(Modifier.weight(1f)) {
                Text(title, style = MaterialTheme.typography.titleLarge)
                Spacer(Modifier.height(4.dp))
                Text(body, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            Icon(Icons.AutoMirrored.Rounded.KeyboardArrowRight, null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

// ---- way A: scan ----

@Composable
private fun ScanContent(problem: PairingQr.Companion.Problem?, onScanned: (String) -> Boolean) {
    val context = LocalContext.current
    var granted by rememberSaveable {
        mutableStateOf(ContextCompat.checkSelfPermission(context, Manifest.permission.CAMERA) == PackageManager.PERMISSION_GRANTED)
    }
    var denied by rememberSaveable { mutableStateOf(false) }
    val launcher = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { ok ->
        granted = ok
        denied = !ok
    }
    Column(
        Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(16.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        if (!granted) {
            CameraRationale(
                denied = denied,
                permanentlyDenied = denied && context.findActivity()?.shouldShowRequestPermissionRationale(Manifest.permission.CAMERA) == false,
                onAllow = { launcher.launch(Manifest.permission.CAMERA) },
            )
            return@Column
        }
        Text(stringResource(R.string.pair_scan_title), style = MaterialTheme.typography.headlineSmall)
        Spacer(Modifier.height(16.dp))
        QrScanner(
            onResult = onScanned,
            modifier = Modifier
                .widthIn(max = 420.dp)
                .fillMaxWidth()
                .aspectRatio(1f)
                .clip(MaterialTheme.shapes.extraLarge),
        )
        Spacer(Modifier.height(16.dp))
        val message = when (problem) {
            null -> stringResource(R.string.pair_scan_hint)
            PairingQr.Companion.Problem.NOT_PHONETOPC -> stringResource(R.string.pair_scan_not_ours)
            PairingQr.Companion.Problem.UNSUPPORTED_VERSION -> stringResource(R.string.pair_scan_version)
            PairingQr.Companion.Problem.MALFORMED -> stringResource(R.string.pair_scan_malformed)
        }
        Text(
            message,
            style = MaterialTheme.typography.bodyMedium,
            textAlign = TextAlign.Center,
            color = if (problem == null) MaterialTheme.colorScheme.onSurfaceVariant else MaterialTheme.colorScheme.error,
        )
    }
}

@Composable
private fun CameraRationale(denied: Boolean, permanentlyDenied: Boolean, onAllow: () -> Unit) {
    val context = LocalContext.current
    Spacer(Modifier.height(24.dp))
    IconBadge(Icons.Rounded.PhotoCamera, size = 88.dp)
    Spacer(Modifier.height(24.dp))
    Text(stringResource(R.string.pair_camera_title), style = MaterialTheme.typography.titleLarge)
    Spacer(Modifier.height(8.dp))
    Text(
        stringResource(if (denied) R.string.pair_camera_denied else R.string.pair_camera_body),
        style = MaterialTheme.typography.bodyMedium,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
        textAlign = TextAlign.Center,
        modifier = Modifier.widthIn(max = 360.dp),
    )
    Spacer(Modifier.height(24.dp))
    if (permanentlyDenied) {
        Button(onClick = {
            runCatching {
                context.startActivity(
                    Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, "package:${context.packageName}".toUri()),
                )
            }
        }) { Text(stringResource(R.string.pair_camera_settings)) }
    } else {
        Button(onClick = onAllow) { Text(stringResource(R.string.pair_camera_allow)) }
    }
}

// ---- way B: browse ----

@Composable
private fun BrowseContent(found: List<FoundPc>, failed: Boolean, onPick: (NetworkPc) -> Unit, onUseQr: () -> Unit) {
    LazyColumn(
        contentPadding = PaddingValues(16.dp),
        verticalArrangement = Arrangement.spacedBy(8.dp),
        modifier = Modifier.fillMaxSize(),
    ) {
        item {
            Text(stringResource(R.string.pair_browse_title), style = MaterialTheme.typography.headlineSmall)
            Spacer(Modifier.height(12.dp))
            if (!failed) {
                LinearProgressIndicator(Modifier.fillMaxWidth())
                Spacer(Modifier.height(8.dp))
                Text(
                    stringResource(R.string.pair_browse_searching),
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
        items(found, key = { it.serviceName }) { entry ->
            val pc = entry.pc
            OutlinedCard(
                onClick = { pc?.let(onPick) },
                enabled = pc != null,
                shape = MaterialTheme.shapes.large,
                modifier = Modifier.fillMaxWidth(),
            ) {
                ListItem(
                    colors = ListItemDefaults.colors(containerColor = Color.Transparent),
                    leadingContent = { IconBadge(Icons.Rounded.Computer) },
                    headlineContent = { Text(pc?.name ?: entry.serviceName, style = MaterialTheme.typography.titleMedium) },
                    supportingContent = {
                        Text(
                            pc?.let { "${it.hosts.first()}:${it.port}" } ?: stringResource(R.string.pair_browse_resolving),
                            fontFamily = if (pc != null) FontFamily.Monospace else null,
                        )
                    },
                    trailingContent = { Icon(Icons.AutoMirrored.Rounded.KeyboardArrowRight, null) },
                )
            }
        }
        item {
            if (failed || found.isEmpty()) {
                Spacer(Modifier.height(8.dp))
                Text(
                    stringResource(if (failed) R.string.pair_browse_failed else R.string.pair_browse_empty),
                    style = MaterialTheme.typography.bodyMedium,
                    color = if (failed) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            Spacer(Modifier.height(16.dp))
            OutlinedButton(onClick = onUseQr) {
                Icon(Icons.Rounded.QrCodeScanner, null, Modifier.size(18.dp))
                Spacer(Modifier.width(8.dp))
                Text(stringResource(R.string.pair_use_qr_instead))
            }
        }
    }
}

// ---- running ----

@Composable
private fun RunContent(
    state: PairingState,
    devMode: Boolean,
    onAnswer: (Boolean) -> Unit,
    onCancel: () -> Unit,
    onRetry: () -> Unit,
    onDone: () -> Unit,
    onOpenDevices: () -> Unit,
) {
    Column(
        Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(24.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
        verticalArrangement = Arrangement.Center,
    ) {
        when (state) {
            PairingState.Idle -> Progress(stringResource(R.string.pair_connecting_unknown), onCancel)
            is PairingState.Connecting -> Progress(
                state.pcName?.let { stringResource(R.string.pair_connecting, it) } ?: stringResource(R.string.pair_connecting_unknown),
                onCancel,
            )
            is PairingState.WaitingForPcApproval -> {
                Progress(stringResource(R.string.pair_approve_title), onCancel) {
                    Text(
                        stringResource(R.string.pair_approve_body, phoneName()),
                        style = MaterialTheme.typography.bodyMedium,
                        textAlign = TextAlign.Center,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
            is PairingState.CompareCode -> CompareCode(state.code, state.pcName, waiting = false, onAnswer, onCancel)
            is PairingState.WaitingForPcCode -> CompareCode(state.code, state.pcName, waiting = true, onAnswer, onCancel)
            is PairingState.Finishing -> Progress(stringResource(R.string.pair_finishing), onCancel = null)
            is PairingState.Paired -> Success(state.pcName, devMode, onDone, onOpenDevices)
            is PairingState.Failed -> Failure(state.failure, onRetry, onCancel)
        }
    }
}

@Composable
private fun phoneName(): String {
    val context = LocalContext.current
    return DeviceIdentity.name(context)
}

@Composable
private fun Progress(title: String, onCancel: (() -> Unit)?, body: (@Composable () -> Unit)? = null) {
    CircularProgressIndicator(Modifier.size(56.dp))
    Spacer(Modifier.height(24.dp))
    Text(title, style = MaterialTheme.typography.titleLarge, textAlign = TextAlign.Center)
    if (body != null) {
        Spacer(Modifier.height(8.dp))
        body()
    }
    if (onCancel != null) {
        Spacer(Modifier.height(24.dp))
        TextButton(onClick = onCancel) { Text(stringResource(R.string.cancel)) }
    }
}

@Composable
private fun CompareCode(code: String, pcName: String, waiting: Boolean, onAnswer: (Boolean) -> Unit, onCancel: () -> Unit) {
    Card(
        shape = MaterialTheme.shapes.extraLarge,
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.primaryContainer),
        modifier = Modifier.fillMaxWidth().widthIn(max = 420.dp),
    ) {
        Column(Modifier.fillMaxWidth().padding(vertical = 32.dp, horizontal = 16.dp), horizontalAlignment = Alignment.CenterHorizontally) {
            Text(stringResource(R.string.pair_code_label), style = MaterialTheme.typography.labelLarge)
            Spacer(Modifier.height(8.dp))
            Text(
                Sas.format(code),
                fontFamily = FontFamily.Monospace,
                fontWeight = FontWeight.SemiBold,
                fontSize = 56.sp,
                lineHeight = 64.sp,
                style = MaterialTheme.typography.displayLarge,
                modifier = Modifier.padding(vertical = 8.dp),
            )
        }
    }
    Spacer(Modifier.height(24.dp))
    if (waiting) {
        CircularProgressIndicator(Modifier.size(32.dp))
        Spacer(Modifier.height(12.dp))
        Text(stringResource(R.string.pair_code_waiting), style = MaterialTheme.typography.titleMedium, textAlign = TextAlign.Center)
        Spacer(Modifier.height(16.dp))
        TextButton(onClick = onCancel) { Text(stringResource(R.string.cancel)) }
    } else {
        Text(
            stringResource(R.string.pair_code_question, pcName),
            style = MaterialTheme.typography.titleMedium,
            textAlign = TextAlign.Center,
        )
        Spacer(Modifier.height(24.dp))
        Row(horizontalArrangement = Arrangement.spacedBy(12.dp), modifier = Modifier.widthIn(max = 420.dp)) {
            OutlinedButton(onClick = { onAnswer(false) }, modifier = Modifier.weight(1f).height(56.dp)) {
                Text(stringResource(R.string.pair_code_mismatch))
            }
            Button(onClick = { onAnswer(true) }, modifier = Modifier.weight(1f).height(56.dp)) {
                Text(stringResource(R.string.pair_code_match))
            }
        }
    }
}

@Composable
private fun Success(pcName: String, devMode: Boolean, onDone: () -> Unit, onOpenDevices: () -> Unit) {
    IconBadge(
        Icons.Rounded.CheckCircle,
        size = 88.dp,
        container = MaterialTheme.colorScheme.primaryContainer,
        content = MaterialTheme.colorScheme.onPrimaryContainer,
    )
    Spacer(Modifier.height(24.dp))
    Text(stringResource(R.string.pair_success_title, pcName), style = MaterialTheme.typography.headlineSmall, textAlign = TextAlign.Center)
    Spacer(Modifier.height(8.dp))
    Text(
        stringResource(R.string.pair_success_body),
        style = MaterialTheme.typography.bodyMedium,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
        textAlign = TextAlign.Center,
        modifier = Modifier.widthIn(max = 360.dp),
    )
    if (devMode) {
        Spacer(Modifier.height(16.dp))
        Card(
            colors = CardDefaults.cardColors(
                containerColor = MaterialTheme.colorScheme.errorContainer,
                contentColor = MaterialTheme.colorScheme.onErrorContainer,
            ),
            shape = MaterialTheme.shapes.large,
            modifier = Modifier.widthIn(max = 420.dp),
        ) {
            Row(Modifier.padding(16.dp)) {
                Icon(Icons.Rounded.Warning, null, Modifier.size(20.dp))
                Spacer(Modifier.width(12.dp))
                Text(stringResource(R.string.pair_success_dev_mode), style = MaterialTheme.typography.bodyMedium)
            }
        }
    }
    Spacer(Modifier.height(24.dp))
    Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
        OutlinedButton(onClick = onOpenDevices) { Text(stringResource(R.string.pair_view_devices)) }
        Button(onClick = onDone) { Text(stringResource(R.string.pair_done)) }
    }
}

@Composable
private fun Failure(failure: PairingFailure, onRetry: () -> Unit, onBack: () -> Unit) {
    IconBadge(
        Icons.Rounded.ErrorOutline,
        size = 88.dp,
        container = MaterialTheme.colorScheme.errorContainer,
        content = MaterialTheme.colorScheme.onErrorContainer,
    )
    Spacer(Modifier.height(24.dp))
    Text(stringResource(R.string.pair_failed_title), style = MaterialTheme.typography.headlineSmall, textAlign = TextAlign.Center)
    Spacer(Modifier.height(8.dp))
    Text(
        failureMessage(failure),
        style = MaterialTheme.typography.bodyMedium,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
        textAlign = TextAlign.Center,
        modifier = Modifier.widthIn(max = 360.dp),
    )
    Spacer(Modifier.height(24.dp))
    Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
        OutlinedButton(onClick = onBack) { Text(stringResource(R.string.back)) }
        Button(onClick = onRetry) { Text(stringResource(R.string.try_again)) }
    }
}

@Composable
private fun failureMessage(f: PairingFailure): String = when (f) {
    PairingFailure.BadSecret -> stringResource(R.string.pair_err_bad_secret)
    PairingFailure.Rejected -> stringResource(R.string.pair_err_rejected)
    PairingFailure.QrOnly -> stringResource(R.string.pair_err_qr_only)
    PairingFailure.OutOfOrder -> stringResource(R.string.pair_err_out_of_order)
    PairingFailure.CodesDiffer -> stringResource(R.string.pair_err_codes_differ)
    PairingFailure.CommitmentMismatch -> stringResource(R.string.pair_err_commitment)
    PairingFailure.Expired -> stringResource(R.string.pair_err_expired)
    PairingFailure.UnsupportedProtocol -> stringResource(R.string.pair_err_unsupported)
    PairingFailure.PinMismatch -> stringResource(R.string.pair_err_pin_mismatch)
    is PairingFailure.IdTaken -> stringResource(R.string.pair_err_id_taken, f.pairedName)
    is PairingFailure.Unreachable -> stringResource(R.string.pair_err_unreachable)
    is PairingFailure.ConnectionLost -> stringResource(R.string.pair_err_connection_lost)
    PairingFailure.TimedOut -> stringResource(R.string.pair_err_timeout)
    is PairingFailure.Protocol -> stringResource(R.string.pair_err_protocol, f.detail)
}

// ---- previews ----

@Preview(showBackground = true)
@Composable
private fun ChoosePreview() = PhonetopcTheme(dynamicColor = false) {
    PairScaffold(showBack = false, onBack = {}) { ChooseContent({}, {}) }
}

@Preview(showBackground = true)
@Composable
private fun CodePreview() = PhonetopcTheme(dynamicColor = false) {
    PairScaffold(showBack = true, onBack = {}) {
        RunContent(PairingState.CompareCode("arch-desktop", "940411"), false, {}, {}, {}, {}, {})
    }
}

@Preview(showBackground = true)
@Composable
private fun FailedPreview() = PhonetopcTheme(darkTheme = true, dynamicColor = false) {
    PairScaffold(showBack = true, onBack = {}) {
        RunContent(PairingState.Failed(PairingFailure.PinMismatch, "arch-desktop"), false, {}, {}, {}, {}, {})
    }
}
