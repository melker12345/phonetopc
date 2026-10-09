package io.github.melker12345.phonetopc.service

import android.content.Context
import android.net.nsd.NsdManager
import android.net.nsd.NsdServiceInfo
import android.util.Log
import androidx.core.content.ContextCompat

/**
 * Advertises `_phonetopc._tcp` with TXT `id`, `proto=1`, `dev=1` (spec §8).
 * Call [refresh] after network changes: NsdManager doesn't reliably re-announce on a new
 * network, so the registration is torn down and made again. Thread-safe: NSD callbacks
 * arrive on a binder thread, so all state is guarded by `this`.
 */
class MdnsAdvertiser(
    context: Context,
    private val serviceName: String,
    private val port: Int,
    private val deviceId: String,
) {
    private val nsd = ContextCompat.getSystemService(context, NsdManager::class.java)
    private var listener: NsdManager.RegistrationListener? = null
    private var reRegisterWhenUnregistered = false
    private var stopped = false

    @Synchronized
    fun start() {
        stopped = false
        register()
    }

    @Synchronized
    fun refresh() {
        if (stopped) return
        val current = listener
        if (current == null) {
            register()
        } else {
            reRegisterWhenUnregistered = true
            unregister(current)
        }
    }

    @Synchronized
    fun stop() {
        stopped = true
        reRegisterWhenUnregistered = false
        listener?.let { unregister(it) }
    }

    @Synchronized
    private fun register() {
        val manager = nsd ?: return
        val info = NsdServiceInfo().apply {
            serviceName = this@MdnsAdvertiser.serviceName
            serviceType = SERVICE_TYPE
            port = this@MdnsAdvertiser.port
            setAttribute("id", deviceId)
            setAttribute("proto", "1")
            setAttribute("dev", "1")
        }
        val l = object : NsdManager.RegistrationListener {
            override fun onServiceRegistered(info: NsdServiceInfo) {
                Log.i(TAG, "registered as ${info.serviceName}")
            }

            override fun onRegistrationFailed(info: NsdServiceInfo, errorCode: Int) {
                Log.w(TAG, "registration failed: $errorCode")
                synchronized(this@MdnsAdvertiser) { if (listener === this) listener = null }
            }

            override fun onServiceUnregistered(info: NsdServiceInfo) {
                synchronized(this@MdnsAdvertiser) {
                    if (listener === this) listener = null
                    if (reRegisterWhenUnregistered && !stopped) {
                        reRegisterWhenUnregistered = false
                        register()
                    }
                }
            }

            override fun onUnregistrationFailed(info: NsdServiceInfo, errorCode: Int) {
                Log.w(TAG, "unregistration failed: $errorCode")
                synchronized(this@MdnsAdvertiser) { if (listener === this) listener = null }
            }
        }
        listener = l
        runCatching { manager.registerService(info, NsdManager.PROTOCOL_DNS_SD, l) }
            .onFailure { Log.w(TAG, "registerService threw", it); listener = null }
    }

    @Synchronized
    private fun unregister(l: NsdManager.RegistrationListener) {
        runCatching { nsd?.unregisterService(l) }.onFailure {
            // Not registered (e.g. registration had failed): treat as unregistered.
            if (listener === l) listener = null
            if (reRegisterWhenUnregistered && !stopped) {
                reRegisterWhenUnregistered = false
                register()
            }
        }
    }

    companion object {
        const val SERVICE_TYPE = "_phonetopc._tcp"
        private const val TAG = "MdnsAdvertiser"
    }
}
