package io.github.melker12345.phonetopc.service

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.util.Log
import io.github.melker12345.phonetopc.appGraph
import kotlinx.coroutines.launch

/**
 * Brings the bridge back after an app update (`MY_PACKAGE_REPLACED`) and, if the user opted
 * in, after a reboot (`BOOT_COMPLETED`). [AutoStart.shouldStart] decides.
 *
 * Why starting a foreground service from here is allowed (checked 2026-10 against
 * developer.android.com):
 * - Android 12+ forbids starting an FGS from the background, but "Exemptions from background
 *   start restrictions" lists: "After the device reboots and receives the
 *   ACTION_BOOT_COMPLETED, ACTION_LOCKED_BOOT_COMPLETED, or ACTION_MY_PACKAGE_REPLACED intent
 *   action in a broadcast receiver." (develop/background-work/services/fgs/restrictions-bg-start)
 * - Apps targeting Android 15+ (we target 36) may not launch `dataSync`, `camera`,
 *   `mediaPlayback`, `phoneCall`, `mediaProjection` or `microphone` (the last since 14) FGS
 *   types from a BOOT_COMPLETED receiver; doing so throws ForegroundServiceStartNotAllowedException.
 *   (about/versions/15/behavior-changes-15#fgs-boot-completed). `connectedDevice` is not on
 *   that list, and Android 16's behavior changes add nothing here.
 * - `connectedDevice` still needs its Android 14 prerequisite, which we meet by declaring
 *   CHANGE_NETWORK_STATE / CHANGE_WIFI_STATE / CHANGE_WIFI_MULTICAST_STATE
 *   (develop/background-work/services/fgs/service-types#connected-device).
 *
 * OEMs and future releases can still refuse, so a refusal (thrown here by
 * startForegroundService, or later by startForeground in the service) is logged and replaced
 * by a plain "Tap to start phonetopc" notification instead of crashing.
 */
class AutoStartReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val trigger = AutoStart.triggerFor(intent.action) ?: return
        val graph = context.appGraph
        // DataStore is async; keep the broadcast (and its FGS exemption) alive until we decide.
        val pending = goAsync()
        graph.appScope.launch {
            try {
                val settings = graph.settings.current()
                if (AutoStart.shouldStart(trigger, settings)) {
                    Log.i(TAG, "$trigger: restarting the bridge")
                    graph.bridge.autoStart()
                } else {
                    Log.i(TAG, "$trigger: not starting ($settings)")
                }
            } catch (e: Exception) {
                Log.w(TAG, "$trigger: auto-start failed", e)
            } finally {
                pending.finish()
            }
        }
    }

    private companion object {
        const val TAG = "AutoStartReceiver"
    }
}
