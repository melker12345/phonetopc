package io.github.melker12345.phonetopc.ui

import android.app.Application
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import io.github.melker12345.phonetopc.appGraph
import io.github.melker12345.phonetopc.data.AppSettings
import io.github.melker12345.phonetopc.data.ApprovalKey
import io.github.melker12345.phonetopc.data.PairedPc
import io.github.melker12345.phonetopc.data.PhoneKey
import io.github.melker12345.phonetopc.protocol.panels.Feature
import io.github.melker12345.phonetopc.service.BridgeStatus
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/** The phone's TLS key as shown in Settings › About. */
data class PhoneKeyInfo(val pin: String, val hardwareBacked: Boolean)

class MainViewModel(app: Application) : AndroidViewModel(app) {
    private val graph = app.appGraph

    /** `null` until DataStore has loaded. */
    val settings: StateFlow<AppSettings?> =
        graph.settings.settings.stateIn(viewModelScope, SharingStarted.Eagerly, null)

    val status: StateFlow<BridgeStatus> = graph.bridge.status

    /** LAN IPv4 addresses; the monitor runs only while this is collected. */
    val addresses: StateFlow<List<String>> = graph.networkMonitor.addresses

    private val _deviceId = MutableStateFlow<String?>(null)
    val deviceId: StateFlow<String?> = _deviceId.asStateFlow()

    /** `null` until loaded. */
    val pairedPcs: StateFlow<List<PairedPc>?> = graph.pairedPcs.loaded

    private val _phoneKey = MutableStateFlow<PhoneKeyInfo?>(null)
    val phoneKey: StateFlow<PhoneKeyInfo?> = _phoneKey.asStateFlow()

    private val _approval = MutableStateFlow<ApprovalKey.Status?>(null)

    /** Settings › Approvals: whether approvals are possible, and the approval key. `null` while checking. */
    val approval: StateFlow<ApprovalKey.Status?> = _approval.asStateFlow()

    /** Re-checks biometrics and the approval key (creating or replacing it if needed). */
    fun refreshApproval() {
        viewModelScope.launch {
            _approval.value = withContext(Dispatchers.IO) {
                runCatching { ApprovalKey.status(getApplication(), fresh = true) }
                    .getOrElse { ApprovalKey.Status.Unavailable(ApprovalKey.Problem.OTHER) }
            }
        }
    }

    init {
        viewModelScope.launch { _deviceId.value = graph.settings.deviceId() }
        viewModelScope.launch {
            _phoneKey.value = withContext(Dispatchers.IO) {
                runCatching { PhoneKey.identity() }.getOrNull()?.let {
                    PhoneKeyInfo(it.pin, PhoneKey.isHardwareBacked(it.privateKey))
                }
            }
        }
    }

    /** SECURITY.md §5: forget the PC; the server closes its open sessions with 1008. */
    fun revoke(id: String) {
        graph.appScope.launch { graph.pairedPcs.remove(id) }
    }

    /** PANELS.md §1: notifications or panels for a paired PC, or ([pcId] `null`) the dev-mode PC. */
    fun setOutputAllowed(pcId: String?, feature: Feature, allowed: Boolean) {
        graph.appScope.launch {
            if (pcId == null) graph.settings.setDevOutputAllowed(feature, allowed) else graph.pairedPcs.setAllowed(pcId, feature, allowed)
        }
    }

    fun hardwarePresent(source: String) = graph.sensorSource.hardwarePresent(source)

    fun locationPermitted() = graph.sensorSource.locationPermitted()

    fun locationEnabled() = graph.sensorSource.locationEnabled()

    fun nfcEnabled() = graph.sensorSource.nfcEnabled()

    fun startServer() = graph.bridge.start()
    fun stopServer() = graph.bridge.stop()
    fun setPaused(paused: Boolean) = graph.bridge.setPaused(paused)

    fun setDevMode(enabled: Boolean) {
        viewModelScope.launch { graph.settings.setDevMode(enabled) }
    }

    fun setPort(port: Int) {
        viewModelScope.launch { graph.settings.setPort(port) }
    }

    fun setStartOnBoot(enabled: Boolean) {
        viewModelScope.launch { graph.settings.setStartOnBoot(enabled) }
    }

    fun setSourceEnabled(source: String, enabled: Boolean) {
        viewModelScope.launch { graph.settings.setSourceEnabled(source, enabled) }
    }

    fun completeOnboarding() {
        viewModelScope.launch { graph.settings.setOnboardingDone(true) }
    }
}
