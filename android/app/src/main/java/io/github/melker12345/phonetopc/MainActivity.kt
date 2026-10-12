package io.github.melker12345.phonetopc

import android.content.Intent
import android.nfc.NfcAdapter
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.lifecycleScope
import io.github.melker12345.phonetopc.protocol.SensorCatalog
import io.github.melker12345.phonetopc.ui.PhonetopcApp
import io.github.melker12345.phonetopc.ui.theme.PhonetopcTheme
import kotlinx.coroutines.launch

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        enableEdgeToEdge()
        super.onCreate(savedInstanceState)
        setContent {
            PhonetopcTheme {
                PhonetopcApp()
            }
        }
        if (savedInstanceState == null) handle(intent)
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        handle(intent)
    }

    override fun onPause() {
        super.onPause()
        nfcAdapter?.let { runCatching { it.disableReaderMode(this) } }
    }

    /**
     * While phonetopc is in front, it reads tags itself (reader mode) instead of letting Android
     * dispatch them, which would show a chooser or open another app. Only when the user allows
     * the nfc source.
     */
    private fun enableNfcReader() {
        val adapter = nfcAdapter ?: return
        lifecycleScope.launch {
            if (SensorCatalog.NFC in appGraph.settings.current().disabledSources) return@launch
            if (!lifecycle.currentState.isAtLeast(Lifecycle.State.RESUMED)) return@launch
            val flags = NfcAdapter.FLAG_READER_NFC_A or NfcAdapter.FLAG_READER_NFC_B or
                NfcAdapter.FLAG_READER_NFC_F or NfcAdapter.FLAG_READER_NFC_V or NfcAdapter.FLAG_READER_NFC_BARCODE
            runCatching {
                adapter.enableReaderMode(this@MainActivity, { tag -> appGraph.sensorSource.emitTag(tag) }, flags, null)
            }
        }
    }

    private val nfcAdapter: NfcAdapter? by lazy { runCatching { NfcAdapter.getDefaultAdapter(this) }.getOrNull() }

    override fun onResume() {
        super.onResume()
        enableNfcReader()
        // The location permission or switch may have changed in system settings or a dialog.
        appGraph.sensorSource.recheckAvailability()
        appGraph.bridge.refreshForeground()
        // Started here, once we're in the foreground, so the FGS start is always allowed.
        if (startServerOnResume) {
            startServerOnResume = false
            appGraph.bridge.start()
        }
    }

    private fun handle(intent: Intent?) {
        if (intent == null || intent.flags and Intent.FLAG_ACTIVITY_LAUNCHED_FROM_HISTORY != 0) return
        if (intent.action == ACTION_START_SERVER) {
            intent.action = null // don't start again on recreation
            startServerOnResume = true
        }
    }

    private var startServerOnResume = false

    companion object {
        /** From the "Tap to start phonetopc" notification. */
        const val ACTION_START_SERVER = "io.github.melker12345.phonetopc.action.START_SERVER"
    }
}
