package io.github.melker12345.phonetopc.sensors

import kotlin.math.atan2
import kotlin.math.max
import kotlin.math.sqrt

/**
 * Pure conversions from Android sensor values to the wire values of spec §6. No Android
 * classes here, so they run in plain JVM unit tests.
 */
object SensorValues {
    /**
     * A rotation vector's `values` as a unit quaternion `[x, y, z, w]`, or `null` if too short.
     * `values[3]` (the scalar part) is optional on older devices; the quaternion is unit length,
     * so it is derived when absent.
     */
    fun quaternion(v: FloatArray): FloatArray? {
        if (v.size < 3) return null
        val w = if (v.size >= 4) v[3] else sqrt(max(0f, 1f - v[0] * v[0] - v[1] * v[1] - v[2] * v[2]))
        return floatArrayOf(v[0], v[1], v[2], w)
    }

    /**
     * The compass heading of an earth-frame rotation (TYPE_ROTATION_VECTOR) quaternion, in degrees
     * `[0, 360)` clockwise from magnetic north. This is the azimuth of
     * `SensorManager.getOrientation(getRotationMatrixFromVector(...))`, `atan2(R[1], R[4])`,
     * written out for the quaternion: the direction the phone's +y axis (its top edge) points
     * while it lies flat.
     */
    fun azimuthDegrees(x: Float, y: Float, z: Float, w: Float): Double {
        val r1 = 2.0 * (x * y - z * w)
        val r4 = 1.0 - 2.0 * (x * x + z * z)
        return normalizeDegrees(Math.toDegrees(atan2(r1, r4)))
    }

    /** [deg] mapped to `[0, 360)`. Rounds to 0.01° first so a value can't come out as 360. */
    fun normalizeDegrees(deg: Double): Double {
        var d = Math.round((deg % 360.0) * 100.0) / 100.0
        if (d < 0) d = Math.round((d + 360.0) * 100.0) / 100.0
        return if (d >= 360.0) 0.0 else d
    }
}
