package attest_test

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/attest"
)

// testdata/*.pem and *.json are real attestation chains, and their decoded
// key descriptions, from Google's verifier library
// (https://github.com/android/keyattestation, testdata/, commit 621149c,
// Apache License 2.0): a TEE key on the new ECDSA root, a StrongBox key
// from remote key provisioning, and a TEE key on the RSA root that needs a
// device credential.

type sample struct {
	AttestationSecurityLevel string `json:"attestationSecurityLevel"`
	KeyMintSecurityLevel     string `json:"keyMintSecurityLevel"`
	AttestationChallenge     string `json:"attestationChallenge"`
	SoftwareEnforced         struct {
		CreationDateTime         string `json:"creationDateTime"`
		AttestationApplicationID struct {
			Packages []struct {
				Name string `json:"name"`
			} `json:"packages"`
			Signatures []string `json:"signatures"`
		} `json:"attestationApplicationId"`
	} `json:"softwareEnforced"`
	HardwareEnforced struct {
		Purposes       []string `json:"purposes"`
		NoAuthRequired bool     `json:"noAuthRequired"`
		UserAuthType   string   `json:"userAuthType"`
		AuthTimeout    string   `json:"authTimeout"`
		OSVersion      string   `json:"osVersion"`
		RootOfTrust    struct {
			DeviceLocked      bool   `json:"deviceLocked"`
			VerifiedBootState string `json:"verifiedBootState"`
		} `json:"rootOfTrust"`
	} `json:"hardwareEnforced"`
}

func loadSample(t *testing.T, name string) ([][]byte, sample) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".pem"))
	if err != nil {
		t.Fatal(err)
	}
	var chain [][]byte
	for {
		var blk *pem.Block
		if blk, b = pem.Decode(b); blk == nil {
			break
		}
		chain = append(chain, blk.Bytes)
	}
	var s sample
	j, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(j, &s); err != nil {
		t.Fatal(err)
	}
	return chain, s
}

var levels = map[string]attest.SecurityLevel{
	"SOFTWARE": attest.Software, "TRUSTED_ENVIRONMENT": attest.TrustedEnvironment, "STRONG_BOX": attest.StrongBox,
}

var bootStates = map[string]int{
	"VERIFIED": attest.BootVerified, "SELF_SIGNED": attest.BootSelfSigned, "UNVERIFIED": attest.BootUnverified, "FAILED": attest.BootFailed,
}

func num(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGoogleSamples(t *testing.T) {
	cases := []struct {
		name  string
		check string // the first check Verify fails, after chain, root, key and challenge passed
	}{
		{"frankel_TEE_EC_2026", attest.CheckBiometric},
		{"caiman_SB_EC_RKP", attest.CheckBiometric},
		{"akita_TEE_RSA_NONE_USERAUTH", attest.CheckBiometric},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			chain, s := loadSample(t, c.name)
			leaf, err := x509.ParseCertificate(chain[0])
			if err != nil {
				t.Fatal(err)
			}
			var ext []byte
			for _, e := range leaf.Extensions {
				if e.Id.Equal(attest.OID) {
					ext = e.Value
				}
			}
			kd, err := attest.ParseKeyDescription(ext)
			if err != nil {
				t.Fatal(err)
			}
			challenge, _ := base64.StdEncoding.DecodeString(s.AttestationChallenge)
			hw, sw := kd.HardwareEnforced, kd.SoftwareEnforced
			if kd.AttestationSecurityLevel != levels[s.AttestationSecurityLevel] || kd.KeyMintSecurityLevel != levels[s.KeyMintSecurityLevel] {
				t.Errorf("levels %v %v", kd.AttestationSecurityLevel, kd.KeyMintSecurityLevel)
			}
			if !bytes.Equal(kd.Challenge, challenge) {
				t.Errorf("challenge %q", kd.Challenge)
			}
			var purposes []int64
			for _, p := range s.HardwareEnforced.Purposes {
				purposes = append(purposes, num(t, p))
			}
			if !slices.Equal(hw.Purposes, purposes) {
				t.Errorf("purposes %v", hw.Purposes)
			}
			if hw.NoAuthRequired != s.HardwareEnforced.NoAuthRequired {
				t.Errorf("noAuthRequired %v", hw.NoAuthRequired)
			}
			if (hw.UserAuthType == nil) != (s.HardwareEnforced.UserAuthType == "") ||
				hw.UserAuthType != nil && *hw.UserAuthType != num(t, s.HardwareEnforced.UserAuthType) {
				t.Errorf("userAuthType %v", hw.UserAuthType)
			}
			if (hw.AuthTimeout == nil) != (s.HardwareEnforced.AuthTimeout == "") ||
				hw.AuthTimeout != nil && *hw.AuthTimeout != num(t, s.HardwareEnforced.AuthTimeout) {
				t.Errorf("authTimeout %v", hw.AuthTimeout)
			}
			if hw.OSVersion == nil || *hw.OSVersion != num(t, s.HardwareEnforced.OSVersion) {
				t.Errorf("osVersion %v", hw.OSVersion)
			}
			rot := hw.RootOfTrust
			if rot == nil || rot.DeviceLocked != s.HardwareEnforced.RootOfTrust.DeviceLocked ||
				rot.VerifiedBootState != bootStates[s.HardwareEnforced.RootOfTrust.VerifiedBootState] {
				t.Errorf("root of trust %+v", rot)
			}
			app := sw.AttestationApplicationID
			want := s.SoftwareEnforced.AttestationApplicationID
			if app == nil || len(app.Packages) != 1 || app.Packages[0].Name != want.Packages[0].Name ||
				len(app.SignatureDigests) != 1 || base64.StdEncoding.EncodeToString(app.SignatureDigests[0]) != want.Signatures[0] {
				t.Errorf("application id %+v", app)
			}

			created := time.UnixMilli(num(t, s.SoftwareEnforced.CreationDateTime))
			o := attest.Options{Now: created.Add(time.Minute), Package: want.Packages[0].Name}
			_, err = attest.Verify(chain, leaf.RawSubjectPublicKeyInfo, challenge, attest.PurposeSign, o)
			wantCheck(t, err, c.check)

			_, err = attest.Verify(chain, leaf.RawSubjectPublicKeyInfo, []byte("other"), attest.PurposeSign, o)
			wantCheck(t, err, attest.CheckChallenge)

			// Its remotely provisioned certificates expired long ago, but
			// were valid when the key was made.
			o.Now = created.AddDate(10, 0, 0)
			_, err = attest.Verify(chain, leaf.RawSubjectPublicKeyInfo, challenge, attest.PurposeSign, o)
			wantCheck(t, err, c.check)
			o.Now = created.AddDate(0, -3, 0)
			_, err = attest.Verify(chain, leaf.RawSubjectPublicKeyInfo, challenge, attest.PurposeSign, o)
			wantCheck(t, err, attest.CheckValidity)

			_, err = attest.Verify(chain, leaf.RawSubjectPublicKeyInfo, challenge, attest.PurposeSign, attest.Options{
				Now: created, Package: o.Package, Roots: newFixture(t).opts().Roots})
			wantCheck(t, err, attest.CheckRoot)
		})
	}
}
