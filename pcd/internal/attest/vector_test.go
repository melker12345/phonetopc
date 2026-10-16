package attest_test

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/attest"
	"github.com/melker12345/phonetopc/pcd/internal/attest/attesttest"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

var writeVector = flag.Bool("write-vector", false, "rewrite spec/conformance/vectors/attestation.json")

var vectorDir = filepath.Join("..", "..", "..", "spec", "conformance", "vectors")

type attestationVector struct {
	Description string   `json:"description"`
	PhoneCert   string   `json:"phone_cert"`
	Challenge   string   `json:"challenge"`
	Key         string   `json:"key"`
	Root        string   `json:"root"`
	Time        string   `json:"time"`
	Valid       []string `json:"valid"`
	Level       string   `json:"level"`
	Reject      []struct {
		Name  string   `json:"name"`
		Check string   `json:"check"`
		Chain []string `json:"chain"`
	} `json:"reject"`
}

func decodeChain(t *testing.T, chain []string) [][]byte {
	t.Helper()
	out := make([][]byte, len(chain))
	for i, s := range chain {
		b, err := security.DecodeB64(s)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = b
	}
	return out
}

func encodeChain(chain [][]byte) []string {
	out := make([]string, len(chain))
	for i, b := range chain {
		out[i] = security.B64(b)
	}
	return out
}

// TestAttestationVector checks vectors/attestation.json: a chain from a
// test root for the approval key of approval.json, made with the challenge
// of phone-cert.pem.
func TestAttestationVector(t *testing.T) {
	if *writeVector {
		writeAttestationVector(t)
	}
	b, err := os.ReadFile(filepath.Join(vectorDir, "attestation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v attestationVector
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	certPEM, err := os.ReadFile(filepath.Join(vectorDir, v.PhoneCert))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(certPEM)
	phone, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	challenge := attest.Challenge(phone.RawSubjectPublicKeyInfo)
	if security.B64(challenge) != v.Challenge {
		t.Fatalf("challenge %s, vector %s", security.B64(challenge), v.Challenge)
	}
	if string(security.PinDigest(security.Pin(phone))) != string(challenge) {
		t.Fatal("the challenge is not the digest in the phone's pin")
	}
	rootDER, _ := security.DecodeB64(v.Root)
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	at, _ := time.Parse(time.RFC3339, v.Time)
	o := attest.Options{Roots: []*x509.Certificate{root}, Now: at}
	key, _ := security.DecodeB64(v.Key)
	res, err := attest.Verify(decodeChain(t, v.Valid), key, challenge, attest.PurposeSign, o)
	if err != nil || res.Level.String() != v.Level {
		t.Fatalf("valid: %+v %v", res, err)
	}
	if len(v.Reject) < 5 {
		t.Fatalf("%d reject cases", len(v.Reject))
	}
	for _, r := range v.Reject {
		_, err := attest.Verify(decodeChain(t, r.Chain), key, challenge, attest.PurposeSign, o)
		t.Run(r.Name, func(t *testing.T) { wantCheck(t, err, r.Check) })
	}
}

func writeAttestationVector(t *testing.T) {
	var approval struct {
		Key string `json:"key"`
	}
	b, err := os.ReadFile(filepath.Join(vectorDir, "approval.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &approval); err != nil {
		t.Fatal(err)
	}
	spki, _ := security.DecodeB64(approval.Key)
	pub, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, _ := os.ReadFile(filepath.Join(vectorDir, "phone-cert.pem"))
	blk, _ := pem.Decode(certPEM)
	phone, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	challenge := attest.Challenge(phone.RawSubjectPublicKeyInfo)
	ca, err := attesttest.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	chain := func(tweak func(*attesttest.Description)) []string {
		d := attesttest.Good(challenge, attest.PurposeSign)
		if tweak != nil {
			tweak(&d)
		}
		c, err := ca.Chain(pub, d)
		if err != nil {
			t.Fatal(err)
		}
		return encodeChain(c)
	}
	v := attestationVector{
		Description: "Key attestation (APPROVAL.md §3.1) from a test root (`root`; never trust it outside tests). " +
			"`challenge` is base64url(SHA-256(SubjectPublicKeyInfo of `phone_cert`)), the digest in the phone's pin. " +
			"`valid` is a chain, leaf first, for the approval key `key` (as in approval.json), which a verifier given `root` " +
			"MUST accept at `time` with security level `level`; it MUST reject every `reject` chain, failing `check`.",
		PhoneCert: "phone-cert.pem",
		Challenge: security.B64(challenge),
		Key:       approval.Key,
		Root:      security.B64(ca.Root.Raw),
		Time:      "2026-10-16T00:00:00Z",
		Valid:     chain(nil),
		Level:     "tee",
	}
	add := func(name, check string, tweak func(*attesttest.Description)) {
		v.Reject = append(v.Reject, struct {
			Name  string   `json:"name"`
			Check string   `json:"check"`
			Chain []string `json:"chain"`
		}{name, check, chain(tweak)})
	}
	add("wrong_challenge", attest.CheckChallenge, func(d *attesttest.Description) { d.Challenge = make([]byte, 32) })
	add("software", attest.CheckSecurityLevel, func(d *attesttest.Description) { d.AttestationLevel, d.KeyMintLevel = attest.Software, attest.Software })
	add("no_biometric", attest.CheckBiometric, func(d *attesttest.Description) { d.UserAuthType = attesttest.Int(attest.AuthPassword) })
	add("auth_timeout", attest.CheckPerUse, func(d *attesttest.Description) { d.AuthTimeout = attesttest.Int(300) })
	add("decrypt_purpose", attest.CheckPurpose, func(d *attesttest.Description) { d.Purposes = []int64{int64(attest.PurposeDecrypt)} })
	add("unverified_boot", attest.CheckBoot, func(d *attesttest.Description) { d.BootState = attest.BootUnverified })
	add("unlocked", attest.CheckBoot, func(d *attesttest.Description) { d.DeviceLocked = false })
	add("wrong_package", attest.CheckPackage, func(d *attesttest.Description) { d.Packages = []string{"com.example.other"} })
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vectorDir, "attestation.json"), append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
