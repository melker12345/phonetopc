package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/security"
)

func TestDefaultDir(t *testing.T) {
	env := map[string]string{"HOME": "/home/u"}
	get := func(k string) string { return env[k] }
	if d, _ := DefaultDir(get); d != "/home/u/.config/phonetopc" {
		t.Errorf("default %s", d)
	}
	env["XDG_CONFIG_HOME"] = "/x/cfg"
	if d, _ := DefaultDir(get); d != "/x/cfg/phonetopc" {
		t.Errorf("xdg %s", d)
	}
	env["XDG_CONFIG_HOME"] = "relative"
	if d, _ := DefaultDir(get); d != "/home/u/.config/phonetopc" {
		t.Errorf("relative xdg %s", d)
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestPCIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg", "phonetopc")
	s := Open(dir)
	pc, err := s.PC()
	if err != nil {
		t.Fatal(err)
	}
	if len(pc.ID) != 16 || pc.Name == "" || !security.ValidPin(pc.Pin) {
		t.Errorf("pc %+v", pc)
	}
	if m := mode(t, dir); m != 0o700 {
		t.Errorf("dir mode %v", m)
	}
	for _, f := range []string{PCFile, PCKeyFile} {
		if m := mode(t, s.Path(f)); m != 0o600 {
			t.Errorf("%s mode %v", f, m)
		}
	}
	again, err := Open(dir).PC()
	if err != nil || again.ID != pc.ID || again.Pin != pc.Pin || again.Name != pc.Name {
		t.Errorf("reloaded %+v, %v", again, err)
	}
	var doc map[string]any
	b, _ := os.ReadFile(s.Path(PCFile))
	if json.Unmarshal(b, &doc) != nil || doc["id"] != pc.ID || doc["name"] != pc.Name || len(doc) != 2 {
		t.Errorf("pc.json %s", b)
	}
	// A user-edited name is kept.
	os.WriteFile(s.Path(PCFile), []byte(`{"id":"`+pc.ID+`","name":"my desk"}`), 0o600)
	if again, _ := Open(dir).PC(); again.Name != "my desk" {
		t.Errorf("name %q", again.Name)
	}
}

func TestDevices(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0o755)
	s := Open(dir)
	if devs, err := s.Devices(); err != nil || len(devs) != 0 {
		t.Fatalf("empty store: %v %v", devs, err)
	}
	if _, err := s.Find(""); !errors.Is(err, ErrNoDevices) {
		t.Errorf("find in empty store: %v", err)
	}
	a := Device{ID: "635f3a01db1859bc", Name: "Melker's S25 Ultra", Pin: "sha256/n4bQgYhMfWWaL-qgxVrQFaO_TxsrC4Is0V1sFbDwCgg",
		Token: "Zm9vYmFy", Port: 8765, Addresses: []string{"192.168.1.153"}, PairedAt: "2026-10-09T16:40:00Z"}
	if err := s.PutDevice(a); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, dir); m != 0o700 {
		t.Errorf("dir mode %v", m)
	}
	if m := mode(t, s.Path(DevicesFile)); m != 0o600 {
		t.Errorf("file mode %v", m)
	}
	// The file has exactly the documented shape.
	b, _ := os.ReadFile(s.Path(DevicesFile))
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"version": 1.0, "devices": []any{map[string]any{
		"id": a.ID, "name": a.Name, "pin": a.Pin, "token": a.Token, "port": 8765.0,
		"addresses": []any{"192.168.1.153"}, "paired_at": "2026-10-09T16:40:00Z",
	}}}
	if !reflect.DeepEqual(raw, want) {
		t.Errorf("devices.json:\n%s", b)
	}

	if d, err := s.Find(""); err != nil || d.ID != a.ID {
		t.Errorf("only device: %v %v", d, err)
	}
	b2 := Device{ID: "b2", Name: "Tablet", Pin: a.Pin, Token: "t2", Port: 9000}
	s.PutDevice(b2)
	var amb *AmbiguousError
	if _, err := s.Find(""); !errors.As(err, &amb) || !strings.Contains(err.Error(), "--device") {
		t.Errorf("two devices: %v", err)
	}
	if d, _ := s.Find("tablet"); d.ID != "b2" {
		t.Errorf("by name: %+v", d)
	}
	if d, _ := s.Find("635f3a01db1859bc"); d.Name != a.Name {
		t.Errorf("by id: %+v", d)
	}
	if _, err := s.Find("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	s.PutDevice(Device{ID: "c3", Name: "tablet"})
	if _, err := s.Find("Tablet"); !errors.As(err, &amb) {
		t.Errorf("ambiguous name: %v", err)
	}

	// Re-pairing replaces by id.
	a2 := a
	a2.Token = "new"
	s.PutDevice(a2)
	if d, _ := s.Find(a.ID); d.Token != "new" {
		t.Errorf("replaced token %q", d.Token)
	}
	if devs, _ := s.Devices(); len(devs) != 3 {
		t.Errorf("%d devices", len(devs))
	}

	// Addresses: the working one moves to the front.
	if err := s.NoteAddress(a.ID, "10.0.0.7", 8766); err != nil {
		t.Fatal(err)
	}
	s.NoteAddress(a.ID, "192.168.1.153", 8766)
	if d, _ := s.Find(a.ID); !reflect.DeepEqual(d.Addresses, []string{"192.168.1.153", "10.0.0.7"}) || d.Port != 8766 {
		t.Errorf("addresses %v port %d", d.Addresses, d.Port)
	}
	if err := s.NoteAddress("gone", "1.2.3.4", 1); err != nil {
		t.Errorf("unknown id: %v", err)
	}

	if d, err := s.RemoveDevice("b2"); err != nil || d.Name != "Tablet" {
		t.Errorf("remove: %v %v", d, err)
	}
	if _, err := s.RemoveDevice("b2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("remove twice: %v", err)
	}

	// No temp files left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestDevicesBadVersion(t *testing.T) {
	s := Open(t.TempDir())
	os.WriteFile(s.Path(DevicesFile), []byte(`{"version":2,"devices":[]}`), 0o600)
	if _, err := s.Devices(); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Errorf("version 2: %v", err)
	}
	if err := s.PutDevice(Device{ID: "x"}); err == nil {
		t.Error("overwrote a newer file")
	}
}

// TestConcurrentUpdates checks that updates from several stores on the same
// directory (as from several processes) don't lose each other's devices.
func TestConcurrentUpdates(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			s := Open(dir)
			if err := s.PutDevice(Device{ID: string(rune('a' + i)), PairedAt: Now(time.Now())}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if devs, _ := Open(dir).Devices(); len(devs) != 20 {
		t.Errorf("%d devices, want 20", len(devs))
	}
}
