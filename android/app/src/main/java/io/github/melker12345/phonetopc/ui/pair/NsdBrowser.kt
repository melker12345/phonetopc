package io.github.melker12345.phonetopc.ui.pair

import android.content.Context
import android.net.nsd.NsdManager
import android.net.nsd.NsdServiceInfo
import android.os.Build
import android.util.Log
import androidx.annotation.RequiresApi
import androidx.core.content.ContextCompat
import io.github.melker12345.phonetopc.pairing.NetworkPc
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import java.net.Inet4Address
import java.net.InetAddress
import java.util.concurrent.Executor

/** One `_phonetopc-pair._tcp` service; [pc] is `null` until it has been resolved. */
data class FoundPc(val serviceName: String, val pc: NetworkPc?)

/**
 * Browses `_phonetopc-pair._tcp` (SECURITY.md §4.3) and resolves each service to addresses,
 * port and TXT `n`/`id`/`v`. Uses `registerServiceInfoCallback` on Android 14+, and one
 * `resolveService` at a time before that (older NsdManager rejects concurrent resolves).
 * Callbacks arrive on binder threads; state is guarded by `this`.
 */
class NsdBrowser(context: Context) {
    private val nsd = ContextCompat.getSystemService(context, NsdManager::class.java)
    private val mainExecutor: Executor = ContextCompat.getMainExecutor(context)

    private val _found = MutableStateFlow<List<FoundPc>>(emptyList())
    val found: StateFlow<List<FoundPc>> = _found.asStateFlow()

    private val _failed = MutableStateFlow(false)

    /** Discovery couldn't start (e.g. no network service); the UI suggests the QR code. */
    val failed: StateFlow<Boolean> = _failed.asStateFlow()

    private var discovery: NsdManager.DiscoveryListener? = null
    private val infoCallbacks = HashMap<String, Any>() // NsdManager.ServiceInfoCallback on 34+
    private val resolveQueue = ArrayDeque<NsdServiceInfo>()
    private var resolving = false

    @Synchronized
    fun start() {
        val manager = nsd ?: run { _failed.value = true; return }
        if (discovery != null) return
        _found.value = emptyList()
        _failed.value = false
        val listener = object : NsdManager.DiscoveryListener {
            override fun onDiscoveryStarted(serviceType: String) = Unit
            override fun onDiscoveryStopped(serviceType: String) = Unit
            override fun onStartDiscoveryFailed(serviceType: String, errorCode: Int) {
                Log.w(TAG, "discovery failed: $errorCode")
                synchronized(this@NsdBrowser) { if (discovery === this) discovery = null }
                _failed.value = true
            }
            override fun onStopDiscoveryFailed(serviceType: String, errorCode: Int) = Unit
            override fun onServiceFound(info: NsdServiceInfo) = found(info)
            override fun onServiceLost(info: NsdServiceInfo) = lost(info.serviceName)
        }
        discovery = listener
        runCatching { manager.discoverServices(SERVICE_TYPE, NsdManager.PROTOCOL_DNS_SD, listener) }
            .onFailure { Log.w(TAG, "discoverServices threw", it); discovery = null; _failed.value = true }
    }

    @Synchronized
    fun stop() {
        val manager = nsd ?: return
        discovery?.let { runCatching { manager.stopServiceDiscovery(it) } }
        discovery = null
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            infoCallbacks.values.forEach { unregister(manager, it) }
        }
        infoCallbacks.clear()
        resolveQueue.clear()
        resolving = false
    }

    @Synchronized
    private fun found(info: NsdServiceInfo) {
        if (discovery == null) return
        val name = info.serviceName
        _found.update { list -> if (list.any { it.serviceName == name }) list else list + FoundPc(name, null) }
        val manager = nsd ?: return
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            if (name !in infoCallbacks) watch(manager, info)
        } else {
            if (resolveQueue.none { it.serviceName == name }) resolveQueue.addLast(info)
            resolveNext(manager)
        }
    }

    @Synchronized
    private fun lost(name: String) {
        _found.update { list -> list.filterNot { it.serviceName == name } }
        val manager = nsd ?: return
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            infoCallbacks.remove(name)?.let { unregister(manager, it) }
        }
    }

    private fun resolved(info: NsdServiceInfo, addresses: List<InetAddress>) {
        val attrs = info.attributes.orEmpty()
        fun txt(key: String) = attrs[key]?.let { String(it, Charsets.UTF_8) }?.takeIf { it.isNotBlank() }
        val version = txt("v")
        if (version != null && version != "1") return // a future pairing protocol we don't speak
        // IPv4 first: link-local IPv6 needs a scope and is the most fragile option.
        val hosts = addresses
            .sortedBy { if (it is Inet4Address) 0 else 1 }
            .mapNotNull { it.hostAddress }
            .distinct()
        if (hosts.isEmpty() || info.port <= 0) return
        val pc = NetworkPc(name = txt("n") ?: info.serviceName, hosts = hosts, port = info.port, id = txt("id"))
        synchronized(this) {
            if (discovery == null) return
            _found.update { list ->
                val entry = FoundPc(info.serviceName, pc)
                if (list.any { it.serviceName == info.serviceName }) {
                    list.map { if (it.serviceName == info.serviceName) entry else it }
                } else {
                    list + entry
                }
            }
        }
    }

    // ---- Android 14+: ServiceInfoCallback ----

    @RequiresApi(Build.VERSION_CODES.UPSIDE_DOWN_CAKE)
    private fun watch(manager: NsdManager, info: NsdServiceInfo) {
        val callback = object : NsdManager.ServiceInfoCallback {
            override fun onServiceInfoCallbackRegistrationFailed(errorCode: Int) {
                Log.w(TAG, "info callback failed: $errorCode")
            }
            override fun onServiceUpdated(serviceInfo: NsdServiceInfo) = resolved(serviceInfo, serviceInfo.hostAddresses)
            override fun onServiceLost() = lost(info.serviceName)
            override fun onServiceInfoCallbackUnregistered() = Unit
        }
        infoCallbacks[info.serviceName] = callback
        runCatching { manager.registerServiceInfoCallback(info, mainExecutor, callback) }
            .onFailure { Log.w(TAG, "registerServiceInfoCallback threw", it) }
    }

    @RequiresApi(Build.VERSION_CODES.UPSIDE_DOWN_CAKE)
    private fun unregister(manager: NsdManager, callback: Any) {
        runCatching { manager.unregisterServiceInfoCallback(callback as NsdManager.ServiceInfoCallback) }
    }

    // ---- before Android 14: one resolve at a time ----

    @Synchronized
    private fun resolveNext(manager: NsdManager) {
        if (resolving || discovery == null) return
        val next = resolveQueue.removeFirstOrNull() ?: return
        resolving = true
        @Suppress("DEPRECATION")
        val listener = object : NsdManager.ResolveListener {
            override fun onResolveFailed(info: NsdServiceInfo, errorCode: Int) {
                Log.w(TAG, "resolve failed: $errorCode")
                done()
            }
            override fun onServiceResolved(info: NsdServiceInfo) {
                resolved(info, listOfNotNull(info.host))
                done()
            }
            private fun done() {
                synchronized(this@NsdBrowser) { resolving = false }
                resolveNext(manager)
            }
        }
        @Suppress("DEPRECATION")
        runCatching { manager.resolveService(next, listener) }.onFailure { resolving = false }
    }

    companion object {
        const val SERVICE_TYPE = "_phonetopc-pair._tcp"
        private const val TAG = "NsdBrowser"
    }
}
