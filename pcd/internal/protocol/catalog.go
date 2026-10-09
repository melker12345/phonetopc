package protocol

import (
	"encoding/json"
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

// Sources in the draft-0 catalog (spec §6).
const (
	SourceAccelerometer      = "accelerometer"
	SourceGyro               = "gyro"
	SourceGameRotationVector = "game_rotation_vector"
	SourceProximity          = "proximity"
	SourceLight              = "light"
	SourceBattery            = "battery"
	SourceUI                 = "ui"
)

// Event types in the draft-0 catalog.
const (
	TypeMotion      = "motion"
	TypeRotation    = "rotation"
	TypeOrientation = "orientation"
	TypeDistance    = "distance"
	TypeIlluminance = "illuminance"
	TypeState       = "state"
	TypeTileTap     = "tile_tap"
)

// Units in the draft-0 catalog.
const (
	UnitMetersPerSecond2 = "m/s^2"
	UnitRadiansPerSecond = "rad/s"
	UnitNone             = ""
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

// Catalog returns the draft-0 sensor catalog (spec §6), all marked
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
		onChange(SourceProximity, TypeDistance, UnitCentimeters, ShapeNumber),
		onChange(SourceLight, TypeIlluminance, UnitLux, ShapeNumber),
		onChange(SourceBattery, TypeState, UnitPercent, ShapeObject),
		onChange(SourceUI, TypeTileTap, UnitNone, ShapeBool),
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

// ValueDistance returns the distance between two values of the given shape,
// as used by the subscribe min_delta filter (spec §4.3): Euclidean distance
// for vectors and quaternions, absolute difference for numbers.
//
// The spec does not define a distance for the "bool" and "object" shapes.
// We use: bool -> 0 if equal, +Inf otherwise (a change is never suppressed);
// object -> the absolute difference of the "level" members if both values are
// battery objects with equal "charging", +Inf otherwise. For any other object,
// 0 if byte-identical, +Inf otherwise.
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
	case ShapeBool:
		var x, y bool
		if err := unmarshal2(a, b, &x, &y); err != nil {
			return 0, err
		}
		if x == y {
			return 0, nil
		}
		return math.Inf(1), nil
	default:
		type battery struct {
			Level    *float64 `json:"level"`
			Charging *bool    `json:"charging"`
		}
		var x, y battery
		if unmarshal2(a, b, &x, &y) == nil &&
			x.Level != nil && y.Level != nil && x.Charging != nil && y.Charging != nil &&
			*x.Charging == *y.Charging {
			return math.Abs(*x.Level - *y.Level), nil
		}
		if string(a) == string(b) {
			return 0, nil
		}
		return math.Inf(1), nil
	}
}

func unmarshal2(a, b json.RawMessage, x, y any) error {
	if err := json.Unmarshal(a, x); err != nil {
		return err
	}
	return json.Unmarshal(b, y)
}
