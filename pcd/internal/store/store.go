// Package store is the PC's credential store of SECURITY.md §6: the PC's own
// identity (pc.json, pc-key.pem) and its paired phones (devices.json), in
// $XDG_CONFIG_HOME/phonetopc (default ~/.config/phonetopc).
//
// The directory is mode 0700 and every file 0600. Files are replaced
// atomically (temp file, fsync, rename), and read-modify-write updates of
// devices.json are serialised with an advisory lock file so that two
// phonetopc processes don't lose each other's changes.
package store

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/security"
)

// File names inside the store directory.
const (
	DevicesFile = "devices.json"
	PCFile      = "pc.json"
	PCKeyFile   = "pc-key.pem"
	lockFile    = ".lock"
)

// FileVersion is the devices.json version this package reads and writes.
const FileVersion = 1

// DefaultDir returns $XDG_CONFIG_HOME/phonetopc, or ~/.config/phonetopc if
// XDG_CONFIG_HOME is unset or not absolute (as the XDG spec requires).
// getenv is usually os.Getenv.
func DefaultDir(getenv func(string) string) (string, error) {
	if x := getenv("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "phonetopc"), nil
	}
	home := getenv("HOME")
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return "", fmt.Errorf("finding the config directory: %w", err)
		}
	}
	return filepath.Join(home, ".config", "phonetopc"), nil
}

// Its methods are safe for concurrent use.
type Store struct {
	dir string
	mu  sync.Mutex // serialises updates within this process
}

// Open returns the store in dir. Nothing is created until something is
// written.
func Open(dir string) *Store { return &Store{dir: dir} }

func (s *Store) Dir() string { return s.dir }

func (s *Store) Path(name string) string { return filepath.Join(s.dir, name) }

// ensureDir creates the directory with mode 0700, and tightens the mode of an
// existing one.
func (s *Store) ensureDir() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(s.dir, 0o700)
}

// DefaultPhonePort is the port of a device whose entry has none.
const DefaultPhonePort = 8765

// Device is one paired phone, exactly as in devices.json.
type Device struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Pin       string   `json:"pin"`
	Token     string   `json:"token"`
	Port      int      `json:"port"`
	Addresses []string `json:"addresses"`
	PairedAt  string   `json:"paired_at,omitempty"`
	// ApprovalKey is the accepted pin of the phone's approval key
	// (APPROVAL.md §3), empty until first seen.
	ApprovalKey string `json:"approval_key,omitempty"`

	// Extra holds members this version doesn't know. They are written back
	// unchanged (SECURITY.md §6: readers keep unknown fields).
	Extra map[string]json.RawMessage `json:"-"`
}

type plainDevice Device

var deviceFields = []string{"id", "name", "pin", "token", "port", "addresses", "paired_at", "approval_key"}

// UnmarshalJSON reads a device, applying the defaults of SECURITY.md §6
// (port 8765, no addresses) and keeping unknown members in Extra.
func (d *Device) UnmarshalJSON(b []byte) error {
	var p plainDevice
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	extra, err := unknownMembers(b, deviceFields)
	if err != nil {
		return err
	}
	p.Extra = extra
	if p.Port == 0 {
		p.Port = DefaultPhonePort
	}
	if p.Addresses == nil {
		p.Addresses = []string{}
	}
	*d = Device(p)
	return nil
}

// MarshalJSON writes the known members in their usual order, then the
// unknown ones.
func (d Device) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(plainDevice(d))
	if err != nil {
		return nil, err
	}
	return appendMembers(b, d.Extra)
}

func unknownMembers(b []byte, known []string) (map[string]json.RawMessage, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	// encoding/json matches member names case-insensitively, so "Token"
	// was read as the token too; written back as an unknown member after
	// the known ones, it would win at the next read.
	maps.DeleteFunc(all, func(k string, _ json.RawMessage) bool {
		return slices.ContainsFunc(known, func(name string) bool { return strings.EqualFold(k, name) })
	})
	if len(all) == 0 {
		return nil, nil
	}
	return all, nil
}

// extra is appended sorted by name.
func appendMembers(b []byte, extra map[string]json.RawMessage) ([]byte, error) {
	if len(extra) == 0 {
		return b, nil
	}
	keys := slices.Sorted(maps.Keys(extra))
	out := bytes.TrimSuffix(bytes.TrimSpace(b), []byte("}"))
	for i, k := range keys {
		if i > 0 || len(out) > 1 {
			out = append(out, ',')
		}
		kb, _ := json.Marshal(k)
		out = append(append(append(out, kb...), ':'), extra[k]...)
	}
	return append(out, '}'), nil
}

// devicesDoc is the content of devices.json.
type devicesDoc struct {
	Version int                        `json:"version"`
	Devices []Device                   `json:"devices"`
	Extra   map[string]json.RawMessage `json:"-"`
}

type plainDoc devicesDoc

func (d *devicesDoc) UnmarshalJSON(b []byte) error {
	var p plainDoc
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	extra, err := unknownMembers(b, []string{"version", "devices"})
	if err != nil {
		return err
	}
	p.Extra = extra
	*d = devicesDoc(p)
	return nil
}

func (d devicesDoc) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(plainDoc(d))
	if err != nil {
		return nil, err
	}
	return appendMembers(b, d.Extra)
}

// Now formats t as devices.json timestamps are written: RFC 3339 in UTC,
// whole seconds.
func Now(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

// A missing file means none.
func (s *Store) Devices() ([]Device, error) {
	doc, err := s.readDevices()
	return doc.Devices, err
}

func (s *Store) readDevices() (devicesDoc, error) {
	doc := devicesDoc{Version: FileVersion}
	b, err := os.ReadFile(s.Path(DevicesFile))
	if errors.Is(err, fs.ErrNotExist) {
		return doc, nil
	}
	if err != nil {
		return doc, err
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return doc, fmt.Errorf("%s: %w", s.Path(DevicesFile), err)
	}
	if doc.Version != FileVersion {
		return doc, fmt.Errorf("%s: unsupported version %d (this phonetopc reads version %d)", s.Path(DevicesFile), doc.Version, FileVersion)
	}
	return doc, nil
}

// updateDevices runs f on the current devices under the lock and writes the
// result back if f returns true.
func (s *Store) updateDevices(f func(devs []Device) ([]Device, bool, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDir(); err != nil {
		return err
	}
	unlock, err := lockPath(s.Path(lockFile))
	if err != nil {
		return err
	}
	defer unlock()
	doc, err := s.readDevices()
	if err != nil {
		return err
	}
	devs, changed, err := f(doc.Devices)
	if err != nil || !changed {
		return err
	}
	if devs == nil {
		devs = []Device{}
	}
	b, err := json.MarshalIndent(devicesDoc{Version: FileVersion, Devices: devs, Extra: doc.Extra}, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(s.Path(DevicesFile), append(b, '\n'))
}

// PutDevice adds d, replacing a device with the same id (a re-pairing).
// A re-pairing clears the stored approval key (APPROVAL.md §3) unless d
// has one, and keeps the old entry's unknown members unless d has its own.
func (s *Store) PutDevice(d Device) error {
	return s.putDevice(d, func([]Device, int) error { return nil })
}

// ErrDeviceChanged means the stored entry for a phone changed after the user
// was asked about it.
var ErrDeviceChanged = errors.New("the stored pairing for this phone changed while you were asked; pair again")

// PutDeviceIf is PutDevice, but only if the device stored under d.ID still
// has pin oldPin ("" for no device): the entry the user was shown when
// agreeing to the pairing. Otherwise it returns ErrDeviceChanged.
func (s *Store) PutDeviceIf(d Device, oldPin string) error {
	return s.putDevice(d, func(devs []Device, i int) error {
		if (i < 0 && oldPin == "") || (i >= 0 && oldPin != "" && devs[i].Pin == oldPin) {
			return nil
		}
		return ErrDeviceChanged
	})
}

func (s *Store) putDevice(d Device, check func(devs []Device, i int) error) error {
	if d.Addresses == nil {
		d.Addresses = []string{}
	}
	if d.Port == 0 {
		d.Port = DefaultPhonePort
	}
	return s.updateDevices(func(devs []Device) ([]Device, bool, error) {
		i := slices.IndexFunc(devs, func(x Device) bool { return x.ID == d.ID })
		if err := check(devs, i); err != nil {
			return nil, false, err
		}
		if i >= 0 {
			if d.Extra == nil {
				d.Extra = devs[i].Extra
			}
			devs[i] = d
		} else {
			devs = append(devs, d)
		}
		return devs, true, nil
	})
}

// RemoveDevice removes the device selected by sel (see Find) and returns it.
func (s *Store) RemoveDevice(sel string) (Device, error) {
	var removed Device
	err := s.updateDevices(func(devs []Device) ([]Device, bool, error) {
		d, err := find(devs, sel)
		if err != nil {
			return nil, false, err
		}
		removed = d
		return slices.DeleteFunc(devs, func(x Device) bool { return x.ID == d.ID }), true, nil
	})
	return removed, err
}

// NoteAddress records that device id was reachable at host:port
// (SECURITY.md §6): host moves to the front of its addresses, unless it is
// a loopback address (e.g. from adb forward), and port is updated only if
// it came from mDNS (fromMDNS). It does nothing if that is already the
// case, or if the device is gone (unpaired meanwhile).
func (s *Store) NoteAddress(id, host string, port int, fromMDNS bool) error {
	if isLoopback(host) {
		return nil
	}
	return s.updateDevices(func(devs []Device) ([]Device, bool, error) {
		i := slices.IndexFunc(devs, func(x Device) bool { return x.ID == id })
		if i < 0 {
			return nil, false, nil
		}
		d := &devs[i]
		if (!fromMDNS || d.Port == port) && len(d.Addresses) > 0 && d.Addresses[0] == host {
			return nil, false, nil
		}
		if fromMDNS {
			d.Port = port
		}
		d.Addresses = append([]string{host}, slices.DeleteFunc(d.Addresses, func(a string) bool { return a == host })...)
		const maxAddresses = 8
		if len(d.Addresses) > maxAddresses {
			d.Addresses = d.Addresses[:maxAddresses]
		}
		return devs, true, nil
	})
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.Unmap().IsLoopback()
}

// TrustApprovalKey implements first sight of APPROVAL.md §3: if device id
// has no approval key stored yet, pin is stored. It returns the key that
// was stored before ("" on first sight). The caller compares it with pin.
func (s *Store) TrustApprovalKey(id, pin string) (stored string, err error) {
	err = s.updateDevices(func(devs []Device) ([]Device, bool, error) {
		i := slices.IndexFunc(devs, func(x Device) bool { return x.ID == id })
		if i < 0 {
			return nil, false, fmt.Errorf("%w %q (unpaired meanwhile?)", ErrNotFound, id)
		}
		stored = devs[i].ApprovalKey
		if stored != "" {
			return nil, false, nil
		}
		devs[i].ApprovalKey = pin
		return devs, true, nil
	})
	return stored, err
}

// AcceptApprovalKey stores pin as device id's approval key, replacing any
// earlier one: the user accepted a changed key (APPROVAL.md §3).
func (s *Store) AcceptApprovalKey(id, pin string) error {
	return s.updateDevices(func(devs []Device) ([]Device, bool, error) {
		i := slices.IndexFunc(devs, func(x Device) bool { return x.ID == id })
		if i < 0 {
			return nil, false, fmt.Errorf("%w %q (unpaired meanwhile?)", ErrNotFound, id)
		}
		if devs[i].ApprovalKey == pin {
			return nil, false, nil
		}
		devs[i].ApprovalKey = pin
		return devs, true, nil
	})
}

// Errors of Find.
var (
	ErrNoDevices = errors.New("no paired phone; run `phonetopc pair` first")
	ErrNotFound  = errors.New("no paired phone matches")
)

// AmbiguousError says that a selector matched several devices, or that none
// was given while several are paired.
type AmbiguousError struct {
	Selector string
	Devices  []Device
}

func (e *AmbiguousError) Error() string {
	names := make([]string, len(e.Devices))
	for i, d := range e.Devices {
		names[i] = fmt.Sprintf("%q (%s)", d.Name, d.ID)
	}
	if e.Selector == "" {
		return fmt.Sprintf("several phones are paired, choose one with --device: %s", strings.Join(names, ", "))
	}
	return fmt.Sprintf("%q matches several phones, use the id: %s", e.Selector, strings.Join(names, ", "))
}

// Find selects a paired device. An empty sel selects the only device; a
// non-empty one matches an id exactly, or else a name (case-insensitively).
func (s *Store) Find(sel string) (Device, error) {
	devs, err := s.Devices()
	if err != nil {
		return Device{}, err
	}
	return find(devs, sel)
}

func find(devs []Device, sel string) (Device, error) {
	if len(devs) == 0 {
		return Device{}, ErrNoDevices
	}
	if sel == "" {
		if len(devs) == 1 {
			return devs[0], nil
		}
		return Device{}, &AmbiguousError{Devices: devs}
	}
	for _, d := range devs {
		if d.ID == sel {
			return d, nil
		}
	}
	var byName []Device
	for _, d := range devs {
		if strings.EqualFold(d.Name, sel) {
			byName = append(byName, d)
		}
	}
	switch len(byName) {
	case 0:
		return Device{}, fmt.Errorf("%w %q (see `phonetopc devices`)", ErrNotFound, sel)
	case 1:
		return byName[0], nil
	}
	return Device{}, &AmbiguousError{Selector: sel, Devices: byName}
}

type PC struct {
	ID   string
	Name string
	Key  *ecdsa.PrivateKey
	Cert tls.Certificate // self-signed, generated from Key at load time
	Pin  string
}

type pcDoc struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// PC loads the PC's identity, creating it on first use: a random id, the
// host name as name, and a new ECDSA P-256 key.
func (s *Store) PC() (*PC, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pc, err := s.loadPC(false); pc != nil || err != nil {
		return pc, err
	}
	// Creating takes the file lock and reads again, so two processes on
	// first run can't each make an identity and pair phones to the loser.
	if err := s.ensureDir(); err != nil {
		return nil, err
	}
	unlock, err := lockPath(s.Path(lockFile))
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.loadPC(true)
}

// loadPC returns nil, nil if the identity is incomplete and create is false.
func (s *Store) loadPC(create bool) (*PC, error) {
	var doc pcDoc
	b, err := os.ReadFile(s.Path(PCFile))
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", s.Path(PCFile), err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	if doc.ID == "" || doc.Name == "" {
		if !create {
			return nil, nil
		}
		if doc.ID == "" {
			doc.ID = security.NewID()
		}
		if doc.Name == "" {
			doc.Name = defaultName()
		}
		b, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := WriteFileAtomic(s.Path(PCFile), append(b, '\n')); err != nil {
			return nil, err
		}
	}

	var key *ecdsa.PrivateKey
	kb, err := os.ReadFile(s.Path(PCKeyFile))
	switch {
	case err == nil:
		if key, err = security.ParseKeyPEM(kb); err != nil {
			return nil, fmt.Errorf("%s: %w", s.Path(PCKeyFile), err)
		}
	case errors.Is(err, fs.ErrNotExist):
		if !create {
			return nil, nil
		}
		if key, err = security.GenerateKey(); err != nil {
			return nil, err
		}
		pemBytes, err := security.EncodeKeyPEM(key)
		if err != nil {
			return nil, err
		}
		if err := WriteFileAtomic(s.Path(PCKeyFile), pemBytes); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	cert, err := security.SelfSigned(key, "phonetopc-pc "+doc.ID)
	if err != nil {
		return nil, err
	}
	pin, err := security.CertPin(cert)
	if err != nil {
		return nil, err
	}
	return &PC{ID: doc.ID, Name: doc.Name, Key: key, Cert: cert, Pin: pin}, nil
}

func defaultName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "phonetopc-pc"
}

// WriteFileAtomic replaces path with data, mode 0600: it writes a temp file
// in the same directory, syncs it and renames it over path, so readers see
// either the old or the new content, never a mix.
func WriteFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if err = f.Chmod(0o600); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir makes a rename durable. Best effort: not every platform can sync
// a directory.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}
