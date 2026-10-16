package attest_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/attest"
	"github.com/melker12345/phonetopc/pcd/internal/attest/attesttest"
)

var now = time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)

type fixture struct {
	ca        *attesttest.CA
	key       *ecdsa.PrivateKey
	spki      []byte
	challenge []byte
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ca, err := attesttest.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	return fixture{ca: ca, key: key, spki: spki, challenge: attest.Challenge([]byte("phone tls spki"))}
}

func (f fixture) chain(t *testing.T, tweak func(*attesttest.Description)) [][]byte {
	t.Helper()
	d := attesttest.Good(f.challenge, attest.PurposeSign)
	if tweak != nil {
		tweak(&d)
	}
	c, err := f.ca.Chain(&f.key.PublicKey, d)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f fixture) opts() attest.Options {
	return attest.Options{Roots: []*x509.Certificate{f.ca.Root}, Now: now}
}

func wantCheck(t *testing.T, err error, check string) {
	t.Helper()
	var ae *attest.Error
	if !errors.As(err, &ae) || ae.Check != check {
		t.Fatalf("got %v, want a failed %q check", err, check)
	}
}

func TestVerifyValid(t *testing.T) {
	f := newFixture(t)
	res, err := attest.Verify(f.chain(t, nil), f.spki, f.challenge, attest.PurposeSign, f.opts())
	if err != nil {
		t.Fatal(err)
	}
	if res.Level != attest.TrustedEnvironment || len(res.SigningDigests) != 1 {
		t.Fatalf("%+v", res)
	}

	sb := f.chain(t, func(d *attesttest.Description) {
		d.AttestationLevel, d.KeyMintLevel = attest.StrongBox, attest.StrongBox
	})
	if res, err := attest.Verify(sb, f.spki, f.challenge, attest.PurposeSign, f.opts()); err != nil || res.Level != attest.StrongBox {
		t.Fatalf("strongbox: %+v %v", res, err)
	}

	// Without the root: the last certificate is issued by it.
	if _, err := attest.Verify(f.chain(t, nil)[:2], f.spki, f.challenge, attest.PurposeSign, f.opts()); err != nil {
		t.Fatalf("without the root: %v", err)
	}
}

func TestVerifyRejects(t *testing.T) {
	f := newFixture(t)
	other := newFixture(t)
	cases := []struct {
		name  string
		tweak func(*attesttest.Description)
		check string
	}{
		{"wrong challenge", func(d *attesttest.Description) { d.Challenge = attest.Challenge([]byte("another phone")) }, attest.CheckChallenge},
		{"software", func(d *attesttest.Description) { d.AttestationLevel, d.KeyMintLevel = attest.Software, attest.Software }, attest.CheckSecurityLevel},
		{"software keymint", func(d *attesttest.Description) { d.KeyMintLevel = attest.Software }, attest.CheckSecurityLevel},
		{"no user auth", func(d *attesttest.Description) { d.UserAuthType = nil }, attest.CheckBiometric},
		{"no auth required", func(d *attesttest.Description) { d.NoAuthRequired = true }, attest.CheckBiometric},
		{"password only", func(d *attesttest.Description) { d.UserAuthType = attesttest.Int(attest.AuthPassword) }, attest.CheckBiometric},
		{"password fallback", func(d *attesttest.Description) {
			d.UserAuthType = attesttest.Int(attest.AuthPassword | attest.AuthFingerprint)
		}, attest.CheckBiometric},
		{"auth timeout", func(d *attesttest.Description) { d.AuthTimeout = attesttest.Int(30) }, attest.CheckPerUse},
		{"wrong purpose", func(d *attesttest.Description) { d.Purposes = []int64{int64(attest.PurposeDecrypt)} }, attest.CheckPurpose},
		{"unverified boot", func(d *attesttest.Description) { d.BootState = attest.BootUnverified }, attest.CheckBoot},
		{"self-signed boot", func(d *attesttest.Description) { d.BootState = attest.BootSelfSigned }, attest.CheckBoot},
		{"unlocked", func(d *attesttest.Description) { d.DeviceLocked = false }, attest.CheckBoot},
		{"no root of trust", func(d *attesttest.Description) { d.NoRootOfTrust = true }, attest.CheckBoot},
		{"wrong package", func(d *attesttest.Description) { d.Packages = []string{"com.example.evil"} }, attest.CheckPackage},
		{"shared uid", func(d *attesttest.Description) { d.Packages = []string{attest.Package, "com.example.other"} }, attest.CheckPackage},
		{"no package", func(d *attesttest.Description) { d.Packages = nil }, attest.CheckPackage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := attest.Verify(f.chain(t, c.tweak), f.spki, f.challenge, attest.PurposeSign, f.opts())
			wantCheck(t, err, c.check)
		})
	}

	t.Run("wrong root", func(t *testing.T) {
		_, err := attest.Verify(f.chain(t, nil), f.spki, f.challenge, attest.PurposeSign, other.opts())
		wantCheck(t, err, attest.CheckRoot)
	})
	t.Run("google roots", func(t *testing.T) {
		_, err := attest.Verify(f.chain(t, nil), f.spki, f.challenge, attest.PurposeSign, attest.Options{Now: now})
		wantCheck(t, err, attest.CheckRoot)
	})
	t.Run("key mismatch", func(t *testing.T) {
		_, err := attest.Verify(f.chain(t, nil), other.spki, f.challenge, attest.PurposeSign, f.opts())
		wantCheck(t, err, attest.CheckKey)
	})
	t.Run("spliced chain", func(t *testing.T) {
		c := f.chain(t, nil)
		c[1] = other.ca.Intermediate.Raw
		_, err := attest.Verify(c, f.spki, f.challenge, attest.PurposeSign, f.opts())
		wantCheck(t, err, attest.CheckChain)
	})
	t.Run("extended chain", func(t *testing.T) {
		// Whoever holds an attested key can sign a certificate for another
		// key, with a key description of its choosing: only the one the
		// hardware made counts.
		c := f.chain(t, nil)
		leaf, _ := x509.ParseCertificate(c[0])
		ext, _ := attesttest.Encode(attesttest.Good(f.challenge, attest.PurposeSign))
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(9), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
			ExtraExtensions: []pkix.Extension{{Id: attest.OID, Value: ext}},
		}
		fake, err := x509.CreateCertificate(rand.Reader, tmpl, leaf, &other.key.PublicKey, f.key)
		if err != nil {
			t.Fatal(err)
		}
		_, err = attest.Verify(append([][]byte{fake}, c...), other.spki, f.challenge, attest.PurposeSign, f.opts())
		wantCheck(t, err, attest.CheckChain)
	})
	t.Run("too short", func(t *testing.T) {
		_, err := attest.Verify(f.chain(t, nil)[:1], f.spki, f.challenge, attest.PurposeSign, f.opts())
		wantCheck(t, err, attest.CheckChain)
	})
	t.Run("garbage", func(t *testing.T) {
		_, err := attest.Verify([][]byte{{1, 2}, {3}}, f.spki, f.challenge, attest.PurposeSign, f.opts())
		wantCheck(t, err, attest.CheckChain)
	})
	t.Run("validity at creation", func(t *testing.T) {
		o := f.opts()
		o.Now = attesttest.NotAfter.Add(time.Hour)
		if _, err := attest.Verify(f.chain(t, nil), f.spki, f.challenge, attest.PurposeSign, o); err != nil {
			t.Fatalf("a key made while its chain was valid: %v", err)
		}
		early := f.chain(t, func(d *attesttest.Description) { d.Created = attesttest.NotBefore.Add(-time.Hour) })
		_, err := attest.Verify(early, f.spki, f.challenge, attest.PurposeSign, o)
		wantCheck(t, err, attest.CheckValidity)
		o.Now = attesttest.NotBefore.Add(-time.Hour)
		_, err = attest.Verify(f.chain(t, nil), f.spki, f.challenge, attest.PurposeSign, o)
		wantCheck(t, err, attest.CheckValidity)
	})
	t.Run("signing certificate", func(t *testing.T) {
		o := f.opts()
		o.SigningDigests = [][]byte{{1, 2, 3}}
		_, err := attest.Verify(f.chain(t, nil), f.spki, f.challenge, attest.PurposeSign, o)
		wantCheck(t, err, attest.CheckSigner)
		o.SigningDigests = append(o.SigningDigests, make([]byte, 32))
		if _, err := attest.Verify(f.chain(t, nil), f.spki, f.challenge, attest.PurposeSign, o); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("revoked", func(t *testing.T) {
		c := f.chain(t, nil)
		inter, _ := x509.ParseCertificate(c[1])
		l, err := attest.ParseStatusList([]byte(`{"entries":{"` + inter.SerialNumber.Text(16) + `":{"status":"REVOKED","reason":"KEY_COMPROMISE"}}}`))
		if err != nil {
			t.Fatal(err)
		}
		o := f.opts()
		o.Status = l
		_, err = attest.Verify(c, f.spki, f.challenge, attest.PurposeSign, o)
		wantCheck(t, err, attest.CheckRevocation)
		o.Status, _ = attest.ParseStatusList([]byte(`{"entries":{}}`))
		if _, err := attest.Verify(c, f.spki, f.challenge, attest.PurposeSign, o); err != nil {
			t.Fatal(err)
		}
	})
}

func TestVerifyDecryptPurpose(t *testing.T) {
	f := newFixture(t)
	c := f.chain(t, func(d *attesttest.Description) { d.Purposes = []int64{int64(attest.PurposeDecrypt)} })
	if _, err := attest.Verify(c, f.spki, f.challenge, attest.PurposeDecrypt, f.opts()); err != nil {
		t.Fatal(err)
	}
	_, err := attest.Verify(c, f.spki, f.challenge, attest.PurposeSign, f.opts())
	wantCheck(t, err, attest.CheckPurpose)
}

// The embedded roots are the ones fetched from the Android documentation
// (attestation/root JSON) on 2026-10-16, by SHA-256 of their DER.
func TestGoogleRoots(t *testing.T) {
	want := []string{
		"cedb1cb6dc896ae5ec797348bce9286753c2b38ee71ce0fbe34a9a1248800dfc",
		"6d9db4ce6c5c0b293166d08986e05774a8776ceb525d9e4329520de12ba4bcc0",
	}
	roots := attest.GoogleRoots()
	if len(roots) != len(want) {
		t.Fatalf("%d roots", len(roots))
	}
	for i, r := range roots {
		sum := sha256.Sum256(r.Raw)
		if got := hex.EncodeToString(sum[:]); got != want[i] {
			t.Errorf("root %d: %s", i, got)
		}
	}
}

func TestParseKeyDescriptionStrict(t *testing.T) {
	d := attesttest.Good([]byte("c"), attest.PurposeSign)
	der, err := attesttest.Encode(d)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := attest.ParseKeyDescription(der)
	if err != nil {
		t.Fatal(err)
	}
	hw := kd.HardwareEnforced
	if string(kd.Challenge) != "c" || hw.UserAuthType == nil || *hw.UserAuthType != attest.AuthFingerprint ||
		hw.RootOfTrust == nil || !hw.RootOfTrust.DeviceLocked || kd.SoftwareEnforced.AttestationApplicationID == nil ||
		kd.SoftwareEnforced.AttestationApplicationID.Packages[0].Name != attest.Package {
		t.Fatalf("%+v", kd)
	}
	if _, err := attest.ParseKeyDescription(append(der, 0)); err == nil {
		t.Error("trailing byte accepted")
	}
}
