package security

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ApprovalVector is one entry of vectors/approval.json.
type approvalVector struct {
	Name      string `json:"name"`
	PhoneID   string `json:"phone_id"`
	PCID      string `json:"pc_id"`
	Nonce     string `json:"nonce"`
	Reason    string `json:"reason"`
	Key       string `json:"key"`
	Signature string `json:"signature"`
}

type approvalVectors struct {
	Key         string           `json:"key"`
	Pin         string           `json:"pin"`
	MessageUTF8 string           `json:"message_utf8"`
	MessageHex  string           `json:"message_hex"`
	Valid       approvalVector   `json:"valid"`
	Invalid     []approvalVector `json:"invalid"`
}

func loadApprovalVectors(t *testing.T) approvalVectors {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(vectorsDir, "approval.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v approvalVectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func verifyVector(v approvalVector) error {
	key, err := ParseApprovalKey(v.Key)
	if err != nil {
		return err
	}
	return VerifyApproval(key, v.Signature, ApprovalMessage(v.PhoneID, v.PCID, v.Nonce, v.Reason))
}

func TestVectorApproval(t *testing.T) {
	v := loadApprovalVectors(t)
	msg := ApprovalMessage(v.Valid.PhoneID, v.Valid.PCID, v.Valid.Nonce, v.Valid.Reason)
	if string(msg) != v.MessageUTF8 || hex.EncodeToString(msg) != v.MessageHex {
		t.Errorf("message %q, want %q", msg, v.MessageUTF8)
	}
	if pin, err := ApprovalKeyPin(v.Key); err != nil || pin != v.Pin {
		t.Errorf("pin %s %v, want %s", pin, err, v.Pin)
	}
	if err := verifyVector(v.Valid); err != nil {
		t.Errorf("valid vector: %v", err)
	}
	if len(v.Invalid) < 6 {
		t.Fatalf("only %d invalid vectors", len(v.Invalid))
	}
	for _, inv := range v.Invalid {
		if err := verifyVector(inv); err == nil {
			t.Errorf("%s: accepted", inv.Name)
		}
	}

	// The key PEM matches the key, and our own signatures verify.
	pemData, err := os.ReadFile(filepath.Join(vectorsDir, "approval-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	priv, err := ParseKeyPEM(pemData)
	if err != nil {
		t.Fatal(err)
	}
	key, pin, err := EncodeApprovalKey(&priv.PublicKey)
	if err != nil || key != v.Key || pin != v.Pin {
		t.Errorf("encoded key %s %s %v", key, pin, err)
	}
	sig, err := SignApproval(priv, msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyApproval(&priv.PublicKey, sig, msg); err != nil {
		t.Errorf("own signature: %v", err)
	}
}

func TestParseApprovalKeyRejects(t *testing.T) {
	for _, k := range []string{"", "not base64!", B64([]byte("garbage"))} {
		if _, err := ParseApprovalKey(k); err == nil {
			t.Errorf("accepted %q", k)
		}
	}
}
