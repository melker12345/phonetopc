package io.github.melker12345.phonetopc.sensors

import android.app.Activity
import android.content.Intent
import android.nfc.NfcAdapter
import android.nfc.Tag
import android.os.Bundle
import androidx.core.content.IntentCompat
import io.github.melker12345.phonetopc.appGraph

/**
 * Receives tags from Android's NFC dispatch while phonetopc isn't in the foreground (screen on
 * and unlocked), hands them to the `nfc` source and finishes at once, without a UI. In the
 * foreground, MainActivity's reader mode gets the tags instead.
 *
 * It must be exported for the system to start it, so any app can start it too: only NFC
 * actions with a [Tag] are accepted, and the tag is treated as untrusted (see
 * [AndroidSensorSource.emitTag]).
 */
class NfcTagActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        if (savedInstanceState == null) handle(intent)
        finish()
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        handle(intent)
        finish()
    }

    private fun handle(intent: Intent?) {
        if (intent?.action !in NFC_ACTIONS) return
        // A malformed parcel from another app must not crash us.
        val tag = runCatching { IntentCompat.getParcelableExtra(intent!!, NfcAdapter.EXTRA_TAG, Tag::class.java) }
            .getOrNull() ?: return
        appGraph.sensorSource.emitTag(tag)
    }

    private companion object {
        val NFC_ACTIONS = setOf(
            NfcAdapter.ACTION_NDEF_DISCOVERED,
            NfcAdapter.ACTION_TECH_DISCOVERED,
            NfcAdapter.ACTION_TAG_DISCOVERED,
        )
    }
}
