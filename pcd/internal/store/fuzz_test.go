package store

import (
	"encoding/json"
	"os"
	"testing"
)

// FuzzDevicesFile loads devices.json and writes it back unchanged: what
// was read must read the same afterwards, unknown members included.
func FuzzDevicesFile(f *testing.F) {
	f.Add([]byte(`{"version":1,"x_top":{"a":1},"devices":[{"id":"p1","name":"Phone","pin":"sha256/x","token":"t","x_dev":[1,2],"approval_key":"sha256/ak"}]}`), "p1")
	f.Add([]byte(`{"version":1,"devices":[{"id":"a","name":"Pixel","port":8765,"addresses":["10.0.0.2","fd00::1"],"paired_at":"2026-01-01T00:00:00Z"},{"id":"b","name":"pixel"}]}`), "PIXEL")
	f.Add([]byte(`{"version":1,"devices":null}`), "")
	f.Add([]byte(`{"version":2,"devices":[]}`), "")
	f.Fuzz(func(t *testing.T, data []byte, sel string) {
		s := Open(t.TempDir())
		if err := os.WriteFile(s.Path(DevicesFile), data, 0o600); err != nil {
			t.Fatal(err)
		}
		before, err := s.readDevices()
		if err != nil {
			return
		}
		_, _ = find(before.Devices, sel)
		if err := s.updateDevices(func(devs []Device) ([]Device, bool, error) { return devs, true, nil }); err != nil {
			t.Fatalf("rewriting %s: %v", data, err)
		}
		after, err := s.readDevices()
		if err != nil {
			b, _ := os.ReadFile(s.Path(DevicesFile))
			t.Fatalf("%s was rewritten as %s, which doesn't load: %v", data, b, err)
		}
		if before.Devices == nil {
			before.Devices = []Device{}
		}
		if a, b := docJSON(t, before), docJSON(t, after); a != b {
			t.Fatalf("%s loads as\n%s\nbut after rewriting as\n%s", data, a, b)
		}
	})
}

func docJSON(t *testing.T, d devicesDoc) string {
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
