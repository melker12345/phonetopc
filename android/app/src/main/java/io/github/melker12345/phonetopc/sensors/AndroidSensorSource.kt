package io.github.melker12345.phonetopc.sensors

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.hardware.Sensor
import android.hardware.SensorEvent
import android.hardware.SensorEventListener
import android.hardware.SensorManager
import android.location.Location
import android.location.LocationManager
import android.os.BatteryManager
import android.os.Handler
import android.os.HandlerThread
import android.os.SystemClock
import androidx.core.content.ContextCompat
import androidx.core.location.LocationListenerCompat
import io.github.melker12345.phonetopc.data.SourceToggles
import io.github.melker12345.phonetopc.protocol.Demand
import io.github.melker12345.phonetopc.protocol.Reading
import io.github.melker12345.phonetopc.protocol.ReadingSink
import io.github.melker12345.phonetopc.protocol.SensorCatalog
import io.github.melker12345.phonetopc.protocol.SensorInfo
import io.github.melker12345.phonetopc.protocol.SensorMode
import io.github.melker12345.phonetopc.protocol.SensorSource
import io.github.melker12345.phonetopc.protocol.Values
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.withTimeoutOrNull
import kotlinx.serialization.json.JsonElement
import java.util.concurrent.ConcurrentHashMap
import kotlin.math.max
import kotlin.math.min

/**
 * [SensorSource] backed by SensorManager, the sticky battery broadcast, LocationManager and
 * the Quick Settings tile. Hardware listeners exist only while the hub demands the source; the hub
 * passes the highest rate any subscription asked for and downsamples per subscription.
 */
class AndroidSensorSource(context: Context) : SensorSource {
    private val appContext = context.applicationContext
    private val sensorManager = ContextCompat.getSystemService(appContext, SensorManager::class.java)
    private val thread = HandlerThread("phonetopc-sensors").apply { start() }
    private val handler = Handler(thread.looper)

    @Volatile private var sink: ReadingSink? = null

    /** Set from the user's per-sensor allow toggles. Call `hub.refreshAvailability()` after changing. */
    @Volatile var disabledSources: Set<String> = SourceToggles.OFF_BY_DEFAULT

    private val latest = ConcurrentHashMap<String, Reading>()
    private val listeners = HashMap<String, SensorEventListener>() // guarded by `this`

    /** Sources whose listener is registered, so `latest` reflects the present (read lock-free). */
    private val liveSources: MutableSet<String> = ConcurrentHashMap.newKeySet()
    private var batteryReceiver: BroadcastReceiver? = null // guarded by `this`
    @Volatile private var lastBattery: JsonElement? = null

    private val location = LocationTracker(appContext)
    private var locationListener: LocationListenerCompat? = null // guarded by `this`
    @Volatile private var lastFix: Location? = null // the last fix sent

    /** Location permission granted and location switched on; see [recheckAvailability]. */
    @Volatile private var locationUsable = false

    /**
     * Called (on any thread) when a source's availability changed outside the app, e.g. the
     * user switched location off. The service calls `hub.refreshAvailability()`.
     */
    @Volatile var onAvailabilityChanged: (() -> Unit)? = null

    private val hardware: Map<String, Sensor> = buildMap {
        fun add(source: String, type: Int) {
            sensorManager?.getDefaultSensor(type)?.let { put(source, it) }
        }
        add(SensorCatalog.ACCELEROMETER, Sensor.TYPE_ACCELEROMETER)
        add(SensorCatalog.GYRO, Sensor.TYPE_GYROSCOPE)
        add(SensorCatalog.GAME_ROTATION_VECTOR, Sensor.TYPE_GAME_ROTATION_VECTOR)
        add(SensorCatalog.ROTATION_VECTOR, Sensor.TYPE_ROTATION_VECTOR)
        add(SensorCatalog.MAGNETOMETER, Sensor.TYPE_MAGNETIC_FIELD)
        // Derived from the rotation vector, so it exists exactly when that does.
        add(SensorCatalog.COMPASS, Sensor.TYPE_ROTATION_VECTOR)
        add(SensorCatalog.BAROMETER, Sensor.TYPE_PRESSURE)
        add(SensorCatalog.PROXIMITY, Sensor.TYPE_PROXIMITY)
        add(SensorCatalog.LIGHT, Sensor.TYPE_LIGHT)
    }

    /** Availability ignoring the user's toggles: what the hardware offers. */
    private val baseInfo: Map<String, SensorInfo> = SensorCatalog.all.associate { spec ->
        val sensor = hardware[spec.source]
        val info = when {
            spec.source == SensorCatalog.BATTERY || spec.source == SensorCatalog.UI ->
                SensorInfo.of(spec, available = true)
            spec.source == SensorCatalog.LOCATION -> SensorInfo.of(spec, available = location.present)
            sensor == null -> SensorInfo.of(spec, available = false)
            spec.mode == SensorMode.CONTINUOUS -> {
                // minDelay is the fastest period in µs. With HIGH_SAMPLING_RATE_SENSORS declared
                // Android 12+ doesn't cap this at 200 Hz.
                val maxHz = if (sensor.minDelay > 0) 1_000_000.0 / sensor.minDelay else FALLBACK_MAX_HZ
                val roundedMax = Math.round(maxHz * 10) / 10.0
                SensorInfo.of(
                    spec,
                    available = true,
                    minRateHz = MIN_RATE_HZ,
                    maxRateHz = max(roundedMax, MIN_RATE_HZ),
                    defaultRateHz = min(DEFAULT_RATE_HZ, max(roundedMax, MIN_RATE_HZ)),
                )
            }
            else -> SensorInfo.of(spec, available = true)
        }
        spec.source to info
    }

    init {
        locationUsable = location.usable()
        // Location switched on or off in system settings. (Revoking the permission kills the
        // process; granting it is picked up by recheckAvailability() when the app resumes.)
        val receiver = object : BroadcastReceiver() {
            override fun onReceive(context: Context, intent: Intent) = recheckAvailability()
        }
        val filter = IntentFilter().apply {
            addAction(LocationManager.MODE_CHANGED_ACTION)
            addAction(LocationManager.PROVIDERS_CHANGED_ACTION)
        }
        ContextCompat.registerReceiver(appContext, receiver, filter, null, handler, ContextCompat.RECEIVER_NOT_EXPORTED)
    }

    /** Hardware presence per source, for the settings screen. */
    fun hardwarePresent(source: String): Boolean = baseInfo[source]?.available == true

    /** Location permission (fine or approximate) is granted. */
    fun locationPermitted(): Boolean = location.permitted()

    /** Location is switched on in system settings. */
    fun locationEnabled(): Boolean = location.enabled()

    /**
     * Re-reads availability that can change behind the app's back (location permission and
     * switch) and calls [onAvailabilityChanged] if it changed. Cheap; call it e.g. on resume.
     */
    fun recheckAvailability() {
        val usable = location.usable()
        if (usable == locationUsable) return
        locationUsable = usable
        onAvailabilityChanged?.invoke()
    }

    override fun describe(): List<SensorInfo> {
        val disabled = disabledSources
        return SensorCatalog.all.map { spec ->
            val info = baseInfo.getValue(spec.source)
            val off = spec.source in disabled || (spec.source == SensorCatalog.LOCATION && !locationUsable)
            if (off) info.copy(available = false) else info
        }
    }

    override fun setSink(sink: ReadingSink?) {
        this.sink = sink
    }

    @Synchronized
    override fun setDemand(source: String, demand: Demand?) {
        when (source) {
            SensorCatalog.BATTERY -> if (demand != null) startBattery() else stopBattery()
            SensorCatalog.LOCATION -> if (demand != null) startLocation() else stopLocation()
            SensorCatalog.UI -> Unit // tile taps are pushed by the tile service
            else -> {
                liveSources.remove(source)
                listeners.remove(source)?.let { sensorManager?.unregisterListener(it) }
                val sensor = hardware[source] ?: return
                if (demand == null) return
                val periodUs = demand.rateHz?.let { (1_000_000.0 / it).toInt() } ?: SensorManager.SENSOR_DELAY_NORMAL
                val listener = Listener(source)
                // A value cached while the listener was off may be stale. Forget it: Android
                // delivers the current state of an on_change sensor right after registering,
                // and that reading becomes the subscription's initial event.
                if (SensorCatalog[source]?.stateful == true) latest.remove(source)
                if (sensorManager?.registerListener(listener, sensor, periodUs, handler) == true) {
                    listeners[source] = listener
                    liveSources += source
                }
            }
        }
    }

    override fun currentReading(source: String): Reading? {
        if (source in disabledSources) return null
        return when (source) {
            SensorCatalog.BATTERY -> readBatterySticky()
            SensorCatalog.UI -> null // event-like: no state to report
            SensorCatalog.LOCATION -> if (source in liveSources) latest[source] else null
            else -> if (source in liveSources) latest[source] else null
        }
    }

    override suspend fun read(source: String): Reading? {
        if (source in disabledSources) return null
        return when (source) {
            SensorCatalog.BATTERY -> readBatterySticky()
            SensorCatalog.UI -> latest[source]
            SensorCatalog.LOCATION -> readLocation()
            else -> latest[source] ?: readOnce(source)
        }
    }

    /** Called from the Quick Settings tile. */
    fun emitTileTap() {
        if (SensorCatalog.UI in disabledSources) return
        val reading = Reading(
            SensorCatalog.UI, "tile_tap", Values.bool(true), "",
            System.currentTimeMillis(), SystemClock.elapsedRealtimeNanos(),
        )
        latest[SensorCatalog.UI] = reading
        sink?.onReading(reading)
    }

    // ---- hardware sensors ----

    private inner class Listener(
        private val source: String,
        private val oneShot: CompletableDeferred<Reading>? = null,
    ) : SensorEventListener {
        override fun onSensorChanged(event: SensorEvent) {
            val reading = toReading(source, event) ?: return
            latest[source] = reading
            if (oneShot != null) oneShot.complete(reading) else sink?.onReading(reading)
        }

        override fun onAccuracyChanged(sensor: Sensor?, accuracy: Int) = Unit
    }

    /** Briefly start a sensor to answer `get` when nothing is cached (spec §4.5). */
    private suspend fun readOnce(source: String): Reading? {
        val sensor = hardware[source] ?: return null
        val manager = sensorManager ?: return null
        val result = CompletableDeferred<Reading>()
        val listener = Listener(source, result)
        if (!manager.registerListener(listener, sensor, SensorManager.SENSOR_DELAY_FASTEST, handler)) return null
        return try {
            withTimeoutOrNull(ONE_SHOT_TIMEOUT_MS) { result.await() }
        } finally {
            manager.unregisterListener(listener)
        }
    }

    private fun toReading(source: String, event: SensorEvent): Reading? {
        val spec = SensorCatalog[source] ?: return null
        val v = event.values
        val value = when (source) {
            SensorCatalog.ACCELEROMETER, SensorCatalog.GYRO, SensorCatalog.MAGNETOMETER ->
                if (v.size >= 3) Values.vec(v[0], v[1], v[2]) else return null
            SensorCatalog.GAME_ROTATION_VECTOR, SensorCatalog.ROTATION_VECTOR ->
                Values.vec(*(SensorValues.quaternion(v) ?: return null))
            SensorCatalog.COMPASS -> {
                val q = SensorValues.quaternion(v) ?: return null
                Values.number(SensorValues.azimuthDegrees(q[0], q[1], q[2], q[3]))
            }
            else -> if (v.isNotEmpty()) Values.number(v[0]) else return null
        }
        return Reading(source, spec.types.first(), value, spec.unit, wallClockMillis(event.timestamp), event.timestamp)
    }

    // ---- location ----

    private fun startLocation() {
        if (locationListener != null) return
        latest.remove(SensorCatalog.LOCATION)
        lastFix = null
        val listener = LocationListenerCompat { fix -> onFix(fix) }
        if (!location.start(listener, handler.looper)) return
        locationListener = listener
        liveSources += SensorCatalog.LOCATION
        // Unlike a sensor, LocationManager doesn't report the current state on registering, and
        // a first GPS fix can take a while. A recent last-known fix (with its own timestamp)
        // stands in until then.
        handler.post {
            if (lastFix == null) location.lastKnown(INITIAL_FIX_MAX_AGE_MS)?.let { onFix(it) }
        }
    }

    private fun stopLocation() {
        liveSources -= SensorCatalog.LOCATION
        locationListener?.let { location.stop(it) }
        locationListener = null
    }

    /** On [handler]. */
    private fun onFix(fix: Location) {
        if (SensorCatalog.LOCATION !in liveSources || !fix.hasAccuracy()) return
        val last = lastFix
        val better = SensorValues.isBetterFix(
            fix.accuracy, fix.elapsedRealtimeNanos, fix.provider,
            last?.accuracy, last?.elapsedRealtimeNanos, last?.provider,
        )
        if (!better) return
        val reading = LocationTracker.reading(fix) ?: return
        lastFix = fix
        latest[SensorCatalog.LOCATION] = reading
        sink?.onReading(reading)
    }

    /** `get`: the live fix, a recent last-known one, or the first fix within a few seconds. */
    private suspend fun readLocation(): Reading? {
        if (!locationUsable) return null
        if (SensorCatalog.LOCATION in liveSources) latest[SensorCatalog.LOCATION]?.let { return it }
        location.lastKnown(GET_FIX_MAX_AGE_MS)?.let { fix -> LocationTracker.reading(fix)?.let { return it } }
        val result = CompletableDeferred<Reading>()
        val listener = LocationListenerCompat { fix -> LocationTracker.reading(fix)?.let { result.complete(it) } }
        if (!location.start(listener, handler.looper)) return null
        return try {
            withTimeoutOrNull(LOCATION_ONE_SHOT_TIMEOUT_MS) { result.await() }
        } finally {
            location.stop(listener)
        }
    }

    // ---- battery ----

    private fun startBattery() {
        if (batteryReceiver != null) return
        lastBattery = null
        val receiver = object : BroadcastReceiver() {
            override fun onReceive(context: Context, intent: Intent) {
                val reading = batteryReading(intent) ?: return
                // ACTION_BATTERY_CHANGED also fires for voltage/temperature; only level and
                // charging state are part of the event, so drop repeats.
                if (reading.value == lastBattery) return
                lastBattery = reading.value
                latest[SensorCatalog.BATTERY] = reading
                sink?.onReading(reading)
            }
        }
        batteryReceiver = receiver
        // Sticky system broadcast: the current state is delivered right away.
        ContextCompat.registerReceiver(
            appContext, receiver, IntentFilter(Intent.ACTION_BATTERY_CHANGED), null, handler,
            ContextCompat.RECEIVER_NOT_EXPORTED,
        )
    }

    private fun stopBattery() {
        batteryReceiver?.let { appContext.unregisterReceiver(it) }
        batteryReceiver = null
    }

    private fun readBatterySticky(): Reading? {
        val intent = ContextCompat.registerReceiver(
            appContext, null, IntentFilter(Intent.ACTION_BATTERY_CHANGED), ContextCompat.RECEIVER_NOT_EXPORTED,
        ) ?: return latest[SensorCatalog.BATTERY]
        return batteryReading(intent)?.also { latest[SensorCatalog.BATTERY] = it }
    }

    private fun batteryReading(intent: Intent): Reading? {
        val level = intent.getIntExtra(BatteryManager.EXTRA_LEVEL, -1)
        val scale = intent.getIntExtra(BatteryManager.EXTRA_SCALE, -1)
        if (level < 0 || scale <= 0) return null
        val status = intent.getIntExtra(BatteryManager.EXTRA_STATUS, BatteryManager.BATTERY_STATUS_UNKNOWN)
        val charging = status == BatteryManager.BATTERY_STATUS_CHARGING || status == BatteryManager.BATTERY_STATUS_FULL
        val percent = Math.round(level * 100f / scale)
        return Reading(
            SensorCatalog.BATTERY, "state", Values.battery(percent, charging), "%",
            System.currentTimeMillis(), SystemClock.elapsedRealtimeNanos(),
        )
    }

    companion object {
        const val MIN_RATE_HZ = 1.0
        const val DEFAULT_RATE_HZ = 50.0
        private const val FALLBACK_MAX_HZ = 200.0
        private const val ONE_SHOT_TIMEOUT_MS = 1_000L

        /** A last-known fix this recent is sent as a new location subscription's first event. */
        private const val INITIAL_FIX_MAX_AGE_MS = 2 * 60_000L

        /** `get location` answers with a last-known fix this recent rather than waiting. */
        private const val GET_FIX_MAX_AGE_MS = 10 * 60_000L
        private const val LOCATION_ONE_SHOT_TIMEOUT_MS = 5_000L

        /** Convert a SensorEvent timestamp (elapsedRealtimeNanos timebase) to Unix millis. */
        fun wallClockMillis(eventNanos: Long): Long =
            System.currentTimeMillis() - (SystemClock.elapsedRealtimeNanos() - eventNanos) / 1_000_000
    }
}
