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
import io.github.melker12345.phonetopc.approval.AndroidApprover
import io.github.melker12345.phonetopc.data.ApprovalKey
import io.github.melker12345.phonetopc.data.DeviceIdentity
import io.github.melker12345.phonetopc.protocol.approval.ApprovalService
import io.github.melker12345.phonetopc.data.PhoneKey
import io.github.melker12345.phonetopc.protocol.BridgeHub
import io.github.melker12345.phonetopc.protocol.SecureAccess
import io.github.melker12345.phonetopc.protocol.SensorCatalog
import io.github.melker12345.phonetopc.protocol.SensorMode
import io.github.melker12345.phonetopc.protocol.ServerInfo
import io.github.melker12345.phonetopc.server.PhonetopcServer
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.delay
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.drop
import kotlinx.coroutines.flow.filterNotNull
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext

/**
 * Foreground service (type connectedDevice, plus location while the `location` source may be
 * served) that runs the WebSocket server, advertises it via mDNS and keeps the process alive.
 * Secure mode (the default) serves `wss://` with the phone's Keystore key and only lets paired
 * PCs in; developer mode serves plain `ws://`.
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

    /** The foreground service types we're running with. */
    private var foregroundTypes = 0
    private var lastAddress: String? = null

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
            ACTION_REFRESH -> {
                if (!inForeground) {
                    // Raced with a stop: don't linger as a plain started service.
                    if (startJob == null) stopSelf()
                    return START_NOT_STICKY
                }
                refreshForeground()
            }
            ACTION_START -> {
                val auto = intent.getBooleanExtra(EXTRA_AUTO_START, false)
                if (!auto) setDesiredRunning(true)
                startBridge(auto)
            }
            else -> startBridge(auto = true) // a sticky restart with a null intent
        }
        return START_STICKY
    }

    private fun goForeground(): Boolean {
        val notification = BridgeNotifications.build(this, BridgeStatus(state = RunState.STARTING), null)
        return startForegroundAs(wantedTypes(), notification)
    }

    /**
     * Type location lets the service use location while the app is in the background, but
     * Android 14+ only accepts it with the permission granted (and refuses it in a start from the
     * background, e.g. after boot, unless "Allow all the time" was granted), so it is only asked
     * for when location may be served and dropped if refused.
     */
    @SuppressLint("InlinedApi") // the constants are inlined; ServiceCompat drops types below API 29
    private fun wantedTypes(): Int {
        val source = appGraph.sensorSource
        val location = source.locationPermitted() && SensorCatalog.LOCATION !in source.disabledSources
        return ServiceInfo.FOREGROUND_SERVICE_TYPE_CONNECTED_DEVICE or
            (if (location) ServiceInfo.FOREGROUND_SERVICE_TYPE_LOCATION else 0)
    }

    @SuppressLint("InlinedApi")
    private fun startForegroundAs(types: Int, notification: android.app.Notification): Boolean {
        val connectedOnly = ServiceInfo.FOREGROUND_SERVICE_TYPE_CONNECTED_DEVICE
        for (t in if (types == connectedOnly) listOf(types) else listOf(types, connectedOnly)) {
            try {
                ServiceCompat.startForeground(this, BridgeNotifications.NOTIFICATION_ID, notification, t)
                inForeground = true
                foregroundTypes = t
                return true
            } catch (e: Exception) {
                // e.g. ForegroundServiceStartNotAllowedException on a background sticky restart,
                // or a SecurityException for type location.
                Log.w(TAG, "startForeground(types=$t) failed", e)
            }
        }
        return false
    }

    /**
     * Re-declares the foreground service types after the location permission or the location
     * toggle changed, so a permission granted while the server runs applies without a restart.
     * Adding type location only works while the app is in the foreground: the UI asks for this.
     */
    private fun refreshForeground() {
        if (!inForeground) return
        val wanted = wantedTypes()
        if (wanted == foregroundTypes) return
        startForegroundAs(wanted, BridgeNotifications.build(this, controller.status.value, lastAddress))
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
            val secure = !settings.devMode
            val deviceId = graph.settings.deviceId()
            val source = graph.sensorSource.apply { disabledSources = settings.disabledSources }
            refreshForeground()

            // Secure mode (SECURITY.md §3): the Keystore key for TLS, and paired PCs' tokens.
            val tls = if (!secure) {
                null
            } else {
                graph.pairedPcs.current() // loaded before the first hello can arrive
                withContext(Dispatchers.IO) { runCatching { PhoneKey.identity() } }.getOrElse { e ->
                    Log.e(TAG, "no TLS key", e)
                    fail(getString(R.string.error_tls_key))
                    return@launch
                }
            }
            val security = if (!secure) {
                null
            } else {
                SecureAccess(
                    authenticate = { token -> graph.pairedPcs.authenticate(token)?.info },
                    onAuthenticated = { pc -> graph.appScope.launch { graph.pairedPcs.touch(pc.id) } },
                )
            }
            // APPROVAL.md: secure mode only. Create or check the approval key now, off the
            // main thread, so the first hello doesn't wait for key generation.
            val approvals = if (!secure) {
                null
            } else {
                withContext(Dispatchers.IO) { ApprovalKey.status(applicationContext, fresh = true) }
                ApprovalService(AndroidApprover(applicationContext), onError = { e -> Log.w(TAG, "approval failed", e) })
            }
            source.recheckAvailability()
            val newHub = BridgeHub(
                source = source,
                serverInfo = ServerInfo("phonetopc-android", BuildConfig.VERSION_NAME),
                deviceInfoProvider = { DeviceIdentity.info(applicationContext, deviceId) },
                security = security,
                approvals = approvals,
            )
            val newServer = PhonetopcServer(newHub, port = settings.port, tls = tls)
            // Tracked before it starts: start() blocks and can't be cancelled, so a Stop
            // meanwhile must find it here; teardown() then stops it once start() is done.
            hub = newHub
            server = newServer
            // Location switched on/off or permitted: list_sensors and running streams follow.
            source.onAvailabilityChanged = { newHub.refreshAvailability() }
            val started = withContext(Dispatchers.IO) {
                graph.bridge.serverLock.withLock {
                    // Taking a free lock doesn't check for cancellation: a Stop whose stop()
                    // already ran must not be followed by a start.
                    ensureActive()
                    runCatching { newServer.start() }
                }
            }
            started.exceptionOrNull()?.let { e ->
                Log.w(TAG, "server start failed", e)
                fail(getString(R.string.error_port_in_use, settings.port))
                return@launch
            }
            controller.update { it.copy(state = RunState.RUNNING, port = settings.port, secure = secure) }

            mdns = MdnsAdvertiser(this@BridgeService, DeviceIdentity.name(this@BridgeService), settings.port, deviceId, secure)
                .also { it.start() }

            observe(newHub, graph.networkMonitor, devMode = settings.devMode)
        }
    }

    /** Runs until the bridge is torn down: [startJob] is cancelled, and these with it. */
    private suspend fun observe(hub: BridgeHub, network: NetworkMonitor, devMode: Boolean) = coroutineScope {
        val graph = appGraph
        // Status for the UI and the notification.
        launch {
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
        launch {
            network.addresses.drop(1).collect { mdns?.refresh() }
        }
        // Wake lock only while a continuous source streams.
        launch {
            hub.demand
                .map { demand -> demand.keys.any { SensorCatalog[it]?.mode == SensorMode.CONTINUOUS } }
                .distinctUntilChanged()
                .collect { setWakeLock(it) }
        }
        // SECURITY.md §5 and §4.4: a revoked PC loses its sessions now, and re-pairing closes
        // the sessions that used the old token (1008). Sessions with the new token stay.
        launch {
            var known = graph.pairedPcs.current().associate { it.id to it.tokenHash }
            graph.pairedPcs.loaded.filterNotNull().collect { list ->
                val now = list.associate { it.id to it.tokenHash }
                val changed = known.filter { (id, hash) -> now[id] != hash }.keys
                if (changed.isNotEmpty()) {
                    hub.disconnectSessions { pc, tokenHash -> pc.id in changed && tokenHash != now[pc.id] }
                }
                known = now
            }
        }
        // Per-sensor toggles apply live; switching between dev and secure mode restarts.
        launch {
            graph.settings.settings.collect { s ->
                if (s.devMode != devMode) {
                    restart()
                    return@collect
                }
                if (graph.sensorSource.disabledSources != s.disabledSources) {
                    graph.sensorSource.disabledSources = s.disabledSources
                    hub.refreshAvailability()
                    refreshForeground()
                }
            }
        }
    }

    /** Stop serving and start again with the current settings, staying in the foreground. */
    private fun restart() {
        teardown()
        startBridge(auto = false)
    }

    /** Persisted on the app scope so it survives the service stopping right after. */
    private fun setDesiredRunning(desired: Boolean) {
        val graph = appGraph
        graph.appScope.launch { graph.settings.setDesiredRunning(desired) }
    }

    private fun updateNotification(address: String?) {
        lastAddress = address
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
        appGraph.sensorSource.onAvailabilityChanged = null
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

        /** Re-declare the foreground service types (location permission or toggle changed). */
        const val ACTION_REFRESH = "io.github.melker12345.phonetopc.action.REFRESH"

        /** Boolean extra on [ACTION_START]: started by the system (update/boot), not the user. */
        const val EXTRA_AUTO_START = "io.github.melker12345.phonetopc.extra.AUTO_START"

        private const val TAG = "BridgeService"
        private const val WAKE_LOCK_TIMEOUT_MS = 10 * 60 * 1000L
        private const val WAKE_LOCK_RENEW_MS = 5 * 60 * 1000L
    }
}
