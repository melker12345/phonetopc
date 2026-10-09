package io.github.melker12345.phonetopc.service

import android.content.Context
import android.net.ConnectivityManager
import android.net.LinkProperties
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import androidx.core.content.ContextCompat
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.flow.conflate
import kotlinx.coroutines.flow.debounce
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.onStart
import kotlinx.coroutines.flow.stateIn
import java.net.Inet4Address
import java.net.NetworkInterface

/**
 * The phone's LAN IPv4 addresses (Wi-Fi, hotspot, USB/Ethernet tethering), updated on any
 * network change. Only active while someone collects it.
 */
class NetworkMonitor(context: Context, scope: CoroutineScope) {
    private val connectivity = ContextCompat.getSystemService(context, ConnectivityManager::class.java)

    private val changes = callbackFlow {
        val callback = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) { trySend(Unit) }
            override fun onLost(network: Network) { trySend(Unit) }
            override fun onLinkPropertiesChanged(network: Network, linkProperties: LinkProperties) { trySend(Unit) }
        }
        // Every network, not just ones with internet: a LAN without uplink is fine for us.
        val request = NetworkRequest.Builder()
            .removeCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
            .build()
        val cm = connectivity
        if (cm != null) runCatching { cm.registerNetworkCallback(request, callback) }
        awaitClose { if (cm != null) runCatching { cm.unregisterNetworkCallback(callback) } }
    }

    /** Lazy so the first interface scan doesn't run during Application.onCreate. */
    @OptIn(kotlinx.coroutines.FlowPreview::class)
    val addresses: StateFlow<List<String>> by lazy {
        changes
            .conflate()
            .onStart { emit(Unit) }
            .debounce(DEBOUNCE_MS)
            .map { localAddresses() }
            .distinctUntilChanged()
            .stateIn(scope, SharingStarted.WhileSubscribed(5_000), localAddresses())
    }

    companion object {
        private const val DEBOUNCE_MS = 500L

        fun localAddresses(): List<String> {
            val result = mutableListOf<Pair<String, String>>()
            val interfaces = runCatching { NetworkInterface.getNetworkInterfaces() }.getOrNull() ?: return emptyList()
            for (nif in interfaces) {
                if (!runCatching { nif.isUp && !nif.isLoopback }.getOrDefault(false)) continue
                for (addr in nif.inetAddresses) {
                    if (addr is Inet4Address && !addr.isLoopbackAddress && !addr.isLinkLocalAddress) {
                        result += nif.name to (addr.hostAddress ?: continue)
                    }
                }
            }
            // Wi-Fi first (what a PC on the same LAN uses), then hotspot, tethering, the rest.
            // Public addresses (e.g. mobile data) only show up if there is nothing private.
            val ordered = result.sortedBy { (name, _) ->
                when {
                    name.startsWith("wlan") -> 0
                    name.startsWith("ap") || name.startsWith("swlan") -> 1
                    name.startsWith("rndis") || name.startsWith("usb") || name.startsWith("eth") -> 2
                    else -> 3
                }
            }
            val lan = ordered.filter { (_, ip) -> isPrivate(ip) }
            return (lan.ifEmpty { ordered }).map { it.second }.distinct()
        }

        private fun isPrivate(ip: String): Boolean {
            val parts = ip.split('.').mapNotNull { it.toIntOrNull() }
            if (parts.size != 4) return false
            return parts[0] == 10 ||
                (parts[0] == 172 && parts[1] in 16..31) ||
                (parts[0] == 192 && parts[1] == 168)
        }
    }
}
