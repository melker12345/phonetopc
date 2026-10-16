package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"filippo.io/age/plugin"

	"github.com/melker12345/phonetopc/pcd/internal/attest"
	"github.com/melker12345/phonetopc/pcd/internal/attest/attesttest"
	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
	"github.com/melker12345/phonetopc/pcd/internal/store"
)

// phone is a secure mock paired with a PC whose store is under
// $XDG_CONFIG_HOME, with the plugin binary on $PATH.
type phone struct {
	st  *mockphone.State
	mu  sync.Mutex
	why protocol.Why
	got []mockphone.ApprovalRequest
}

func (p *phone) answer(why protocol.Why) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.why = why
}

func (p *phone) requests() []mockphone.ApprovalRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]mockphone.ApprovalRequest(nil), p.got...)
}

// testCA attests the mock phones' keys; the plugin trusts it in place of
// Google's roots.
var testCA *attesttest.CA

func init() {
	var err error
	if testCA, err = attesttest.NewCA(); err != nil {
		panic(err)
	}
	attestationRoots = []*x509.Certificate{testCA.Root}
}

func newPhone(t *testing.T) *phone {
	return newPhoneAttested(t, true)
}

func newPhoneAttested(t *testing.T, attested bool) *phone {
	st, err := mockphone.OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Identity("", "Test Phone")
	if err != nil {
		t.Fatal(err)
	}
	p := &phone{st: st}
	var attestation func([]byte, attest.Purpose) []string
	if attested {
		attestation = mockphone.TestAttestation(testCA, id.Pin, nil)
	}
	srv := mockphone.New(mockphone.Options{
		Attest:       attestation,
		Device:       protocol.DeviceInfo{ID: id.ID, Name: id.Name, Platform: "mock"},
		Authenticate: st.Authenticate,
		ApprovalKey:  func() *ecdsa.PrivateKey { k, _ := st.ApprovalKey(); return k },
		DecryptKey:   func() *rsa.PrivateKey { k, _ := st.DecryptKey(); return k },
		Approve: func(ctx context.Context, req mockphone.ApprovalRequest) protocol.Why {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.got = append(p.got, req)
			return p.why
		},
		Logf: func(string, ...any) {},
	})
	hs := httptest.NewUnstartedServer(srv.Handler())
	hs.TLS = security.ServerTLSConfig(id.Cert)
	hs.Config.ErrorLog = log.New(io.Discard, "", 0)
	hs.StartTLS()
	t.Cleanup(func() { srv.Close(); hs.Close() })

	cfgHome := t.TempDir()
	cfg := store.Open(filepath.Join(cfgHome, "phonetopc"))
	pc, err := cfg.PC()
	if err != nil {
		t.Fatal(err)
	}
	tok := security.NewToken()
	st.PutPC(mockphone.PairedPC{ID: pc.ID, Name: "test PC", Token: tok})
	if err := cfg.PutDevice(store.Device{ID: id.ID, Name: "phone", Pin: id.Pin, Token: tok, PairedAt: store.Now(time.Now())}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("PHONETOPC_ADDR", strings.TrimPrefix(hs.URL, "https://"))
	t.Setenv("PHONETOPC_NO_MDNS", "1")
	t.Setenv("PHONETOPC_MODE", "user")
	t.Setenv("PHONETOPC_DEVICE", "")

	bin := t.TempDir()
	out, err := exec.Command("go", "build", "-o", filepath.Join(bin, "age-plugin-phonetopc"), ".").CombinedOutput()
	if err != nil {
		t.Fatalf("building the plugin: %v\n%s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return p
}

var ui = &plugin.ClientUI{
	DisplayMessage: func(string, string) error { return nil },
}

func generated(t *testing.T) (identity, recipient string) {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := run([]string{"--generate"}, &out, &errOut); code != 0 {
		t.Fatalf("--generate: %d %s", code, errOut.String())
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if r, ok := strings.CutPrefix(line, "# recipient: "); ok {
			recipient = r
		}
		if strings.HasPrefix(line, "AGE-PLUGIN-PHONETOPC-1") {
			identity = line
		}
	}
	if identity == "" || !strings.HasPrefix(recipient, "age1phonetopc1") {
		t.Fatalf("--generate printed:\n%s", out.String())
	}
	return identity, recipient
}

func encrypt(t *testing.T, msg string, rs ...age.Recipient) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, rs...)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, msg)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decrypt(file []byte, ids ...age.Identity) (string, error) {
	r, err := age.Decrypt(bytes.NewReader(file), ids...)
	if err != nil {
		return "", err
	}
	b, err := io.ReadAll(r)
	return string(b), err
}

func TestEncryptDecryptWithThePhone(t *testing.T) {
	p := newPhone(t)
	idStr, recStr := generated(t)
	rec, err := plugin.NewRecipient(recStr, ui)
	if err != nil {
		t.Fatal(err)
	}
	recovery, _ := age.GenerateX25519Identity()
	file := encrypt(t, "the secret", rec, recovery.Recipient())
	if len(p.requests()) != 0 {
		t.Fatal("encrypting asked the phone")
	}

	var shown []string
	id, err := plugin.NewIdentity(idStr, &plugin.ClientUI{DisplayMessage: func(name, msg string) error {
		shown = append(shown, name+": "+msg)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := decrypt(file, id); err != nil || got != "the secret" {
		t.Fatalf("decrypt: %q %v", got, err)
	}
	reqs := p.requests()
	if len(reqs) != 1 || !reqs[0].Decrypt || !strings.HasPrefix(reqs[0].Reason, "Decrypt") || reqs[0].Program != "age-plugin-phonetopc" {
		t.Fatalf("the phone was asked %+v", reqs)
	}
	// age shows the number to pick (DECRYPT.md §5).
	want := fmt.Sprintf(`phonetopc: confirm with your fingerprint on "Test Phone"; pick %d`, reqs[0].Match)
	if len(shown) != 1 || shown[0] != want {
		t.Errorf("age showed %q, want %q", shown, want)
	}

	// The identity is also a recipient.
	if got, err := decrypt(encrypt(t, "again", id.Recipient()), id); err != nil || got != "again" {
		t.Fatalf("identity as recipient: %q %v", got, err)
	}

	p.answer(protocol.WhyDenied)
	if _, err := decrypt(file, id); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("denied: %v", err)
	}
	p.answer("")

	// Fingerprints changed: the phone's key is gone; the recovery key still works.
	if _, err := p.st.RotateDecryptKey(); err != nil {
		t.Fatal(err)
	}
	n := len(p.requests())
	if _, err := decrypt(file, id); err == nil {
		t.Fatal("decrypted with a replaced key")
	}
	if len(p.requests()) != n {
		t.Error("the user was asked about a file for the old key")
	}
	if got, err := decrypt(file, recovery); err != nil || got != "the secret" {
		t.Fatalf("recovery: %q %v", got, err)
	}
}

func TestOtherFilesAreNotForThisIdentity(t *testing.T) {
	newPhone(t)
	idStr, _ := generated(t)
	id, _ := plugin.NewIdentity(idStr, ui)
	other, _ := age.GenerateX25519Identity()
	_, err := decrypt(encrypt(t, "x", other.Recipient()), id)
	var noMatch *age.NoIdentityMatchError
	if !errors.As(err, &noMatch) {
		t.Fatalf("want no identity match, got %v", err)
	}
}

func TestParseIdentityRejects(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":     nil,
		"no id":     {0},
		"short":     {5, 'a'},
		"no key":    {1, 'a'},
		"bad utf-8": {1, 0xff, 0x30},
		"junk key":  {1, 'a', 1, 2, 3},
	} {
		if _, _, err := parseIdentity(data); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestReason(t *testing.T) {
	for cmd, want := range map[string]string{
		"":                       plainReason,
		"age -d -i id.txt a.age": "Decrypt: age -d -i id.txt a.age",
		"age\t-d   x":            "Decrypt: age -d x",
		"age \xe2\x80\xae-d":     plainReason,
	} {
		if got := reason(cmd); got != want {
			t.Errorf("reason(%q) = %q, want %q", cmd, got, want)
		}
	}
	long := reason(strings.Repeat("x", 500))
	if protocol.CheckReason(long) != nil || !strings.HasSuffix(long, "…") {
		t.Errorf("long: %q", long)
	}
}

// TestGenerateNeedsAttestation: --generate refuses a key whose attestation
// is missing, since the recipient is derived from it, unless --unattested.
func TestGenerateNeedsAttestation(t *testing.T) {
	newPhoneAttested(t, false)
	var out, errOut bytes.Buffer
	if code := run([]string{"--generate"}, &out, &errOut); code != 1 || out.Len() != 0 ||
		!strings.Contains(errOut.String(), "failed key attestation") || !strings.Contains(errOut.String(), "--unattested") {
		t.Fatalf("--generate: %d %q %s", code, out.String(), errOut.String())
	}
	errOut.Reset()
	if code := run([]string{"--generate", "--unattested"}, &out, &errOut); code != 0 ||
		!strings.Contains(out.String(), "# attestation: none") || !strings.Contains(out.String(), "AGE-PLUGIN-PHONETOPC-1") {
		t.Fatalf("--generate --unattested: %d %s %s", code, out.String(), errOut.String())
	}
}
