package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// Shape is the JSON shape of a reading's value.
type Shape string

const (
	ShapeNumber Shape = "number"
	ShapeBool   Shape = "bool"
	ShapeVec3   Shape = "vec3"
	ShapeQuat   Shape = "quat"
	ShapeObject Shape = "object"
)

// Mode says how a sensor produces events.
type Mode string

const (
	// ModeContinuous sensors are sampled at a rate.
	ModeContinuous Mode = "continuous"
	// ModeOnChange sensors emit when their value changes.
	ModeOnChange Mode = "on_change"
)

// Sources in the sensor catalog (spec §6).
const (
	SourceAccelerometer      = "accelerometer"
	SourceGyro               = "gyro"
	SourceGameRotationVector = "game_rotation_vector"
	SourceRotationVector     = "rotation_vector"
	SourceMagnetometer       = "magnetometer"
	SourceCompass            = "compass"
	SourceBarometer          = "barometer"
	SourceProximity          = "proximity"
	SourceLight              = "light"
	SourceBattery            = "battery"
	SourceLocation           = "location"
	SourceUI                 = "ui"
	SourceNFC                = "nfc"
)

// Event types in the sensor catalog.
const (
	TypeMotion        = "motion"
	TypeRotation      = "rotation"
	TypeOrientation   = "orientation"
	TypeMagneticField = "magnetic_field"
	TypeHeading       = "heading"
	TypePressure      = "pressure"
	TypeDistance      = "distance"
	TypeIlluminance   = "illuminance"
	TypeState         = "state"
	TypePosition      = "position"
	TypeTileTap       = "tile_tap"
	TypeTag           = "tag"
)

// Units in the sensor catalog.
const (
	UnitMetersPerSecond2 = "m/s^2"
	UnitRadiansPerSecond = "rad/s"
	UnitNone             = ""
	UnitMicrotesla       = "uT"
	UnitDegrees          = "deg"
	UnitHectopascal      = "hPa"
	UnitCentimeters      = "cm"
	UnitLux              = "lx"
	UnitPercent          = "%"
)

// Rate limits used for every continuous sensor in the catalog. Real phones
// report their own limits through list_sensors; these match the spec's
// example and Android 12+'s 200 Hz cap without HIGH_SAMPLING_RATE_SENSORS.
const (
	DefaultMinRateHz     = 1
	DefaultMaxRateHz     = 200
	DefaultDefaultRateHz = 50
)

// Catalog returns the sensor catalog (spec §6), all marked
// available. The returned slice is a fresh copy and may be modified.
func Catalog() []SensorInfo {
	cont := func(source, typ, unit string, shape Shape) SensorInfo {
		return SensorInfo{
			Source: source, Types: []string{typ}, Unit: unit, Shape: shape, Mode: ModeContinuous,
			MinRateHz: DefaultMinRateHz, MaxRateHz: DefaultMaxRateHz, DefaultRateHz: DefaultDefaultRateHz,
			Available: true,
		}
	}
	onChange := func(source, typ, unit string, shape Shape) SensorInfo {
		return SensorInfo{
			Source: source, Types: []string{typ}, Unit: unit, Shape: shape, Mode: ModeOnChange,
			Available: true,
		}
	}
	return []SensorInfo{
		cont(SourceAccelerometer, TypeMotion, UnitMetersPerSecond2, ShapeVec3),
		cont(SourceGyro, TypeRotation, UnitRadiansPerSecond, ShapeVec3),
		cont(SourceGameRotationVector, TypeOrientation, UnitNone, ShapeQuat),
		cont(SourceRotationVector, TypeOrientation, UnitNone, ShapeQuat),
		cont(SourceMagnetometer, TypeMagneticField, UnitMicrotesla, ShapeVec3),
		cont(SourceCompass, TypeHeading, UnitDegrees, ShapeNumber),
		cont(SourceBarometer, TypePressure, UnitHectopascal, ShapeNumber),
		onChange(SourceProximity, TypeDistance, UnitCentimeters, ShapeNumber),
		onChange(SourceLight, TypeIlluminance, UnitLux, ShapeNumber),
		onChange(SourceBattery, TypeState, UnitPercent, ShapeObject),
		onChange(SourceLocation, TypePosition, UnitNone, ShapeObject),
		onChange(SourceUI, TypeTileTap, UnitNone, ShapeBool),
		onChange(SourceNFC, TypeTag, UnitNone, ShapeObject),
	}
}

// LookupSensor returns the catalog entry for source.
func LookupSensor(source string) (SensorInfo, bool) {
	for _, s := range Catalog() {
		if s.Source == source {
			return s, true
		}
	}
	return SensorInfo{}, false
}

// ErrNoDistance is returned by ValueDistance for shapes min_delta doesn't
// apply to.
var ErrNoDistance = errors.New("min_delta does not apply to this shape")

// ValueDistance returns the distance between two values of the given shape,
// as used by the subscribe min_delta filter (spec §4.3): Euclidean distance
// for vec3 and quat, absolute difference for number.
//
// min_delta is ignored for the "bool" and "object" shapes (otherwise every
// repeated ui/tile_tap, always true, would be suppressed as a duplicate).
// For those ValueDistance returns ErrNoDistance and the event is always
// sent.
func ValueDistance(shape Shape, a, b json.RawMessage) (float64, error) {
	switch shape {
	case ShapeNumber:
		var x, y float64
		if err := unmarshal2(a, b, &x, &y); err != nil {
			return 0, err
		}
		return math.Abs(x - y), nil
	case ShapeVec3, ShapeQuat:
		var x, y []float64
		if err := unmarshal2(a, b, &x, &y); err != nil {
			return 0, err
		}
		if len(x) != len(y) {
			return 0, fmt.Errorf("value length mismatch: %d vs %d", len(x), len(y))
		}
		var sum float64
		for i := range x {
			d := x[i] - y[i]
			sum += d * d
		}
		return math.Sqrt(sum), nil
	}
	return 0, ErrNoDistance
}

func unmarshal2(a, b json.RawMessage, x, y any) error {
	if err := json.Unmarshal(a, x); err != nil {
		return err
	}
	return json.Unmarshal(b, y)
}
