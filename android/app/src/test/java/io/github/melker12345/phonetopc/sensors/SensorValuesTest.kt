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

    @Test fun positionLeavesOutWhatTheFixLacks() {
        assertEquals(
            """{"lat":59.3293,"lon":18.0686,"accuracy_m":12.5}""",
            SensorValues.position(59.3293, 18.0686, 12.5f).toString(),
        )
        assertEquals(
            """{"lat":-33.85,"lon":151.2,"accuracy_m":4.0,"altitude_m":31.5,"speed_mps":1.25,"bearing_deg":271.5}""",
            SensorValues.position(-33.85, 151.2, 4f, altitudeM = 31.5, speedMps = 1.25f, bearingDeg = 271.5f).toString(),
        )
        // A bearing of exactly 360 is north again; NaN members are left out like missing ones.
        assertEquals(
            """{"lat":1.0,"lon":2.0,"accuracy_m":3.0,"bearing_deg":0.0}""",
            SensorValues.position(1.0, 2.0, 3f, altitudeM = Double.NaN, speedMps = Float.NaN, bearingDeg = 360f).toString(),
        )
    }

    @Test fun positionRejectsFixesTheValueCantDescribe() {
        assertNull(SensorValues.position(1.0, 2.0, null)) // accuracy_m is required
        assertNull(SensorValues.position(1.0, 2.0, Float.NaN))
        assertNull(SensorValues.position(91.0, 2.0, 3f))
        assertNull(SensorValues.position(1.0, -180.5, 3f))
        assertNull(SensorValues.position(Double.NaN, 2.0, 3f))
    }

    @Test fun betterFix() {
        val s = 1_000_000_000L
        fun better(acc: Float, t: Long, p: String, lastAcc: Float, lastT: Long, lastP: String) =
            SensorValues.isBetterFix(acc, t * s, p, lastAcc, lastT * s, lastP)
        assertTrue(SensorValues.isBetterFix(50f, 1, "network", null, null, null)) // the first fix
        assertTrue(better(30f, 2, "gps", 5f, 1, "gps")) // same provider: always the newest
        assertTrue(better(4f, 2, "gps", 30f, 1, "network")) // more accurate
        assertTrue(!better(40f, 2, "network", 5f, 1, "gps")) // a coarse fix between GPS fixes
        assertTrue(better(40f, 12, "network", 5f, 1, "gps")) // ... unless GPS went quiet
        assertTrue(!better(4f, 1, "gps", 30f, 2, "network")) // older than what was sent
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
