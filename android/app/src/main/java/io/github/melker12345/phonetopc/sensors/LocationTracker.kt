package io.github.melker12345.phonetopc.sensors

import android.Manifest
import android.annotation.SuppressLint
import android.content.Context
import android.content.pm.PackageManager
import android.location.Location
import android.location.LocationManager
import android.os.Build
import android.os.Looper
import android.os.SystemClock
import androidx.core.content.ContextCompat
import androidx.core.location.LocationListenerCompat
import androidx.core.location.LocationManagerCompat
import androidx.core.location.LocationRequestCompat
import io.github.melker12345.phonetopc.protocol.Reading
import io.github.melker12345.phonetopc.protocol.SensorCatalog

/**
 * The platform [LocationManager] side of the `location` source. No Google Play Services: the
 * fused provider on Android 12+ where the phone has one, else GPS and network.
 */
class LocationTracker(context: Context) {
    private val appContext = context.applicationContext
    private val manager = ContextCompat.getSystemService(appContext, LocationManager::class.java)

    /** The phone has some way to locate itself at all. */
    val present: Boolean = manager != null && manager.allProviders.any { it != LocationManager.PASSIVE_PROVIDER }

    fun fineGranted(): Boolean = granted(Manifest.permission.ACCESS_FINE_LOCATION)

    /** Fine or (Android 12+ "approximate") coarse access. */
    fun permitted(): Boolean = fineGranted() || granted(Manifest.permission.ACCESS_COARSE_LOCATION)

    fun enabled(): Boolean = manager != null && LocationManagerCompat.isLocationEnabled(manager)

    /** Whether `location` can be served now: present, permitted and switched on. */
    fun usable(): Boolean = present && permitted() && enabled()

    private fun granted(permission: String) =
        ContextCompat.checkSelfPermission(appContext, permission) == PackageManager.PERMISSION_GRANTED

    /** The providers to listen to, best first. */
    private fun providers(): List<String> {
        val all = manager?.allProviders.orEmpty()
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S && LocationManager.FUSED_PROVIDER in all) {
            return listOf(LocationManager.FUSED_PROVIDER)
        }
        return buildList {
            // GPS needs fine access; with approximate access only the network provider is left.
            if (fineGranted() && LocationManager.GPS_PROVIDER in all) add(LocationManager.GPS_PROVIDER)
            if (LocationManager.NETWORK_PROVIDER in all) add(LocationManager.NETWORK_PROVIDER)
        }
    }

    /** Start ~1 Hz updates to [listener] on [looper]. Returns false if nothing could be started. */
    @SuppressLint("MissingPermission") // checked by permitted()
    fun start(listener: LocationListenerCompat, looper: Looper): Boolean {
        val lm = manager ?: return false
        if (!permitted()) return false
        val request = LocationRequestCompat.Builder(UPDATE_INTERVAL_MS)
            .setMinUpdateIntervalMillis(UPDATE_INTERVAL_MS)
            .setQuality(LocationRequestCompat.QUALITY_HIGH_ACCURACY)
            .build()
        var started = false
        for (provider in providers()) {
            try {
                LocationManagerCompat.requestLocationUpdates(lm, provider, request, listener, looper)
                started = true
            } catch (_: SecurityException) {
                // Permission revoked meanwhile.
            } catch (_: IllegalArgumentException) {
                // Provider vanished.
            }
        }
        if (!started) stop(listener)
        return started
    }

    fun stop(listener: LocationListenerCompat) {
        manager?.let { LocationManagerCompat.removeUpdates(it, listener) }
    }

    /** The newest last-known fix of any provider no older than [maxAgeMs], or `null`. */
    @SuppressLint("MissingPermission")
    fun lastKnown(maxAgeMs: Long): Location? {
        val lm = manager ?: return null
        if (!permitted()) return null
        val now = SystemClock.elapsedRealtimeNanos()
        return (providers() + LocationManager.PASSIVE_PROVIDER)
            .mapNotNull { runCatching { lm.getLastKnownLocation(it) }.getOrNull() }
            .filter { (now - it.elapsedRealtimeNanos) / 1_000_000 <= maxAgeMs }
            .maxByOrNull { it.elapsedRealtimeNanos }
    }

    companion object {
        const val UPDATE_INTERVAL_MS = 1_000L

        /** A `location` reading for [location], or `null` if it has no accuracy (spec §6). */
        fun reading(location: Location): Reading? {
            val value = SensorValues.position(
                lat = location.latitude,
                lon = location.longitude,
                accuracyM = location.accuracy.takeIf { location.hasAccuracy() },
                altitudeM = location.altitude.takeIf { location.hasAltitude() },
                speedMps = location.speed.takeIf { location.hasSpeed() },
                bearingDeg = location.bearing.takeIf { location.hasBearing() },
            ) ?: return null
            val spec = SensorCatalog[SensorCatalog.LOCATION]!!
            return Reading(spec.source, spec.types.first(), value, spec.unit, location.time, location.elapsedRealtimeNanos)
        }
    }
}
