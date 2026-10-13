package io.github.melker12345.phonetopc

import android.app.Application
import android.content.Context
import io.github.melker12345.phonetopc.approval.AppVisibility
import io.github.melker12345.phonetopc.approval.ApprovalNotifications
import io.github.melker12345.phonetopc.data.PairedPcRepository
import io.github.melker12345.phonetopc.data.SettingsRepository
import io.github.melker12345.phonetopc.output.AndroidPcNotifier
import io.github.melker12345.phonetopc.output.OutputAccess
import io.github.melker12345.phonetopc.panels.AndroidPanelDisplay
import io.github.melker12345.phonetopc.panels.PanelRequests
import io.github.melker12345.phonetopc.protocol.panels.NotificationService
import io.github.melker12345.phonetopc.protocol.panels.PanelService
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import io.github.melker12345.phonetopc.sensors.AndroidSensorSource
import io.github.melker12345.phonetopc.service.BridgeController
import io.github.melker12345.phonetopc.service.NetworkMonitor
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob

/** Process-wide singletons. Small enough that a DI framework isn't worth it. */
class AppGraph(context: Context) {
    val appScope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    val settings = SettingsRepository(context)
    val pairedPcs = PairedPcRepository(context, appScope)
    val sensorSource = AndroidSensorSource(context)
    val networkMonitor = NetworkMonitor(context, appScope)
    val bridge = BridgeController(context.applicationContext)

    /** What the user allows each PC (PANELS.md §1). */
    val outputAccess = OutputAccess(appScope, pairedPcs, settings)

    /** PC notifications (PANELS.md §3); process-wide, so they outlive a server restart. */
    val notifications = NotificationService(outputAccess, AndroidPcNotifier(context))

    /** Panels (PANELS.md §4–§7); process-wide, so they wait detached across server restarts. */
    val panels = PanelService(outputAccess, AndroidPanelDisplay(context))

    init {
        outputAccess.onChanged = {
            notifications.permissionsChanged()
            panels.permissionsChanged()
        }
        val app = context.applicationContext
        // A "wants to show" notification goes once its panel is on screen or gone.
        appScope.launch { panels.snapshots.collect { PanelRequests.sync(app, it) } }
        // Detached panels expire after 10 minutes (§4.1) even if no PC calls anything.
        appScope.launch {
            while (true) {
                delay(30_000)
                panels.expire()
            }
        }
    }
}

class PhonetopcApp : Application() {
    lateinit var graph: AppGraph
        private set

    override fun onCreate() {
        super.onCreate()
        graph = AppGraph(this)
        AppVisibility.install(this)
        // Exists from the start, so users can adjust it in system settings before a request.
        ApprovalNotifications.ensureChannel(this)
        AndroidPcNotifier.ensureChannels(this)
        PanelRequests.ensureChannel(this)
    }
}

val Context.appGraph: AppGraph get() = (applicationContext as PhonetopcApp).graph
