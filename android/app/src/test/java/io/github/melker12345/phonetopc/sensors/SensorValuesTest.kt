package io.github.melker12345.phonetopc.sensors

import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import kotlin.math.cos
import kotlin.math.sin

class SensorValuesTest {
    /** Quaternion of a rotation by [deg] around the z axis (counter-clockwise seen from above). */
    private fun yaw(deg: Double): FloatArray {
        val half = Math.toRadians(deg) / 2
        return floatArrayOf(0f, 0f, sin(half).toFloat(), cos(half).toFloat())
    }

    private fun azimuth(q: FloatArray) = SensorValues.azimuthDegrees(q[0], q[1], q[2], q[3])

    @Test fun flatPhoneHeadings() {
        // Lying flat with its top to magnetic north, the earth and device frames coincide.
        assertEquals(0.0, azimuth(floatArrayOf(0f, 0f, 0f, 1f)), 1e-9)
        // Turning the phone counter-clockwise points its top west: the heading goes the other way.
        assertEquals(270.0, azimuth(yaw(90.0)), 0.01)
        assertEquals(90.0, azimuth(yaw(-90.0)), 0.01)
        assertEquals(180.0, azimuth(yaw(180.0)), 0.01)
        assertEquals(315.0, azimuth(yaw(45.0)), 0.01)
        // q and -q are the same rotation.
        assertEquals(315.0, azimuth(yaw(45.0).map { -it }.toFloatArray()), 0.01)
    }

    @Test fun tiltDoesNotChangeTheHeading() {
        // Pitch the phone 30° up (around x) after turning it to 60° east of north (yaw -60°).
        val half = Math.toRadians(30.0) / 2
        val pitch = floatArrayOf(sin(half).toFloat(), 0f, 0f, cos(half).toFloat())
        val q = multiply(yaw(-60.0), pitch)
        assertEquals(60.0, azimuth(q), 0.01)
    }

    @Test fun headingStaysBelow360() {
        // A hair west of north must not round up to 360, which the schema forbids.
        for (deg in listOf(1e-4, 1e-3, 0.004)) {
            val h = azimuth(yaw(deg))
            assertTrue("heading $h for yaw $deg", h >= 0.0 && h < 360.0)
        }
        assertEquals(0.0, SensorValues.normalizeDegrees(359.999), 0.0)
        assertEquals(0.0, SensorValues.normalizeDegrees(-0.001), 0.0)
        assertEquals(10.0, SensorValues.normalizeDegrees(370.0), 1e-9)
        assertEquals(350.0, SensorValues.normalizeDegrees(-10.0), 1e-9)
    }

    @Test fun quaternionDerivesMissingScalar() {
        assertNull(SensorValues.quaternion(floatArrayOf(0f, 0f)))
        assertArrayEquals(floatArrayOf(0f, 0f, 0.6f, 0.8f), SensorValues.quaternion(floatArrayOf(0f, 0f, 0.6f))!!, 1e-6f)
        // Newer devices also report an accuracy estimate after w; it is dropped.
        assertArrayEquals(
            floatArrayOf(0.1f, 0.2f, 0.3f, 0.9f),
            SensorValues.quaternion(floatArrayOf(0.1f, 0.2f, 0.3f, 0.9f, 0.05f))!!,
            0f,
        )
    }

    /** Hamilton product a * b of quaternions [x, y, z, w]: rotate by b, then by a. */
    private fun multiply(a: FloatArray, b: FloatArray): FloatArray {
        val (ax, ay, az, aw) = a
        val (bx, by, bz, bw) = b
        return floatArrayOf(
            aw * bx + ax * bw + ay * bz - az * by,
            aw * by - ax * bz + ay * bw + az * bx,
            aw * bz + ax * by - ay * bx + az * bw,
            aw * bw - ax * bx - ay * by - az * bz,
        )
    }
}
