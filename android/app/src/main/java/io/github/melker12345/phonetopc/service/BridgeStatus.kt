package io.github.melker12345.phonetopc.service

import android.content.Context
import android.content.Intent
import androidx.core.content.ContextCompat
import io.github.melker12345.phonetopc.protocol.ClientSnapshot
import io.github.melker12345.phonetopc.protocol.Demand
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.sync.Mutex

enum class RunState { STOPPED, STARTING, RUNNING, PAUSED }

data class BridgeStatus(
    val state: RunState = RunState.STOPPED,
    /** Port the server is bound to while running. */
    val port: Int? = null,
    /** Why the last start attempt failed, if it did. */
    val error: String? = null,
    val clients: List<ClientSnapshot> = emptyList(),
    /** Sources currently running and their hardware rate. */
    val streaming: Map<String, Demand> = emptyMap(),
) {
    val active: Boolean get() = state != RunState.STOPPED
}

/** UI-facing handle on [BridgeService]: observe [status], send commands. */
class BridgeController(private val context: Context) {
    private val _status = MutableStateFlow(BridgeStatus())
    val status: StateFlow<BridgeStatus> = _status.asStateFlow()

    /** Serialises server start/stop so a quick restart doesn't race the old socket. */
    internal val serverLock = Mutex()

    internal fun update(transform: (BridgeStatus) -> BridgeStatus) = _status.update(transform)

    fun start() {
        _status.update { it.copy(error = null) }
        ContextCompat.startForegroundService(context, intent(BridgeService.ACTION_START))
    }

    fun stop() = send(BridgeService.ACTION_STOP)

    fun setPaused(paused: Boolean) = send(if (paused) BridgeService.ACTION_PAUSE else BridgeService.ACTION_RESUME)

    private fun send(action: String) {
        if (!_status.value.active) return
        // The service is already in the foreground, so a plain startService is allowed.
        context.startService(intent(action))
    }

    private fun intent(action: String) = Intent(context, BridgeService::class.java).setAction(action)
}
