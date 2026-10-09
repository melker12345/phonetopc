package protocol

import (
	"encoding/json"
	"fmt"
)

// ClientInfo identifies the client in hello.
type ClientInfo struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

// HelloParams are the params of a hello request.
type HelloParams struct {
	Protocol string      `json:"protocol"`
	Client   *ClientInfo `json:"client,omitempty"`
}

// ServerInfo identifies the server implementation.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// DeviceInfo describes the phone.
type DeviceInfo struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Manufacturer    string `json:"manufacturer,omitempty"`
	Model           string `json:"model,omitempty"`
	Platform        string `json:"platform"`
	PlatformVersion string `json:"platform_version,omitempty"`
}

// HelloResult is the result of hello.
type HelloResult struct {
	Protocol     string     `json:"protocol"`
	Server       ServerInfo `json:"server"`
	Device       DeviceInfo `json:"device"`
	Capabilities []string   `json:"capabilities"`
}

// SensorInfo describes one source in list_sensors.
type SensorInfo struct {
	Source string   `json:"source"`
	Types  []string `json:"types"`
	Unit   string   `json:"unit"`
	Shape  Shape    `json:"shape"`
	Mode   Mode     `json:"mode"`
	// Rate fields are only set for continuous sensors (zero means absent).
	MinRateHz     float64 `json:"min_rate_hz,omitempty"`
	MaxRateHz     float64 `json:"max_rate_hz,omitempty"`
	DefaultRateHz float64 `json:"default_rate_hz,omitempty"`
	Available     bool    `json:"available"`
}

// HasType reports whether t is one of the sensor's event types.
func (s *SensorInfo) HasType(t string) bool {
	for _, x := range s.Types {
		if x == t {
			return true
		}
	}
	return false
}

// ClampRate returns the effective rate for a requested rate: the default rate
// if requested is nil, otherwise requested clamped to [MinRateHz, MaxRateHz].
// It returns 0 for on_change sensors.
func (s *SensorInfo) ClampRate(requested *float64) float64 {
	if s.Mode != ModeContinuous {
		return 0
	}
	if requested == nil {
		return s.DefaultRateHz
	}
	return min(max(*requested, s.MinRateHz), s.MaxRateHz)
}

// ListSensorsResult is the result of list_sensors.
type ListSensorsResult struct {
	Sensors []SensorInfo `json:"sensors"`
}

// SubscribeParams are the params of subscribe. Optional numeric members are
// pointers so that "absent" can be told apart from zero.
type SubscribeParams struct {
	Source   string   `json:"source"`
	Type     string   `json:"type,omitempty"`
	RateHz   *float64 `json:"rate_hz,omitempty"`
	MinDelta *float64 `json:"min_delta,omitempty"`
}

// SubscribeResult is the result of subscribe.
type SubscribeResult struct {
	Subscription string   `json:"subscription"`
	Source       string   `json:"source"`
	Types        []string `json:"types"`
	// RateHz is the effective rate; omitted for on_change sources.
	RateHz float64 `json:"rate_hz,omitempty"`
}

// UnsubscribeParams are the params of unsubscribe.
type UnsubscribeParams struct {
	Subscription string `json:"subscription"`
}

// UnsubscribeResult is the (empty) result of unsubscribe.
type UnsubscribeResult struct{}

// GetParams are the params of get.
type GetParams struct {
	Source string `json:"source"`
	Type   string `json:"type,omitempty"`
}

// Reading is one sensor reading: the result of get, and the body of an event.
type Reading struct {
	Source   string          `json:"source"`
	Type     string          `json:"type"`
	Value    json.RawMessage `json:"value"`
	Unit     string          `json:"unit"`
	TS       int64           `json:"ts"`
	TSMonoNS int64           `json:"ts_mono_ns"`
	Device   string          `json:"device"`
}

// Event is the params of an "event" notification.
type Event struct {
	Subscription string `json:"subscription"`
	Seq          uint64 `json:"seq"`
	Reading
}

// Battery is the value of a battery/state reading.
type Battery struct {
	Level    float64 `json:"level"`
	Charging bool    `json:"charging"`
}

// SetValue marshals v into r.Value.
func (r *Reading) SetValue(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	r.Value = b
	return nil
}

// Number decodes a number value (shape "number").
func (r *Reading) Number() (float64, error) {
	var f float64
	return f, r.decode(&f)
}

// Bool decodes a bool value (shape "bool").
func (r *Reading) Bool() (bool, error) {
	var b bool
	return b, r.decode(&b)
}

// Vec3 decodes a vec3 value.
func (r *Reading) Vec3() ([3]float64, error) {
	var v [3]float64
	return v, r.decodeFixed(v[:])
}

// Quat decodes a quat value as [x, y, z, w].
func (r *Reading) Quat() ([4]float64, error) {
	var v [4]float64
	return v, r.decodeFixed(v[:])
}

// Battery decodes a battery/state value.
func (r *Reading) Battery() (Battery, error) {
	var b Battery
	return b, r.decode(&b)
}

func (r *Reading) decode(v any) error {
	if err := json.Unmarshal(r.Value, v); err != nil {
		return fmt.Errorf("%s/%s value: %w", r.Source, r.Type, err)
	}
	return nil
}

func (r *Reading) decodeFixed(dst []float64) error {
	var s []float64
	if err := r.decode(&s); err != nil {
		return err
	}
	if len(s) != len(dst) {
		return fmt.Errorf("%s/%s value: want %d components, got %d", r.Source, r.Type, len(dst), len(s))
	}
	copy(dst, s)
	return nil
}
