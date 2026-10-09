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
	"crypto/ecdsa"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

// Store is a credential store directory. Its methods are safe for concurrent
// use.
type Store struct {
	dir string
	mu  sync.Mutex // serialises updates within this process
}

// Open returns the store in dir. Nothing is created until something is
// written.
func Open(dir string) *Store { return &Store{dir: dir} }

// Dir returns the store's directory.
func (s *Store) Dir() string { return s.dir }

// Path returns the path of a file in the store.
func (s *Store) Path(name string) string { return filepath.Join(s.dir, name) }

// ensureDir creates the directory with mode 0700, and tightens the mode of an
// existing one.
func (s *Store) ensureDir() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(s.dir, 0o700)
}

// Device is one paired phone, exactly as in devices.json.
type Device struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Pin       string   `json:"pin"`
	Token     string   `json:"token"`
	Port      int      `json:"port"`
	Addresses []string `json:"addresses"`
	PairedAt  string   `json:"paired_at"`
}

// devicesDoc is the content of devices.json.
type devicesDoc struct {
	Version int      `json:"version"`
	Devices []Device `json:"devices"`
}

// Now formats t as devices.json timestamps are written: RFC 3339 in UTC,
// whole seconds.
func Now(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

// Devices returns the paired devices. A missing file means none.
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
	b, err := json.MarshalIndent(devicesDoc{Version: FileVersion, Devices: devs}, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(s.Path(DevicesFile), append(b, '\n'))
}

// PutDevice adds d, replacing a device with the same id (a re-pairing).
func (s *Store) PutDevice(d Device) error {
	if d.Addresses == nil {
		d.Addresses = []string{}
	}
	return s.updateDevices(func(devs []Device) ([]Device, bool, error) {
		i := slices.IndexFunc(devs, func(x Device) bool { return x.ID == d.ID })
		if i >= 0 {
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

// NoteAddress records that device id was reachable at host:port: host moves
// to the front of its addresses and port is updated. It does nothing if that
// is already the case, or if the device is gone (unpaired meanwhile).
func (s *Store) NoteAddress(id, host string, port int) error {
	return s.updateDevices(func(devs []Device) ([]Device, bool, error) {
		i := slices.IndexFunc(devs, func(x Device) bool { return x.ID == id })
		if i < 0 {
			return nil, false, nil
		}
		d := &devs[i]
		if d.Port == port && len(d.Addresses) > 0 && d.Addresses[0] == host {
			return nil, false, nil
		}
		d.Port = port
		d.Addresses = append([]string{host}, slices.DeleteFunc(d.Addresses, func(a string) bool { return a == host })...)
		const maxAddresses = 8
		if len(d.Addresses) > maxAddresses {
			d.Addresses = d.Addresses[:maxAddresses]
		}
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

// PC is the PC's own identity.
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
		if err := s.ensureDir(); err != nil {
			return nil, err
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
		if err := s.ensureDir(); err != nil {
			return nil, err
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
