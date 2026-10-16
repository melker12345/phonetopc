package mockphone

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/pairing"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
	"github.com/melker12345/phonetopc/pcd/internal/store"
)

// State is the mock phone's persistent state in a directory: what a real
// phone keeps in its app data.
//
//	phone.json       {"id": ..., "name": ...}
//	phone-key.pem    its long-term TLS key (ECDSA P-256)
//	approval-key.pem its approval key (ECDSA P-256, APPROVAL.md §2)
//	pcs.json         {"version": 1, "pcs": [{"id","name","pin","token","paired_at"}]}
//
// The layout is the mock's own; only the PC's devices.json is specified.
type State struct {
	dir string
	mu  sync.Mutex
}

type PairedPC struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Pin      string `json:"pin"`
	Token    string `json:"token"`
	PairedAt string `json:"paired_at"`
}

type pcsDoc struct {
	Version int        `json:"version"`
	PCs     []PairedPC `json:"pcs"`
}

type Identity struct {
	ID   string
	Name string
	Cert tls.Certificate
	Pin  string
}

// OpenState opens (creating, mode 0700) a state directory.
func OpenState(dir string) (*State, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &State{dir: dir}, nil
}

func (st *State) path(name string) string { return filepath.Join(st.dir, name) }

// Identity loads the phone's id, name and key, creating them on first use.
// id and name, if not empty, are used when creating, and name also
// overrides a stored name.
func (st *State) Identity(id, name string) (Identity, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	var doc struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	b, err := os.ReadFile(st.path("phone.json"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Identity{}, err
	}
	if err == nil {
		if err := json.Unmarshal(b, &doc); err != nil {
			return Identity{}, fmt.Errorf("phone.json: %w", err)
		}
	}
	changed := false
	if doc.ID == "" {
		doc.ID, changed = id, true
		if doc.ID == "" {
			doc.ID = security.NewID()
		}
	}
	if (name != "" && doc.Name != name) || doc.Name == "" {
		doc.Name, changed = name, true
		if doc.Name == "" {
			doc.Name = "phonetopc mock"
		}
	}
	if changed {
		b, _ := json.MarshalIndent(doc, "", "  ")
		if err := store.WriteFileAtomic(st.path("phone.json"), append(b, '\n')); err != nil {
			return Identity{}, err
		}
	}
	kb, err := os.ReadFile(st.path("phone-key.pem"))
	var key *ecdsa.PrivateKey
	switch {
	case err == nil:
		k, err := security.ParseKeyPEM(kb)
		if err != nil {
			return Identity{}, fmt.Errorf("phone-key.pem: %w", err)
		}
		key = k
	case errors.Is(err, fs.ErrNotExist):
		k, err := security.GenerateKey()
		if err != nil {
			return Identity{}, err
		}
		pemBytes, err := security.EncodeKeyPEM(k)
		if err != nil {
			return Identity{}, err
		}
		if err := store.WriteFileAtomic(st.path("phone-key.pem"), pemBytes); err != nil {
			return Identity{}, err
		}
		key = k
	default:
		return Identity{}, err
	}
	cert, err := security.SelfSigned(key, "phonetopc-mock "+doc.ID)
	if err != nil {
		return Identity{}, err
	}
	pin, _ := security.CertPin(cert)
	return Identity{ID: doc.ID, Name: doc.Name, Cert: cert, Pin: pin}, nil
}

// approvalKeyFile holds the mock's approval key: a software stand-in for
// the phone's biometric-bound Keystore key (APPROVAL.md §2).
const approvalKeyFile = "approval-key.pem"

// ApprovalKey loads the mock's approval key, creating it on first use.
func (st *State) ApprovalKey() (*ecdsa.PrivateKey, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	b, err := os.ReadFile(st.path(approvalKeyFile))
	switch {
	case err == nil:
		k, err := security.ParseKeyPEM(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", approvalKeyFile, err)
		}
		return k, nil
	case errors.Is(err, fs.ErrNotExist):
		return st.newApprovalKey()
	default:
		return nil, err
	}
}

// RotateApprovalKey replaces the approval key with a new one, as a phone
// does when biometric enrollment changed (APPROVAL.md §2). PCs then see a
// different approval pin and refuse approvals until their user accepts it.
func (st *State) RotateApprovalKey() (*ecdsa.PrivateKey, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.newApprovalKey()
}

func (st *State) newApprovalKey() (*ecdsa.PrivateKey, error) {
	k, err := security.GenerateKey()
	if err != nil {
		return nil, err
	}
	pemBytes, err := security.EncodeKeyPEM(k)
	if err != nil {
		return nil, err
	}
	if err := store.WriteFileAtomic(st.path(approvalKeyFile), pemBytes); err != nil {
		return nil, err
	}
	return k, nil
}

func (st *State) PCs() ([]PairedPC, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.readPCs()
}

func (st *State) readPCs() ([]PairedPC, error) {
	b, err := os.ReadFile(st.path("pcs.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc pcsDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("pcs.json: %w", err)
	}
	return doc.PCs, nil
}

func (st *State) writePCs(pcs []PairedPC) error {
	if pcs == nil {
		pcs = []PairedPC{}
	}
	b, err := json.MarshalIndent(pcsDoc{Version: 1, PCs: pcs}, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteFileAtomic(st.path("pcs.json"), append(b, '\n'))
}

// PutPC stores a pairing, replacing an earlier one with the same PC id and
// with it the old token (SECURITY.md §4.4).
func (st *State) PutPC(pc PairedPC) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	pcs, err := st.readPCs()
	if err != nil {
		return err
	}
	if pc.PairedAt == "" {
		pc.PairedAt = store.Now(time.Now())
	}
	if i := slices.IndexFunc(pcs, func(x PairedPC) bool { return x.ID == pc.ID }); i >= 0 {
		pcs[i] = pc
	} else {
		pcs = append(pcs, pc)
	}
	return st.writePCs(pcs)
}

// RemovePC revokes a PC, selected by id or (case-insensitive) name.
func (st *State) RemovePC(sel string) (PairedPC, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	pcs, err := st.readPCs()
	if err != nil {
		return PairedPC{}, err
	}
	i := slices.IndexFunc(pcs, func(x PairedPC) bool { return x.ID == sel })
	if i < 0 {
		i = slices.IndexFunc(pcs, func(x PairedPC) bool { return strings.EqualFold(x.Name, sel) })
	}
	if i < 0 {
		return PairedPC{}, fmt.Errorf("no paired PC %q", sel)
	}
	pc := pcs[i]
	return pc, st.writePCs(slices.Delete(pcs, i, i+1))
}

// Authenticate maps a token to its PC, comparing in constant time against
// every stored token. It reads pcs.json each time, so pairings and
// revocations made by another process take effect at the next hello.
func (st *State) Authenticate(token string) (protocol.PCInfo, bool) {
	pcs, err := st.PCs()
	if err != nil {
		return protocol.PCInfo{}, false
	}
	var (
		hit   protocol.PCInfo
		found bool
	)
	for _, pc := range pcs {
		if security.Equal(pc.Token, token) && pc.Token != "" {
			hit, found = protocol.PCInfo{ID: pc.ID, Name: pc.Name}, true
		}
	}
	return hit, found
}

// Changed returns a function reporting whether pcs.json changed since its
// previous call (by size and modification time). For polling revocations.
func (st *State) Changed() func() bool {
	var last fs.FileInfo
	return func() bool {
		fi, _ := os.Stat(st.path("pcs.json"))
		changed := (fi == nil) != (last == nil) || (fi != nil && (fi.ModTime() != last.ModTime() || fi.Size() != last.Size()))
		last = fi
		return changed
	}
}

// PhoneInfo describes this phone to a PC during pairing: it serves on port
// at addrs.
func (id Identity) PhoneInfo(port int, addrs []string) pairing.PhoneInfo {
	return pairing.PhoneInfo{ID: id.ID, Name: id.Name, Pin: id.Pin, Port: port, Addresses: addrs}
}

// Record stores the outcome of a pairing made with package pairing.
func (st *State) Record(p pairing.PairedPC) error {
	return st.PutPC(PairedPC{ID: p.ID, Name: p.Name, Pin: p.Pin, Token: p.Token})
}

// decryptKeyFile holds the mock's decryption key: a software stand-in for
// the phone's biometric-bound Keystore key (DECRYPT.md §2).
const decryptKeyFile = "decrypt-key.pem"

// DecryptKey loads the mock's decryption key, creating it on first use.
func (st *State) DecryptKey() (*rsa.PrivateKey, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	b, err := os.ReadFile(st.path(decryptKeyFile))
	switch {
	case err == nil:
		block, _ := pem.Decode(b)
		if block == nil {
			return nil, fmt.Errorf("%s: no PEM block", decryptKeyFile)
		}
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", decryptKeyFile, err)
		}
		r, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%s: not an RSA key", decryptKeyFile)
		}
		return r, nil
	case errors.Is(err, fs.ErrNotExist):
		return st.newDecryptKey()
	default:
		return nil, err
	}
}

// RotateDecryptKey replaces the decryption key, as a phone does when
// biometric enrollment changed (DECRYPT.md §2). What was encrypted to the
// old key can't be decrypted any more.
func (st *State) RotateDecryptKey() (*rsa.PrivateKey, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.newDecryptKey()
}

func (st *State) newDecryptKey() (*rsa.PrivateKey, error) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	if err := store.WriteFileAtomic(st.path(decryptKeyFile), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
		return nil, err
	}
	return k, nil
}
