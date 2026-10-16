package io.github.melker12345.phonetopc.ui.pair

import android.app.Application
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import io.github.melker12345.phonetopc.appGraph
import io.github.melker12345.phonetopc.data.DeviceIdentity
import io.github.melker12345.phonetopc.data.PhoneKey
import io.github.melker12345.phonetopc.pairing.NetworkPc
import io.github.melker12345.phonetopc.pairing.PairingClient
import io.github.melker12345.phonetopc.pairing.PairingQr
import io.github.melker12345.phonetopc.pairing.PairingState
import io.github.melker12345.phonetopc.pairing.PhoneInfo
import io.github.melker12345.phonetopc.pairing.WebSocketPairingConnector
import io.github.melker12345.phonetopc.service.NetworkMonitor
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

enum class PairStep { CHOOSE, SCAN, BROWSE, RUN }

class PairViewModel(app: Application) : AndroidViewModel(app) {
    private val graph = app.appGraph
    private val browser = NsdBrowser(app)

    private val client = PairingClient(
        connector = WebSocketPairingConnector(),
        phone = ::phoneInfo,
        store = { graph.pairedPcs.add(it) },
        paired = { id -> graph.pairedPcs.current().find { it.id == id }?.let { it.name to it.pin } },
    )

    val state: StateFlow<PairingState> = client.state
    val found = browser.found
    val browseFailed = browser.failed

    private val _step = MutableStateFlow(PairStep.CHOOSE)
    val step: StateFlow<PairStep> = _step.asStateFlow()

    private val _scanProblem = MutableStateFlow<PairingQr.Companion.Problem?>(null)

    /** The last scanned code wasn't usable; shown under the viewfinder. */
    val scanProblem: StateFlow<PairingQr.Companion.Problem?> = _scanProblem.asStateFlow()

    private var job: Job? = null
    private var lastQr: Boolean = false
    private var lastPc: NetworkPc? = null

    fun scan() {
        cancelRun()
        browser.stop()
        _scanProblem.value = null
        _step.value = PairStep.SCAN
    }

    fun browse() {
        cancelRun()
        _step.value = PairStep.BROWSE
        browser.start()
    }

    /** From the scanner, on the main thread. Returns whether scanning should stop. */
    fun onScanned(text: String): Boolean {
        if (_step.value != PairStep.SCAN) return true
        val qr = try {
            PairingQr.parse(text)
        } catch (e: PairingQr.Companion.Invalid) {
            _scanProblem.value = e.problem
            return false
        }
        lastQr = true
        lastPc = null
        run { client.pairWithQr(qr) }
        return true
    }

    fun pick(pc: NetworkPc) {
        lastQr = false
        lastPc = pc
        browser.stop()
        run { client.pairOverNetwork(pc) }
    }

    fun answerCode(match: Boolean) = client.answerCode(match)

    /** After a failure: scan again (QR), or retry the same PC (network). */
    fun retry() {
        val pc = lastPc
        when {
            lastQr -> scan()
            pc != null -> pick(pc)
            else -> back()
        }
    }

    /** Abort whatever is running and go back to the two options. */
    fun back() {
        cancelRun()
        browser.stop()
        client.reset()
        _step.value = PairStep.CHOOSE
    }

    private fun run(block: suspend () -> Unit) {
        cancelRun()
        _step.value = PairStep.RUN
        job = viewModelScope.launch { block() }
    }

    private fun cancelRun() {
        job?.cancel()
        job = null
    }

    private suspend fun phoneInfo(): PhoneInfo {
        val settings = graph.settings.current()
        val pin = withContext(Dispatchers.IO) { PhoneKey.identity().pin }
        return PhoneInfo(
            id = graph.settings.deviceId(),
            name = DeviceIdentity.name(getApplication()),
            pin = pin,
            port = settings.port,
            addresses = withContext(Dispatchers.IO) { NetworkMonitor.localAddresses() },
        )
    }

    override fun onCleared() {
        browser.stop()
        super.onCleared()
    }
}
