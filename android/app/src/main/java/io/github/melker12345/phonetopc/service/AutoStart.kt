package io.github.melker12345.phonetopc.service

import android.content.Intent
import io.github.melker12345.phonetopc.data.AppSettings

/** What woke us up without the user opening the app. */
enum class AutoStartTrigger { PACKAGE_REPLACED, BOOT_COMPLETED }

/** Pure decision logic for bringing the bridge back on its own; no Android calls. */
object AutoStart {
    /**
     * `LOCKED_BOOT_COMPLETED` is deliberately not handled: the app isn't direct-boot aware and
     * its DataStore lives in credential-encrypted storage, which can't be read before the
     * first unlock. `BOOT_COMPLETED` arrives after that unlock.
     */
    fun triggerFor(action: String?): AutoStartTrigger? = when (action) {
        Intent.ACTION_MY_PACKAGE_REPLACED -> AutoStartTrigger.PACKAGE_REPLACED
        Intent.ACTION_BOOT_COMPLETED -> AutoStartTrigger.BOOT_COMPLETED
        else -> null
    }

    /**
     * Start only if the user left it running (and didn't stop it). An update restores what the
     * update interrupted; a reboot additionally needs the user's opt-in. The server comes back
     * in whichever mode is set (secure, or dev mode if the user chose it).
     */
    fun shouldStart(trigger: AutoStartTrigger, settings: AppSettings): Boolean {
        if (!settings.desiredRunning) return false
        return when (trigger) {
            AutoStartTrigger.PACKAGE_REPLACED -> true
            AutoStartTrigger.BOOT_COMPLETED -> settings.startOnBoot
        }
    }
}
