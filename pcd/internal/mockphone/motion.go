package mockphone

import (
	"math"
	"math/rand/v2"
)

// The simulated phone is held in a hand: it rolls and pitches gently and
// slowly turns around the vertical axis. The orientation is given as Z-Y-X
// (yaw, pitch, roll) Euler angles as smooth functions of time, and all three
// motion sensors are derived from the same orientation so they are mutually
// consistent: the gyro is the body-frame angular velocity, the accelerometer
// is gravity seen in the device frame plus a little hand shake, and the game
// rotation vector is the orientation quaternion.
//
// The game rotation vector's yaw has an arbitrary origin. In the earth frame
// (x east, y magnetic north, z up) the phone is turned by headingOffset
// more; the rotation vector, the compass and the magnetometer use that.

const gravity = 9.80665

// headingOffset is the yaw between the game rotation vector's frame and the
// earth frame, in rad.
const headingOffset = 0.9

// earthField is the geomagnetic field in the earth frame [east, north, up]
// in µT, roughly that of northern Europe (pointing north and steeply down).
var earthField = [3]float64{0, 15.2, -48.5}

type euler struct{ roll, pitch, yaw float64 }

// orientation returns the Euler angles and their time derivatives at t
// seconds.
func orientation(t float64) (a, da euler) {
	const (
		rollAmp, rollHz   = 0.35, 0.11
		pitchAmp, pitchHz = 0.25, 0.07
		yawRate           = 0.12 // rad/s, a slow continuous turn
		yawWobAmp, yawHz  = 0.20, 0.05
	)
	w := func(hz float64) float64 { return 2 * math.Pi * hz }
	a.roll = rollAmp * math.Sin(w(rollHz)*t)
	da.roll = rollAmp * w(rollHz) * math.Cos(w(rollHz)*t)
	a.pitch = pitchAmp * math.Sin(w(pitchHz)*t+1)
	da.pitch = pitchAmp * w(pitchHz) * math.Cos(w(pitchHz)*t+1)
	a.yaw = yawRate*t + yawWobAmp*math.Sin(w(yawHz)*t)
	da.yaw = yawRate + yawWobAmp*w(yawHz)*math.Cos(w(yawHz)*t)
	return a, da
}

// gyro returns the body-frame angular velocity [x, y, z] in rad/s.
func gyro(t float64, rng *rand.Rand) []float64 {
	a, d := orientation(t)
	sr, cr := math.Sincos(a.roll)
	sp, cp := math.Sincos(a.pitch)
	// Z-Y-X Euler rates to body rates.
	p := d.roll - d.yaw*sp
	q := d.pitch*cr + d.yaw*cp*sr
	r := -d.pitch*sr + d.yaw*cp*cr
	return []float64{
		round(p+noise(rng, 0.002), 5),
		round(q+noise(rng, 0.002), 5),
		round(r+noise(rng, 0.002), 5),
	}
}

// accelerometer returns the specific force [x, y, z] in m/s^2. At rest and
// face up it reads about [0, 0, 9.81], like an Android phone on a table.
func accelerometer(t float64, rng *rand.Rand) []float64 {
	a, _ := orientation(t)
	sr, cr := math.Sincos(a.roll)
	sp, cp := math.Sincos(a.pitch)
	// R^T * [0, 0, g] for R = Rz(yaw) Ry(pitch) Rx(roll).
	gx := -gravity * sp
	gy := gravity * cp * sr
	gz := gravity * cp * cr
	// Hand tremor: a few Hz, small amplitude.
	shake := func(hz, phase float64) float64 { return 0.08 * math.Sin(2*math.Pi*hz*t+phase) }
	return []float64{
		round(gx+shake(3.1, 0)+noise(rng, 0.02), 4),
		round(gy+shake(2.7, 1)+noise(rng, 0.02), 4),
		round(gz+shake(3.4, 2)+noise(rng, 0.02), 4),
	}
}

// gameRotationVector returns the orientation as a unit quaternion
// [x, y, z, w].
func gameRotationVector(t float64) []float64 {
	a, _ := orientation(t)
	return quaternion(a)
}

// earthOrientation is orientation(t) relative to the earth frame.
func earthOrientation(t float64) euler {
	a, _ := orientation(t)
	a.yaw += headingOffset
	return a
}

// bodyFromWorld returns R^T v for R = Rz(yaw) Ry(pitch) Rx(roll): a world
// vector seen in the device frame.
func bodyFromWorld(a euler, v [3]float64) [3]float64 {
	sy, cy := math.Sincos(a.yaw)
	sp, cp := math.Sincos(a.pitch)
	sr, cr := math.Sincos(a.roll)
	x, y, z := cy*v[0]+sy*v[1], -sy*v[0]+cy*v[1], v[2]
	x, z = cp*x-sp*z, sp*x+cp*z
	y, z = cr*y+sr*z, -sr*y+cr*z
	return [3]float64{x, y, z}
}

// rotationVector returns the earth-frame orientation as a unit quaternion
// [x, y, z, w].
func rotationVector(t float64) []float64 {
	return quaternion(earthOrientation(t))
}

// magnetometer returns the geomagnetic field in the device frame in µT.
func magnetometer(t float64, rng *rand.Rand) []float64 {
	m := bodyFromWorld(earthOrientation(t), earthField)
	return []float64{
		round(m[0]+noise(rng, 0.3), 2),
		round(m[1]+noise(rng, 0.3), 2),
		round(m[2]+noise(rng, 0.3), 2),
	}
}

// compass returns the heading in degrees, [0, 360), computed from the
// rotation vector the way the Android app does.
func compass(t float64, rng *rand.Rand) float64 {
	return normalizeDegrees(azimuth(rotationVector(t))+noise(rng, 0.2), 1)
}

// azimuth returns atan2(R[1], R[4]) of the rotation matrix of the unit
// quaternion q = [x, y, z, w] (SensorManager.getOrientation's azimuth) in
// degrees, in (-180, 180].
func azimuth(q []float64) float64 {
	x, y, z, w := q[0], q[1], q[2], q[3]
	return math.Atan2(2*(x*y-z*w), 1-2*(x*x+z*z)) * 180 / math.Pi
}

// normalizeDegrees maps deg to [0, 360) after rounding to n decimals.
func normalizeDegrees(deg float64, n int) float64 {
	deg = round(math.Mod(deg, 360), n)
	if deg < 0 {
		deg = round(deg+360, n)
	}
	if deg >= 360 {
		deg = 0
	}
	return deg
}

// barometer returns the air pressure in hPa: a slow weather drift around
// standard pressure plus sensor noise.
func barometer(t float64, rng *rand.Rand) float64 {
	return round(1013.25+1.5*math.Sin(2*math.Pi*t/3600)+noise(rng, 0.03), 2)
}

// quaternion returns a as a unit quaternion [x, y, z, w] with w >= 0.
func quaternion(a euler) []float64 {
	sr, cr := math.Sincos(a.roll / 2)
	sp, cp := math.Sincos(a.pitch / 2)
	sy, cy := math.Sincos(a.yaw / 2)
	q := []float64{
		sr*cp*cy - cr*sp*sy,
		cr*sp*cy + sr*cp*sy,
		cr*cp*sy - sr*sp*cy,
		cr*cp*cy + sr*sp*sy,
	}
	// Android reports w >= 0; q and -q are the same rotation.
	if q[3] < 0 {
		for i := range q {
			q[i] = -q[i]
		}
	}
	var n float64
	for _, v := range q {
		n += v * v
	}
	n = math.Sqrt(n)
	for i := range q {
		q[i] = round(q[i]/n, 7)
	}
	return q
}

func noise(rng *rand.Rand, sigma float64) float64 {
	if rng == nil {
		return 0
	}
	return rng.NormFloat64() * sigma
}

// round rounds to n decimal places to keep frames short.
func round(v float64, n int) float64 {
	p := math.Pow(10, float64(n))
	return math.Round(v*p) / p
}
