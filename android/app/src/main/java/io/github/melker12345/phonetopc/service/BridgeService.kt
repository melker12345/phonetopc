package io.github.melker12345.phonetopc.service

import android.annotation.SuppressLint
import android.app.Service
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.IBinder
import android.os.PowerManager
import android.util.Log
import androidx.core.app.NotificationManagerCompat
import androidx.core.app.ServiceCompat
import androidx.core.content.ContextCompat
import io.github.melker12345.phonetopc.BuildConfig
import io.github.melker12345.phonetopc.R
import io.github.melker12345.phonetopc.appGraph
import io.github.melker12345.phonetopc.data.DeviceIdentity
import io.github.melker12345.phonetopc.protocol.BridgeHub
import io.github.melker12345.phonetopc.protocol.SensorCatalog
import io.github.melker12345.phonetopc.protocol.SensorMode
import io.github.melker12345.phonetopc.protocol.ServerInfo
import io.github.melker12345.phonetopc.server.PhonetopcServer
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.drop
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext

/**
 * Foreground service (type connectedDevice) that runs the WebSocket server, advertises it via
 * mDNS and keeps the process alive. Draft 1 is plaintext, so it refuses to run unless the
 * user enabled developer mode.
 */
class BridgeService : Service() {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private val controller get() = appGraph.bridge

    private var startJob: Job? = null
    private var hub: BridgeHub? = null
    private var server: PhonetopcServer? = null
    private var mdns: MdnsAdvertiser? = null
    private var wakeLock: PowerManager.WakeLock? = null
    private var wakeLockJob: Job? = null
    private var inForeground = false

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        BridgeNotifications.ensureChannel(this)
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_STOP -> {
                // From the UI toggle or the notification's Stop: the user no longer wants it.
                setDesiredRunning(false)
                shutdown()
                return START_NOT_STICKY
            }
            ACTION_PAUSE -> hub?.setPaused(true) // paused is still "running" as far as intent goes
            ACTION_RESUME -> hub?.setPaused(false)
            ACTION_START -> {
                val auto = intent.getBooleanExtra(EXTRA_AUTO_START, false)
                if (!auto) setDesiredRunning(true)
                startBridge(auto)
            }
            else -> startBridge(auto = true) // a sticky restart with a null intent
        }
        return START_STICKY
    }

    @SuppressLint("InlinedApi") // the constant is inlined; ServiceCompat drops the type below API 29
    private fun goForeground(): Boolean {
        val notification = BridgeNotifications.build(this, BridgeStatus(state = RunState.STARTING), null)
        return try {
            ServiceCompat.startForeground(
                this, BridgeNotifications.NOTIFICATION_ID, notification,
                ServiceInfo.FOREGROUND_SERVICE_TYPE_CONNECTED_DEVICE,
            )
            inForeground = true
            true
        } catch (e: Exception) {
            // e.g. ForegroundServiceStartNotAllowedException on a background sticky restart.
            Log.w(TAG, "startForeground failed", e)
            false
        }
    }

    private fun startBridge(auto: Boolean) {
        if (startJob != null) return
        if (!goForeground()) {
            controller.update { BridgeStatus(error = getString(R.string.error_foreground_not_allowed)) }
            // Nobody is looking at the app, so leave a way back in (see AutoStartReceiver).
            if (auto) BridgeNotifications.postTapToStart(this)
            stopSelf()
            return
        }
        BridgeNotifications.cancelTapToStart(this)
        controller.update { it.copy(state = RunState.STARTING, error = null) }
        startJob = scope.launch {
            val graph = appGraph
            val settings = graph.settings.current()
            if (!settings.devMode) {
                fail(getString(R.string.error_dev_mode_off))
                return@launch
            }
            val deviceId = graph.settings.deviceId()
            val source = graph.sensorSource.apply { disabledSources = settings.disabledSources }
            val newHub = BridgeHub(
                source = source,
                serverInfo = ServerInfo("phonetopc-android", BuildConfig.VERSION_NAME),
                deviceInfoProvider = { DeviceIdentity.info(applicationContext, deviceId) },
            )
            val newServer = PhonetopcServer(newHub, port = settings.port)
            val started = withContext(Dispatchers.IO) {
                graph.bridge.serverLock.withLock { runCatching { newServer.start() } }
            }
            started.exceptionOrNull()?.let { e ->
                Log.w(TAG, "server start failed", e)
                newHub.shutdown()
                fail(getString(R.string.error_port_in_use, settings.port))
                return@launch
            }
            hub = newHub
            server = newServer
            controller.update { it.copy(state = RunState.RUNNING, port = settings.port) }

            mdns = MdnsAdvertiser(this@BridgeService, DeviceIdentity.name(this@BridgeService), settings.port, deviceId)
                .also { it.start() }

            observe(newHub, graph.networkMonitor)
        }
    }

    private fun observe(hub: BridgeHub, network: NetworkMonitor) {
        val graph = appGraph
        // Status for the UI and the notification.
        scope.launch {
            combine(hub.clients, hub.demand, hub.paused, network.addresses) { clients, demand, paused, addrs ->
                controller.update {
                    it.copy(
                        state = if (paused) RunState.PAUSED else RunState.RUNNING,
                        clients = clients,
                        streaming = demand,
                    )
                }
                addrs.firstOrNull()
            }.collect { address -> updateNotification(address) }
        }
        // Re-announce mDNS when the network changes.
        scope.launch {
            network.addresses.drop(1).collect { mdns?.refresh() }
        }
        // Wake lock only while a continuous source streams.
        scope.launch {
            hub.demand
                .map { demand -> demand.keys.any { SensorCatalog[it]?.mode == SensorMode.CONTINUOUS } }
                .distinctUntilChanged()
                .collect { setWakeLock(it) }
        }
        // Per-sensor toggles apply live; turning dev mode off stops the server.
        scope.launch {
            graph.settings.settings.collect { s ->
                if (!s.devMode) {
                    // Turning developer mode off is an explicit stop, so don't bring it back later.
                    setDesiredRunning(false)
                    shutdown()
                    return@collect
                }
                if (graph.sensorSource.disabledSources != s.disabledSources) {
                    graph.sensorSource.disabledSources = s.disabledSources
                    hub.refreshAvailability()
                }
            }
        }
    }

    /** Persisted on the app scope so it survives the service stopping right after. */
    private fun setDesiredRunning(desired: Boolean) {
        val graph = appGraph
        graph.appScope.launch { graph.settings.setDesiredRunning(desired) }
    }

    private fun updateNotification(address: String?) {
        if (!inForeground) return
        val notification = BridgeNotifications.build(this, controller.status.value, address)
        // Without POST_NOTIFICATIONS (13+) the FGS still runs; the notification is just hidden.
        if (NotificationManagerCompat.from(this).areNotificationsEnabled()) {
            runCatching { NotificationManagerCompat.from(this).notify(BridgeNotifications.NOTIFICATION_ID, notification) }
        }
    }

    private fun setWakeLock(needed: Boolean) {
        wakeLockJob?.cancel()
        wakeLockJob = null
        if (!needed) {
            wakeLock?.takeIf { it.isHeld }?.release()
            return
        }
        val lock = wakeLock ?: ContextCompat.getSystemService(this, PowerManager::class.java)
            ?.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "phonetopc:stream")
            ?.apply { setReferenceCounted(false) }
            ?.also { wakeLock = it }
            ?: return
        // Timed acquire, renewed while still needed, so a bug can never pin the CPU forever.
        wakeLockJob = scope.launch {
            while (isActive) {
                lock.acquire(WAKE_LOCK_TIMEOUT_MS)
                delay(WAKE_LOCK_RENEW_MS)
            }
        }
    }

    private fun fail(message: String) {
        controller.update { BridgeStatus(error = message) }
        shutdown()
    }

    private fun shutdown() {
        teardown()
        ServiceCompat.stopForeground(this, ServiceCompat.STOP_FOREGROUND_REMOVE)
        inForeground = false
        stopSelf()
    }

    /** Idempotent. Server stop is blocking, so it runs on the app scope after we're gone. */
    private fun teardown() {
        startJob?.cancel()
        startJob = null
        setWakeLock(false)
        mdns?.stop()
        mdns = null
        hub?.shutdown()
        hub = null
        server?.let { s ->
            val graph = appGraph
            graph.appScope.launch(Dispatchers.IO) { graph.bridge.serverLock.withLock { s.stop() } }
        }
        server = null
        controller.update { BridgeStatus(error = it.error) }
    }

    override fun onDestroy() {
        teardown()
        scope.cancel()
        super.onDestroy()
    }

    companion object {
        const val ACTION_START = "io.github.melker12345.phonetopc.action.START"
        const val ACTION_STOP = "io.github.melker12345.phonetopc.action.STOP"
        const val ACTION_PAUSE = "io.github.melker12345.phonetopc.action.PAUSE"
        const val ACTION_RESUME = "io.github.melker12345.phonetopc.action.RESUME"

        /** Boolean extra on [ACTION_START]: started by the system (update/boot), not the user. */
        const val EXTRA_AUTO_START = "io.github.melker12345.phonetopc.extra.AUTO_START"

        private const val TAG = "BridgeService"
        private const val WAKE_LOCK_TIMEOUT_MS = 10 * 60 * 1000L
        private const val WAKE_LOCK_RENEW_MS = 5 * 60 * 1000L
    }
}
