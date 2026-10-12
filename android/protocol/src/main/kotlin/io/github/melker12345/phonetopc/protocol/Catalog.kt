package io.github.melker12345.phonetopc.protocol

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

const val PROTOCOL_VERSION = "phonetopc/1"
val SUPPORTED_PROTOCOLS = listOf(PROTOCOL_VERSION)

@Serializable
enum class SensorMode {
    @SerialName("continuous") CONTINUOUS,
    @SerialName("on_change") ON_CHANGE,
}

@Serializable
enum class Shape {
    @SerialName("number") NUMBER,
    @SerialName("bool") BOOL,
    @SerialName("vec3") VEC3,
    @SerialName("quat") QUAT,
    @SerialName("object") OBJECT,
}

/** Static description of a source from the spec's sensor catalog (§6). */
data class SensorSpec(
    val source: String,
    val types: List<String>,
    val unit: String,
    val shape: Shape,
    val mode: SensorMode,
    /**
     * An on_change source whose value is a state (light, proximity, battery, location), as
     * opposed to an event-like one (`ui`, `nfc`). New subscriptions to a stateful source start with the current
     * value (spec §4.3), and readings that don't change the state are not sent.
     */
    val stateful: Boolean = mode == SensorMode.ON_CHANGE,
)

/** The sensor catalog, spec §6. */
object SensorCatalog {
    const val ACCELEROMETER = "accelerometer"
    const val GYRO = "gyro"
    const val GAME_ROTATION_VECTOR = "game_rotation_vector"
    const val ROTATION_VECTOR = "rotation_vector"
    const val MAGNETOMETER = "magnetometer"
    const val COMPASS = "compass"
    const val BAROMETER = "barometer"
    const val PROXIMITY = "proximity"
    const val LIGHT = "light"
    const val BATTERY = "battery"
    const val LOCATION = "location"
    const val UI = "ui"
    const val NFC = "nfc"

    val all: List<SensorSpec> = listOf(
        SensorSpec(ACCELEROMETER, listOf("motion"), "m/s^2", Shape.VEC3, SensorMode.CONTINUOUS),
        SensorSpec(GYRO, listOf("rotation"), "rad/s", Shape.VEC3, SensorMode.CONTINUOUS),
        SensorSpec(GAME_ROTATION_VECTOR, listOf("orientation"), "", Shape.QUAT, SensorMode.CONTINUOUS),
        SensorSpec(ROTATION_VECTOR, listOf("orientation"), "", Shape.QUAT, SensorMode.CONTINUOUS),
        SensorSpec(MAGNETOMETER, listOf("magnetic_field"), "uT", Shape.VEC3, SensorMode.CONTINUOUS),
        SensorSpec(COMPASS, listOf("heading"), "deg", Shape.NUMBER, SensorMode.CONTINUOUS),
        SensorSpec(BAROMETER, listOf("pressure"), "hPa", Shape.NUMBER, SensorMode.CONTINUOUS),
        SensorSpec(PROXIMITY, listOf("distance"), "cm", Shape.NUMBER, SensorMode.ON_CHANGE),
        SensorSpec(LIGHT, listOf("illuminance"), "lx", Shape.NUMBER, SensorMode.ON_CHANGE),
        SensorSpec(BATTERY, listOf("state"), "%", Shape.OBJECT, SensorMode.ON_CHANGE),
        SensorSpec(LOCATION, listOf("position"), "", Shape.OBJECT, SensorMode.ON_CHANGE),
        SensorSpec(UI, listOf("tile_tap"), "", Shape.BOOL, SensorMode.ON_CHANGE, stateful = false),
        SensorSpec(NFC, listOf("tag"), "", Shape.OBJECT, SensorMode.ON_CHANGE, stateful = false),
    )

    private val bySource = all.associateBy { it.source }

    operator fun get(source: String): SensorSpec? = bySource[source]
}

/** One entry of the `list_sensors` result. Rate fields are only set for continuous sources. */
@Serializable
data class SensorInfo(
    val source: String,
    val types: List<String>,
    val unit: String,
    val shape: Shape,
    val mode: SensorMode,
    @SerialName("min_rate_hz") val minRateHz: Double? = null,
    @SerialName("max_rate_hz") val maxRateHz: Double? = null,
    @SerialName("default_rate_hz") val defaultRateHz: Double? = null,
    val available: Boolean,
) {
    companion object {
        fun of(
            spec: SensorSpec,
            available: Boolean,
            minRateHz: Double? = null,
            maxRateHz: Double? = null,
            defaultRateHz: Double? = null,
        ): SensorInfo {
            val continuous = spec.mode == SensorMode.CONTINUOUS
            return SensorInfo(
                source = spec.source,
                types = spec.types,
                unit = spec.unit,
                shape = spec.shape,
                mode = spec.mode,
                minRateHz = minRateHz.takeIf { continuous },
                maxRateHz = maxRateHz.takeIf { continuous },
                defaultRateHz = defaultRateHz.takeIf { continuous },
                available = available,
            )
        }
    }
}
