package protocol

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
)

type ClientInfo struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

type HelloParams struct {
	Protocol string      `json:"protocol"`
	Client   *ClientInfo `json:"client,omitempty"`
	// Auth carries the pairing token in secure mode (SECURITY.md §5).
	Auth *Auth `json:"auth,omitempty"`
}

type Auth struct {
	Token string `json:"token"`
}

// PCInfo identifies a paired PC: in the hello result (which pairing the
// server recognised) and in pairing messages.
type PCInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type DeviceInfo struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Manufacturer    string `json:"manufacturer,omitempty"`
	Model           string `json:"model,omitempty"`
	Platform        string `json:"platform"`
	PlatformVersion string `json:"platform_version,omitempty"`
}

type HelloResult struct {
	Protocol     string     `json:"protocol"`
	Server       ServerInfo `json:"server"`
	Device       DeviceInfo `json:"device"`
	Capabilities []string   `json:"capabilities"`
	// PC is set in secure mode: the pairing the server recognised.
	PC *PCInfo `json:"pc,omitempty"`
	// Approval is set when the phone can do approvals (APPROVAL.md §1).
	Approval *ApprovalInfo `json:"approval,omitempty"`
	// Decrypt is set when the phone can decrypt (DECRYPT.md §1).
	Decrypt *DecryptInfo `json:"decrypt,omitempty"`
}

func (h *HelloResult) HasCapability(c string) bool {
	for _, x := range h.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

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

func (s *SensorInfo) HasType(t string) bool {
	for _, x := range s.Types {
		if x == t {
			return true
		}
	}
	return false
}

// ClampRate returns the default rate if requested is nil, otherwise
// requested clamped to [MinRateHz, MaxRateHz]; 0 for on_change sensors.
func (s *SensorInfo) ClampRate(requested *float64) float64 {
	if s.Mode != ModeContinuous {
		return 0
	}
	if requested == nil {
		return s.DefaultRateHz
	}
	return min(max(*requested, s.MinRateHz), s.MaxRateHz)
}

type ListSensorsResult struct {
	Sensors []SensorInfo `json:"sensors"`
}

// Optional numeric members are pointers so that "absent" can be told apart
// from zero.
type SubscribeParams struct {
	Source   string   `json:"source"`
	Type     string   `json:"type,omitempty"`
	RateHz   *float64 `json:"rate_hz,omitempty"`
	MinDelta *float64 `json:"min_delta,omitempty"`
}

type SubscribeResult struct {
	Subscription string   `json:"subscription"`
	Source       string   `json:"source"`
	Types        []string `json:"types"`
	// Mode is copied from the source's list_sensors entry.
	Mode Mode `json:"mode"`
	// RateHz is the effective rate; omitted for on_change sources.
	RateHz float64 `json:"rate_hz,omitempty"`
}

type UnsubscribeParams struct {
	Subscription string `json:"subscription"`
}

type UnsubscribeResult struct{}

type GetParams struct {
	Source string `json:"source"`
	Type   string `json:"type,omitempty"`
}

// Reading is the result of get, and the body of an event.
type Reading struct {
	Source   string          `json:"source"`
	Type     string          `json:"type"`
	Value    json.RawMessage `json:"value"`
	Unit     string          `json:"unit"`
	TS       int64           `json:"ts"`
	TSMonoNS int64           `json:"ts_mono_ns"`
	Device   string          `json:"device"`
}

type Event struct {
	Subscription string `json:"subscription"`
	Seq          uint64 `json:"seq"`
	Reading
}

// The value of a battery/state reading.
type Battery struct {
	Level    float64 `json:"level"`
	Charging bool    `json:"charging"`
}

// The value of a location/position reading. The optional fields are nil
// when the fix doesn't have them.
type Position struct {
	Lat        float64  `json:"lat"`
	Lon        float64  `json:"lon"`
	AccuracyM  float64  `json:"accuracy_m"`
	AltitudeM  *float64 `json:"altitude_m,omitempty"`
	SpeedMPS   *float64 `json:"speed_mps,omitempty"`
	BearingDeg *float64 `json:"bearing_deg,omitempty"`
}

// The value of an nfc/tag reading.
type Tag struct {
	// ID is the tag UID in lowercase hex.
	ID string `json:"id"`
	// Tech lists the technologies the tag supports, e.g. "NfcA", "Ndef".
	Tech []string     `json:"tech"`
	NDEF []NDEFRecord `json:"ndef,omitempty"`
}

type NDEFRecord struct {
	TNF  int    `json:"tnf"`
	Type string `json:"type"`
	// Text is set for well-known text records, URI for URI records.
	Text *string `json:"text,omitempty"`
	URI  *string `json:"uri,omitempty"`
	// Payload is the record payload in lowercase hex.
	Payload string `json:"payload"`
}

func (r NDEFRecord) PayloadBytes() ([]byte, error) { return hex.DecodeString(r.Payload) }

func (r *Reading) SetValue(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	r.Value = b
	return nil
}

func (r Reading) Number() (float64, error) {
	var f float64
	return f, r.decode(&f)
}

func (r Reading) Bool() (bool, error) {
	var b bool
	return b, r.decode(&b)
}

func (r Reading) Vec3() ([3]float64, error) {
	var v [3]float64
	return v, r.decodeFixed(v[:])
}

// Quat returns [x, y, z, w].
func (r Reading) Quat() ([4]float64, error) {
	var v [4]float64
	return v, r.decodeFixed(v[:])
}

func (r Reading) Battery() (Battery, error) {
	var b Battery
	return b, r.decode(&b)
}

func (r Reading) Position() (Position, error) {
	var p Position
	return p, r.decode(&p)
}

func (r Reading) Tag() (Tag, error) {
	var t Tag
	return t, r.decode(&t)
}

func (r Reading) decode(v any) error {
	if err := json.Unmarshal(r.Value, v); err != nil {
		return fmt.Errorf("%s/%s value: %w", r.Source, r.Type, err)
	}
	return nil
}

func (r Reading) decodeFixed(dst []float64) error {
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
