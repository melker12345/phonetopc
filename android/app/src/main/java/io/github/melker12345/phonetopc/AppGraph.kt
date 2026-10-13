package io.github.melker12345.phonetopc

import android.app.Application
import android.content.Context
import io.github.melker12345.phonetopc.approval.AppVisibility
import io.github.melker12345.phonetopc.approval.ApprovalNotifications
import io.github.melker12345.phonetopc.data.PairedPcRepository
import io.github.melker12345.phonetopc.data.SettingsRepository
import io.github.melker12345.phonetopc.output.AndroidPcNotifier
import io.github.melker12345.phonetopc.output.OutputAccess
import io.github.melker12345.phonetopc.protocol.panels.NotificationService
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

    init {
        outputAccess.onChanged = { notifications.permissionsChanged() }
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
    }
}

val Context.appGraph: AppGraph get() = (applicationContext as PhonetopcApp).graph
