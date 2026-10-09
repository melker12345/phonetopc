package io.github.melker12345.phonetopc

import android.content.Intent
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import io.github.melker12345.phonetopc.ui.PhonetopcApp
import io.github.melker12345.phonetopc.ui.theme.PhonetopcTheme

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

    override fun onResume() {
        super.onResume()
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
