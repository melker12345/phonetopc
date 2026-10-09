package io.github.melker12345.phonetopc.ui

import android.app.Application
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import io.github.melker12345.phonetopc.appGraph
import io.github.melker12345.phonetopc.data.AppSettings
import io.github.melker12345.phonetopc.service.BridgeStatus
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.launch

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

    init {
        viewModelScope.launch { _deviceId.value = graph.settings.deviceId() }
    }

    fun hardwarePresent(source: String) = graph.sensorSource.hardwarePresent(source)

    fun startServer() = graph.bridge.start()
    fun stopServer() = graph.bridge.stop()
    fun setPaused(paused: Boolean) = graph.bridge.setPaused(paused)

    fun setDevMode(enabled: Boolean) {
        viewModelScope.launch { graph.settings.setDevMode(enabled) }
    }

    fun setPort(port: Int) {
        viewModelScope.launch { graph.settings.setPort(port) }
    }

    fun setSourceEnabled(source: String, enabled: Boolean) {
        viewModelScope.launch { graph.settings.setSourceEnabled(source, enabled) }
    }

    fun completeOnboarding() {
        viewModelScope.launch { graph.settings.setOnboardingDone(true) }
    }
}
