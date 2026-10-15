package io.github.melker12345.phonetopc.protocol

import io.github.melker12345.phonetopc.protocol.approval.ApprovalKeyInfo
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

val ProtocolJson: Json = Json {
    ignoreUnknownKeys = true
    explicitNulls = false
    encodeDefaults = true
}

@Serializable
data class ServerInfo(val name: String, val version: String)

@Serializable
data class ClientInfo(val name: String? = null, val version: String? = null)

@Serializable
data class DeviceInfo(
    val id: String,
    val name: String,
    val manufacturer: String? = null,
    val model: String? = null,
    val platform: String,
    @SerialName("platform_version") val platformVersion: String? = null,
)

/** A paired PC as the server recognised it from its token (SECURITY.md §5). */
@Serializable
data class PcInfo(val id: String, val name: String)

@Serializable
internal data class HelloParams(val protocol: String, val client: ClientInfo? = null)

@Serializable
internal data class HelloResult(
    val protocol: String,
    val server: ServerInfo,
    val device: DeviceInfo,
    val capabilities: List<String>,
    /** Secure mode only: which pairing the token belongs to. */
    val pc: PcInfo? = null,
    /** With capability `approval`: the approval key (APPROVAL.md §1). */
    val approval: ApprovalKeyInfo? = null,
)

@Serializable
internal data class ListSensorsResult(val sensors: List<SensorInfo>)

@Serializable
internal data class SubscribeResult(
    val subscription: String,
    val source: String,
    val types: List<String>,
    /** Copied from `list_sensors` (spec §4.3, draft 1). */
    val mode: SensorMode,
    /** Effective rate; continuous sources only. */
    @SerialName("rate_hz") val rateHz: Double? = null,
)

/**
 * A single sample produced by a [SensorSource]. The device id, subscription and seq are
 * added by the hub when it is turned into an event.
 *
 * @param value already in the wire shape (number, array, object or bool). Use [Values] to build it.
 * @param ts wall-clock Unix milliseconds when the reading was taken.
 * @param tsMonoNs monotonic nanoseconds (Android elapsedRealtimeNanos timebase).
 */
data class Reading(
    val source: String,
    val type: String,
    val value: JsonElement,
    val unit: String,
    val ts: Long,
    val tsMonoNs: Long,
) {
    /** The `get` result / event body without `subscription` and `seq`. */
    fun toJson(deviceId: String, subscription: String? = null, seq: Long? = null) = buildJsonObject {
        if (subscription != null) put("subscription", subscription)
        if (seq != null) put("seq", seq)
        put("source", source)
        put("type", type)
        put("value", value)
        put("unit", unit)
        put("ts", ts)
        put("ts_mono_ns", tsMonoNs)
        put("device", deviceId)
    }
}

/** Helpers that build values with the wire shapes from spec §6. */
object Values {
    fun number(v: Float): JsonElement = JsonPrimitive(v)
    fun number(v: Double): JsonElement = JsonPrimitive(v)
    fun vec(vararg v: Float): JsonElement = JsonArray(v.map { JsonPrimitive(it) })
    fun bool(v: Boolean): JsonElement = JsonPrimitive(v)
    fun battery(level: Int, charging: Boolean): JsonElement = buildJsonObject {
        put("level", level)
        put("charging", charging)
    }
}

/**
 * What the hub asks of a source.
 * @param rateHz sampling rate for continuous sources, null for on_change sources.
 */
data class Demand(val rateHz: Double?)

fun interface ReadingSink {
    /** Called from any thread, possibly at a high rate. Must not block. */
    fun onReading(reading: Reading)
}

/**
 * Platform side of the bridge (Android SensorManager, a fake in tests, ...).
 * Everything here is driven by [BridgeHub].
 */
interface SensorSource {
    /** Current availability and rate limits for every catalog source. Called often; keep it cheap. */
    fun describe(): List<SensorInfo>

    /**
     * Start, retune or stop a source. `null` means nobody needs it any more and the hardware
     * listener should be released. Called with the hub's lock held, so it must not call back
     * into the hub synchronously except through the [ReadingSink].
     */
    fun setDemand(source: String, demand: Demand?)

    /** Latest known reading for `get`. May briefly start the sensor. `null` if there is none. */
    suspend fun read(source: String): Reading?

    /**
     * The current value of a stateful on_change source, sent as the first event of a new
     * subscription (spec §4.3). Called right after the hub demanded the source, from the
     * session's thread; must not block or call back into the hub.
     *
     * Only return a value that reflects the present state, e.g. one produced by a listener that
     * is running now. A stale cached value would be reported as current. Return `null` when
     * unknown; the first reading the source delivers then becomes the initial event.
     */
    fun currentReading(source: String): Reading? = null

    /** Where readings go; `null` detaches. */
    fun setSink(sink: ReadingSink?)
}
