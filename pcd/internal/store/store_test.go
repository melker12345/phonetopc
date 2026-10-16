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

// TestPCIdentityFirstRunRace: stores that don't share a mutex, like two
// processes on first run, all end up with the same identity.
func TestPCIdentityFirstRunRace(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "phonetopc")
	pins := make([]string, 8)
	var wg sync.WaitGroup
	for i := range pins {
		wg.Go(func() {
			pc, err := Open(dir).PC()
			if err != nil {
				t.Error(err)
				return
			}
			pins[i] = pc.ID + " " + pc.Pin
		})
	}
	wg.Wait()
	for _, p := range pins[1:] {
		if p != pins[0] {
			t.Fatalf("identities differ: %q", pins)
		}
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

	// Addresses: the working one moves to the front; the port changes only
	// when it came from mDNS; loopback addresses are never stored.
	if err := s.NoteAddress(a.ID, "10.0.0.7", 9999, false); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Find(a.ID); !reflect.DeepEqual(d.Addresses, []string{"10.0.0.7", "192.168.1.153"}) || d.Port != 8765 {
		t.Errorf("addresses %v port %d", d.Addresses, d.Port)
	}
	s.NoteAddress(a.ID, "192.168.1.153", 8766, true)
	s.NoteAddress(a.ID, "127.0.0.1", 8767, true)
	s.NoteAddress(a.ID, "::1", 8767, false)
	if d, _ := s.Find(a.ID); !reflect.DeepEqual(d.Addresses, []string{"192.168.1.153", "10.0.0.7"}) || d.Port != 8766 {
		t.Errorf("addresses %v port %d", d.Addresses, d.Port)
	}
	if err := s.NoteAddress("gone", "1.2.3.4", 1, true); err != nil {
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

func TestPutDeviceIf(t *testing.T) {
	s := Open(t.TempDir())
	a := Device{ID: "635f3a01db1859bc", Name: "phone", Pin: "sha256/a", Token: "t1"}
	if err := s.PutDeviceIf(a, "sha256/x"); !errors.Is(err, ErrDeviceChanged) {
		t.Errorf("expected a device that isn't there: %v", err)
	}
	if err := s.PutDeviceIf(a, ""); err != nil {
		t.Fatal(err)
	}
	b := a
	b.Pin, b.Token = "sha256/b", "t2"
	if err := s.PutDeviceIf(b, ""); !errors.Is(err, ErrDeviceChanged) {
		t.Errorf("expected no device: %v", err)
	}
	if err := s.PutDeviceIf(b, "sha256/c"); !errors.Is(err, ErrDeviceChanged) {
		t.Errorf("expected another pin: %v", err)
	}
	if d, _ := s.Find(a.ID); d.Pin != a.Pin || d.Token != "t1" {
		t.Fatalf("overwritten: %+v", d)
	}
	if err := s.PutDeviceIf(b, a.Pin); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Find(a.ID); d.Pin != b.Pin {
		t.Errorf("not replaced: %+v", d)
	}
}

// TestDevicesUnknownFieldsAndDefaults: SECURITY.md §6 defaults, unknown
// members kept when rewriting, and the approval key's life cycle.
func TestDevicesUnknownFieldsAndDefaults(t *testing.T) {
	s := Open(t.TempDir())
	os.WriteFile(s.Path(DevicesFile), []byte(`{"version":1,"x_top":{"a":1},"devices":[
		{"id":"p1","name":"Phone","pin":"sha256/x","token":"t","x_dev":[1,2],"approval_key":"sha256/ak"}]}`), 0o600)
	d, err := s.Find("p1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Port != 8765 || d.Addresses == nil || len(d.Addresses) != 0 || d.ApprovalKey != "sha256/ak" {
		t.Errorf("defaults: %+v", d)
	}
	if err := s.NoteAddress("p1", "10.1.1.1", 0, false); err != nil {
		t.Fatal(err)
	}
	raw := func() map[string]any {
		b, _ := os.ReadFile(s.Path(DevicesFile))
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%v\n%s", err, b)
		}
		return m
	}
	m := raw()
	dev := m["devices"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(m["x_top"], map[string]any{"a": 1.0}) || !reflect.DeepEqual(dev["x_dev"], []any{1.0, 2.0}) ||
		dev["approval_key"] != "sha256/ak" || dev["port"] != 8765.0 {
		t.Errorf("rewritten: %v", m)
	}

	// First sight keeps an existing key; a changed one needs accepting.
	// A key stored before attestation gets its outcome added.
	if prev, err := s.TrustApprovalKey("p1", "sha256/new", AttestedTEE); err != nil || prev != "sha256/ak" {
		t.Errorf("trust: %q %v", prev, err)
	}
	if d, _ := s.Find("p1"); d.ApprovalAttestation != "" {
		t.Errorf("attestation of another key stored: %q", d.ApprovalAttestation)
	}
	if prev, err := s.TrustApprovalKey("p1", "sha256/ak", AttestedStrongBox); err != nil || prev != "sha256/ak" {
		t.Errorf("trust: %q %v", prev, err)
	}
	if d, _ := s.Find("p1"); d.ApprovalAttestation != AttestedStrongBox {
		t.Errorf("attestation: %q", d.ApprovalAttestation)
	}
	if err := s.AcceptApprovalKey("p1", "sha256/new", Unattested); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Find("p1"); d.ApprovalKey != "sha256/new" || d.ApprovalAttestation != Unattested {
		t.Errorf("accepted: %q %q", d.ApprovalKey, d.ApprovalAttestation)
	}
	if err := s.NoteDecryptKey("p1", "sha256/dk", AttestedTEE); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Find("p1"); d.DecryptKey != "sha256/dk" || d.DecryptAttestation != AttestedTEE {
		t.Errorf("decryption key: %q %q", d.DecryptKey, d.DecryptAttestation)
	}
	// Re-pairing clears the approval key but keeps unknown members.
	d.Token, d.ApprovalKey, d.Extra = "t2", "", nil
	if err := s.PutDevice(d); err != nil {
		t.Fatal(err)
	}
	dev = raw()["devices"].([]any)[0].(map[string]any)
	if _, has := dev["approval_key"]; has || dev["token"] != "t2" || dev["x_dev"] == nil {
		t.Errorf("re-paired: %v", dev)
	}
	if _, has := dev["approval_attestation"]; has {
		t.Errorf("re-paired: %v", dev)
	}
	if prev, err := s.TrustApprovalKey("p1", "sha256/third", AttestedTEE); err != nil || prev != "" {
		t.Errorf("first sight: %q %v", prev, err)
	}
	if d, _ := s.Find("p1"); d.ApprovalKey != "sha256/third" || d.ApprovalAttestation != AttestedTEE {
		t.Errorf("first sight stored %q %q", d.ApprovalKey, d.ApprovalAttestation)
	}
	if _, err := s.TrustApprovalKey("gone", "sha256/x", AttestedTEE); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown device: %v", err)
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

// Members that differ from known ones only in case are read as those, so a
// rewrite must not keep them as unknown members that win at the next read.
func TestDevicesCaseInsensitiveMembers(t *testing.T) {
	s := Open(t.TempDir())
	os.WriteFile(s.Path(DevicesFile), []byte(`{"Version":2,"version":1,"devices":[
		{"id":"p1","Approval_Key":"sha256/other","approval_key":"sha256/ak","Token":"x","token":"t"}]}`), 0o600)
	if err := s.NoteAddress("p1", "10.1.1.1", 0, false); err != nil {
		t.Fatal(err)
	}
	d, err := s.Find("p1")
	if err != nil {
		t.Fatal(err)
	}
	if d.ApprovalKey != "sha256/ak" || d.Token != "t" || d.Extra != nil {
		t.Errorf("after a rewrite: %+v", d)
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
