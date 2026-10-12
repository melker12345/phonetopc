package mockphone

import (
	"context"
	"encoding/hex"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

// world holds the state of the on_change sources and notifies listeners when
// a value changes.
type world struct {
	srv *Server

	mu        sync.Mutex
	latest    map[string]protocol.Reading
	listeners map[string]map[*listener]struct{}

	// simulation state, only touched by run
	light     float64
	proximity float64
	battery   protocol.Battery
	walk      [2]float64 // location offset from home in m, [east, north]

	tags int // NFC scans so far; guarded by mu (ScanTag runs elsewhere)
}

// listener receives readings of one source. fn is called with world.mu held
// and must not block.
type listener struct {
	fn func(protocol.Reading)
}

// proximityFar is the "far" value; like many phones, the mock only reports
// 0 (near) or its maximum range.
const proximityFar = 5.0

func newWorld(srv *Server) *world {
	w := &world{
		srv:       srv,
		latest:    make(map[string]protocol.Reading),
		listeners: make(map[string]map[*listener]struct{}),
		light:     320,
		proximity: proximityFar,
		battery:   protocol.Battery{Level: 87, Charging: false},
	}
	// Initial values exist from the start, like on a real phone (except ui
	// and nfc: there is no tap or scan until the user does one).
	now := time.Now()
	w.publish(protocol.SourceLight, w.light, now)
	w.publish(protocol.SourceProximity, w.proximity, now)
	w.publish(protocol.SourceBattery, w.battery, now)
	w.publish(protocol.SourceLocation, fix(w.walk, nil), now)
	return w
}

// run drives the on_change sources until ctx is done.
func (w *world) run(ctx context.Context, rng *rand.Rand) {
	o := &w.srv.opts
	jitter := func(d time.Duration) time.Duration {
		return time.Duration(float64(d) * (0.5 + rng.Float64()))
	}
	newTimer := func(d time.Duration, jit bool) *time.Timer {
		if d <= 0 {
			return nil // disabled: a nil channel never fires
		}
		if jit {
			d = jitter(d)
		}
		return time.NewTimer(d)
	}
	ch := func(t *time.Timer) <-chan time.Time {
		if t == nil {
			return nil
		}
		return t.C
	}
	light := newTimer(o.LightEvery, true)
	prox := newTimer(o.ProximityEvery, true)
	batt := newTimer(o.BatteryEvery, false)
	tap := newTimer(o.TapEvery, false)
	loc := newTimer(o.LocationEvery, false)
	nfc := newTimer(o.NFCEvery, true)
	for {
		select {
		case <-ctx.Done():
			for _, t := range []*time.Timer{light, prox, batt, tap, loc, nfc} {
				if t != nil {
					t.Stop()
				}
			}
			return
		case now := <-ch(light):
			w.light = nextLight(w.light, rng)
			w.publish(protocol.SourceLight, w.light, now)
			light.Reset(jitter(o.LightEvery))
		case now := <-ch(prox):
			if w.proximity == 0 {
				w.proximity = proximityFar
			} else {
				w.proximity = 0
			}
			w.publish(protocol.SourceProximity, w.proximity, now)
			prox.Reset(jitter(o.ProximityEvery))
		case now := <-ch(batt):
			w.battery = nextBattery(w.battery)
			w.publish(protocol.SourceBattery, w.battery, now)
			batt.Reset(o.BatteryEvery)
		case now := <-ch(tap):
			w.publish(protocol.SourceUI, true, now)
			tap.Reset(o.TapEvery)
		case now := <-ch(loc):
			// A slow random walk around home, like the GPS jitter of a
			// phone lying on a desk; it stays within a few tens of meters.
			for i := range w.walk {
				w.walk[i] = 0.97*w.walk[i] + rng.NormFloat64()*1.5
			}
			w.publish(protocol.SourceLocation, fix(w.walk, rng), now)
			loc.Reset(o.LocationEvery)
		case now := <-ch(nfc):
			w.publish(protocol.SourceNFC, w.nextTag(), now)
			nfc.Reset(jitter(o.NFCEvery))
		}
	}
}

// ScanTag emits an nfc/tag event now, as if a tag had been held to the
// phone. Scans alternate between a sticker with a URL and a text record, and
// a bare MIFARE Classic card without NDEF.
func (s *Server) ScanTag() {
	s.world.publish(protocol.SourceNFC, s.world.nextTag(), time.Now())
}

func (w *world) nextTag() protocol.Tag {
	w.mu.Lock()
	w.tags++
	n := w.tags
	w.mu.Unlock()
	if n%2 == 0 {
		return protocol.Tag{ID: "5b1e0a9c", Tech: []string{"NfcA", "MifareClassic", "NdefFormatable"}}
	}
	uri, text := "https://example.com/", "hello from phonetopc"
	return protocol.Tag{
		ID:   "04a2246a8e5d80",
		Tech: []string{"NfcA", "MifareUltralight", "Ndef"},
		NDEF: []protocol.NDEFRecord{
			// 0x04 abbreviates "https://"; 0x02 is UTF-8 with a 2-byte language code.
			{TNF: 1, Type: "U", URI: &uri, Payload: "04" + hex.EncodeToString([]byte("example.com/"))},
			{TNF: 1, Type: "T", Text: &text, Payload: "02" + hex.EncodeToString([]byte("en"+text))},
		},
	}
}

// home is where the mock phone lies: Stockholm's old town.
const homeLat, homeLon = 59.32528, 18.07109

// fix returns a location/position value offset [east, north] m from home.
// With rng nil it is the first fix: no speed or bearing yet.
func fix(offset [2]float64, rng *rand.Rand) protocol.Position {
	const metersPerDegLat = 111_320.0
	p := protocol.Position{
		Lat:       round(homeLat+offset[1]/metersPerDegLat, 7),
		Lon:       round(homeLon+offset[0]/(metersPerDegLat*math.Cos(homeLat*math.Pi/180)), 7),
		AccuracyM: 8,
	}
	alt := 28.0
	if rng != nil {
		p.AccuracyM = round(4+rng.Float64()*8, 1)
		alt = round(alt+rng.NormFloat64(), 1)
		speed := round(math.Abs(rng.NormFloat64())*0.3, 2)
		p.SpeedMPS = &speed
		if speed >= 0.2 { // slower than that, the direction is noise
			bearing := normalizeDegrees(rng.Float64()*360, 1)
			p.BearingDeg = &bearing
		}
	}
	p.AltitudeM = &alt
	return p
}

// Tap emits a ui/tile_tap event now, as if the Quick Settings tile had been
// tapped.
func (s *Server) Tap() {
	s.world.publish(protocol.SourceUI, true, time.Now())
}

func nextLight(cur float64, rng *rand.Rand) float64 {
	switch r := rng.Float64(); {
	case r < 0.08: // covered / dark room
		return round(1+rng.Float64()*4, 1)
	case r < 0.14: // near a window
		return round(2000+rng.Float64()*8000, 0)
	default: // drift around indoor levels
		v := max(cur, 20) * math.Exp(rng.NormFloat64()*0.35)
		return round(min(max(v, 20), 1500), 1)
	}
}

func nextBattery(b protocol.Battery) protocol.Battery {
	switch {
	case b.Charging && b.Level >= 100:
		b.Charging = false
	case b.Charging:
		b.Level++
	case b.Level <= 15:
		b.Charging = true // the user plugged it in
	default:
		b.Level--
	}
	return b
}

func (w *world) publish(source string, value any, now time.Time) {
	r, err := w.srv.reading(source, value, now)
	if err != nil {
		w.srv.logf("mock: %v", err)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.latest[source] = r
	for l := range w.listeners[source] {
		l.fn(r)
	}
}

// get returns the latest reading of source.
func (w *world) get(source string) (protocol.Reading, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r, ok := w.latest[source]
	return r, ok
}

// listen registers fn for source and returns a function that unregisters it.
// For state-like sources (everything but ui and nfc) the current value is
// delivered immediately, mirroring Android, which reports the current value
// of an on-change sensor (and the sticky battery broadcast) on registration.
func (w *world) listen(source string, fn func(protocol.Reading)) (stop func()) {
	l := &listener{fn: fn}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.listeners[source] == nil {
		w.listeners[source] = make(map[*listener]struct{})
	}
	w.listeners[source][l] = struct{}{}
	if r, ok := w.latest[source]; ok && source != protocol.SourceUI && source != protocol.SourceNFC {
		fn(r)
	}
	return func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		delete(w.listeners[source], l)
	}
}
